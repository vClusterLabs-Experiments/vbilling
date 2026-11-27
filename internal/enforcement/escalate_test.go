package enforcement

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// escalationEnv is an enforcer with escalation rules, on a clock the test
// moves, for tenant acme (Stripe customer cus_acme, cluster gpu in
// namespace team-a), whose Stripe account shows owed what the test says.
type escalationEnv struct {
	t    *testing.T
	e    *Enforcer
	kube *fake.Clientset
	clk  *testClock
	acct *account
	opts Options
}

func escalating(t *testing.T, mode, escalations, rules string, state *kv.Store) *escalationEnv {
	t.Helper()
	esc, err := ParseEscalations(escalations)
	if err != nil {
		t.Fatal(err)
	}
	ruleMap, err := ParseRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		state = kv.Memory()
	}
	kube := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		controlPlane("gpu", "team-a", 1, nil),
	)
	acct := &account{resolver: resolver{"cus_acme": "acme"}}
	acct.still()
	clk := newClock()
	opts := Options{Mode: mode, StripeWebhookSecret: "whsec_test", State: state, Rules: ruleMap, Escalations: esc, Now: clk.Now,
		Pauses: pause.Memory(),
		Clusters: func(tenant string) []discovery.TenantCluster {
			if tenant != "acme" {
				return nil
			}
			return []discovery.TenantCluster{{Name: "gpu", Namespace: "team-a", Source: "statefulset"}}
		},
		Resolvers: map[string]destinations.CustomerResolver{"stripe": acct}}
	env := &escalationEnv{t: t, kube: kube, clk: clk, acct: acct, opts: opts}
	env.e = New(kube, opts)
	t.Cleanup(env.e.Wait)
	return env
}

// restart is vBilling starting again with the billing state read back
// from disk, and counters from zero.
func (env *escalationEnv) restart(state *kv.Store) {
	env.e.Wait()
	env.opts.State = state
	env.e = New(env.kube, env.opts)
}

