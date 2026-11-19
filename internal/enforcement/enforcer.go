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
// "billing" hold; operators set others ("operator" by default, "support",
// "abuse", ...) through the API, and no webhook touches them. A billing
// webhook can only raise the billing hold. A payment or an active
// subscription never lowers it by itself: it makes vBilling read the billing
// platforms again, and the hold clears only when they show nothing owed.
// Webhooks apply in the order the billing platform says they happened, and
// each once.
//
// Suspension stops growth, not service: running workloads keep running.
// Pausing (state "paused", enforce mode) stops service: each of the tenant's
// clusters is paused the way `vcluster pause` does it, after its own nodes
// are drained, and billing for capacity stops once vBilling verified its
// workloads are gone (see pauseCluster and package pause). The tenant
// cannot restart it: its control plane is stopped, and vBilling pauses it
// again if anything else resumes it. Clearing the hold resumes it.
package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// State is a tenant's billing standing.
type State string

const (
	Active     State = "active"
	Warning    State = "warning"    // budget or credit alert
	Delinquent State = "delinquent" // payment failed, retries pending
	Suspended  State = "suspended"  // unpaid, canceled, or a rule says so
	Paused     State = "paused"     // workloads stopped and capacity not billed until the hold clears
)

func (s State) valid() bool {
	return s == Active || s == Warning || s == Delinquent || s == Suspended || s == Paused
}

// blocks reports whether the state blocks new workloads.
func (s State) blocks() bool { return s == Suspended || s == Paused }

// Labels and annotations written to tenant clusters.
const (
	LabelState      = "vbilling.vcluster.com/billing-state"
	LabelSuspended  = "vbilling.vcluster.com/suspended"
	AnnotationState = "vbilling.vcluster.com/billing-state"
	AnnotationWhy   = "vbilling.vcluster.com/billing-reason"
	AnnotationAt    = "vbilling.vcluster.com/billing-state-at"
	// AnnotationActivePrefix plus a cluster's name, on a namespace labeled
	// suspended, lets that cluster's synced pods start: the namespace hosts
	// a suspended tenant's cluster, but not only that one. The chart's
	// admission policy reads it.
	AnnotationActivePrefix = "vbilling.vcluster.com/active."
)

// Signal is a normalized billing event.
type Signal struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Type       string `json:"type"`
	CustomerID string `json:"customer_id"`
	// Tenant is the tenant the signal applies to; when a customer's
	// aliases name several, Tenant lists them comma-separated and Tenants
	// holds each.
	Tenant  string   `json:"tenant"`
	Tenants []string `json:"tenants,omitempty"`
	State   State    `json:"state"`
	Reason  string   `json:"reason"`
	// At is when vBilling received the event; EventAt is when the billing
	// platform says it happened, which orders it against the others.
	At      time.Time `json:"at"`
	EventAt time.Time `json:"event_at"`
	// Positive is true for a payment that succeeded, a paid invoice or an
	// active subscription. It lowers nothing by itself: it makes vBilling
	// read the billing platforms again (ArrearsReader), and the billing hold
	// clears only when they show nothing owed.
	Positive bool `json:"positive,omitempty"`
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
	// EventTimes is, per hold, when the newest change applied to it
	// happened: the billing platform's time for a webhook, vBilling's for an
	// operator. It outlives the hold, so a webhook about something older,
	// delivered late or retried, never sets the hold again.
	EventTimes map[string]time.Time `json:"event_times,omitempty"`
}

