package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

func webhooks(e *Enforcer, source, result string) float64 {
	return e.opts.Telemetry.Get(webhooksMetric, map[string]string{"source": source, "result": result})
}

// Every billing webhook is counted in vbilling_webhooks_total by its
// platform and by what became of it, and every series is there from the
// start, at zero.
func TestWebhooksAreCountedByWhatBecameOfThem(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	for _, source := range []string{"stripe", "metronome"} {
		for _, result := range webhookResults {
			if !e.opts.Telemetry.Has(webhooksMetric, map[string]string{"source": source, "result": result}) {
				t.Fatalf("no %s series for %s %s before the first webhook", webhooksMetric, source, result)
			}
		}
	}
	acct := readable(e, "stripe")
	want := map[string]float64{}
	step := func(result string, send func() error) {
		t.Helper()
		err := send()
		want[result]++
		for _, r := range webhookResults {
			if got := webhooks(e, "stripe", r); got != want[r] {
				t.Fatalf("after a webhook that was %s (%v): %s{source=stripe,result=%s} = %v, want %v", result, err, webhooksMetric, r, got, want[r])
			}
		}
	}
	at := now.Add(-time.Hour)
	unpaid := stripeBody("e1", "customer.subscription.updated", at, `"object":"subscription","id":"sub_1","status":"unpaid"`)
	step(WebhookApplied, func() error { return sendStripe(t, e, unpaid) })
	step(WebhookDuplicate, func() error { return sendStripe(t, e, unpaid) })
	step(WebhookIgnored, func() error {
		return sendStripe(t, e, stripeBody("e2", "customer.created", at, `"object":"customer","id":"cus_acme"`))
	})
	step(WebhookIgnored, func() error { // older than the newest event applied to the hold
		return sendStripe(t, e, stripeBody("e3", "invoice.payment_failed", at.Add(-time.Hour), `"object":"invoice","id":"in_old"`))
	})
	step(WebhookUnknownCustomer, func() error {
		body := []byte(`{"id":"e4","type":"invoice.payment_failed","data":{"object":{"customer":"cus_stranger"}}}`)
		_, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now))
		return err
	})
	step(WebhookInvalid, func() error {
		_, err := e.HandleStripe(context.Background(), unpaid, stripe.SignatureHeader(unpaid, "another secret", now))
		return err
	})
	step(WebhookInvalid, func() error {
		body := []byte(`{"id":"e5","type":`)
		_, err := e.HandleStripe(context.Background(), body, stripe.SignatureHeader(body, "whsec_test", now))
		return err
	})
	// A payment while Stripe still shows something owed lifts nothing.
	acct.still("subscription sub_1 unpaid")
	step(WebhookRefused, func() error {
		return sendStripe(t, e, stripeBody("e6", "invoice.paid", at.Add(time.Minute), `"object":"invoice","id":"in_small","subscription":"sub_1"`))
	})
	// A billing platform that cannot be read: the webhook fails and is
	// delivered again.
	acct.err = errors.New("stripe: 503")
	paid := stripeBody("e7", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_big","subscription":"sub_1"`)
	step(WebhookError, func() error { return sendStripe(t, e, paid) })
	acct.err = nil
	acct.still()
	step(WebhookApplied, func() error { return sendStripe(t, e, paid) })
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("test setup: state %s after the payment", st.State)
	}
	if got := webhooks(e, "metronome", WebhookApplied); got != 0 {
		t.Fatalf("Stripe webhooks counted for Metronome: %v", got)
	}
}

