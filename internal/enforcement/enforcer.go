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
// each once. Escalation rules (ENFORCEMENT_ESCALATIONS, escalate.go) raise a
// billing hold that stayed at one state for a set time, such as delinquent
// to suspended after 72 hours: a grace period.
//
// Suspension stops growth, not service: running workloads keep running.
// Pausing (state "paused", enforce mode) stops service: each of the tenant's
// clusters is paused the way `vcluster pause` does it, after its own nodes
// are drained, and billing for capacity stops once vBilling verified its
// workloads are gone (see pauseCluster and package pause). The tenant
// cannot restart it: its control plane is stopped, and vBilling pauses it
// again if anything else resumes it. Clearing the hold resumes it. A hold
// lifted while a pause is still under way ends that pause where it is, and a
// pause lowered to a suspension restores the cluster before the suspension
// applies (restore.go).
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
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
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
	// Notification names the threshold notification a Metronome alert
	// webhook is about (its ID and name), so a rule can pick it out from
	// others of its type: a low-balance warning and a zero-balance pause
	// are both alerts.low_remaining_commit_balance_reached.
	NotificationID   string `json:"notification_id,omitempty"`
	NotificationName string `json:"notification_name,omitempty"`

	// refused: a payment that was applied and lifted nothing for at least
	// one of its tenants, because something is still owed (WebhookRefused).
	refused bool
}

// ruleKeys lists the keys ENFORCEMENT_RULES can match a signal by, most
// specific first: its threshold notification's ID, then its name, then its
// event type.
func (s *Signal) ruleKeys() []string {
	return RuleKeys(s.Type, s.NotificationID, s.NotificationName)
}

// RuleKeys lists the ENFORCEMENT_RULES keys of a webhook event type, and of
// the threshold notification it is about when one is named, most specific
// first: alert:<notification ID>, alert:<notification name>, the event type.
func RuleKeys(eventType, notificationID, notificationName string) []string {
	var keys []string
	if notificationID != "" {
		keys = append(keys, AlertRulePrefix+notificationID)
	}
	if notificationName != "" {
		keys = append(keys, AlertRulePrefix+notificationName)
	}
	if eventType != "" {
		keys = append(keys, eventType)
	}
	return keys
}

