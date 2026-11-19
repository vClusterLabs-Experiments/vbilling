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
)

// account is a billing platform that can be read again (ArrearsReader):
// owed is what it still shows unpaid or in alarm.
type account struct {
	resolver
	mu    sync.Mutex
	owed  map[string]bool
	err   error
	reads int
	asked [][]string
}

func (a *account) Owed(_ context.Context, _, _ string, causes []string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	a.asked = append(a.asked, causes)
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

// A payment lowers a hold only as far as it settles what caused it: the
// subscription that is active again settles that subscription, not a
// one-off invoice that failed meanwhile, which keeps the tenant delinquent
// until it is paid itself.
func TestPaymentLowersAHoldOnlyAsFarAsItSettlesItsCauses(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
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
	step(stripeBody("e3", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"active"`), Delinquent)
	if l := nsLabels(t, kube); l[LabelSuspended] != "" {
		t.Fatalf("the subscription is active again, the namespace is still blocked: %v", l)
	}
	if st := e.StateOf("acme"); !strings.Contains(st.Reason, "invoice payment failed") || len(st.Holds[HoldBilling].Causes) != 1 {
		t.Fatalf("state %+v, want the one-off invoice left as the cause", st)
	}
	step(stripeBody("e4", "invoice.paid", at, `"object":"invoice","id":"in_oneoff","subscription":null`), Active)
}

// Paying another invoice does not settle a subscription in arrears by
// itself. Stripe, read again, decides: while it still shows the
// subscription unpaid the suspension stays; once nothing is owed, the next
// payment lifts it. The invoice's subscription is read from the field newer
// API versions moved it to as well.
func TestPaymentLiftsAHoldOnceStripeShowsNothingOwed(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	acct := readable(e, "stripe", "subscription:sub_1")
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "invoice.payment_failed", at,
		`"object":"invoice","id":"in_big","parent":{"subscription_details":{"subscription":"sub_1"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("e2", "customer.subscription.updated", at.Add(time.Minute), `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	if err := sendStripe(t, e, stripeBody("e3", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_small","subscription":"sub_1"`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended || nsLabels(t, kube)[LabelSuspended] != "true" {
		t.Fatalf("a small payment lifted the suspension while Stripe shows sub_1 unpaid: %+v", st)
	}
	if got := fmt.Sprint(acct.asked); got != "[[subscription:sub_1]]" {
		t.Fatalf("Stripe was asked about %s, want the subscription that caused the hold", got)
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

// A billing hold set through the API, or stored before causes were
// recorded, names no cause: a payment lowers it only once its platform,
// read again, confirms the tenant owes nothing.
func TestHoldWithoutACauseNeedsTheProviderToConfirm(t *testing.T) {
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
		t.Fatalf("a payment lifted an operator's billing hold no platform confirmed: %s", st.State)
	}
	acct := readable(e, "stripe", "subscription:sub_1")
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
	// An operator's write replaces the hold and its causes.
	if err := sendStripe(t, e, stripeBody("e4", "customer.subscription.updated", at.Add(3*time.Minute), `"object":"subscription","id":"sub_1","status":"unpaid"`)); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if h := e.StateOf("acme").Holds[HoldBilling]; h.State != Paused || len(h.Causes) != 0 {
		t.Fatalf("hold after an operator's write = %+v, want paused with no causes", h)
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
// and a top-up resumes it only once Metronome evaluates that alert ok
// again. A top-up too small to lift the balance over the threshold leaves
// the alert in alarm. Metronome evaluates again within minutes of a top-up,
// so for an hour the payment is answered with an error and retried; after
// that it is applied and the pause stays.
func TestTopUpResumesOnlyOnceTheAlertIsOk(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	e, _, _ := setup(t, "observe", rules)
	acct := readable(e, "metronome", "alert:a-1")
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
	if got := fmt.Sprint(acct.asked[len(acct.asked)-1]); got != "[alert:a-1]" {
		t.Fatalf("Metronome was asked about %s, want only the alert (the failed top-up is settled by the payment)", got)
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
	readable(f, "metronome", "alert:a-2")
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

// A resolver that cannot be read again (no ArrearsReader) never confirms
// anything: a payment that does not settle the hold itself leaves it.
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
