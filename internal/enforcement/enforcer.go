// Package enforcement turns billing webhooks (Stripe dunning and alerts,
// Metronome spend and credit alerts) into a billing state per tenant and,
// depending on ENFORCEMENT_MODE, reflects it on the tenant's clusters:
//
//	observe   record and expose the state (API, metrics) only
//	annotate  also annotate the tenant cluster workload and label its namespace
//	enforce   also label suspended namespaces vbilling.vcluster.com/suspended=true,
//	          which the shipped ValidatingAdmissionPolicy uses to deny new pods,
//	          and install a ValidatingAdmissionPolicy inside each of the
//	          tenant's clusters through its own API. Clusters with their own
//	          nodes (Private Nodes, Auto Nodes, Standalone) never send pods
//	          through the control plane cluster, so only the in-cluster policy
//	          reaches them.
//
// A tenant's state is the most severe of its holds. Billing webhooks own the
// "billing" hold; operators can set others ("support", "abuse", ...) through
// the API, and a payment never clears them.
//
// Running workloads are never deleted: suspension stops growth, not service.
package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// State is a tenant's billing standing.
type State string

const (
	Active     State = "active"
	Warning    State = "warning"    // budget or credit alert
	Delinquent State = "delinquent" // payment failed, retries pending
	Suspended  State = "suspended"  // unpaid, canceled, or a rule says so
)

func (s State) valid() bool { return s == Active || s == Warning || s == Delinquent || s == Suspended }

// Labels and annotations written to tenant clusters.
const (
	LabelState      = "vbilling.vcluster.com/billing-state"
	LabelSuspended  = "vbilling.vcluster.com/suspended"
	AnnotationState = "vbilling.vcluster.com/billing-state"
	AnnotationWhy   = "vbilling.vcluster.com/billing-reason"
	AnnotationAt    = "vbilling.vcluster.com/billing-state-at"
)

// Signal is a normalized billing event.
type Signal struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Type       string    `json:"type"`
	CustomerID string    `json:"customer_id"`
	Tenant     string    `json:"tenant"`
	State      State     `json:"state"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// TenantState is the current standing of one tenant: the most severe of
// its holds.
type TenantState struct {
	Tenant    string    `json:"tenant"`
	State     State     `json:"state"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	EventType string    `json:"event_type"`
	Since     time.Time `json:"since"`
	// Hold names the hold the state comes from ("billing", "abuse", ...).
	Hold string `json:"hold,omitempty"`
	// Holds are the independent reasons a tenant is not active, by name.
	Holds map[string]Hold `json:"holds,omitempty"`
}