// AlertRulePrefix starts an ENFORCEMENT_RULES key that names one threshold
// notification by its ID or name instead of an event type.
const AlertRulePrefix = "alert:"

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
	// EscalatedBy is the escalation rule that raised the hold the state
	// comes from (escalate.go).
	EscalatedBy string `json:"escalated_by,omitempty"`
	// Escalation is when the billing hold raises the tenant's state next,
	// and to what. It is computed when the state is read, never stored.
	Escalation *NextEscalation `json:"escalation,omitempty"`
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
	// EscalatedBy is the escalation rule that raised the hold to its state
	// (from:after:to); empty when a signal or an operator set it.
	EscalatedBy string `json:"escalated_by,omitempty"`
	// Escalation is when the hold escalates next: computed when the state
	// is read, never stored.
	Escalation *NextEscalation `json:"escalation,omitempty"`
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
			ts.EscalatedBy = h.EscalatedBy
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
	// Escalations raise a billing hold that stayed at a state for a set
	// time (ENFORCEMENT_ESCALATIONS, see ParseEscalations and Escalate).
	Escalations []Escalation
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

	// Health, Tenants and AllClusters are optional (see UseHealth,
	// UseTenants and UseClusters): where a payment that did not lift a hold
	// and a cluster whose API refuses vBilling are reported; the tenants
	// discovery knows; and every cluster metering has, with false until the
	// first discovery succeeded.
	Health      *health.Registry
	Tenants     func() []string
	AllClusters func() (clusters []discovery.TenantCluster, ok bool)
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
	// there. counts reports whether ENFORCEMENT_RULES map a threshold
	// notification to a state above warning, given its rule keys most
	// specific first (RuleKeys: alert:<ID>, alert:<name>, its webhook event
	// types). An error means the platform could not be read.
	Owed(ctx context.Context, tenant, customerID string, counts func(keys ...string) bool) ([]string, error)
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

	// The escalation rules as configured, and by the state they start at.
	rules       []Escalation
	escalations map[State]Escalation

	// applied are the webhook events already applied (ID -> when), kept
	// for appliedKeep and at most appliedMax of them, in State.
	evMu    sync.Mutex
	applied map[string]time.Time

	bg                         sync.WaitGroup // background pauses and resumes
	resumeTimeout, resumeEvery time.Duration
	// restoreTimeout bounds how long a suspension is held back while a
	// cluster is restored after its pause; restoreEvery is how often the
	// restore looks whether the workloads are back (restore.go).
	restoreTimeout, restoreEvery time.Duration
	wmu                          sync.Mutex
	workers                      map[string]*worker
	status                       map[string]PauseStatus
	// Pause bookkeeping per cluster (see pause.go), under wmu.
	failing    map[string]time.Time // first failed drain of a streak
	checked    map[string]ownCheck  // what the last pause pass found on its own nodes
	stopped    map[string]time.Time // last time its control plane was seen stopped
	verifiedAt map[string]time.Time // external clusters: last pass that found nothing running
	older      map[string]bool      // pauses an older version opened, without verifying them

	// rmu guards records: what vBilling remembers across restarts about the
	// clusters it paused (restore.go).
	rmu     sync.Mutex
	records map[string]pauseRecord

	// gmu guards exported: the tenants vbilling_tenant_billing_state has
	// series for, with the state it shows (observe.go).
	gmu      sync.Mutex
	exported map[string]State

	// omu guards what is reported to an operator (observe.go).
	omu         sync.Mutex
	unlocks     map[string]unlockNote // tenant: a payment that did not lift its billing hold
	refused     map[string]string     // cluster: how its API refused vBilling
	installed   map[string]bool       // the in-cluster policies and bindings this process installed
	nudgeWarned map[string]bool       // clusters whose workload controllers could not be nudged
	unverified  map[string]bool       // paused clusters reported as not verified
	goneWarned  bool                  // deleted clusters could not be told from the rest
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
		restoreTimeout: defaultRestoreTimeout, restoreEvery: 2 * time.Second,
		workers: map[string]*worker{}, status: map[string]PauseStatus{},
		failing: map[string]time.Time{}, checked: map[string]ownCheck{}, stopped: map[string]time.Time{}, verifiedAt: map[string]time.Time{},
		older: map[string]bool{}, records: map[string]pauseRecord{}, exported: map[string]State{},
		unlocks: map[string]unlockNote{}, refused: map[string]string{}, installed: map[string]bool{}, nudgeWarned: map[string]bool{}, unverified: map[string]bool{},
		rules: append([]Escalation(nil), opts.Escalations...), escalations: map[State]Escalation{}}
	for _, r := range e.rules {
		e.escalations[r.From] = r
		opts.Telemetry.Add(escalationsMetric, escalationsHelp, map[string]string{"from": string(r.From), "to": string(r.To)}, 0)
	}
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
	e.loadRecords()
	e.loadUnlocks()
	// Every metric is there from the start, at zero: a rate over it never
	// begins with a gap, and a missing series never reads as "no tenant".
	for tenant, ts := range e.states {
		if len(ts.Holds) > 0 {
			e.exportState(tenant, ts.State)
		}
	}
	for source, secret := range map[string]string{"stripe": opts.StripeWebhookSecret, "metronome": opts.MetronomeWebhookSecret} {
		if secret == "" {
			continue
		}
		for _, result := range webhookResults {
			opts.Telemetry.Add(webhooksMetric, webhooksHelp, map[string]string{"source": source, "result": result}, 0)
		}
	}
	for _, kind := range []string{RepairPolicyDeleted, RepairPolicyChanged, RepairBindingDeleted, RepairBindingChanged} {
		opts.Telemetry.Add(repairsMetric, repairsHelp, map[string]string{"kind": kind}, 0)
	}
	return e
}

