// Package enforcement turns billing webhooks (Stripe dunning and alerts,
// Metronome spend and credit alerts) into a billing state per tenant and,
// depending on ENFORCEMENT_MODE, reflects it on the tenant's clusters:
//
//	observe   record and expose the state (API, metrics) only
//	annotate  also annotate the tenant cluster workload and label its namespace
//	enforce   also label suspended namespaces vbilling.vcluster.com/suspended=true,
//	          which the shipped ValidatingAdmissionPolicy uses to deny new pods
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

	corev1 "k8s.io/api/core/v1"
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

// TenantState is the current standing of one tenant.
type TenantState struct {
	Tenant    string    `json:"tenant"`
	State     State     `json:"state"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	EventType string    `json:"event_type"`
	Since     time.Time `json:"since"`
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

// Set records a tenant's state and reflects it on its tenant clusters.
// It is also used by the API for manual overrides.
func (e *Enforcer) Set(ctx context.Context, tenant string, state State, reason, source, eventType string) error {
	if !state.valid() {
		return fmt.Errorf("invalid state %q", state)
	}
	now := e.opts.Now().UTC()
	e.mu.Lock()
	prev := e.states[tenant]
	ts := TenantState{Tenant: tenant, State: state, Reason: reason, Source: source, EventType: eventType, Since: now}
	if prev.State == state {
		ts.Since = prev.Since
	}
	e.states[tenant] = ts
	e.mu.Unlock()
	b, _ := json.Marshal(ts)
	if err := e.opts.State.Set("state/"+tenant, string(b)); err != nil {
		return err
	}
	if prev.State != state {
		log.Printf("[enforcement] tenant %s: %s -> %s (%s via %s %s, mode=%s)", tenant, orActive(prev.State), state, reason, source, eventType, e.opts.Mode)
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
		if err := e.reflect(ctx, cl, ts); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cl.ExternalID(), err))
		}
	}
	return errors.Join(errs...)
}

func orActive(s State) State {
	if s == "" {
		return Active
	}
	return s
}

// reflect writes the state onto one tenant cluster.
func (e *Enforcer) reflect(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
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
