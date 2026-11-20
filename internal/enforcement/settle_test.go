package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// account is a billing platform that can be read again (ArrearsReader):
// owed is what it still shows unpaid or in alarm.
type account struct {
	resolver
	mu    sync.Mutex
	owed  map[string]bool
	err   error
	reads int
	asked []string // the customer of each read ("" for the tenant's own)
}

func (a *account) Owed(_ context.Context, _, customer string, _ func(...string) bool) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	a.asked = append(a.asked, customer)
	if a.err != nil {
		return nil, a.err
	}
	out := make([]string, 0, len(a.owed))
	for k := range a.owed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// still sets what the platform shows owed from now on.
func (a *account) still(owed ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.owed = map[string]bool{}
	for _, o := range owed {
		a.owed[o] = true
	}
}

// readable makes a provider of e's resolvers readable again; it shows
// owed until told otherwise.
func readable(e *Enforcer, provider string, owed ...string) *account {
	a := &account{resolver: e.opts.Resolvers[provider].(resolver)}
	a.still(owed...)
	e.opts.Resolvers[provider] = a
	return a
}

func stripeBody(id, typ string, created time.Time, object string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{%s,"customer":"cus_acme"}}}`, id, typ, created.Unix(), object))
}

func sendStripe(t *testing.T, e *Enforcer, body []byte) error {
	t.Helper()
	_, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now))
	return err
}

// A payment never lowers a hold by itself: the subscription that is active
// again leaves the tenant suspended while Stripe, read again, still shows a
// one-off invoice that failed meanwhile. Once nothing is owed, a payment
// clears the hold.
func TestPaymentClearsAHoldOnlyOnceNothingIsOwed(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	acct := readable(e, "stripe")
	at := now.Add(-time.Hour)
	step := func(body []byte, want State) {
		t.Helper()
		if err := sendStripe(t, e, body); err != nil {
			t.Fatal(err)
		}
		if st := e.StateOf("acme"); st.State != want {
			t.Fatalf("state %s (%s), want %s", st.State, st.Reason, want)
		}
		at = at.Add(time.Minute)
	}
	step(stripeBody("e1", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"unpaid"`), Suspended)
	step(stripeBody("e2", "invoice.payment_failed", at, `"object":"invoice","id":"in_oneoff","subscription":null`), Suspended)
	acct.still("invoice in_oneoff open, payment failed (attempt 1)")
	step(stripeBody("e3", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"active"`), Suspended)
	if l := nsLabels(t, kube); l[LabelSuspended] != "true" {
		t.Fatalf("the one-off invoice is still owed, the namespace is unblocked: %v", l)
	}
	acct.still()
	step(stripeBody("e4", "invoice.paid", at, `"object":"invoice","id":"in_oneoff","subscription":null`), Active)
	if l := nsLabels(t, kube); l[LabelSuspended] != "" {
		t.Fatalf("nothing is owed, the namespace is still blocked: %v", l)
	}
}

// Paying an invoice does not tell what else is owed. Stripe, read again for
// the paying customer, decides: while it still shows a subscription unpaid
// the suspension stays; once nothing is owed, the next payment lifts it.
func TestPaymentLiftsAHoldOnceStripeShowsNothingOwed(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	acct := readable(e, "stripe", "subscription sub_1 unpaid")
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "invoice.payment_failed", at,
		`"object":"invoice","id":"in_big","parent":{"subscription_details":{"subscription":"sub_1"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("e2", "customer.subscription.updated", at.Add(time.Minute), `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	if acct.reads != 0 {
		t.Fatalf("Stripe was read %d times for events that only raise the hold", acct.reads)
	}
	if err := sendStripe(t, e, stripeBody("e3", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_small","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended || nsLabels(t, kube)[LabelSuspended] != "true" {
		t.Fatalf("a small payment lifted the suspension while Stripe shows sub_1 unpaid: %+v", st)
	}
	if got := strings.Join(acct.asked, ","); got != "cus_acme" {
		t.Fatalf("Stripe was read for %q, want the paying customer cus_acme", got)
	}
	acct.still()
	if err := sendStripe(t, e, stripeBody("e4", "invoice.paid", at.Add(3*time.Minute), `"object":"invoice","id":"in_big","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Active || nsLabels(t, kube)[LabelSuspended] != "" {
		t.Fatalf("the latest invoice is paid and Stripe shows nothing owed: %+v", st)
	}
}

// Stripe also shows what no webhook reported: another subscription of the
// tenant still past due keeps the hold.
func TestAnythingElseStillOwedKeepsTheHold(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	acct := readable(e, "stripe", "subscription:sub_other")
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("e2", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_1","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("state %s, want suspended: Stripe still shows sub_other in arrears", st.State)
	}
	acct.still()
	if err := sendStripe(t, e, stripeBody("e3", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_2","subscription":"sub_other"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("state %s, want active once nothing is owed", st.State)
	}
}

// A billing platform that cannot be read again never lowers a hold on a
// guess: the webhook fails, is not remembered as applied, and the
// provider's retry applies it once the platform answers.
func TestFailedReadIsRetriedNotGuessed(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	acct := readable(e, "stripe")
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	acct.err = errors.New("stripe: 503")
	paid := stripeBody("e2", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_1","subscription":"sub_1"`)
	err := sendStripe(t, e, paid)
	if err == nil || errors.Is(err, ErrIgnored) || errors.Is(err, ErrDuplicate) || errors.Is(err, ErrUnknownCustomer) {
		t.Fatalf("a failed read: %v, want an error the provider retries", err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("state %s after a failed read, want suspended", st.State)
	}
	acct.err = nil
	if err := sendStripe(t, e, paid); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("state %s after the retry, want active", st.State)
	}
}

// A billing hold set through the API (with "hold": "billing"), or by an
// older version, follows the same rule as any other: a payment clears it
// once the billing platform, read again, shows nothing owed, and not
// before.
func TestBillingHoldFromTheAPIClearsOnceNothingIsOwed(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "unpaid since October", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	at := now.Add(time.Minute) // after the operator's change
	if err := sendStripe(t, e, stripeBody("e1", "invoice.paid", at, `"object":"invoice","id":"in_1","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("a payment lifted a billing hold no platform confirmed: %s", st.State)
	}
	acct := readable(e, "stripe", "subscription sub_1 unpaid")
	if err := sendStripe(t, e, stripeBody("e2", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_2","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("state %s while Stripe shows sub_1 owed, want suspended", st.State)
	}
	acct.still()
	if err := sendStripe(t, e, stripeBody("e3", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_3","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("state %s once Stripe shows nothing owed, want active", st.State)
	}
	// An operator's write replaces the hold.
	if err := sendStripe(t, e, stripeBody("e4", "customer.subscription.updated", at.Add(3*time.Minute), `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if h := e.StateOf("acme").Holds[HoldBilling]; h.State != Paused || h.Source != "api" {
		t.Fatalf("hold after an operator's write = %+v, want the operator's pause", h)
	}
}

// Holds stored by v0.3.0, and by an earlier v0.3.1 build that recorded
// causes, load as billing holds and follow the same rule: a payment clears
// them only once nothing is owed. The causes are ignored, and dropped when
// the hold is stored again.
func TestOlderHoldsLoadAsBillingHolds(t *testing.T) {
	since := now.Add(-20 * 24 * time.Hour)
	state := kv.Memory()
	for tenant, raw := range map[string]string{
		"v030": `{"tenant":"v030","state":"suspended","reason":"subscription unpaid","source":"stripe","event_type":"customer.subscription.updated",` +
			`"since":"` + since.Format(time.RFC3339) + `","hold":"billing","holds":{"billing":{"state":"suspended","reason":"subscription unpaid",` +
			`"source":"stripe","event_type":"customer.subscription.updated","since":"` + since.Format(time.RFC3339) + `"}},` +
			`"event_times":{"billing":"` + since.Format(time.RFC3339) + `"}}`,
		"v031dev": `{"tenant":"v031dev","state":"paused","reason":"Prepaid balance low","source":"metronome","since":"` + since.Format(time.RFC3339) + `",` +
			`"holds":{"billing":{"state":"paused","reason":"Prepaid balance low","source":"metronome","since":"` + since.Format(time.RFC3339) + `",` +
			`"causes":[{"provider":"metronome","customer":"cus-1","object":"alert:a-1","state":"paused","at":"` + since.Format(time.RFC3339) + `"}]}},` +
			`"event_times":{"billing":"` + since.Format(time.RFC3339) + `"}}`,
	} {
		if err := state.Set("state/"+tenant, raw); err != nil {
			t.Fatal(err)
		}
	}
	acct := &account{resolver: resolver{"cus_v030": "v030", "cus_v031dev": "v031dev"}}
	acct.still("subscription sub_1 unpaid")
	e := New(nil, Options{Mode: "observe", StripeWebhookSecret: "whsec_test", State: state, Now: func() time.Time { return now },
		Resolvers: map[string]destinations.CustomerResolver{"stripe": acct}})
	for _, tenant := range []string{"v030", "v031dev"} {
		if st := e.StateOf(tenant); !st.State.blocks() || st.Holds[HoldBilling].State != st.State {
			t.Fatalf("%s loaded as %+v, want its billing hold", tenant, st)
		}
	}
	pay := func(id, cus string) {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"id":%q,"type":"invoice.paid","created":%d,"data":{"object":{"object":"invoice","id":"in_x","customer":%q}}}`,
			id, now.Add(-time.Minute).Unix(), cus))
		if _, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now)); err != nil {
			t.Fatal(err)
		}
	}
	pay("p1", "cus_v030")
	pay("p2", "cus_v031dev")
	for _, tenant := range []string{"v030", "v031dev"} {
		if st := e.StateOf(tenant); !st.State.blocks() {
			t.Fatalf("%s: a payment lifted an older hold while Stripe still shows something owed: %+v", tenant, st)
		}
	}
	if raw, _ := state.Get("state/v031dev"); strings.Contains(raw, `"causes"`) {
		t.Fatalf("the causes of an older build were stored again: %s", raw)
	}
	acct.still()
	pay("p3", "cus_v030")
	pay("p4", "cus_v031dev")
	for _, tenant := range []string{"v030", "v031dev"} {
		if st := e.StateOf(tenant); st.State != Active {
			t.Fatalf("%s: nothing owed, and the older hold stays: %+v", tenant, st)
		}
	}
}

func sendMetronome(t *testing.T, e *Enforcer, id, typ, props string, at time.Time) error {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225","timestamp":%q%s}}`,
		id, typ, at.Format(time.RFC3339), props))
	date := now.Format(http.TimeFormat)
	_, err := e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret"))
	return err
}

// The prepaid wallet: the commit's low-balance alert pauses the tenant,
// and a top-up resumes it only once Metronome, read again, shows no alert
// that counts in alarm. A top-up too small to lift the balance over the
// threshold leaves the alert in alarm. Metronome evaluates again within
// minutes of a top-up, so for an hour the payment is answered with an
// error and retried; after that it is applied and the pause stays.
func TestTopUpResumesOnlyOnceTheAlertIsOk(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	e, _, _ := setup(t, "observe", rules)
	acct := readable(e, "metronome", `threshold notification "Prepaid balance low" (low_remaining_commit_balance_reached) in alarm`)
	at := now.Add(-10 * time.Minute)
	if err := sendMetronome(t, e, "m1", "alerts.low_remaining_commit_balance_reached", `,"alert_id":"a-1"`, at); err != nil {
		t.Fatal(err)
	}
	if err := sendMetronome(t, e, "m2", "payment_gate.payment_status", `,"payment_status":"failed","invoice_id":"inv-1"`, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	topUp := func() error {
		return sendMetronome(t, e, "m3", "payment_gate.payment_status", `,"payment_status":"paid","invoice_id":"inv-2"`, at.Add(2*time.Minute))
	}
	if err := topUp(); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("a top-up while the alert is in alarm: %v, want ErrNotSettled so Metronome retries", err)
	}
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("state %s, want paused while the alert is in alarm", st.State)
	}
	if got := acct.asked[len(acct.asked)-1]; got != "5f794d50-085a-4db6-8d15-286e518b7225" {
		t.Fatalf("Metronome was read for customer %q, want the paying customer", got)
	}
	acct.still() // Metronome evaluated the alert again: ok
	if err := topUp(); err != nil {
		t.Fatalf("the retried top-up: %v", err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("state %s once the alert is ok, want active", st.State)
	}

	// A top-up more than an hour old whose alert is still in alarm is
	// applied and changes nothing.
	old := now.Add(-3 * time.Hour)
	f, _, _ := setup(t, "observe", rules)
	readable(f, "metronome", "alert in alarm")
	if err := sendMetronome(t, f, "m5", "alerts.low_remaining_commit_balance_reached", `,"alert_id":"a-2"`, old); err != nil {
		t.Fatal(err)
	}
	if err := sendMetronome(t, f, "m6", "payment_gate.payment_status", `,"payment_status":"paid","invoice_id":"inv-3"`, old.Add(time.Minute)); err != nil {
		t.Fatalf("an old top-up: %v, want it applied", err)
	}
	if st := f.StateOf("acme"); st.State != Paused {
		t.Fatalf("state %s, want paused: the alert is still in alarm", st.State)
	}
}

// Every billing platform vBilling reads is read before a payment clears a
// hold, not only the paying one: in Metronome + Stripe mode a Stripe
// invoice paid (a $1 top-up is charged through Stripe too) never resumes a
// tenant whose Metronome wallet is still empty, and a Metronome top-up
// never resumes one Stripe still shows in arrears. The other platform is
// read for the tenant's own customer there.
func TestEveryPlatformIsReadBeforeAHoldClears(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	e, _, _ := setup(t, "observe", rules)
	stripeAcct := readable(e, "stripe")
	metro := readable(e, "metronome", "wallet notification in alarm")
	at := now.Add(-3 * time.Hour) // past Metronome's retry hour
	if err := sendMetronome(t, e, "m1", "alerts.low_remaining_commit_balance_reached", `,"alert_id":"a-1"`, at); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("s1", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_topup"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("a Stripe payment resumed a tenant whose Metronome wallet is empty: %s (%s)", st.State, st.Reason)
	}
	if len(metro.asked) != 1 || metro.asked[0] != "" {
		t.Fatalf("Metronome was read for customers %q, want once, for the tenant's own (\"\")", metro.asked)
	}
	metro.still()
	stripeAcct.still("subscription sub_1 past_due")
	if err := sendMetronome(t, e, "m2", "payment_gate.payment_status", `,"payment_status":"paid"`, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("a Metronome top-up resumed a tenant Stripe shows in arrears: %s (%s)", st.State, st.Reason)
	}
	stripeAcct.still()
	if err := sendStripe(t, e, stripeBody("s2", "invoice.paid", at.Add(3*time.Minute), `"object":"invoice","id":"in_2"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("nothing owed anywhere, state %s, want active", st.State)
	}
}

// A resolver that cannot be read again (no ArrearsReader) never confirms
// anything: a payment leaves the hold as it is.
func TestResolverThatCannotBeReadNeverConfirms(t *testing.T) {
	e := New(nil, Options{Mode: "observe", StripeWebhookSecret: "whsec_test", Now: func() time.Time { return now },
		Resolvers: map[string]destinations.CustomerResolver{"stripe": resolver{"cus_acme": "acme"}}})
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "customer.subscription.deleted", at, `"object":"subscription","id":"sub_1","status":"canceled"`)); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("e2", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_1","subscription":"sub_2"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("state %s, want suspended", st.State)
	}
	if err := sendStripe(t, e, stripeBody("e3", "customer.subscription.created", at.Add(2*time.Minute), `"object":"subscription","id":"sub_2","status":"active"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("a new subscription lifted the suspension of a deleted one nothing confirmed paid: %s", st.State)
	}
}

// A payment that finds a billing hold raised after it read the platforms
// (it found none then) applies nothing to it and is retried, so the retry
// reads them for the new hold.
func TestHoldRaisedWhileAPaymentIsReadIsRetried(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	at := now.Add(-time.Hour)
	// The payment found no hold; a failure that happened before it is
	// stored meanwhile.
	ts, _, res, err := e.store("acme", update{hold: HoldBilling, state: Suspended, reason: "subscription unpaid", source: "stripe",
		eventType: "customer.subscription.updated", webhook: true, at: at})
	if err != nil || res.kind != outChanged || ts.State != Suspended {
		t.Fatalf("test setup: %+v %+v %v", ts, res, err)
	}
	_, _, res, err = e.store("acme", update{hold: HoldBilling, state: Active, reason: "invoice paid", source: "stripe",
		eventType: "invoice.paid", webhook: true, positive: true, at: at.Add(time.Minute)})
	if err != nil || res.kind != outUnread {
		t.Fatalf("a payment that read nothing for the hold: result %+v, %v; want it retried", res, err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("state %s: a payment that read nothing lowered the hold", st.State)
	}
}