// ParseRules parses "key=state,..." (ENFORCEMENT_RULES). A key is a webhook
// event type, or alert:<ID or name> for one Metronome threshold
// notification, which wins over a rule on its event type. A name may hold
// "=" but not ",".
func ParseRules(spec string) (map[string]State, error) {
	out := map[string]State{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		i := strings.LastIndex(item, "=")
		if i < 0 {
			return nil, fmt.Errorf("enforcement rule %q must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)", item)
		}
		k, v := strings.TrimSpace(item[:i]), State(strings.TrimSpace(item[i+1:]))
		if k == "" || k == AlertRulePrefix || !v.valid() {
			return nil, fmt.Errorf("enforcement rule %q must be event.type=state or alert:<notification ID or name>=state (states: active, warning, delinquent, suspended, paused)", item)
		}
		out[k] = v
	}
	return out, nil
}

// ruleFor returns the state the most specific matching rule maps keys to.
func (e *Enforcer) ruleFor(keys []string) (State, bool) {
	for _, k := range keys {
		if s, ok := e.opts.Rules[k]; ok {
			return s, true
		}
	}
	return "", false
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
// their created time. What became of it is counted in
// vbilling_webhooks_total (WebhookResult).
func (e *Enforcer) HandleStripe(ctx context.Context, body []byte, sigHeader string) (Signal, error) {
	sig, err := e.handleStripe(ctx, body, sigHeader)
	e.countWebhook("stripe", sig, err)
	return sig, err
}

func (e *Enforcer) handleStripe(ctx context.Context, body []byte, sigHeader string) (Signal, error) {
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
		return Signal{}, malformed{err}
	}
	var obj struct {
		Customer     string `json:"customer"`
		Status       string `json:"status"`
		AttemptCount int    `json:"attempt_count"`
	}
	if len(ev.Data.Object) > 0 {
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return Signal{}, malformed{err}
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
// when it was sent. What became of it is counted in vbilling_webhooks_total
// (WebhookResult).
func (e *Enforcer) HandleMetronome(ctx context.Context, body []byte, date, signature string) (Signal, error) {
	sig, err := e.handleMetronome(ctx, body, date, signature)
	e.countWebhook("metronome", sig, err)
	return sig, err
}

func (e *Enforcer) handleMetronome(ctx context.Context, body []byte, date, signature string) (Signal, error) {
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
		return Signal{}, malformed{err}
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
		sig.NotificationID, sig.NotificationName = str("alert_id"), str("alert_name")
		if sig.NotificationName != "" {
			sig.Reason = sig.NotificationName
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
	if override, ok := e.ruleFor(sig.ruleKeys()); ok {
		sig.State = override
		if sig.Reason == "" {
			// An event type vBilling gives no reason of its own (any type a
			// rule names): the reason names the event.
			sig.Reason = sig.Type
		}
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
			if errors.Is(err, ErrNotSettled) {
				// The payment is answered with an error and delivered again:
				// until the hold lifts, it shows as a payment that did not
				// unlock its tenant.
				e.noteUnlock(tenant, sig, WebhookUnsettled, trimSettle(err))
			}
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
			if sig.Positive {
				// A payment that lifted nothing: something is still owed.
				sig.refused = true
				e.noteUnlock(tenant, sig, WebhookRefused, res.why)
			}
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
		errs = append(errs, e.reflectState(ctx, c.prev, c.ts, HoldBilling, sig.Source, sig.Type))
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

// countsAbove reports whether ENFORCEMENT_RULES map a threshold
// notification to a state above warning: the notifications that keep a
// billing hold while they are in alarm. keys are its rule keys, most
// specific first (RuleKeys): the first that has a rule decides, so a
// warning rule on one notification keeps it from counting even when its
// type pauses.
func (e *Enforcer) countsAbove(keys ...string) bool {
	s, ok := e.ruleFor(keys)
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
	if hold == "" {
		hold = HoldBilling
	}
	return e.reflectState(ctx, prev, ts, hold, source, eventType)
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
	// escalate raises the hold by an escalation rule (Escalate), only while
	// it is still at the rule's state with the since it was read with.
	escalate *Escalation
	since    time.Time
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
	if u.escalate != nil {
		cur, held := holds[u.hold]
		if !held || cur.State != u.escalate.From || !cur.Since.Equal(u.since) {
			return prev, prev, storeResult{outStale, "the hold changed since it was read"}, nil
		}
		// An escalation is no billing event: a payment made before it and
		// delivered after it still applies.
		advance = false
	} else if u.webhook {
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
		switch {
		case u.escalate != nil:
			// The hold keeps what raised it (its source and event type, which
			// decide what lifts it) and reaches the rule's state now: the next
			// rule's clock starts here.
			h := holds[u.hold]
			h.State, h.Reason, h.EscalatedBy, h.Since = u.escalate.To, u.reason, u.escalate.String(), now
			holds[u.hold] = h
		case u.state == Active:
			delete(holds, u.hold)
		default:
			// An operator's change replaces the hold.
			h := Hold{State: u.state, Reason: u.reason, Source: u.source, EventType: u.eventType, Since: now}
			if old, ok := holds[u.hold]; ok && old.State == u.state {
				// The same state: its clock, and the escalation that raised
				// it, carry on.
				h.Since, h.EscalatedBy = old.Since, old.EscalatedBy
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
// gateway identities and clusters. hold is the hold the change was made to,
// by source and eventType.
func (e *Enforcer) reflectState(ctx context.Context, prev, ts TenantState, hold, source, eventType string) error {
	tenant, state := ts.Tenant, ts.State
	if prev.State != state {
		if ts.Hold == "" || ts.Hold == hold {
			log.Printf("[enforcement] tenant %s: %s -> %s (%s via %s %s, mode=%s)", tenant, orActive(prev.State), state, ts.Reason, source, eventType, e.opts.Mode)
		} else {
			// The change lowered or lifted one hold, and the state now comes
			// from another one, set earlier by someone else: the line names
			// both, each with its own source.
			change := "lifted"
			if _, still := ts.Holds[hold]; still {
				change = "lowered"
			}
			log.Printf("[enforcement] tenant %s: %s -> %s (the %s hold remains: %s, set via %s %s; the %s hold was %s via %s %s, mode=%s)", tenant, orActive(prev.State), state,
				ts.Hold, ts.Holds[ts.Hold].Reason, ts.Source, ts.EventType, hold, change, source, eventType, e.opts.Mode)
		}
	}
	if len(ts.Holds) > 0 || e.knownTenant(tenant) {
		e.exportState(tenant, state)
	} else {
		e.syncStateMetric() // a tenant discovery does not know has no state to show once its last hold is lifted
	}
	if _, held := ts.Holds[HoldBilling]; !held {
		e.clearUnlock(tenant) // the hold a payment did not lift is lifted
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
// when it is not paused. A cluster whose API refuses vBilling is reported
// (noteAccess).
func (e *Enforcer) reflectInside(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	kube, err := e.opts.TenantClient(ctx, cl)
	if err != nil {
		return err
	}
	if kube == nil {
		return nil
	}
	err = e.reflectWith(ctx, kube, cl, ts)
	e.noteAccess(cl, err)
	return err
}

// reflectWith is reflectInside through a connection to the cluster's API.
func (e *Enforcer) reflectWith(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster, ts TenantState) error {
	lifted, err := e.policyInside(ctx, kube, cl, ts)
	if ts.State != Paused && (err == nil || refusedAccess(err)) {
		// The cluster's API answers, so the cluster can start pods again
		// (and a tenant who took vBilling's access away can let it): the
		// time of a restore runs from here, whether or not the rest of
		// this pass succeeds. Nothing that fails later holds a suspension
		// back any longer.
		e.restoreLifted(cl.ExternalID())
	}
	if err != nil {
		return err
	}
	if ts.State == Paused {
		return nil
	}
	removed, err := e.removePolicy(ctx, kube, cl, PausedPolicyName)
	if apierrors.IsForbidden(err) {
		// Credentials that may not read or delete vbilling-paused never
		// let a pause install it (it reads the policy first), so there
		// is nothing to remove: lift the taint all the same.
		log.Printf("[enforcement] %s: %v (grant get, update and delete on %s to vbilling-enforcer inside the cluster)", cl.ExternalID(), err, PausedPolicyName)
	} else if err != nil {
		return err
	}
	if err := e.untaint(ctx, kube); err != nil {
		return err
	}
	if lifted || removed {
		// A policy that refused new pods is gone: the workload controllers
		// that failed against it try again now, not when their waits end.
		e.nudge(ctx, kube, cl)
	}
	return nil
}

// policyInside installs the admission policy inside a cluster while its
// tenant's state blocks new workloads, and removes it otherwise, or while
// the cluster is restored after a pause (restore.go); lifted reports
// whether it removed one. The policy and its binding are set to vBilling's
// spec at every pass, whoever created or changed them (a deleted or
// weakened one is logged and counted, see repaired); only kube-system is
// exempt. The tenant is cluster admin inside its cluster and can still
// delete them between two passes or run pods in kube-system, so suspension
// inside a tenant's cluster is best effort: a pause is the hard stop.
func (e *Enforcer) policyInside(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster, ts TenantState) (lifted bool, err error) {
	if !ts.State.blocks() || ts.State != Paused && e.restoring(cl.ExternalID()) {
		return e.removePolicy(ctx, kube, cl, PolicyName)
	}
	return false, e.applyPolicy(ctx, kube, cl, ts.Tenant, PolicyName, podPolicy(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
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
func (e *Enforcer) pausePolicy(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster, tenant string) error {
	return e.applyPolicy(ctx, kube, cl, tenant, PausedPolicyName, pausedPolicy())
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
// binding. One of them that this process installed and finds deleted, or
// that it finds no longer denying what vBilling's does, was taken apart
// inside the cluster: putting it back is logged and counted (repaired).
func (e *Enforcer) applyPolicy(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster, tenant, name string, spec admissionv1.ValidatingAdmissionPolicySpec) error {
	api := kube.AdmissionregistrationV1()
	id := cl.ExternalID()
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
		if why := policyWeakened(spec, cur.Spec); why != "" {
			e.repaired(cl, tenant, RepairPolicyChanged, "the admission policy "+name+" was changed ("+why+")")
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := api.ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create admission policy: %w", err)
		}
		if e.wasInstalled(id, name, "policy") {
			e.repaired(cl, tenant, RepairPolicyDeleted, "the admission policy "+name+" was deleted")
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
		if why := bindingWeakened(name, cur.Spec); why != "" {
			e.repaired(cl, tenant, RepairBindingChanged, "the binding of the admission policy "+name+" was changed ("+why+")")
		}
	} else if apierrors.IsNotFound(err) {
		if _, err := api.ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create admission policy binding: %w", err)
		}
		if e.wasInstalled(id, name, "binding") {
			e.repaired(cl, tenant, RepairBindingDeleted, "the binding of the admission policy "+name+" was deleted")
		}
	} else {
		return fmt.Errorf("read admission policy binding: %w", err)
	}
	e.setInstalled(id, name, true)
	return nil
}

// removePolicy deletes an in-cluster policy and its binding; removed
// reports whether one of them was there.
func (e *Enforcer) removePolicy(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster, name string) (removed bool, err error) {
	api := kube.AdmissionregistrationV1()
	for _, del := range []func() error{
		func() error { return api.ValidatingAdmissionPolicyBindings().Delete(ctx, name, metav1.DeleteOptions{}) },
		func() error { return api.ValidatingAdmissionPolicies().Delete(ctx, name, metav1.DeleteOptions{}) },
	} {
		switch err := del(); {
		case err == nil:
			removed = true
		case !apierrors.IsNotFound(err):
			return removed, fmt.Errorf("remove admission policy %s: %w", name, err)
		}
	}
	e.setInstalled(cl.ExternalID(), name, false)
	return removed, nil
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

// RecheckInterval is how often RecheckHolds reads Metronome for the billing
// holds its threshold notifications raised.
const RecheckInterval = time.Minute

// RecheckHolds lowers a billing hold that a Metronome threshold
// notification raised once Metronome, read again, shows nothing owed: the
// notification is ok again because the wallet was refilled, however that
// happened. A payment-gated top-up also sends a payment webhook, but a
// commit or credit Finance adds to the contract, or a promotional grant,
// sends none, and the tenant would otherwise stay paused. Only holds whose
// newest event is an alerts.* webhook from Metronome are read: a hold a
// failed payment raised waits for a payment, and holds an operator set are
// never touched. Like a payment, the read clears the hold only when no
// billing platform shows anything owed; one that cannot be read leaves the
// hold until the next round.
func (e *Enforcer) RecheckHolds(ctx context.Context) {
	if _, ok := e.opts.Resolvers["metronome"]; !ok {
		return
	}
	for _, ts := range e.States() {
		// A warning blocks nothing, and stays while it says what Metronome
		// says (a low-balance warning in alarm): only a pause, suspension or
		// delinquency a notification raised is lifted here.
		h, held := ts.Holds[HoldBilling]
		if !held || severity[orActive(h.State)] <= severity[Warning] || h.Source != "metronome" || !strings.HasPrefix(h.EventType, "alerts.") {
			continue
		}
		now := e.opts.Now().UTC()
		sig := Signal{Source: "metronome", Type: RecheckEventType, Tenant: ts.Tenant, Tenants: []string{ts.Tenant},
			State: Active, Reason: "Metronome shows the threshold notifications clear again", At: now, EventAt: now, Positive: true}
		owed, read, err := e.stillOwed(ctx, ts.Tenant, &sig, map[string]readResult{})
		if errors.Is(err, ErrNotSettled) {
			continue // a notification that counts is still in alarm
		}
		if err != nil {
			log.Printf("[enforcement] recheck tenant %s: %v", ts.Tenant, err)
			continue
		}
		if !read || len(owed) > 0 {
			continue
		}
		next, prev, res, err := e.store(ts.Tenant, update{hold: HoldBilling, state: Active, reason: sig.Reason, source: sig.Source,
			eventType: sig.Type, webhook: true, positive: true, at: now, read: true})
		if err != nil {
			log.Printf("[enforcement] recheck tenant %s: %v", ts.Tenant, err)
			continue
		}
		if res.kind != outChanged {
			continue
		}
		if err := e.reflectState(ctx, prev, next, HoldBilling, sig.Source, sig.Type); err != nil {
			log.Printf("[enforcement] recheck tenant %s: %v", ts.Tenant, err)
		}
	}
}

// RecheckEventType is the event type RecheckHolds records when it lowers a
// hold.
const RecheckEventType = "threshold_notifications_clear"

// Resync re-applies every recorded state to its tenant's clusters, without
// emitting events. A cluster created while its tenant was suspended, or one
// that was unreachable when the state changed (so a paying tenant could stay
// blocked), converges within one resync.
func (e *Enforcer) Resync(ctx context.Context) {
	e.reportNotes()
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
	// A cluster that is restored after a pause is not blocked yet, whatever
	// its tenant's state (restore.go).
	blocked := func(name string, s State) bool {
		other := discovery.TenantCluster{Name: name, Namespace: cl.Namespace}
		return e.enforced(other.ExternalID(), s)
	}
	worst, anyBlocked := Active, false
	for name, s := range states {
		if severity[s] > severity[worst] {
			worst = s
		}
		anyBlocked = anyBlocked || blocked(name, s)
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
		if anyBlocked && !blocked(name, s) {
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
	// The name makes the event of one state change one object: finding it
	// there (the state was reflected again, unchanged) is not an error.
	if err != nil && !apierrors.IsAlreadyExists(err) {
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

// States returns all recorded tenant states, each with its next
// escalation.
func (e *Enforcer) States() []TenantState {
	e.mu.Lock()
	out := make([]TenantState, 0, len(e.states))
	for _, s := range e.states {
		out = append(out, s)
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	for i := range out {
		out[i] = e.withEscalation(out[i])
	}
	return out
}

// StateOf returns a tenant's state (active when unknown), with its next
// escalation.
func (e *Enforcer) StateOf(tenant string) TenantState {
	e.mu.Lock()
	s, ok := e.states[tenant]
	e.mu.Unlock()
	if !ok {
		return TenantState{Tenant: tenant, State: Active}
	}
	return e.withEscalation(s)
}