// Hold is one reason for a tenant's state.
type Hold struct {
	State     State     `json:"state"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	EventType string    `json:"event_type"`
	Since     time.Time `json:"since"`
}

// HoldBilling is the hold billing webhooks set and clear.
const HoldBilling = "billing"

var severity = map[State]int{Active: 0, Warning: 1, Delinquent: 2, Suspended: 3}

// effective folds holds into a state: the most severe wins.
func effective(tenant string, holds map[string]Hold) TenantState {
	ts := TenantState{Tenant: tenant, State: Active, Holds: holds}
	names := make([]string, 0, len(holds))
	for n := range holds {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := holds[n]
		if severity[h.State] > severity[ts.State] || (ts.Since.IsZero() && h.State == ts.State) {
			ts.State, ts.Reason, ts.Source, ts.EventType, ts.Since, ts.Hold = h.State, h.Reason, h.Source, h.EventType, h.Since, n
			if n != HoldBilling {
				ts.Reason = n + ": " + h.Reason
			}
		}
	}
	return ts
}

// Options configure the enforcer.
type Options struct {
	Mode                   string // observe | annotate | enforce
	StripeWebhookSecret    string
	MetronomeWebhookSecret string
	Rules                  map[string]State // event type -> state overrides
	Clusters               func(tenant string) []discovery.TenantCluster
	Resolvers              map[string]destinations.CustomerResolver // "stripe", "metronome"
	State                  *kv.Store
	Telemetry              *telemetry.Registry
	Now                    func() time.Time
	// TenantClient connects to a tenant's cluster through its own API. When
	// set, enforce mode also installs an admission policy inside the
	// cluster. The credentials need create, update and delete on
	// validatingadmissionpolicies and validatingadmissionpolicybindings.
	TenantClient func(ctx context.Context, cl discovery.TenantCluster) (kubernetes.Interface, error)
}

// Enforcer is safe for concurrent use.
type Enforcer struct {
	kube kubernetes.Interface
	opts Options
	seen *destinations.SentCache

	mu     sync.Mutex
	states map[string]TenantState
}

func New(kube kubernetes.Interface, opts Options) *Enforcer {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.State == nil {
		opts.State = kv.Memory()
	}
	if opts.Telemetry == nil {
		opts.Telemetry = telemetry.New()
	}
	e := &Enforcer{kube: kube, opts: opts, seen: destinations.NewSentCache(10_000), states: map[string]TenantState{}}
	for tenant, raw := range opts.State.WithPrefix("state/") {
		var ts TenantState
		if json.Unmarshal([]byte(raw), &ts) == nil {
			if ts.Holds == nil && ts.State != "" && ts.State != Active {
				// v0.2 state: one reason, owned by billing.
				ts.Holds = map[string]Hold{HoldBilling: {State: ts.State, Reason: ts.Reason, Source: ts.Source, EventType: ts.EventType, Since: ts.Since}}
			}
			e.states[tenant] = ts
		}
	}
	return e
}

// ParseRules parses "event.type=state,..." (ENFORCEMENT_RULES).
func ParseRules(spec string) (map[string]State, error) {
	out := map[string]State{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		if !ok || !State(v).valid() {
			return nil, fmt.Errorf("enforcement rule %q must be event.type=active|warning|delinquent|suspended", item)
		}
		out[strings.TrimSpace(k)] = State(strings.TrimSpace(v))
	}
	return out, nil
}

// Errors the webhook handlers map to HTTP status codes.
var (
	// ErrIgnored: verified, but carries no billing signal (reply 200).
	ErrIgnored = errors.New("event type not relevant to billing state")
	// ErrSignature: the webhook failed verification (reply 400).
	ErrSignature = errors.New("webhook signature verification failed")
	// ErrUnknownCustomer: not one of our tenants (reply 200 so the provider stops retrying).
	ErrUnknownCustomer = errors.New("customer is not a vBilling tenant")
	// ErrDuplicate: this provider event was already applied (reply 200).
	ErrDuplicate = errors.New("event already applied")
)

// HandleStripe verifies and applies a Stripe webhook.
func (e *Enforcer) HandleStripe(ctx context.Context, body []byte, sigHeader string) (Signal, error) {
	if err := stripe.VerifySignature(body, sigHeader, e.opts.StripeWebhookSecret, stripe.DefaultTolerance, e.opts.Now()); err != nil {
		return Signal{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				Customer     string `json:"customer"`
				Status       string `json:"status"`
				AttemptCount int    `json:"attempt_count"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return Signal{}, fmt.Errorf("decode: %w", err)
	}
	sig := Signal{ID: "stripe:" + ev.ID, Source: "stripe", Type: ev.Type, CustomerID: ev.Data.Object.Customer, At: e.opts.Now().UTC()}
	switch ev.Type {
	case "invoice.payment_failed":
		sig.State, sig.Reason = Delinquent, fmt.Sprintf("invoice payment failed (attempt %d)", ev.Data.Object.AttemptCount)
	case "invoice.paid", "invoice.payment_succeeded":
		sig.State, sig.Reason = Active, "invoice paid"
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.resumed":
		switch ev.Data.Object.Status {
		case "past_due":
			sig.State = Delinquent
		case "unpaid", "canceled", "incomplete_expired":
			sig.State = Suspended
		case "active", "trialing":
			sig.State = Active
		}
		sig.Reason = "subscription " + ev.Data.Object.Status
	case "customer.subscription.deleted":
		sig.State, sig.Reason = Suspended, "subscription deleted"
	case "billing.alert.triggered":
		sig.State, sig.Reason = Warning, "usage alert triggered"
	}
	err := e.apply(ctx, &sig)
	return sig, err
}