// A payment that does not unlock its tenant. In the live validation of
// v0.3.2 a payment that found the wallet's threshold notification still in
// alarm showed only as a log line and an HTTP 500 to the billing platform.
// It is counted (unsettled while the platform delivers it again, refused
// once it is applied and the hold stays), and it shows as the source
// billing-unlock/<tenant> until the hold is lifted, across restarts.
func TestPaymentThatDoesNotUnlockItsTenantIsCountedAndShownUntilTheHoldLifts(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	e, _, state := setup(t, "observe", rules)
	sources := quietHealth()
	e.UseHealth(sources)
	acct := readable(e, "metronome", `threshold notification "Prepaid balance low" (low_remaining_commit_balance_reached) in alarm`)
	at := now.Add(-10 * time.Minute)
	if err := sendMetronome(t, e, "m1", "alerts.low_remaining_commit_balance_reached", `,"alert_id":"a-1"`, at); err != nil {
		t.Fatal(err)
	}
	const name = "billing-unlock/acme"
	if _, ok := source(sources, name); ok {
		t.Fatal("a tenant that was paused, with no payment yet, shows as a payment that did not unlock")
	}
	topUp := func() error {
		return sendMetronome(t, e, "m2", "payment_gate.payment_status", `,"payment_status":"paid","invoice_id":"inv-2"`, at.Add(2*time.Minute))
	}
	if err := topUp(); !errors.Is(err, ErrNotSettled) {
		t.Fatalf("a top-up while the notification is in alarm: %v, want ErrNotSettled", err)
	}
	if got := webhooks(e, "metronome", WebhookUnsettled); got != 1 {
		t.Fatalf("%s{source=metronome,result=unsettled} = %v, want 1", webhooksMetric, got)
	}
	src, ok := source(sources, name)
	if !ok || src.Status != health.Failing || src.Kind != "billing-unlock" || src.Subject != "acme" ||
		!strings.Contains(src.Message, "Prepaid balance low") || !strings.Contains(src.Message, "paused") || !strings.Contains(src.Message, "payment_gate.payment_status") ||
		!strings.Contains(src.Hint, `PUT /api/v1/billing-states/acme {"state":"active","hold":"billing"}`) {
		t.Fatalf("source = %+v (found %t), want it failing for tenant acme, naming the payment, the hold and the notification", src, ok)
	}
	if len(sources.Problems()) != 1 {
		t.Fatalf("problems = %+v, want the payment that did not unlock", sources.Problems())
	}

	// A restart does not hide it: the new process reports it again.
	restarted := New(nil, Options{Mode: "observe", State: state, Rules: rules, Now: func() time.Time { return now }})
	again := quietHealth()
	restarted.UseHealth(again)
	restarted.Resync(context.Background())
	if src, ok := source(again, name); !ok || src.Status != health.Failing || !strings.Contains(src.Message, "Prepaid balance low") {
		t.Fatalf("after a restart the source is %+v (found %t), want it failing still", src, ok)
	}

	// Metronome evaluated the notification again: the payment it delivers
	// again lifts the hold, and the source is ok.
	acct.still()
	if err := topUp(); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("state %s, want active", st.State)
	}
	if src, _ := source(sources, name); src.Status != health.OK {
		t.Fatalf("source = %+v, want it ok once the hold is lifted", src)
	}
	if _, kept := state.Get(unlockPrefix + "acme"); kept {
		t.Fatal("the payment is still on record after the hold was lifted")
	}

	// A payment more than an hour old whose notification is still in alarm
	// is applied and lifts nothing: refused, and shown the same way, until
	// an operator lifts the hold.
	acct.still("wallet notification in alarm")
	old := now.Add(-3 * time.Hour)
	if err := sendMetronome(t, e, "m3", "alerts.low_remaining_commit_balance_reached", `,"alert_id":"a-1"`, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Paused {
		t.Fatalf("test setup: state %s", st.State)
	}
	// It is older than the alert: stale, ignored, and no payment to show.
	if err := sendMetronome(t, e, "m4", "payment_gate.payment_status", `,"payment_status":"paid","invoice_id":"inv-old"`, old); !errors.Is(err, ErrIgnored) {
		t.Fatalf("a payment older than the alert: %v, want it ignored", err)
	}
	if src, _ := source(sources, name); src.Status != health.OK {
		t.Fatalf("an ignored payment shows as one that did not unlock: %+v", src)
	}
	later := now.Add(2 * time.Hour)
	e.opts.Now = func() time.Time { return later }
	date := later.Format(http.TimeFormat)
	body := []byte(fmt.Sprintf(`{"id":"m5","type":"payment_gate.payment_status","properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225","timestamp":%q,"payment_status":"paid"}}`,
		now.Add(30*time.Minute).Format(time.RFC3339)))
	if _, err := e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret")); err != nil {
		t.Fatalf("a payment more than an hour old: %v, want it applied", err)
	}
	if got := webhooks(e, "metronome", WebhookRefused); got != 1 {
		t.Fatalf("%s{source=metronome,result=refused} = %v, want 1", webhooksMetric, got)
	}
	if src, _ := source(sources, name); src.Status != health.Failing || !strings.Contains(src.Message, "wallet notification in alarm") {
		t.Fatalf("source = %+v, want it failing again", src)
	}
	if err := e.SetHold(context.Background(), "acme", HoldBilling, Active, "wallet topped up by wire", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if src, _ := source(sources, name); src.Status != health.OK {
		t.Fatalf("source = %+v, want it ok once an operator lifted the hold", src)
	}
}