// Hold is one reason for a tenant's state. A billing hold records no more
// than this: what is owed is read from the billing platforms whenever a
// payment could clear it. (Holds an earlier v0.3.1 build stored also carry
// "causes", which are ignored.)
type Hold struct {
	State     State     `json:"state"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	EventType string    `json:"event_type"`
	Since     time.Time `json:"since"`
}

// HoldBilling is the hold billing webhooks raise, and clear once the billing
// platforms, read again, show nothing owed.
const HoldBilling = "billing"

// HoldOperator is the hold PUT /api/v1/billing-states sets when the request
// names none. No webhook touches it, nor any other hold but HoldBilling.
const HoldOperator = "operator"

var severity = map[State]int{Active: 0, Warning: 1, Delinquent: 2, Suspended: 3, Paused: 4}

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
	// State keeps billing states and the IDs of applied webhook events
	// (DATA_DIR/billing-state.json).
	State     *kv.Store
	Telemetry *telemetry.Registry
	Now       func() time.Time
	// TenantClient connects to a tenant's cluster through its own API. When
	// set, enforce mode also installs an admission policy inside the
	// cluster. The credentials need create, update and delete on
	// validatingadmissionpolicies and validatingadmissionpolicybindings.
	// It returns nil and no error for a cluster vBilling has no
	// credentials for.
	TenantClient func(ctx context.Context, cl discovery.TenantCluster) (kubernetes.Interface, error)
	// IsTenant reports whether an ID is a tenant vBilling knows besides
	// those holding a billing state: discovered from a cluster, or named by
	// the records configuration. A webhook for a billing customer with
	// several aliases applies to the ones that are known tenants.
	IsTenant func(id string) bool
	// Neighbors lists the clusters in a control plane cluster namespace, by
	// name, with the tenant each bills to. The namespace's labels then
	// reflect every one of them: two tenants' clusters can share one.
	Neighbors func(namespace string) map[string]string

	// Pausing (state paused, enforce mode).
	Pauses *pause.Store // pause spans, so capacity is not billed while paused
	// Freeze stores a cluster's inventory right before its control plane
	// stops, to meter its volumes from while paused. Set it only when
	// metering reads clusters' own APIs (TENANT_API): without it, nothing
	// on a cluster's own nodes is billed.
	Freeze func(ctx context.Context, cl discovery.TenantCluster) error
	// OwnNodes reports whether metering bills nodes of a cluster's own
	// (known is false when it cannot tell). A pause that cannot drain them
	// (no TenantClient) is verified only for a cluster that has none.
	OwnNodes func(cl discovery.TenantCluster) (has, known bool)
	// OnPause tells metering a cluster was paused or resumed.
	OnPause func(id string, paused bool)
	// DrainTimeout bounds each wait of a pause: for the workloads on a
	// cluster's own nodes and its synced pods to stop before what is left
	// is deleted without a grace period, and for its control plane to stop
	// (default 2m). A drain that keeps failing for that long stops the
	// control plane anyway.
	DrainTimeout time.Duration
	// KeepNamespaces are the namespaces whose DaemonSets (node
	// infrastructure) keep running while a cluster is paused
	// (default DefaultKeepNamespaces).
	KeepNamespaces []string
	// Gateway, in enforce mode, blocks a suspended or paused tenant's
	// requests at its AI gateway and unblocks them when it is active again.
	Gateway Gateway
}

// Gateway stops and restarts a tenant's requests at its AI gateway.
type Gateway interface {
	Apply(ctx context.Context, tenant string, blocked bool) error
}

// AliasResolver is implemented by customer resolvers that can list every
// ingest alias of a billing customer (Metronome). A webhook for such a
// customer applies to the tenant TenantForCustomer maps it to and to each
// alias that is a tenant vBilling knows, or, when none is, to every alias.
// One customer's prepaid commit can fund several tenants, so an empty
// wallet pauses each of them; and a customer created outside vBilling may
// hold an alias for something else (an identity alias next to the compute
// one), so no single alias stands for the customer.
type AliasResolver interface {
	CustomerAliases(ctx context.Context, customerID string) ([]string, error)
}

// TenantKnower is implemented by alias resolvers that know aliases as
// vBilling's tenants themselves (Metronome: the aliases vBilling sends
// usage under). Those count as known tenants too.
type TenantKnower interface {
	KnowsTenant(alias string) bool
}

// ArrearsReader is implemented by customer resolvers that can read what a
// billing customer owes now (Stripe, Metronome). A positive webhook never
// lowers a billing hold by itself: the hold clears only once every billing
// platform vBilling can read shows the tenant owing nothing.
type ArrearsReader interface {
	// Owed reads what the billing platform shows a customer owing now, each
	// thing described; nothing when it owes nothing. customerID is the
	// customer a webhook named; empty, the platform reads the customer it
	// bills the tenant as, and a tenant it has no customer for owes nothing
	// there. counts reports whether ENFORCEMENT_RULES map a webhook event
	// type to a state above warning (for Metronome, the threshold
	// notifications that count). An error means the platform could not be
	// read.
	Owed(ctx context.Context, tenant, customerID string, counts func(eventType string) bool) ([]string, error)
}

// The billing adapters are the customer resolvers (cmd/vbilling wires
// them): they must stay readable again, and Metronome's must list aliases.
var (
	_ ArrearsReader = (*stripe.Adapter)(nil)
	_ ArrearsReader = (*metronome.Adapter)(nil)
	_ AliasResolver = (*metronome.Adapter)(nil)
	_ TenantKnower  = (*metronome.Adapter)(nil)
)

// settleRetry is how long a Metronome payment that finds a threshold
// notification still in alarm (or still evaluating) is answered with an
// error, so Metronome delivers it again: Metronome evaluates a notification
// again when a commit changes, and notifies "within minutes", so the read
// can trail the payment. After that the payment is applied, and the hold
// stays.
const settleRetry = time.Hour

// ErrNotSettled is returned for a Metronome payment that finds a threshold
// notification that counts still in alarm, within settleRetry of the
// payment (the provider retries it).
var ErrNotSettled = errors.New("payment does not settle the billing hold yet")

// Enforcer is safe for concurrent use.
type Enforcer struct {
	kube kubernetes.Interface
	opts Options

	// setMu serializes state changes, so each is decided on the state the
	// previous one left and written to disk in order.
	setMu  sync.Mutex
	mu     sync.Mutex
	states map[string]TenantState

	// applied are the webhook events already applied (ID -> when), kept
	// for appliedKeep and at most appliedMax of them, in State.
	evMu    sync.Mutex
	applied map[string]time.Time

	bg                         sync.WaitGroup // background pauses and resumes
	resumeTimeout, resumeEvery time.Duration
	wmu                        sync.Mutex
	workers                    map[string]*worker
	status                     map[string]PauseStatus
	// Pause bookkeeping per cluster (see pause.go), under wmu.
	failing    map[string]time.Time // first failed drain of a streak
	checked    map[string]ownCheck  // what the last pause pass found on its own nodes
	stopped    map[string]time.Time // last time its control plane was seen stopped
	verifiedAt map[string]time.Time // external clusters: last pass that found nothing running
	older      map[string]bool      // pauses an older version opened, without verifying them
}

// Applied webhook event IDs are kept this long, and at most appliedMax of
// them (a variable for tests).
const (
	appliedKey  = "applied-events"
	appliedKeep = 30 * 24 * time.Hour
)

var appliedMax = 20000

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
	e := &Enforcer{kube: kube, opts: opts, states: map[string]TenantState{}, applied: map[string]time.Time{},
		resumeTimeout: 10 * time.Minute, resumeEvery: 5 * time.Second,
		workers: map[string]*worker{}, status: map[string]PauseStatus{},
		failing: map[string]time.Time{}, checked: map[string]ownCheck{}, stopped: map[string]time.Time{}, verifiedAt: map[string]time.Time{},
		older: map[string]bool{}}
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
	if raw, ok := opts.State.Get(appliedKey); ok {
		if err := json.Unmarshal([]byte(raw), &e.applied); err != nil {
			log.Printf("[enforcement] applied webhook events unreadable, starting over: %v", err)
			e.applied = map[string]time.Time{}
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
			return nil, fmt.Errorf("enforcement rule %q must be event.type=active|warning|delinquent|suspended|paused", item)
		}
		out[strings.TrimSpace(k)] = State(strings.TrimSpace(v))
	}
	return out, nil
}

// Errors the webhook handlers map to HTTP status codes.
var (
	// ErrIgnored: verified, but carries no billing signal or changes
	// nothing (reply 200).
	ErrIgnored = errors.New("event type not relevant to billing state")
	// ErrSignature: the webhook failed verification (reply 400).
	ErrSignature = errors.New("webhook signature verification failed")
	// ErrUnknownCustomer: not one of our tenants (reply 200 so the provider stops retrying).
	ErrUnknownCustomer = errors.New("customer is not a vBilling tenant")
	// ErrDuplicate: this provider event was already applied (reply 200).
	ErrDuplicate = errors.New("event already applied")
)

// HandleStripe verifies and applies a Stripe webhook. Events are ordered by
// their created time.
func (e *Enforcer) HandleStripe(ctx context.Context, body []byte, sigHeader string) (Signal, error) {
	if err := stripe.VerifySignature(body, sigHeader, e.opts.StripeWebhookSecret, stripe.DefaultTolerance, e.opts.Now()); err != nil {
		return Signal{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	var ev struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return Signal{}, fmt.Errorf("decode: %w", err)
	}
	var obj struct {
		Customer     string `json:"customer"`
		Status       string `json:"status"`
		AttemptCount int    `json:"attempt_count"`
	}
	if len(ev.Data.Object) > 0 {
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return Signal{}, fmt.Errorf("decode: %w", err)
		}
	}
	now := e.opts.Now().UTC()
	sig := Signal{ID: "stripe:" + ev.ID, Source: "stripe", Type: ev.Type, CustomerID: obj.Customer, At: now, EventAt: now}
	if ev.Created > 0 {
		sig.EventAt = time.Unix(ev.Created, 0).UTC()
	}
	switch ev.Type {
	case "invoice.payment_failed":
		sig.State, sig.Reason = Delinquent, fmt.Sprintf("invoice payment failed (attempt %d)", obj.AttemptCount)
	case "invoice.paid", "invoice.payment_succeeded":
		sig.State, sig.Reason, sig.Positive = Active, "invoice paid", true
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.resumed":
		switch obj.Status {
		case "past_due":
			sig.State = Delinquent
		case "unpaid", "canceled", "incomplete_expired":
			sig.State = Suspended
		case "active", "trialing":
			sig.State, sig.Positive = Active, true
		}
		sig.Reason = "subscription " + obj.Status
	case "customer.subscription.deleted":
		sig.State, sig.Reason = Suspended, "subscription deleted"
	case "billing.alert.triggered":
		sig.State, sig.Reason = Warning, "usage alert triggered"
	}
	err := e.apply(ctx, &sig)
	return sig, err
}

// HandleMetronome verifies and applies a Metronome webhook. Events are
// ordered by the notification's own timestamp (top level, or under
// properties for alerts and payment gate notifications); a notification
// without one is ordered by its signed X-Metronome-Date header, which is
// when it was sent.
func (e *Enforcer) HandleMetronome(ctx context.Context, body []byte, date, signature string) (Signal, error) {
	if err := metronome.VerifySignature(body, date, signature, e.opts.MetronomeWebhookSecret, metronome.DefaultTolerance, e.opts.Now()); err != nil {
		return Signal{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	var ev struct {
		ID         string         `json:"id"`
		Type       string         `json:"type"`
		Timestamp  string         `json:"timestamp"`
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
	now := e.opts.Now().UTC()
	sig := Signal{ID: "metronome:" + ev.ID, Source: "metronome", Type: ev.Type, CustomerID: cus, At: now, EventAt: now}
	inner, _ := ev.Properties["timestamp"].(string)
	if t, ok := firstTime(ev.Timestamp, inner); ok {
		sig.EventAt = t
	} else if t, err := http.ParseTime(date); err == nil {
		sig.EventAt = t.UTC()
	}
	str := func(k string) string { v, _ := ev.Properties[k].(string); return v }
	switch {
	case strings.HasPrefix(ev.Type, "alerts."):
		sig.State, sig.Reason = Warning, strings.ReplaceAll(strings.TrimPrefix(ev.Type, "alerts."), "_", " ")
		if name := str("alert_name"); name != "" {
			sig.Reason = name
		}
	case ev.Type == "payment_gate.payment_status":
		switch ev.Properties["payment_status"] {
		case "failed":
			sig.State, sig.Reason = Delinquent, "threshold billing payment failed"
		case "paid":
			sig.State, sig.Reason, sig.Positive = Active, "threshold billing payment succeeded", true
		}
	}
	err := e.apply(ctx, &sig)
	return sig, err
}

// firstTime parses the first RFC 3339 timestamp among values.
func firstTime(values ...string) (time.Time, bool) {
	for _, v := range values {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// apply records a webhook's signal on the billing hold of each tenant it
// names. The event counts as applied once the states are stored: a retry
// is then a duplicate, and clusters that could not be reached converge at
// the next resync (the error still reaches the provider).
func (e *Enforcer) apply(ctx context.Context, sig *Signal) error {
	if override, ok := e.opts.Rules[sig.Type]; ok {
		sig.State = override
		sig.Reason += " (rule: " + string(override) + ")"
	}
	if sig.State == "" {
		return ErrIgnored
	}
	if e.wasApplied(sig.ID) {
		return ErrDuplicate // providers retry and reorder; each event is applied once
	}
	resolver := e.opts.Resolvers[sig.Source]
	if resolver == nil {
		return fmt.Errorf("%w: no %s destination configured to resolve %s", ErrUnknownCustomer, sig.Source, sig.CustomerID)
	}
	tenants, err := e.tenantsFor(ctx, resolver, sig.CustomerID)
	if err != nil {
		return err
	}
	sig.Tenants, sig.Tenant = tenants, strings.Join(tenants, ",")
	// A positive signal lowers nothing by itself: what each held tenant still
	// owes is read from the billing platforms before anything is stored. A
	// platform that cannot be read fails the whole event, which the provider
	// retries.
	type reading struct {
		read bool
		owed []string
	}
	readings := map[string]reading{}
	if sig.Positive {
		reads := map[string]readResult{}
		for _, tenant := range tenants {
			owed, read, err := e.stillOwed(ctx, tenant, sig, reads)
			if err != nil {
				return err
			}
			readings[tenant] = reading{read, owed}
		}
	}
	type change struct{ prev, ts TenantState }
	var changed []change
	var stale, unread []string
	for _, tenant := range tenants {
		r := readings[tenant]
		ts, prev, res, err := e.store(tenant, update{hold: HoldBilling, state: sig.State, reason: sig.Reason,
			source: sig.Source, eventType: sig.Type, webhook: true, positive: sig.Positive, at: sig.EventAt,
			read: r.read, owed: r.owed})
		if err != nil {
			return err // not marked applied: the provider retries it
		}
		switch res.kind {
		case outStale:
			stale = append(stale, tenant+": "+res.why)
			continue
		case outKept:
			// Applied: under the hold rules it changes nothing.
			log.Printf("[enforcement] tenant %s: %s %s leaves the billing hold as it is: %s", tenant, sig.Source, sig.Type, res.why)
			continue
		case outUnread:
			unread = append(unread, tenant)
			continue
		}
		changed = append(changed, change{prev, ts})
	}
	// A payment that found a billing hold it read nothing for is not
	// remembered as applied: its retry reads the billing platforms.
	if len(unread) == 0 {
		if err := e.markApplied(sig.ID); err != nil {
			return err
		}
	}
	if len(stale) == len(tenants) {
		return fmt.Errorf("%w: %s", ErrIgnored, strings.Join(stale, "; "))
	}
	var errs []error
	for _, c := range changed {
		errs = append(errs, e.reflectState(ctx, c.prev, c.ts, sig.Source, sig.Type))
	}
	if len(unread) > 0 {
		errs = append(errs, fmt.Errorf("the billing hold of tenant(s) %s was raised while %s %s was being applied: retried, to read the billing platforms again",
			strings.Join(unread, ", "), sig.Source, sig.Type))
	}
	return errors.Join(errs...)
}

// readResult is one ArrearsReader answer, shared by the tenants of one
// customer.
type readResult struct {
	owed []string
	err  error
}

// countsAbove reports whether ENFORCEMENT_RULES map a webhook event type to
// a state above warning: the threshold notifications that keep a billing
// hold while they are in alarm.
func (e *Enforcer) countsAbove(eventType string) bool {
	s, ok := e.opts.Rules[eventType]
	return ok && severity[s] > severity[Warning]
}

// stillOwed reads, for a positive signal, what keeps a tenant's billing
// hold: what each billing platform vBilling can read again (ArrearsReader)
// shows owed now. The signal's platform is read for the signal's customer,
// and every other one for the customer it bills the tenant as, so a Stripe
// payment never resumes a tenant whose Metronome wallet is still empty.
// Nothing owed anywhere clears the hold. read is false when the tenant has
// no billing hold this signal could lower, and nothing was read. reads
// shares answers between the tenants of one customer.
//
// A platform that cannot be read is an error: nothing is lowered on a
// guess. The signal's own platform, when it cannot be read again at all,
// confirms nothing, and the hold stays. A Metronome payment that finds a
// threshold notification still in alarm is answered with ErrNotSettled for
// settleRetry, so Metronome delivers it again once the notification was
// evaluated after the top-up.
func (e *Enforcer) stillOwed(ctx context.Context, tenant string, sig *Signal, reads map[string]readResult) (owed []string, read bool, err error) {
	ts := e.StateOf(tenant)
	if h, held := ts.Holds[HoldBilling]; !held || orActive(h.State) == Active {
		return nil, false, nil
	}
	if last := ts.EventTimes[HoldBilling]; sig.EventAt.UTC().Truncate(time.Second).Before(last.Truncate(time.Second)) {
		return nil, false, nil // older than the hold's newest event: store ignores it
	}
	providers := make([]string, 0, len(e.opts.Resolvers))
	for p := range e.opts.Resolvers {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		customer := ""
		if p == sig.Source {
			customer = sig.CustomerID
		}
		reader, ok := e.opts.Resolvers[p].(ArrearsReader)
		if !ok {
			if p == sig.Source {
				owed = append(owed, p+" cannot be read again, so nothing confirms what customer "+customer+" owes")
			}
			continue
		}
		key := p + "\x00" + customer
		if customer == "" {
			key += "\x00" + tenant
		}
		res, done := reads[key]
		if !done {
			res.owed, res.err = reader.Owed(ctx, tenant, customer, e.countsAbove)
			reads[key] = res
		}
		if res.err != nil {
			return nil, false, fmt.Errorf("read %s again before clearing the billing hold of tenant %s: %w", p, tenant, res.err)
		}
		if len(res.owed) > 0 && p == sig.Source && p == "metronome" && e.opts.Now().Sub(sig.EventAt) < settleRetry {
			return nil, false, fmt.Errorf("%w: tenant %s: Metronome still shows customer %s owing: %s (retried until %s)", ErrNotSettled,
				tenant, customer, strings.Join(res.owed, "; "), sig.EventAt.Add(settleRetry).UTC().Format(time.RFC3339))
		}
		for _, o := range res.owed {
			owed = append(owed, p+": "+o)
		}
	}
	return owed, true, nil
}

// tenantsFor resolves a billing customer to the tenants its signals apply
// to: the one it is mapped to and, for a resolver that lists a customer's
// aliases (AliasResolver), every alias that is a tenant vBilling knows, or
// all of the aliases when none is known. The aliases are read even for a
// mapped customer, and a failed read is an error the provider retries: a
// signal applied to one of the tenants a wallet funds would leave the
// others running on it.
func (e *Enforcer) tenantsFor(ctx context.Context, resolver destinations.CustomerResolver, cus string) ([]string, error) {
	mapped, isMapped := resolver.TenantForCustomer(ctx, cus)
	ar, ok := resolver.(AliasResolver)
	if !ok {
		if isMapped {
			return []string{mapped}, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrUnknownCustomer, cus)
	}
	list, err := ar.CustomerAliases(ctx, cus)
	if err != nil {
		return nil, fmt.Errorf("list the aliases of customer %s: %w", cus, err)
	}
	knower, _ := resolver.(TenantKnower)
	seen := map[string]bool{}
	var aliases, known []string
	if isMapped {
		seen[mapped] = true
		known = append(known, mapped)
	}
	for _, a := range list {
		if a = strings.TrimSpace(a); a == "" || seen[a] {
			continue
		}
		seen[a] = true
		aliases = append(aliases, a)
		if e.isTenant(a) || knower != nil && knower.KnowsTenant(a) {
			known = append(known, a)
		}
	}
	if len(known) > 0 {
		return known, nil
	}
	if len(aliases) == 0 {
		return nil, fmt.Errorf("%w: %s has no ingest alias", ErrUnknownCustomer, cus)
	}
	return aliases, nil
}

// isTenant reports whether an ID is a tenant vBilling knows.
func (e *Enforcer) isTenant(id string) bool {
	return e.HasState(id) || e.opts.IsTenant != nil && e.opts.IsTenant(id)
}

func (e *Enforcer) wasApplied(id string) bool {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	_, ok := e.applied[id]
	return ok
}

// markApplied durably records a webhook event as applied, forgetting those
// older than appliedKeep and, past appliedMax, the oldest.
func (e *Enforcer) markApplied(id string) error {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	now := e.opts.Now().UTC()
	next := make(map[string]time.Time, len(e.applied)+1)
	for k, at := range e.applied {
		if now.Sub(at) < appliedKeep {
			next[k] = at
		}
	}
	next[id] = now
	if over := len(next) - appliedMax; over > 0 {
		ids := make([]string, 0, len(next))
		for k := range next {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool { return next[ids[i]].Before(next[ids[j]]) })
		for _, k := range ids[:over] {
			delete(next, k)
		}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := e.opts.State.Set(appliedKey, string(b)); err != nil {
		return fmt.Errorf("record applied event: %w", err)
	}
	e.applied = next
	return nil
}

// Set records the billing hold of a tenant and reflects the resulting state
// on its clusters. It is the operator's: any state goes.
func (e *Enforcer) Set(ctx context.Context, tenant string, state State, reason, source, eventType string) error {
	return e.SetHold(ctx, tenant, HoldBilling, state, reason, source, eventType)
}

// SetHold sets one named hold, as an operator does through the API: any
// state goes. Setting a hold to active removes it; the tenant's state is
// the most severe hold left.
func (e *Enforcer) SetHold(ctx context.Context, tenant, hold string, state State, reason, source, eventType string) error {
	if !state.valid() {
		return fmt.Errorf("invalid state %q", state)
	}
	ts, prev, _, err := e.store(tenant, update{hold: hold, state: state, reason: reason, source: source, eventType: eventType})
	if err != nil {
		return err
	}
	return e.reflectState(ctx, prev, ts, source, eventType)
}

// update is one change to a hold.
type update struct {
	hold, reason, source, eventType string
	state                           State
	// webhook: a billing platform's event, applied only if it is not older
	// than the newest one applied to the hold, and only if it raises the
	// hold, unless it is positive.
	webhook, positive bool
	at                time.Time // when the platform says it happened
	// A positive webhook clears the hold only when the billing platforms
	// were read again for it (read, see stillOwed) and showed nothing owed.
	read bool
	owed []string
}

// storeResult says what store did with an update.
type storeResult struct {
	kind int    // outChanged, outKept, outStale or outUnread
	why  string // for outKept and outStale
}

const (
	outChanged = iota // stored
	outKept           // the hold stays as it is: a lesser signal, or a payment while something is owed
	outStale          // older than the newest event applied to the hold
	outUnread         // a payment found a billing hold it read nothing for (raised meanwhile): nothing stored
)

// store applies an update and writes the tenant's state to disk before it
// is visible. Updates are serialized, so they are decided and written in
// order.
func (e *Enforcer) store(tenant string, u update) (ts, prev TenantState, res storeResult, err error) {
	if u.hold == "" {
		u.hold = HoldBilling
	}
	e.setMu.Lock()
	defer e.setMu.Unlock()
	e.mu.Lock()
	prev = e.states[tenant]
	e.mu.Unlock()
	now := e.opts.Now().UTC()
	at := now
	holds := map[string]Hold{}
	for k, v := range prev.Holds {
		holds[k] = v
	}
	kept := ""      // why the update leaves the hold as it is
	advance := true // whether the hold's newest event time moves to this one
	if u.webhook {
		if !u.at.IsZero() {
			at = u.at.UTC()
		}
		// Providers stamp events at different precisions (a payload
		// timestamp with milliseconds, a signed Date header in whole
		// seconds), so events are ordered by the second, and events in the
		// same second apply in arrival order: a payment right after an
		// alert must never look older than it.
		if last := prev.EventTimes[u.hold]; at.Truncate(time.Second).Before(last.Truncate(time.Second)) {
			return prev, prev, storeResult{outStale, fmt.Sprintf("the event happened at %s, before the newest one applied to the %s hold (%s)",
				at.Format(time.RFC3339), u.hold, last.Format(time.RFC3339))}, nil
		}
		cur, held := holds[u.hold]
		curState := orActive(cur.State)
		switch {
		case u.positive && held && curState != Active && !u.read:
			// Raised after the payment read the billing platforms (it found
			// no hold then): nothing confirms the new one is paid.
			return prev, prev, storeResult{kind: outUnread}, nil
		case u.positive && held && curState != Active && len(u.owed) > 0:
			// A payment while something is still owed changes nothing, and
			// does not move the hold's event time either: an older event that
			// raises the hold, delivered late, still applies.
			kept = "still owed: " + strings.Join(u.owed, "; ")
			advance = false
		case u.positive:
			// Nothing owed (or no hold): the hold clears.
		case severity[u.state] < severity[curState]:
			// A lesser signal: the hold stays. It does not move the hold's
			// event time, so a payment that happened before it still applies.
			kept = fmt.Sprintf("%s is less severe than its %s; only a payment or an active subscription lowers it, once nothing is owed", u.state, curState)
			advance = false
		}
	}
	if kept == "" {
		if u.state == Active {
			delete(holds, u.hold)
		} else {
			// An operator's change replaces the hold.
			h := Hold{State: u.state, Reason: u.reason, Source: u.source, EventType: u.eventType, Since: now}
			if old, ok := holds[u.hold]; ok && old.State == u.state {
				h.Since = old.Since
			}
			holds[u.hold] = h
		}
	}
	ts = effective(tenant, holds)
	if len(holds) == 0 {
		ts = TenantState{Tenant: tenant, State: Active, Reason: u.reason, Source: u.source, EventType: u.eventType}
	}
	// Since is when the effective state began.
	if orActive(prev.State) == ts.State && !prev.Since.IsZero() {
		ts.Since = prev.Since
	} else {
		ts.Since = now
	}
	ts.EventTimes = map[string]time.Time{}
	for k, v := range prev.EventTimes {
		ts.EventTimes[k] = v
	}
	if advance && at.After(ts.EventTimes[u.hold]) {
		ts.EventTimes[u.hold] = at
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return prev, prev, storeResult{}, err
	}
	if err := e.opts.State.Set("state/"+tenant, string(b)); err != nil {
		return prev, prev, storeResult{}, fmt.Errorf("store state of tenant %s: %w", tenant, err)
	}
	e.mu.Lock()
	e.states[tenant] = ts
	e.mu.Unlock()
	if kept != "" {
		return ts, prev, storeResult{outKept, kept}, nil
	}
	return ts, prev, storeResult{}, nil
}

// reflectState reports a stored state and reflects it on the tenant's
// gateway identities and clusters.
func (e *Enforcer) reflectState(ctx context.Context, prev, ts TenantState, source, eventType string) error {
	tenant, state := ts.Tenant, ts.State
	if prev.State != state {
		log.Printf("[enforcement] tenant %s: %s -> %s (%s via %s %s, mode=%s)", tenant, orActive(prev.State), state, ts.Reason, source, eventType, e.opts.Mode)
	}
	for _, s := range []State{Active, Warning, Delinquent, Suspended, Paused} {
		v := 0.0
		if s == state {
			v = 1
		}
		e.opts.Telemetry.Set("vbilling_tenant_billing_state", "1 for the tenant's current billing state.", map[string]string{"tenant": tenant, "state": string(s)}, v)
	}
	var errs []error
	if e.opts.Mode == "enforce" && e.opts.Gateway != nil {
		errs = append(errs, e.opts.Gateway.Apply(ctx, tenant, state.blocks()))
	}
	if e.opts.Mode == "observe" || e.kube == nil || e.opts.Clusters == nil {
		return errors.Join(errs...)
	}
	for _, cl := range e.opts.Clusters(tenant) {
		errs = append(errs, e.converge(ctx, tenant, cl, ts, true))
	}
	return errors.Join(errs...)
}

// converge makes one cluster match its tenant's state: annotations and
// labels on the control plane cluster, then, in enforce mode, the policy
// inside the cluster, and pause or resume.
func (e *Enforcer) converge(ctx context.Context, tenant string, cl discovery.TenantCluster, ts TenantState, withEvent bool) error {
	var errs []error
	if err := e.reflect(ctx, cl, ts, withEvent); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", cl.ExternalID(), err))
	}
	if e.opts.Mode != "enforce" {
		return errors.Join(errs...)
	}
	if ts.State != Paused {
		e.clearFailing(cl.ExternalID()) // a later pause starts its retries afresh
	}
	if e.needsWorker(ctx, cl, ts) {
		e.kick(tenant, cl) // pausing or resuming, in the background
		return errors.Join(errs...)
	}
	if e.opts.TenantClient != nil && !cl.Paused {
		if err := e.reflectInside(ctx, cl, ts); err != nil {
			errs = append(errs, fmt.Errorf("%s (inside the cluster): %w", cl.ExternalID(), err))
		}
	}
	return errors.Join(errs...)
}

// PolicyName is the admission policy (and binding) vBilling installs inside
// a suspended tenant's clusters.
const PolicyName = "vbilling-suspended"

func suspendedMessage(ts TenantState) string {
	if ts.State == Paused {
		if ts.Hold != "" && ts.Hold != HoldBilling {
			return "This tenant is paused (" + ts.Reason + "). Workloads stay stopped until the hold is lifted."
		}
		return "This tenant is paused for billing (" + ts.Reason + "). Workloads stay stopped until the account is current."
	}
	if ts.Hold != "" && ts.Hold != HoldBilling {
		return "This tenant is suspended (" + ts.Reason + "). New workloads are blocked until the hold is lifted; running workloads are not affected."
	}
	return "This tenant is suspended for billing (" + ts.Reason + "). New workloads are blocked until the account is current; running workloads are not affected."
}

// reflectInside installs or removes the in-cluster admission policy, and
// lifts the pause admission policy and the pause taint from the cluster
// when it is not paused.
func (e *Enforcer) reflectInside(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	kube, err := e.opts.TenantClient(ctx, cl)
	if err != nil {
		return err
	}
	if kube == nil {
		return nil
	}
	if err := e.policyInside(ctx, kube, ts); err != nil {
		return err
	}
	if ts.State != Paused {
		if err := removePolicy(ctx, kube, PausedPolicyName); apierrors.IsForbidden(err) {
			// Credentials that may not read or delete vbilling-paused never
			// let a pause install it (it reads the policy first), so there
			// is nothing to remove: lift the taint all the same.
			log.Printf("[enforcement] %s: %v (grant get, update and delete on %s to vbilling-enforcer inside the cluster)", cl.ExternalID(), err, PausedPolicyName)
		} else if err != nil {
			return err
		}
		return e.untaint(ctx, kube)
	}
	return nil
}

// policyInside installs the admission policy inside a cluster while its
// tenant's state blocks new workloads, and removes it otherwise. The
// policy and its binding are set to vBilling's spec at every pass, whoever
// created or changed them; only kube-system is exempt. The tenant is
// cluster admin inside its cluster and can still delete them between two
// passes or run pods in kube-system, so suspension inside a tenant's
// cluster is best effort: a pause is the hard stop.
func (e *Enforcer) policyInside(ctx context.Context, kube kubernetes.Interface, ts TenantState) error {
	if !ts.State.blocks() {
		return removePolicy(ctx, kube, PolicyName)
	}
	return applyPolicy(ctx, kube, PolicyName, podPolicy(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"}},
	}}, []admissionv1.OperationType{admissionv1.Create}, admissionv1.Validation{Expression: mirrorPod, Message: suspendedMessage(ts)}))
}

// PausedPolicyName is the admission policy (and binding) vBilling installs
// inside a paused cluster before it drains it, and removes on resume. In
// every namespace, kube-system included, it denies every new pod except the
// kubelet's mirror pods (createdMirror), and every update that removes a
// pod's scheduling gates (gatesKept). A pod started after the pause's last
// check needs it removed or changed first, which the checks look for.
const PausedPolicyName = "vbilling-paused"

// mirrorPod is the expression both in-cluster policies let pods through
// with: a mirror pod, the API's copy of a static pod. A static pod runs
// from a file on its node whatever the API says, and its mirror pod is how
// a drain or a check sees it, so it must never be denied. A pod that only
// claims to be one, with no static pod behind it, is never run: the kubelet
// deletes it.
const mirrorPod = "has(object.metadata.annotations) && 'kubernetes.io/config.mirror' in object.metadata.annotations"

// createdMirror is mirrorPod for a policy that also matches pod updates:
// it judges creations only.
const createdMirror = "request.operation != 'CREATE' || (" + mirrorPod + ")"

// gatesKept lets a pod update through unless it removes scheduling gates. A
// pod created with gates waits, unscheduled, for them to go: removing them
// would start a pod made before the pause without creating one. Kubernetes
// lets gates be removed after creation, never added, so fewer is a removal.
const gatesKept = "request.operation != 'UPDATE' || !has(oldObject.spec.schedulingGates) || size(oldObject.spec.schedulingGates) == 0 || " +
	"(has(object.spec.schedulingGates) && size(object.spec.schedulingGates) >= size(oldObject.spec.schedulingGates))"

// pausedPolicy is the spec of vbilling-paused.
func pausedPolicy() admissionv1.ValidatingAdmissionPolicySpec {
	return podPolicy(nil, []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
		admissionv1.Validation{Expression: createdMirror,
			Message: "This cluster is paused. No pod can start until its tenant's hold is lifted and the cluster resumes."},
		admissionv1.Validation{Expression: gatesKept,
			Message: "This cluster is paused. No pod's scheduling gates can be removed until its tenant's hold is lifted and the cluster resumes."})
}

// pausePolicy installs the pause admission policy and binding inside a
// cluster, with vBilling's spec whoever created or changed them.
func (e *Enforcer) pausePolicy(ctx context.Context, kube kubernetes.Interface) error {
	return applyPolicy(ctx, kube, PausedPolicyName, pausedPolicy())
}

// podPolicy is the spec of a policy that judges the given operations on
// pods in the namespaces sel selects (all when nil), and denies what one of
// the validations rejects.
func podPolicy(sel *metav1.LabelSelector, ops []admissionv1.OperationType, validations ...admissionv1.Validation) admissionv1.ValidatingAdmissionPolicySpec {
	fail := admissionv1.Fail
	forbidden := metav1.StatusReasonForbidden
	for i := range validations {
		validations[i].Reason = &forbidden
	}
	return admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: &fail,
		MatchConstraints: &admissionv1.MatchResources{
			NamespaceSelector: sel,
			ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: ops,
				Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
			}}},
		},
		Validations: validations,
	}
}

// applyPolicy creates or updates a policy with vBilling's spec, and its
// binding.
func applyPolicy(ctx context.Context, kube kubernetes.Interface, name string, spec admissionv1.ValidatingAdmissionPolicySpec) error {
	api := kube.AdmissionregistrationV1()
	labels := map[string]string{"app.kubernetes.io/managed-by": "vbilling"}
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: spec}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		},
	}
	if cur, err := api.ValidatingAdmissionPolicies().Get(ctx, name, metav1.GetOptions{}); err == nil {
		policy.ResourceVersion = cur.ResourceVersion
		if _, err := api.ValidatingAdmissionPolicies().Update(ctx, policy, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update admission policy: %w", err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := api.ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create admission policy: %w", err)
		}
	} else {
		return fmt.Errorf("read admission policy: %w", err)
	}
	// A binding that already exists may not be vBilling's (one that only
	// audits disarms the policy): it gets vBilling's spec too.
	if cur, err := api.ValidatingAdmissionPolicyBindings().Get(ctx, name, metav1.GetOptions{}); err == nil {
		binding.ResourceVersion = cur.ResourceVersion
		if _, err := api.ValidatingAdmissionPolicyBindings().Update(ctx, binding, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update admission policy binding: %w", err)
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := api.ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create admission policy binding: %w", err)
		}
	} else {
		return fmt.Errorf("read admission policy binding: %w", err)
	}
	return nil
}

// removePolicy deletes an in-cluster policy and its binding.
func removePolicy(ctx context.Context, kube kubernetes.Interface, name string) error {
	api := kube.AdmissionregistrationV1()
	for _, del := range []func() error{
		func() error { return api.ValidatingAdmissionPolicyBindings().Delete(ctx, name, metav1.DeleteOptions{}) },
		func() error { return api.ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{}) },
	} {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("remove admission policy %s: %w", name, err)
		}
	}
	return nil
}

// pausePolicyChanged reads the pause admission policy and binding inside a
// cluster and says how they no longer deny every new pod ("" when they
// still do). Only what decides that is compared, so the API server's
// defaults never count as a change.
func pausePolicyChanged(ctx context.Context, kube kubernetes.Interface) (string, error) {
	api := kube.AdmissionregistrationV1()
	p, err := api.ValidatingAdmissionPolicies().Get(ctx, PausedPolicyName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "the pause admission policy " + PausedPolicyName + " was removed", nil
	}
	if err != nil {
		return "", err
	}
	b, err := api.ValidatingAdmissionPolicyBindings().Get(ctx, PausedPolicyName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "the binding of the pause admission policy " + PausedPolicyName + " was removed", nil
	}
	if err != nil {
		return "", err
	}
	if why := policyNarrowed(p.Spec, b.Spec); why != "" {
		return "the pause admission policy " + PausedPolicyName + " was changed: " + why, nil
	}
	return "", nil
}

// policyNarrowed says how a policy and binding no longer deny every new
// pod but mirror pods, and every removal of a pod's scheduling gates, ""
// if they still do.
func policyNarrowed(p admissionv1.ValidatingAdmissionPolicySpec, b admissionv1.ValidatingAdmissionPolicyBindingSpec) string {
	want := pausedPolicy().Validations
	same := len(p.Validations) == len(want)
	for i := 0; same && i < len(want); i++ {
		same = p.Validations[i].Expression == want[i].Expression
	}
	switch mc := p.MatchConstraints; {
	case p.FailurePolicy != nil && *p.FailurePolicy != admissionv1.Fail:
		return "its failure policy is " + string(*p.FailurePolicy)
	case p.ParamKind != nil:
		return "it takes parameters"
	case len(p.MatchConditions) > 0:
		return "it has match conditions"
	case mc == nil || !emptySelector(mc.NamespaceSelector) || !emptySelector(mc.ObjectSelector) || len(mc.ExcludeResourceRules) > 0 ||
		!coversPods(mc.ResourceRules, admissionv1.Create) || !coversPods(mc.ResourceRules, admissionv1.Update):
		return "it no longer matches every new pod and every pod update"
	case !same:
		return "its validation is not vBilling's"
	case b.PolicyName != PausedPolicyName || b.ParamRef != nil:
		return "its binding names another policy or parameters"
	case !slices.Contains(b.ValidationActions, admissionv1.Deny):
		return "its binding does not deny"
	}
	if r := b.MatchResources; r != nil && (!emptySelector(r.NamespaceSelector) || !emptySelector(r.ObjectSelector) || len(r.ResourceRules) > 0 || len(r.ExcludeResourceRules) > 0) {
		return "its binding narrows it"
	}
	return ""
}

func emptySelector(s *metav1.LabelSelector) bool {
	return s == nil || len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}

// coversPods reports whether resource rules match an operation on every
// pod.
func coversPods(rules []admissionv1.NamedRuleWithOperations, op admissionv1.OperationType) bool {
	has := func(l []string, v string) bool { return slices.Contains(l, v) || slices.Contains(l, "*") }
	for _, r := range rules {
		if (slices.Contains(r.Operations, op) || slices.Contains(r.Operations, admissionv1.OperationAll)) && len(r.ResourceNames) == 0 &&
			has(r.APIGroups, "") && has(r.APIVersions, "v1") && has(r.Resources, "pods") &&
			(r.Scope == nil || *r.Scope == admissionv1.AllScopes || *r.Scope == admissionv1.NamespacedScope) {
			return true
		}
	}
	return false
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
	if e.opts.Mode == "enforce" && e.opts.Gateway != nil {
		// Tenants that only buy tokens have no cluster: the gateway is
		// all there is to converge.
		for _, ts := range e.States() {
			if err := e.opts.Gateway.Apply(ctx, ts.Tenant, ts.State.blocks()); err != nil {
				log.Printf("[enforcement] resync gateway: %v", err)
			}
		}
	}
	if e.opts.Mode == "observe" || e.kube == nil || e.opts.Clusters == nil {
		return
	}
	for _, ts := range e.States() {
		for _, cl := range e.opts.Clusters(ts.Tenant) {
			if err := e.converge(ctx, ts.Tenant, cl, ts, false); err != nil {
				log.Printf("[enforcement] resync: %v", err)
			}
		}
	}
}

// reflect writes the state onto one tenant cluster: its workload's
// annotations, and its namespace's labels. The namespace reflects every
// cluster in it (Options.Neighbors): it is labeled suspended while any of
// them is blocked, and each of the others is marked active there, so the
// chart's admission policy lets their synced pods start.
func (e *Enforcer) reflect(ctx context.Context, cl discovery.TenantCluster, ts TenantState, withEvent bool) error {
	if cl.External {
		return nil // nothing of it lives in the control plane cluster: only the in-cluster policy applies
	}
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

	states := map[string]State{cl.Name: ts.State}
	if e.opts.Neighbors != nil {
		for name, tenant := range e.opts.Neighbors(cl.Namespace) {
			if name != cl.Name {
				states[name] = e.StateOf(tenant).State
			}
		}
	}
	blocked := func(s State) bool { return e.opts.Mode == "enforce" && s.blocks() }
	worst, anyBlocked := Active, false
	for _, s := range states {
		if severity[s] > severity[worst] {
			worst = s
		}
		anyBlocked = anyBlocked || blocked(s)
	}
	labels := map[string]any{LabelState: string(worst), LabelSuspended: nil} // merge patch: nil removes
	if anyBlocked {
		labels[LabelSuspended] = "true"
	}
	nsAnn := map[string]any{}
	for name, s := range states {
		key := AnnotationActivePrefix + name
		if len(validation.IsQualifiedName(key)) > 0 {
			continue // a name too long to mark: its pods stay blocked with the namespace
		}
		nsAnn[key] = nil
		if anyBlocked && !blocked(s) {
			nsAnn[key] = "true"
		}
	}
	nsPatch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels, "annotations": nsAnn}})
	if _, err := e.kube.CoreV1().Namespaces().Patch(ctx, cl.Namespace, types.MergePatchType, nsPatch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label namespace: %w", err)
	}
	if err := e.reflectSynced(ctx, cl, ts); err != nil {
		return err
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

// HasState reports whether a tenant has a recorded billing state.
func (e *Enforcer) HasState(tenant string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.states[tenant]
	return ok
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
