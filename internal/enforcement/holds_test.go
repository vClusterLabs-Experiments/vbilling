package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// stripeAt sends a Stripe event created at the given time.
func stripeAt(e *Enforcer, id, typ, status string, created time.Time) error {
	body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{"customer":"cus_acme","status":%q}}}`, id, typ, created.Unix(), status))
	_, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now))
	return err
}

// Events apply in the order Stripe says they happened, whatever order they
// arrive in: an older event delivered late never undoes a newer one.
func TestWebhooksApplyInTheOrderTheyHappened(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	if err := stripeAt(e, "evt_paid", "invoice.paid", "paid", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err := stripeAt(e, "evt_unpaid", "customer.subscription.updated", "unpaid", now)
	if !errors.Is(err, ErrIgnored) {
		t.Fatalf("an event older than the newest applied one: %v, want it ignored", err)
	}
	if st := e.StateOf("acme"); st.State != Active || nsLabels(t, kube)[LabelSuspended] != "" {
		t.Fatalf("an older unpaid event suspended a tenant that paid since: %+v", st)
	}
	if err := stripeAt(e, "evt_unpaid_2", "customer.subscription.updated", "unpaid", now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("a newer unpaid event did not suspend: %+v", st)
	}
	// An old payment retried late does not lift it either.
	if err := stripeAt(e, "evt_paid_old", "invoice.paid", "paid", now.Add(5*time.Minute)); !errors.Is(err, ErrIgnored) {
		t.Fatalf("an old payment: %v, want it ignored", err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("an old payment lifted a newer suspension: %+v", st)
	}
}

// Only a payment or an active subscription lowers the billing hold; any
// other signal can only raise it.
func TestOnlyPositiveSignalsLowerTheBillingHold(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	e, _, _ := setup(t, "observe", rules)
	at := now
	next := func() time.Time { at = at.Add(time.Minute); return at }
	step := func(id, typ, status string, want State) {
		t.Helper()
		if err := stripeAt(e, id, typ, status, next()); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if st := e.StateOf("acme"); st.State != want {
			t.Fatalf("after %s (%s %s): state %s (%s), want %s", id, typ, status, st.State, st.Reason, want)
		}
	}
	step("e1", "customer.subscription.updated", "unpaid", Suspended)
	step("e2", "invoice.payment_failed", "", Suspended)
	step("e3", "customer.subscription.updated", "past_due", Suspended)
	step("e4", "billing.alert.triggered", "", Suspended)
	step("e5", "customer.subscription.updated", "active", Active)
	step("e6", "customer.subscription.deleted", "", Suspended)
	step("e7", "invoice.payment_succeeded", "", Active)
	step("e8", "invoice.payment_failed", "", Delinquent)
	step("e9", "customer.subscription.updated", "unpaid", Suspended)

	metro := func(id, typ, props string) {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"timestamp":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"%s}}`,
			id, typ, next().Format(time.RFC3339), props))
		date := now.Format(http.TimeFormat)
		if _, err := e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret")); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	metro("m1", "alerts.low_remaining_commit_balance_reached", "")
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("prepaid commit alert rule: %+v", st)
	}
	metro("m2", "payment_gate.payment_status", `,"payment_status":"failed"`)
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("a failed threshold payment lifted the pause: %+v", st)
	}
	metro("m3", "payment_gate.payment_status", `,"payment_status":"paid"`)
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("a successful threshold payment did not clear the hold: %+v", st)
	}
}

// Operators set any state through the API, and what they set is newer than
// any event about something earlier.
func TestOperatorSetsAnyBillingState(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Warning, "topped up by hand", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Warning {
		t.Fatalf("operator could not lower the hold: %+v", st)
	}
	if err := stripeAt(e, "evt_old", "customer.subscription.updated", "unpaid", now.Add(-time.Hour)); !errors.Is(err, ErrIgnored) {
		t.Fatalf("an event older than the operator's change: %v, want it ignored", err)
	}
}