func repairs(e *Enforcer, kind string) float64 {
	return e.opts.Telemetry.Get(repairsMetric, map[string]string{"kind": kind})
}

// A tenant's admins are cluster admins inside its cluster: they can delete
// the suspension's binding, turn it into one that only audits, or delete
// the policy. vBilling puts each back at its next pass; in the live
// validation of v0.3.2 it did so without a word. Each repair is logged, with
// the cluster and the tenant, and counted in
// vbilling_enforcement_repairs_total. What is not a repair is not counted:
// the first install, a new reason in the message, and the defaults the API
// server fills in.
func TestSuspensionTakenApartInsideAClusterIsRestoredLoggedAndCounted(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset()
	e := New(host, Options{Mode: "enforce",
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	logs := captureLog(t)
	kinds := []string{RepairPolicyDeleted, RepairPolicyChanged, RepairBindingDeleted, RepairBindingChanged}
	want := map[string]float64{}
	check := func(when string) {
		t.Helper()
		for _, k := range kinds {
			if !e.opts.Telemetry.Has(repairsMetric, map[string]string{"kind": k}) {
				t.Fatalf("%s: no series for kind %s", when, k)
			}
			if got := repairs(e, k); got != want[k] {
				t.Fatalf("%s: %s{kind=%s} = %v, want %v\n%s", when, repairsMetric, k, got, want[k], logs)
			}
		}
	}
	check("before any suspension")
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	check("after the suspension was installed and kept")
	api := inside.AdmissionregistrationV1()

	if err := api.ValidatingAdmissionPolicyBindings().Delete(ctx, PolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	want[RepairBindingDeleted]++
	check("after the binding was deleted")
	if n := logs.count(cl.ExternalID(), "tenant acme", "the binding of the admission policy "+PolicyName+" was deleted", "restored"); n != 1 {
		t.Fatalf("the repair of the deleted binding is logged %d time(s), want once:\n%s", n, logs)
	}

	b, err := api.ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
	if _, err := api.ValidatingAdmissionPolicyBindings().Update(ctx, b, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	want[RepairBindingChanged]++
	check("after the binding was weakened to Audit")
	if n := logs.count(cl.ExternalID(), "tenant acme", "was changed (it does not deny)"); n != 1 {
		t.Fatalf("the repair of the weakened binding is logged %d time(s), want once:\n%s", n, logs)
	}

	if err := api.ValidatingAdmissionPolicies().Delete(ctx, PolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	want[RepairPolicyDeleted]++
	check("after the policy was deleted")
	if n := logs.count(cl.ExternalID(), "tenant acme: the admission policy "+PolicyName+" was deleted"); n != 1 {
		t.Fatalf("the repair of the deleted policy is logged %d time(s), want once:\n%s", n, logs)
	}

	p, err := api.ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Spec.Validations[0].Expression = "true"
	if _, err := api.ValidatingAdmissionPolicies().Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	want[RepairPolicyChanged]++
	check("after the policy was made to let every pod through")

	// The API server's defaults, and a new reason, are not repairs.
	p, _ = api.ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	equivalent, all := admissionv1.Equivalent, admissionv1.AllScopes
	p.Spec.MatchConstraints.MatchPolicy, p.Spec.MatchConstraints.ObjectSelector = &equivalent, &metav1.LabelSelector{}
	p.Spec.MatchConstraints.ResourceRules[0].Scope = &all
	if _, err := api.ValidatingAdmissionPolicies().Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _ = api.ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{})
	b.Spec.MatchResources = &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{}, ObjectSelector: &metav1.LabelSelector{}, MatchPolicy: &equivalent}
	if _, err := api.ValidatingAdmissionPolicyBindings().Update(ctx, b, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	if err := e.Set(ctx, "acme", Suspended, "still unpaid, second notice", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	check("after a pass over the API server's defaults and a new reason")

	// Lifted and suspended again: a first install again, not a repair.
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Suspended, "unpaid again", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	check("after the suspension was lifted and set again")
}

// A binding the tenant created ahead of the suspension, one that only
// audits, disarms the policy from the first pass: overwriting it counts.
func TestBindingPlantedBeforeTheSuspensionCountsAsARepair(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: PolicyName},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: PolicyName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Audit}}})
	e := New(host, Options{Mode: "enforce",
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if got := repairs(e, RepairBindingChanged); got != 1 {
		t.Fatalf("%s{kind=binding_changed} = %v, want 1", repairsMetric, got)
	}
}

func stateSeries(tel *telemetry.Registry, tenant string, state State) (float64, bool) {
	labels := map[string]string{"tenant": tenant, "state": string(state)}
	return tel.Get(stateMetric, labels), tel.Has(stateMetric, labels)
}

// vbilling_tenant_billing_state has a series for every tenant vBilling
// knows, from the start: a tenant with a hold before any change (after a
// restart it had none until its state first changed), and every tenant
// discovery knows, active when it has no hold. It follows tenants that
// appear and tenants that are offboarded.
func TestBillingStateMetricHasASeriesForEveryTenant(t *testing.T) {
	ctx := context.Background()
	state := kvWith(t, "held", Suspended)
	// A tenant that paid long ago and was offboarded since: its state is
	// still recorded, as active.
	if err := state.Set("state/long-gone", `{"tenant":"long-gone","state":"active","reason":"invoice paid","source":"stripe"}`); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.New()
	known := []string{"acme", "globex"}
	e := New(nil, Options{Mode: "observe", State: state, Telemetry: tel})
	one := func(when, tenant string, want State) {
		t.Helper()
		for _, s := range allStates {
			v, ok := stateSeries(tel, tenant, s)
			if !ok || v != map[bool]float64{true: 1, false: 0}[s == want] {
				t.Fatalf("%s: %s{tenant=%s,state=%s} = %v (series %t), want %s at 1 and the other states at 0", when, stateMetric, tenant, s, v, ok, want)
			}
		}
	}
	none := func(when, tenant string) {
		t.Helper()
		for _, s := range allStates {
			if v, ok := stateSeries(tel, tenant, s); ok {
				t.Fatalf("%s: %s{tenant=%s,state=%s} = %v, want no series", when, stateMetric, tenant, s, v)
			}
		}
	}
	one("right after the start", "held", Suspended)
	e.UseTenants(func() []string { return known })
	e.Guard(ctx) // every 15 seconds, in every mode
	one("after the first pass", "held", Suspended)
	one("after the first pass", "acme", Active)
	one("after the first pass", "globex", Active)
	none("after the first pass", "long-gone")

	if err := e.Set(ctx, "acme", Warning, "budget alert", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	one("after acme's state changed", "acme", Warning)

	known = []string{"acme", "initech"} // globex was offboarded, initech appeared
	e.Guard(ctx)
	one("after initech appeared", "initech", Active)
	none("after globex was offboarded", "globex")
	one("a tenant with a hold keeps its series, known to discovery or not", "held", Suspended)

	// Its hold lifted, a tenant discovery does not know has no state left
	// to show.
	if err := e.Set(ctx, "held", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	none("after the last hold of a tenant without clusters was lifted", "held")
	one("acme is unchanged", "acme", Warning)
}

// Without a list of tenants (nothing wired it), every tenant with a
// recorded state keeps its series, as before.
func TestBillingStateMetricWithoutAListOfTenants(t *testing.T) {
	tel := telemetry.New()
	e := New(nil, Options{Mode: "observe", State: kv.Memory(), Telemetry: tel})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Delinquent, "payment failed", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Guard(ctx)
	if v, ok := stateSeries(tel, "acme", Active); !ok || v != 1 {
		t.Fatalf("%s{tenant=acme,state=active} = %v (series %t), want 1", stateMetric, v, ok)
	}
}

var _ = destinations.CustomerResolver(resolver{})