// stripe delivers a Stripe event now that happened at created.
func (env *escalationEnv) stripe(id, typ, status string, created time.Time) {
	env.t.Helper()
	body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{%s,"customer":"cus_acme","status":%q,"attempt_count":1}}}`,
		id, typ, created.Unix(), stripeObject(typ), status))
	if _, err := env.e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", env.clk.Now())); err != nil {
		env.t.Fatalf("%s %s: %v", id, typ, err)
	}
}

func (env *escalationEnv) escalate() {
	env.e.Escalate(context.Background())
	env.e.Wait()
}

func (env *escalationEnv) state() TenantState { return env.e.StateOf("acme") }

func (env *escalationEnv) count(from, to State) float64 {
	return env.e.opts.Telemetry.Get(escalationsMetric, map[string]string{"from": string(from), "to": string(to)})
}

// want checks the tenant's state, since, the rule that raised it, and its
// next escalation (to "" for none).
func (env *escalationEnv) want(what string, state State, since time.Time, by string, nextTo State, nextAt time.Time) {
	env.t.Helper()
	st := env.state()
	if st.State != state || !st.Since.Equal(since) || st.EscalatedBy != by {
		env.t.Fatalf("%s: state %s since %s escalated by %q (%s), want %s since %s escalated by %q",
			what, st.State, st.Since.Format(time.RFC3339), st.EscalatedBy, st.Reason, state, since.Format(time.RFC3339), by)
	}
	switch {
	case nextTo == "" && st.Escalation != nil:
		env.t.Fatalf("%s: next escalation %+v, want none", what, *st.Escalation)
	case nextTo != "" && (st.Escalation == nil || st.Escalation.To != nextTo || !st.Escalation.At.Equal(nextAt)):
		env.t.Fatalf("%s: next escalation %+v, want to %s at %s", what, st.Escalation, nextTo, nextAt.Format(time.RFC3339))
	}
}

// A billing hold that stays delinquent for 72 hours is suspended at the
// 72nd hour, not a second earlier. The state shows when that happens
// before it does; when it fires, the change is stored, counted, logged and,
// in enforce mode, reflected on the tenant's cluster with a Kubernetes
// event, like any other state change.
func TestEscalationFiresAtItsTime(t *testing.T) {
	logs := captureLog(t)
	env := escalating(t, "enforce", "delinquent:72h:suspended", "", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	due := t0.Add(72 * time.Hour)
	env.want("after the failed payment", Delinquent, t0, "", Suspended, due)
	if h := env.state().Holds[HoldBilling]; h.Escalation == nil || h.Escalation.Rule != "delinquent:72h:suspended" || !h.Escalation.At.Equal(due) {
		t.Fatalf("the billing hold shows no escalation: %+v", h)
	}
	if env.count(Delinquent, Suspended) != 0 || !env.e.opts.Telemetry.Has(escalationsMetric, map[string]string{"from": "delinquent", "to": "suspended"}) {
		t.Fatal("the escalation counter is not there at zero from the start")
	}

	env.clk.add(72*time.Hour - time.Second)
	env.escalate()
	env.want("a second before the grace ends", Delinquent, t0, "", Suspended, due)
	if l := nsLabels(t, env.kube); l[LabelSuspended] != "" {
		t.Fatalf("suspended before the grace ended: %v", l)
	}

	env.clk.add(time.Second)
	env.escalate()
	env.want("when the grace ends", Suspended, due, "delinquent:72h:suspended", "", time.Time{})
	st := env.state()
	h := st.Holds[HoldBilling]
	if st.Reason != "delinquent for 72h: invoice payment failed (attempt 1)" || h.Source != "stripe" || h.EventType != "invoice.payment_failed" {
		t.Fatalf("escalated hold: %+v; want the reason to say why, and the hold to keep what raised it", h)
	}
	if env.count(Delinquent, Suspended) != 1 {
		t.Fatalf("escalations counted: %v, want 1", env.count(Delinquent, Suspended))
	}
	if l := nsLabels(t, env.kube); l[LabelSuspended] != "true" || l[LabelState] != "suspended" {
		t.Fatalf("enforce mode: the escalation did not suspend the namespace: %v", l)
	}
	events, _ := env.kube.CoreV1().Events("team-a").List(context.Background(), metav1.ListOptions{})
	found := false
	for _, ev := range events.Items {
		found = found || ev.Reason == "BillingStateSuspended" && strings.Contains(ev.Message, "delinquent for 72h")
	}
	if !found {
		t.Fatalf("no Kubernetes event for the escalation: %+v", events.Items)
	}
	if logs.count("tenant acme: delinquent -> suspended", "via escalation delinquent:72h:suspended") != 1 {
		t.Fatalf("the escalation was not logged once:\n%s", logs)
	}

	// Nothing more to do: the next passes change nothing.
	env.clk.add(30 * 24 * time.Hour)
	env.escalate()
	if env.state().State != Suspended || env.count(Delinquent, Suspended) != 1 {
		t.Fatalf("a later pass changed the escalated state: %+v", env.state())
	}
}

// Escalations chain: delinquent to suspended after 72 hours, then
// suspended to paused 7 days after the suspension. A pass that finds a
// hold long past its time (vBilling was down) raises it one state, and the
// next rule's clock starts then: each state lasts its full grace.
func TestEscalationsChain(t *testing.T) {
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	suspendAt := t0.Add(72 * time.Hour)
	env.want("delinquent", Delinquent, t0, "", Suspended, suspendAt)

	env.clk.add(72 * time.Hour)
	env.escalate()
	pauseAt := suspendAt.Add(7 * 24 * time.Hour)
	env.want("after 72 hours", Suspended, suspendAt, "delinquent:72h:suspended", Paused, pauseAt)

	env.clk.add(7*24*time.Hour - time.Second)
	env.escalate()
	env.want("a second before the second grace ends", Suspended, suspendAt, "delinquent:72h:suspended", Paused, pauseAt)
	env.clk.add(time.Second)
	env.escalate()
	env.want("7 days after the suspension", Paused, pauseAt, "suspended:7d:paused", "", time.Time{})
	if r := env.state().Reason; r != "suspended for 7d: delinquent for 72h: invoice payment failed (attempt 1)" {
		t.Fatalf("reason %q", r)
	}
	if env.count(Delinquent, Suspended) != 1 || env.count(Suspended, Paused) != 1 {
		t.Fatalf("counted %v and %v, want 1 each", env.count(Delinquent, Suspended), env.count(Suspended, Paused))
	}
	// Observe mode records the state and touches no cluster.
	if l := nsLabels(t, env.kube); l[LabelState] != "" || *sts(t, env.kube, "team-a", "gpu").Spec.Replicas != 1 {
		t.Fatalf("observe mode changed the cluster: labels %v", l)
	}

	// A hold found 20 days past its start rises one state, and the pause
	// comes 7 days after that, not at once.
	late := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	t1 := late.clk.Now()
	late.stripe("evt_failed", "invoice.payment_failed", "", t1)
	late.clk.add(20 * 24 * time.Hour)
	late.escalate()
	now := late.clk.Now()
	late.want("a pass 20 days late", Suspended, now, "delinquent:72h:suspended", Paused, now.Add(7*24*time.Hour))
}

// In enforce mode, an escalation to paused pauses the tenant's clusters
// like any other pause.
func TestEscalationToPausedPausesTheCluster(t *testing.T) {
	env := escalating(t, "enforce", "suspended:1h:paused", "", nil)
	env.stripe("evt_deleted", "customer.subscription.deleted", "", env.clk.Now())
	if env.state().State != Suspended || *sts(t, env.kube, "team-a", "gpu").Spec.Replicas != 1 {
		t.Fatalf("before the escalation: %+v", env.state())
	}
	env.clk.add(time.Hour)
	env.escalate()
	s := sts(t, env.kube, "team-a", "gpu")
	if env.state().State != Paused || *s.Spec.Replicas != 0 || s.Annotations[discovery.AnnotationPausedBy] != pause.ByVBilling {
		t.Fatalf("after the escalation: state %s, control plane replicas %d, annotations %v", env.state().State, *s.Spec.Replicas, s.Annotations)
	}
}

// The clock is stored with the billing hold: a restart before the grace
// ends keeps the same deadline, a restart after it escalates at the first
// pass, and the escalated state, the rule that raised it and the next
// deadline survive another restart. Only the rule that raised a hold is
// stored, never the computed next escalation.
func TestEscalationSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing-state.json")
	open := func() *kv.Store {
		s, err := kv.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", open())
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)

	env.clk.add(71 * time.Hour)
	env.restart(open())
	env.escalate()
	env.want("restarted an hour before the deadline", Delinquent, t0, "", Suspended, t0.Add(72*time.Hour))

	env.clk.add(9 * time.Hour) // down past the deadline
	env.restart(open())
	env.escalate()
	escalatedAt := t0.Add(80 * time.Hour)
	env.want("restarted 8 hours after the deadline", Suspended, escalatedAt, "delinquent:72h:suspended", Paused, escalatedAt.Add(7*24*time.Hour))
	if env.count(Delinquent, Suspended) != 1 {
		t.Fatalf("the escalation at start was not counted: %v", env.count(Delinquent, Suspended))
	}

	state := open()
	env.restart(state)
	env.want("after another restart", Suspended, escalatedAt, "delinquent:72h:suspended", Paused, escalatedAt.Add(7*24*time.Hour))
	raw, _ := state.Get("state/acme")
	if !strings.Contains(raw, `"escalated_by":"delinquent:72h:suspended"`) || strings.Contains(raw, `"escalation":`) {
		t.Fatalf("stored state: %s; want the rule that raised the hold and no computed escalation", raw)
	}
}

// A payment that lifts the billing hold ends the escalation; a payment
// while something is still owed changes neither the state nor its clock.
// A payment made before an escalation and delivered after it still lifts
// the hold: an escalation is no billing event.
func TestPaymentEndsTheEscalation(t *testing.T) {
	env := escalating(t, "enforce", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	env.clk.add(72 * time.Hour)
	env.escalate()
	suspendedAt := env.clk.Now()

	env.acct.still("invoice in_acme open, payment failed (attempt 1)")
	env.clk.add(time.Hour)
	env.stripe("evt_partial", "invoice.payment_succeeded", "", env.clk.Now())
	env.want("a payment while something is owed", Suspended, suspendedAt, "delinquent:72h:suspended", Paused, suspendedAt.Add(7*24*time.Hour))

	env.acct.still()
	env.clk.add(time.Hour)
	env.stripe("evt_paid", "invoice.paid", "", t0.Add(71*time.Hour)) // paid before the escalation, delivered after it
	st := env.state()
	if st.State != Active || len(st.Holds) != 0 || st.Escalation != nil {
		t.Fatalf("a payment that leaves nothing owed: %+v, want active with no hold and no escalation", st)
	}
	if l := nsLabels(t, env.kube); l[LabelSuspended] != "" {
		t.Fatalf("the namespace stays suspended after the payment: %v", l)
	}
	env.clk.add(30 * 24 * time.Hour)
	env.escalate()
	if env.state().State != Active || env.count(Suspended, Paused) != 0 {
		t.Fatalf("an escalation went on after the payment: %+v", env.state())
	}
}

// A billing hold that is lowered starts the clock again from its new
// state, whether the billing platform lowers it (a payment a rule maps to
// a state below the hold, with nothing owed) or the billing hold is set
// through the API.
func TestLoweredHoldRestartsTheClock(t *testing.T) {
	// A paid invoice leaves the tenant delinquent, by a rule.
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "invoice.paid=delinquent", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	env.clk.add(72 * time.Hour)
	env.escalate()
	env.clk.add(24 * time.Hour)
	env.stripe("evt_paid", "invoice.paid", "", env.clk.Now())
	lowered := env.clk.Now()
	env.want("lowered by the billing platform", Delinquent, lowered, "", Suspended, lowered.Add(72*time.Hour))
	env.clk.add(72*time.Hour - time.Second)
	env.escalate()
	if env.state().State != Delinquent {
		t.Fatalf("escalated on the old clock: %+v", env.state())
	}
	env.clk.add(time.Second)
	env.escalate()
	env.want("72 hours after it was lowered", Suspended, lowered.Add(72*time.Hour), "delinquent:72h:suspended", Paused, lowered.Add(72*time.Hour+7*24*time.Hour))

	// The billing hold set lower through the API.
	api := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	api.stripe("evt_failed", "invoice.payment_failed", "", api.clk.Now())
	api.clk.add(72 * time.Hour)
	api.escalate()
	api.clk.add(time.Hour)
	if err := api.e.SetHold(context.Background(), "acme", HoldBilling, Delinquent, "payment plan agreed", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	now := api.clk.Now()
	api.want("lowered through the API", Delinquent, now, "", Suspended, now.Add(72*time.Hour))
}

// What a new billing signal does to a hold an escalation raised: a lesser
// one changes nothing; one at the same state keeps the clock and the rule
// that raised the hold; one above it raises the hold, which then carries
// the platform's state, not the escalation's, and starts its own clock.
func TestNewSignalOnAnEscalatedHold(t *testing.T) {
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "customer.subscription.deleted=paused", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	env.clk.add(72 * time.Hour)
	env.escalate()
	escalatedAt := env.clk.Now()
	pauseAt := escalatedAt.Add(7 * 24 * time.Hour)

	env.clk.add(time.Hour)
	env.stripe("evt_failed_again", "invoice.payment_failed", "", env.clk.Now())
	env.want("another failed payment (delinquent)", Suspended, escalatedAt, "delinquent:72h:suspended", Paused, pauseAt)

	env.clk.add(time.Hour)
	env.stripe("evt_unpaid", "customer.subscription.updated", "unpaid", env.clk.Now())
	env.want("the subscription unpaid (suspended)", Suspended, escalatedAt, "delinquent:72h:suspended", Paused, pauseAt)
	if r := env.state().Reason; r != "subscription unpaid" {
		t.Fatalf("reason %q, want the new signal's", r)
	}

	env.clk.add(time.Hour)
	env.stripe("evt_deleted", "customer.subscription.deleted", "", env.clk.Now())
	env.want("the subscription deleted (paused by a rule)", Paused, env.clk.Now(), "", "", time.Time{})
	if h := env.state().Holds[HoldBilling]; h.EventType != "customer.subscription.deleted" || h.EscalatedBy != "" {
		t.Fatalf("billing hold %+v", h)
	}
}

// Operator holds never escalate: the operator chose their state. The
// billing hold escalates under an operator hold that keeps the tenant
// higher, without changing the tenant's state, and shows when on the hold
// only; once the operator lifts theirs, the escalated billing hold decides.
func TestOperatorHoldsNeverEscalate(t *testing.T) {
	logs := captureLog(t)
	ctx := context.Background()
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	for hold, state := range map[string]State{HoldOperator: Delinquent, "support": Suspended} {
		if err := env.e.SetHold(ctx, "acme", hold, state, "set by hand", "api", "manual"); err != nil {
			t.Fatal(err)
		}
	}
	env.clk.add(60 * 24 * time.Hour)
	env.escalate()
	st := env.state()
	if st.State != Suspended || st.Holds[HoldOperator].State != Delinquent || st.Holds["support"].State != Suspended || st.Escalation != nil {
		t.Fatalf("operator holds after 60 days: %+v", st)
	}
	if env.count(Delinquent, Suspended) != 0 || env.count(Suspended, Paused) != 0 {
		t.Fatal("an operator hold was counted as escalated")
	}

	// The billing hold escalates below a paused operator hold.
	if err := env.e.SetHold(ctx, "acme", "support", Active, "", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := env.e.SetHold(ctx, "acme", HoldOperator, Paused, "under review", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	t1 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t1)
	st = env.state()
	if st.State != Paused || st.Escalation != nil || st.Holds[HoldBilling].Escalation == nil || !st.Holds[HoldBilling].Escalation.At.Equal(t1.Add(72*time.Hour)) {
		t.Fatalf("under a paused operator hold: %+v; want the next escalation on the billing hold only", st)
	}
	env.clk.add(72 * time.Hour)
	env.escalate()
	st = env.state()
	if st.State != Paused || st.Hold != HoldOperator || st.Holds[HoldBilling].State != Suspended || st.Holds[HoldBilling].EscalatedBy != "delinquent:72h:suspended" {
		t.Fatalf("after the billing hold's grace: %+v", st)
	}
	if logs.count("tenant acme: the billing hold escalated delinquent -> suspended", "the state stays paused from the operator hold") != 1 {
		t.Fatalf("the escalation under the operator hold was not logged:\n%s", logs)
	}
	if err := env.e.SetHold(ctx, "acme", HoldOperator, Active, "", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	escalatedAt := t1.Add(72 * time.Hour)
	env.want("the operator hold lifted", Suspended, env.clk.Now(), "delinquent:72h:suspended", Paused, escalatedAt.Add(7*24*time.Hour))
}

// An escalated hold keeps what raised it, so it lifts on the same terms. A
// zero-balance notification mapped to delinquent and escalated to suspended
// still lifts when Metronome, read again, shows the notification clear (a
// commit Finance added sends no payment webhook).
func TestRecheckLiftsAnEscalatedMetronomeHold(t *testing.T) {
	esc, _ := ParseEscalations("delinquent:72h:suspended")
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=delinquent")
	clk := newClock()
	acct := &account{resolver: resolver{"5f794d50-085a-4db6-8d15-286e518b7225": "acme"}}
	acct.still()
	e := New(nil, Options{Mode: "observe", MetronomeWebhookSecret: "mtr_secret", Rules: rules, Escalations: esc, Now: clk.Now,
		Resolvers: map[string]destinations.CustomerResolver{"metronome": acct}})
	body := []byte(fmt.Sprintf(`{"id":"alert-1","type":"alerts.low_remaining_commit_balance_reached","timestamp":%q,`+
		`"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225","alert_id":"a-1","alert_name":"Wallet empty"}}`, clk.Now().Format(time.RFC3339)))
	date := clk.Now().Format(http.TimeFormat)
	if _, err := e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret")); err != nil {
		t.Fatal(err)
	}
	clk.add(72 * time.Hour)
	e.Escalate(context.Background())
	if st := e.StateOf("acme"); st.State != Suspended || st.EscalatedBy == "" {
		t.Fatalf("after the grace: %+v", st)
	}
	acct.still(`threshold notification "Wallet empty" (low_remaining_commit_balance_reached) in alarm`)
	clk.add(time.Minute)
	e.RecheckHolds(context.Background())
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("lifted while the notification is in alarm: %+v", st)
	}
	acct.still()
	clk.add(time.Minute)
	e.RecheckHolds(context.Background())
	if st := e.StateOf("acme"); st.State != Active || len(st.Holds) != 0 {
		t.Fatalf("the wallet was refilled, the escalated hold stays: %+v", st)
	}
}

// An escalation applies only to the hold it was decided on: a hold that
// changed in between (a payment, a new signal) is left alone.
func TestEscalationLeavesAChangedHoldAlone(t *testing.T) {
	env := escalating(t, "observe", "delinquent:72h:suspended", "", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	env.clk.add(73 * time.Hour)
	r := env.e.escalations[Delinquent]
	_, _, res, err := env.e.store("acme", update{hold: HoldBilling, state: Suspended, reason: "x", escalate: &r, since: t0.Add(-time.Minute)})
	if err != nil || res.kind != outStale {
		t.Fatalf("an escalation decided on another since: %+v, %v; want it left alone", res, err)
	}
	if st := env.state(); st.State != Delinquent || st.EscalatedBy != "" {
		t.Fatalf("state %+v", st)
	}
}

// The next escalation is part of what GET /api/v1/billing-states and
// /api/v1/tenants serve: escalation {to, at, rule} on the state and the
// hold, escalated_by on an escalated one.
func TestEscalationJSON(t *testing.T) {
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	t0 := env.clk.Now()
	env.stripe("evt_failed", "invoice.payment_failed", "", t0)
	b, _ := json.Marshal(env.state())
	want := fmt.Sprintf(`"escalation":{"to":"suspended","at":%q,"rule":"delinquent:72h:suspended"}`, t0.Add(72*time.Hour).Format(time.RFC3339))
	if !strings.Contains(string(b), want) {
		t.Fatalf("state JSON %s\nlacks %s", b, want)
	}
	env.clk.add(72 * time.Hour)
	env.escalate()
	b, _ = json.Marshal(env.state())
	if !strings.Contains(string(b), `"escalated_by":"delinquent:72h:suspended"`) || !strings.Contains(string(b), `"to":"paused"`) {
		t.Fatalf("escalated state JSON %s", b)
	}
	rules, _ := json.Marshal(env.e.Escalations())
	if string(rules) != `[{"from":"delinquent","after":"72h","to":"suspended"},{"from":"suspended","after":"7d","to":"paused"}]` {
		t.Fatalf("rules JSON %s", rules)
	}
}

func TestParseEscalations(t *testing.T) {
	got, err := ParseEscalations(" delinquent : 72h : suspended , suspended:7d:paused,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "delinquent:72h:suspended" || got[0].After != 72*time.Hour ||
		got[1].String() != "suspended:7d:paused" || got[1].After != 7*24*time.Hour {
		t.Fatalf("parsed %+v", got)
	}
	for spec, want := range map[string]time.Duration{
		"delinquent:1d12h:paused": 36 * time.Hour, "delinquent:90m:suspended": 90 * time.Minute,
		"delinquent:60s:suspended": time.Minute, "delinquent:1.5h:suspended": 90 * time.Minute,
	} {
		if got, err := ParseEscalations(spec); err != nil || got[0].After != want {
			t.Errorf("%s: %+v, %v; want after %s", spec, got, err, want)
		}
	}
	if got, err := ParseEscalations(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v, %v", got, err)
	}
	for _, tc := range []struct{ spec, want string }{
		{"delinquent:72h", `escalation "delinquent:72h" must be from:after:to, for example delinquent:72h:suspended`},
		{"delinquent:72h:frozen", `escalation "delinquent:72h:frozen": unknown state "frozen" (states: active, warning, delinquent, suspended, paused)`},
		{"delinquent:3 days:suspended", `escalation "delinquent:3 days:suspended": after "3 days" must be a duration in d, h, m or s, such as 72h or 7d`},
		{"delinquent:500ms:suspended", `escalation "delinquent:500ms:suspended": after "500ms" must be a duration in d, h, m or s, such as 72h or 7d`},
		{"delinquent:72:suspended", `escalation "delinquent:72:suspended": after "72" must be a duration in d, h, m or s, such as 72h or 7d`},
		{"delinquent:30s:suspended", `escalation "delinquent:30s:suspended": after "30s" must be at least 1m`},
		{"delinquent:0d:suspended", `escalation "delinquent:0d:suspended": after "0d" must be at least 1m`},
		{"delinquent:72h:suspended,delinquent:7d:paused", `escalations "delinquent:72h:suspended" and "delinquent:7d:paused" both start at delinquent: give each state one rule`},
		{"delinquent:72h:suspended,suspended:7d:delinquent", `escalations loop: delinquent -> suspended -> delinquent`},
		{"suspended:1h:suspended", `escalations loop: suspended -> suspended`},
		{"suspended:7d:delinquent", `escalation "suspended:7d:delinquent": delinquent is not more severe than suspended`},
		{"delinquent:72h:warning", `escalation "delinquent:72h:warning": warning is not more severe than delinquent`},
		{"warning:24h:suspended", `escalation "warning:24h:suspended": only delinquent and suspended escalate`},
		{"active:24h:warning", `escalation "active:24h:warning": only delinquent and suspended escalate`},
		{"paused:30d:suspended", `escalation "paused:30d:suspended": only delinquent and suspended escalate`},
	} {
		if _, err := ParseEscalations(tc.spec); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v\nwant: %s", tc.spec, err, tc.want)
		}
	}
}

// vbilling check-config and start-up refuse escalations they cannot use,
// before the Lease is taken.
func TestValidateConfigChecksEscalations(t *testing.T) {
	cfg := &config.Config{EnforcementMode: "observe", PauseKeepBilling: "storage", PauseDrainTimeout: time.Minute,
		EnforcementEscalations: "delinquent:72h:suspended,suspended:7d:paused"}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("valid escalations: %v", err)
	}
	cfg.EnforcementEscalations = "delinquent:30s:suspended"
	if err := ValidateConfig(cfg); err == nil || err.Error() != `ENFORCEMENT_ESCALATIONS: escalation "delinquent:30s:suspended": after "30s" must be at least 1m` {
		t.Fatalf("an escalation shorter than a minute: %v", err)
	}
}

// vbilling_enforcement_escalations_total has a series for every rule from
// the start, and none for a state no rule leaves.
func TestEscalationCounterSeries(t *testing.T) {
	env := escalating(t, "observe", "delinquent:72h:suspended,suspended:7d:paused", "", nil)
	for _, l := range []map[string]string{{"from": "delinquent", "to": "suspended"}, {"from": "suspended", "to": "paused"}} {
		if !env.e.opts.Telemetry.Has(escalationsMetric, l) {
			t.Errorf("no series %v", l)
		}
	}
	if env.e.opts.Telemetry.Has(escalationsMetric, map[string]string{"from": "delinquent", "to": "paused"}) {
		t.Error("a series for a rule that does not exist")
	}
	// Without rules, nothing escalates and nothing is shown.
	none := escalating(t, "observe", "", "", nil)
	none.stripe("evt_failed", "invoice.payment_failed", "", none.clk.Now())
	none.clk.add(365 * 24 * time.Hour)
	none.escalate()
	if st := none.state(); st.State != Delinquent || st.Escalation != nil || st.Holds[HoldBilling].Escalation != nil {
		t.Fatalf("without escalation rules: %+v", st)
	}
}