// Applied event IDs survive a restart, so a redelivery is still a
// duplicate.
func TestAppliedEventsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing-state.json")
	open := func() *Enforcer {
		state, err := kv.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return New(nil, Options{Mode: "observe", StripeWebhookSecret: "whsec_test", State: state,
			Resolvers: map[string]destinations.CustomerResolver{"stripe": resolver{"cus_acme": "acme"}},
			Now:       func() time.Time { return now }})
	}
	if err := stripeAt(open(), "evt_1", "invoice.payment_failed", "", now); err != nil {
		t.Fatal(err)
	}
	if err := stripeAt(open(), "evt_1", "invoice.payment_failed", "", now); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("redelivery after a restart: %v, want a duplicate", err)
	}
}

// The applied events kept are bounded: those older than 30 days go, and
// past the limit the oldest go.
func TestAppliedEventsAreBounded(t *testing.T) {
	defer func(n int) { appliedMax = n }(appliedMax)
	appliedMax = 3
	at := now
	e := New(nil, Options{Mode: "observe", StripeWebhookSecret: "whsec_test",
		Resolvers: map[string]destinations.CustomerResolver{"stripe": resolver{"cus_acme": "acme"}},
		Now:       func() time.Time { return at }})
	for i := 0; i < 5; i++ {
		at = at.Add(time.Minute)
		body := []byte(fmt.Sprintf(`{"id":"evt_%d","type":"invoice.payment_failed","created":%d,"data":{"object":{"customer":"cus_acme"}}}`, i, at.Unix()))
		if _, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", at)); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.applied) != 3 || !e.wasApplied("stripe:evt_4") || e.wasApplied("stripe:evt_0") {
		t.Fatalf("applied = %v, want the 3 newest", e.applied)
	}
	at = at.Add(31 * 24 * time.Hour)
	if err := e.markApplied("stripe:evt_new"); err != nil {
		t.Fatal(err)
	}
	if len(e.applied) != 1 {
		t.Fatalf("applied after 31 days = %v, want only the new one", e.applied)
	}
}