// HandleMetronome verifies and applies a Metronome webhook.
func (e *Enforcer) HandleMetronome(ctx context.Context, body []byte, date, signature string) (Signal, error) {
	if err := metronome.VerifySignature(body, date, signature, e.opts.MetronomeWebhookSecret, metronome.DefaultTolerance, e.opts.Now()); err != nil {
		return Signal{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	var ev struct {
		ID         string         `json:"id"`
		Type       string         `json:"type"`
		CustomerID string         `json:"customer_id"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return Signal{}, fmt.Errorf("decode: %w", err)
	}
	cus := ev.CustomerID
	if v, ok := ev.Properties["customer_id"].(string); ok && v != "" {
		cus = v
	}
	sig := Signal{ID: "metronome:" + ev.ID, Source: "metronome", Type: ev.Type, CustomerID: cus, At: e.opts.Now().UTC()}
	switch {
	case strings.HasPrefix(ev.Type, "alerts."):
		sig.State, sig.Reason = Warning, strings.ReplaceAll(strings.TrimPrefix(ev.Type, "alerts."), "_", " ")
		if name, ok := ev.Properties["alert_name"].(string); ok && name != "" {
			sig.Reason = name
		}
	case ev.Type == "payment_gate.payment_status":
		switch ev.Properties["payment_status"] {
		case "failed":
			sig.State, sig.Reason = Delinquent, "threshold billing payment failed"
		case "paid":
			sig.State, sig.Reason = Active, "threshold billing payment succeeded"
		}
	}
	err := e.apply(ctx, &sig)
	return sig, err
}

func (e *Enforcer) apply(ctx context.Context, sig *Signal) error {
	if override, ok := e.opts.Rules[sig.Type]; ok {
		sig.State = override
		sig.Reason += " (rule: " + string(override) + ")"
	}
	if sig.State == "" {
		return ErrIgnored
	}
	if e.seen.Has(sig.ID) {
		return ErrDuplicate // providers retry and reorder; each event is applied once
	}
	resolver := e.opts.Resolvers[sig.Source]
	if resolver == nil {
		return fmt.Errorf("%w: no %s destination configured to resolve %s", ErrUnknownCustomer, sig.Source, sig.CustomerID)
	}
	tenant, ok := resolver.TenantForCustomer(ctx, sig.CustomerID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownCustomer, sig.CustomerID)
	}
	sig.Tenant = tenant
	if err := e.Set(ctx, tenant, sig.State, sig.Reason, sig.Source, sig.Type); err != nil {
		return err
	}
	e.seen.Add(sig.ID)
	return nil
}

// Set records the billing hold of a tenant and reflects the resulting state
// on its clusters.
func (e *Enforcer) Set(ctx context.Context, tenant string, state State, reason, source, eventType string) error {
	return e.SetHold(ctx, tenant, HoldBilling, state, reason, source, eventType)
}

// SetHold sets one named hold. Setting a hold to active removes it; the
// tenant's state is the most severe hold left.
func (e *Enforcer) SetHold(ctx context.Context, tenant, hold string, state State, reason, source, eventType string) error {
	if !state.valid() {
		return fmt.Errorf("invalid state %q", state)
	}
	if hold == "" {
		hold = HoldBilling
	}
	now := e.opts.Now().UTC()
	e.mu.Lock()
	prev := e.states[tenant]
	holds := map[string]Hold{}
	for k, v := range prev.Holds {
		holds[k] = v
	}
	if state == Active {
		delete(holds, hold)
	} else {
		h := Hold{State: state, Reason: reason, Source: source, EventType: eventType, Since: now}
		if old, ok := holds[hold]; ok && old.State == state {
			h.Since = old.Since
		}
		holds[hold] = h
	}
	ts := effective(tenant, holds)
	if len(holds) == 0 {
		ts = TenantState{Tenant: tenant, State: Active, Reason: reason, Source: source, EventType: eventType}
	}
	// Since is when the effective state began.
	if orActive(prev.State) == ts.State && !prev.Since.IsZero() {
		ts.Since = prev.Since
	} else {
		ts.Since = now
	}
	state = ts.State
	e.states[tenant] = ts
	e.mu.Unlock()
	b, _ := json.Marshal(ts)
	if err := e.opts.State.Set("state/"+tenant, string(b)); err != nil {
		return err
	}
	if prev.State != state {
		log.Printf("[enforcement] tenant %s: %s -> %s (%s via %s %s, mode=%s)", tenant, orActive(prev.State), state, ts.Reason, source, eventType, e.opts.Mode)
	}
	for _, s := range []State{Active, Warning, Delinquent, Suspended} {
		v := 0.0
		if s == state {
			v = 1
		}
		e.opts.Telemetry.Set("vbilling_tenant_billing_state", "1 for the tenant's current billing state.", map[string]string{"tenant": tenant, "state": string(s)}, v)
	}
	if e.opts.Mode == "observe" || e.kube == nil || e.opts.Clusters == nil {
		return nil
	}
	var errs []error
	for _, cl := range e.opts.Clusters(tenant) {
		if err := e.reflect(ctx, cl, ts, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cl.ExternalID(), err))
		}
		if e.opts.Mode == "enforce" && e.opts.TenantClient != nil {
			if err := e.reflectInside(ctx, cl, ts); err != nil {
				errs = append(errs, fmt.Errorf("%s (inside the cluster): %w", cl.ExternalID(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// PolicyName is the admission policy (and binding) vBilling installs inside
// a suspended tenant's clusters.
const PolicyName = "vbilling-suspended"

// LabelExempt on a namespace inside a tenant's cluster exempts it from the
// in-cluster policy (kube-system always is).
const LabelExempt = "vbilling.vcluster.com/enforcement-exempt"

func suspendedMessage(ts TenantState) string {
	if ts.Hold != "" && ts.Hold != HoldBilling {
		return "This tenant is suspended (" + ts.Reason + "). New workloads are blocked until the hold is lifted; running workloads are not affected."
	}
	return "This tenant is suspended for billing (" + ts.Reason + "). New workloads are blocked until the account is current; running workloads are not affected."
}

// reflectInside installs or removes the in-cluster admission policy.
func (e *Enforcer) reflectInside(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	kube, err := e.opts.TenantClient(ctx, cl)
	if err != nil {
		return err
	}
	if kube == nil {
		return nil
	}
	api := kube.AdmissionregistrationV1()
	if ts.State != Suspended {
		for _, del := range []func() error{
			func() error {
				return api.ValidatingAdmissionPolicyBindings().Delete(ctx, PolicyName, metav1.DeleteOptions{})
			},
			func() error { return api.ValidatingAdmissionPolicies().Delete(ctx, PolicyName, metav1.DeleteOptions{}) },
		} {
			if err := del(); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("remove admission policy: %w", err)
			}
		}
		return nil
	}
	fail := admissionv1.Fail
	forbidden := metav1.StatusReasonForbidden
	labels := map[string]string{"app.kubernetes.io/managed-by": "vbilling"}
	policy := &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName, Labels: labels},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}},
					{Key: LabelExempt, Operator: metav1.LabelSelectorOpDoesNotExist},
				}},
				ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
					Operations: []admissionv1.OperationType{admissionv1.Create},
					Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
				}}},
			},
			Validations: []admissionv1.Validation{{
				Expression: "false",
				Message:    suspendedMessage(ts),
				Reason:     &forbidden,
			}},
		},
	}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName, Labels: labels},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        PolicyName,
			ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		},
	}
	if cur, err := api.ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{}); err == nil {
		policy.ResourceVersion = cur.ResourceVersion
		_, err = api.ValidatingAdmissionPolicies().Update(ctx, policy, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update admission policy: %w", err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := api.ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create admission policy: %w", err)
		}
	} else {
		return fmt.Errorf("read admission policy: %w", err)
	}
	if _, err := api.ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create admission policy binding: %w", err)
	}
	return nil
}

func orActive(s State) State {
	if s == "" {
		return Active
	}
	return s
}

// Resync re-applies every recorded state to its tenant's clusters, without
// emitting events. A cluster created while its tenant was suspended, or one
// that was unreachable when the state changed (so a paying tenant could stay
// blocked), converges within one resync.
func (e *Enforcer) Resync(ctx context.Context) {
	if e.opts.Mode == "observe" || e.kube == nil || e.opts.Clusters == nil {
		return
	}
	for _, ts := range e.States() {
		for _, cl := range e.opts.Clusters(ts.Tenant) {
			if err := e.reflect(ctx, cl, ts, false); err != nil {
				log.Printf("[enforcement] resync %s: %v", cl.ExternalID(), err)
			}
			if e.opts.Mode == "enforce" && e.opts.TenantClient != nil {
				if err := e.reflectInside(ctx, cl, ts); err != nil {
					log.Printf("[enforcement] resync %s (inside the cluster): %v", cl.ExternalID(), err)
				}
			}
		}
	}
}

// reflect writes the state onto one tenant cluster.
func (e *Enforcer) reflect(ctx context.Context, cl discovery.TenantCluster, ts TenantState, withEvent bool) error {
	ann := map[string]any{AnnotationState: string(ts.State), AnnotationWhy: ts.Reason, AnnotationAt: ts.Since.Format(time.RFC3339)}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	var err error
	switch cl.Source {
	case "deployment":
		_, err = e.kube.AppsV1().Deployments(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		_, err = e.kube.AppsV1().StatefulSets(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	if err != nil {
		return fmt.Errorf("annotate workload: %w", err)
	}

	labels := map[string]any{LabelState: string(ts.State)}
	if e.opts.Mode == "enforce" && ts.State == Suspended {
		labels[LabelSuspended] = "true"
	} else {
		labels[LabelSuspended] = nil // merge patch: remove
	}
	nsPatch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if _, err := e.kube.CoreV1().Namespaces().Patch(ctx, cl.Namespace, types.MergePatchType, nsPatch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label namespace: %w", err)
	}

	if !withEvent {
		return nil
	}
	kind := "StatefulSet"
	if cl.Source == "deployment" {
		kind = "Deployment"
	}
	evType := corev1.EventTypeNormal
	if ts.State != Active {
		evType = corev1.EventTypeWarning
	}
	now := metav1.NewTime(e.opts.Now())
	_, err = e.kube.CoreV1().Events(cl.Namespace).Create(ctx, &corev1.Event{
		// Explicit name (not GenerateName) so the event is idempotent per state change.
		ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("%s.billing.%s.%x", cl.Name, ts.State, ts.Since.UnixNano())},
		InvolvedObject: corev1.ObjectReference{Kind: kind, APIVersion: "apps/v1", Namespace: cl.Namespace, Name: cl.Name},
		Reason:         "BillingState" + strings.ToUpper(string(ts.State[:1])) + string(ts.State[1:]),
		Message:        fmt.Sprintf("Billing state %s: %s", ts.State, ts.Reason),
		Type:           evType, Source: corev1.EventSource{Component: "vbilling"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}, metav1.CreateOptions{})
	if err != nil {
		log.Printf("[enforcement] event for %s: %v", cl.ExternalID(), err) // best effort
	}
	return nil
}

// States returns all recorded tenant states.
func (e *Enforcer) States() []TenantState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TenantState, 0, len(e.states))
	for _, s := range e.states {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

// StateOf returns a tenant's state (active when unknown).
func (e *Enforcer) StateOf(tenant string) TenantState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.states[tenant]; ok {
		return s
	}
	return TenantState{Tenant: tenant, State: Active}
}