// Metronome notifications are ordered by their own timestamp (top level,
// or under properties), else by the signed date they were sent.
func TestMetronomeEventTime(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	send := func(body string, date time.Time) Signal {
		t.Helper()
		d := date.Format(http.TimeFormat)
		sig, err := e.HandleMetronome(context.Background(), []byte(body), d, metronome.Sign([]byte(body), d, "mtr_secret"))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	top := now.Add(-time.Hour)
	if sig := send(fmt.Sprintf(`{"id":"a","type":"alerts.spend_threshold_reached","timestamp":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"}}`, top.Format(time.RFC3339)), now); !sig.EventAt.Equal(top) {
		t.Fatalf("top-level timestamp: event at %s, want %s", sig.EventAt, top)
	}
	inner := now.Add(-30 * time.Minute)
	if sig := send(fmt.Sprintf(`{"id":"b","type":"alerts.spend_threshold_reached","properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225","timestamp":%q}}`, inner.Format(time.RFC3339Nano)), now); !sig.EventAt.Equal(inner) {
		t.Fatalf("properties timestamp: event at %s, want %s", sig.EventAt, inner)
	}
	if sig := send(`{"id":"c","type":"alerts.spend_threshold_reached","properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"}}`, now); !sig.EventAt.Equal(now) {
		t.Fatalf("no timestamp: event at %s, want the signed date %s", sig.EventAt, now)
	}
}

// Concurrent changes to one tenant's holds are decided and written in
// order: what is on disk is what is in memory, and no hold is lost.
func TestHoldChangesAreSerializedAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing-state.json")
	state, err := kv.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := New(nil, Options{Mode: "observe", State: state})
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := e.SetHold(context.Background(), "acme", fmt.Sprintf("h%02d", i), Suspended, "x", "api", "manual"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	state2, err := kv.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if mem, disk := e.StateOf("acme"), New(nil, Options{State: state2}).StateOf("acme"); len(mem.Holds) != 24 || len(disk.Holds) != 24 {
		t.Fatalf("holds in memory %d, on disk %d, want 24 each", len(mem.Holds), len(disk.Holds))
	}
}

// aliasResolver is a billing platform whose customers can carry several
// ingest aliases (Metronome).
type aliasResolver struct {
	mapped  map[string]string
	aliases map[string][]string
	err     error
	mu      sync.Mutex
	listed  int
}

func (r *aliasResolver) TenantForCustomer(_ context.Context, id string) (string, bool) {
	t, ok := r.mapped[id]
	return t, ok
}

func (r *aliasResolver) CustomerAliases(_ context.Context, id string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listed++
	return r.aliases[id], r.err
}

// A webhook for a customer vBilling did not map applies to the customer's
// aliases that are tenants vBilling knows, or to all of them when none is:
// never to the first alone.
func TestWebhookForACustomerWithSeveralAliases(t *testing.T) {
	r := &aliasResolver{mapped: map[string]string{"cus-mapped": "mapped-co"}, aliases: map[string][]string{
		"cus-portal": {"identity-1", "compute-1"},
		"cus-new":    {"identity-2", "compute-2", "identity-2"},
		"cus-held":   {"identity-3", "compute-3"},
		"cus-none":   nil,
	}}
	e := New(nil, Options{Mode: "observe", MetronomeWebhookSecret: "mtr_secret", Now: func() time.Time { return now },
		Resolvers: map[string]destinations.CustomerResolver{"metronome": r},
		IsTenant:  func(id string) bool { return id == "compute-1" }}) // discovered from a cluster
	send := func(id, cus string) (Signal, error) {
		body := []byte(fmt.Sprintf(`{"id":%q,"type":"payment_gate.payment_status","properties":{"customer_id":%q,"payment_status":"failed"}}`, id, cus))
		date := now.Format(http.TimeFormat)
		return e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret"))
	}
	if sig, err := send("w1", "cus-portal"); err != nil || len(sig.Tenants) != 1 || sig.Tenants[0] != "compute-1" {
		t.Fatalf("known alias: %+v, %v; want only compute-1", sig, err)
	}
	if e.StateOf("identity-1").State != Active || e.StateOf("compute-1").State != Delinquent {
		t.Fatalf("states: identity-1 %s, compute-1 %s", e.StateOf("identity-1").State, e.StateOf("compute-1").State)
	}
	if sig, err := send("w2", "cus-new"); err != nil || len(sig.Tenants) != 2 {
		t.Fatalf("no known alias: %+v, %v; want both aliases", sig, err)
	}
	if e.StateOf("identity-2").State != Delinquent || e.StateOf("compute-2").State != Delinquent {
		t.Fatal("a signal for a customer with no known alias did not reach every alias")
	}
	// A tenant that already holds a state is known.
	if err := e.Set(context.Background(), "identity-3", Warning, "set by hand", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if sig, err := send("w3", "cus-held"); err != nil || len(sig.Tenants) != 1 || sig.Tenants[0] != "identity-3" {
		t.Fatalf("alias holding a state: %+v, %v", sig, err)
	}
	before := r.listed
	if sig, err := send("w4", "cus-mapped"); err != nil || sig.Tenant != "mapped-co" || r.listed != before {
		t.Fatalf("mapped customer: %+v, %v, aliases listed %d times", sig, err, r.listed-before)
	}
	if _, err := send("w5", "cus-none"); !errors.Is(err, ErrUnknownCustomer) {
		t.Fatalf("customer without aliases: %v", err)
	}
	r.err = errors.New("metronome: 503")
	if _, err := send("w6", "cus-portal"); err == nil || errors.Is(err, ErrUnknownCustomer) {
		t.Fatalf("aliases unavailable: %v, want an error the provider retries", err)
	}
}
