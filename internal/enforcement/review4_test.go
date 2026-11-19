package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// Fourth review: the fixes made after the third review.

// r4StripeAccount serves the part of Stripe's API the arrears read uses
// (customers, subscriptions with status=all, invoices by ID and as a list
// filtered by subscription, customer and status), from what the account
// holds.
type r4StripeAccount struct {
	mu       sync.Mutex
	subs     []map[string]any
	invoices map[string]map[string]any
}

func (s *r4StripeAccount) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()
	missing := func() {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": "resource_missing",
			"message": "no such object: " + r.Method + " " + r.URL.Path}})
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/customers/cus_acme":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_acme", "metadata": map[string]string{"vbilling_tenant_id": "acme"}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": s.subs, "has_more": false})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/invoices":
		out := []map[string]any{}
		for _, inv := range s.invoices {
			if v := q.Get("subscription"); v != "" && inv["subscription"] != v {
				continue
			}
			if v := q.Get("customer"); v != "" && inv["customer"] != v {
				continue
			}
			if v := q.Get("status"); v != "" && inv["status"] != v {
				continue
			}
			out = append(out, inv)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out, "has_more": false})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/invoices/"):
		inv, ok := s.invoices[strings.TrimPrefix(r.URL.Path, "/v1/invoices/")]
		if !ok {
			missing()
			return
		}
		_ = json.NewEncoder(w).Encode(inv)
	default:
		missing()
	}
}

// With Stripe's default subscription status resolution ("most recent
// invoice"), a past_due or unpaid subscription becomes active once its most
// recent invoice is paid, whatever older invoices are still open: "active
// doesn't necessarily indicate that all outstanding invoices associated with
// the subscription have been paid" (docs.stripe.com/billing/subscriptions/
// overview, statuses and "Configure subscription status resolution").
//
// The October invoice of a GPU tenant (10,000.00) fails and suspends it
// (invoice.payment_failed=suspended, the documented rule). A month later
// Stripe charges November's invoice, small because the tenant was held, and
// it succeeds: the subscription is active again while October's invoice is
// still open. vBilling lifts the hold, by the subscription's own event
// (settled by itself, never read) and by invoice.paid (read again: the
// subscription is active, and nothing reads its open invoices).
func TestReview4PayingTheLatestInvoiceLiftsAHoldWhileTheFailedOneIsOpen(t *testing.T) {
	for _, path := range []string{"invoice.paid", "customer.subscription.updated"} {
		t.Run(path, func(t *testing.T) {
			acct := &r4StripeAccount{
				subs: []map[string]any{{"id": "sub_1", "object": "subscription", "status": "past_due", "customer": "cus_acme",
					"metadata": map[string]string{"vbilling_tenant_id": "acme"}, "latest_invoice": "in_oct"}},
				invoices: map[string]map[string]any{
					"in_oct": {"id": "in_oct", "object": "invoice", "customer": "cus_acme", "subscription": "sub_1", "status": "open", "amount_remaining": 1000000},
				},
			}
			srv := httptest.NewServer(http.HandlerFunc(acct.serve))
			defer srv.Close()
			rules, _ := ParseRules("invoice.payment_failed=suspended")
			e, kube, _ := setup(t, "enforce", rules)
			e.opts.Resolvers["stripe"] = stripe.New(stripe.NewClient(srv.URL, "sk_test_123", "2026-09-30.endive", 0),
				&config.Config{BillingCurrency: "usd", DefaultPlanCode: "standard"}, kv.Memory())

			oct := now.Add(-30 * 24 * time.Hour)
			if err := sendStripe(t, e, stripeBody("e1", "invoice.payment_failed", oct, `"object":"invoice","id":"in_oct","subscription":"sub_1"`)); err != nil {
				t.Fatal(err)
			}
			if st := e.StateOf("acme"); st.State != Suspended {
				t.Fatalf("test setup: state %s after October's invoice failed, want suspended", st.State)
			}

			// November's invoice is charged and paid; October's stays open.
			acct.mu.Lock()
			acct.invoices["in_nov"] = map[string]any{"id": "in_nov", "object": "invoice", "customer": "cus_acme", "subscription": "sub_1", "status": "paid"}
			acct.subs[0]["status"], acct.subs[0]["latest_invoice"] = "active", "in_nov"
			acct.mu.Unlock()
			nov := now.Add(-time.Minute)
			body := stripeBody("e2", "invoice.paid", nov, `"object":"invoice","id":"in_nov","subscription":"sub_1"`)
			if path == "customer.subscription.updated" {
				body = stripeBody("e2", "customer.subscription.updated", nov, `"object":"subscription","id":"sub_1","status":"active"`)
			}
			if err := sendStripe(t, e, body); err != nil {
				t.Fatal(err)
			}
			if st := e.StateOf("acme"); st.State != Suspended || nsLabels(t, kube)[LabelSuspended] != "true" {
				t.Fatalf("%s for November lifted the suspension (state %s, %q) while October's invoice in_oct (10,000.00) of the same "+
					"subscription is still open: Stripe marks a subscription active once its most recent invoice is paid", path, st.State, st.Reason)
			}
		})
	}
}

// A billing hold without a recorded cause (every hold v0.3.0 persisted, and
// any PUT) is lowered by a payment "once Stripe shows nothing of the
// tenant's owed" (docs/private-nodes.html, the upgrade notes). The read
// behind it lists only the subscriptions that carry vBilling's tenant
// metadata and are past_due, unpaid or incomplete: no invoice is read, and
// no subscription the AI Cloud's own portal created. After the upgrade, a
// tenant that v0.3.0 suspended pays any small invoice and the suspension
// lifts:
//   - its subscription was canceled for non-payment
//     (customer.subscription.deleted; a canceled subscription stops
//     collecting, so its last invoice stays open);
//   - its metered subscription, created outside vBilling, is unpaid (the
//     third review's finding 3, which v0.3.0 holds still have).
func TestReview4HoldFromV030IsLiftedByAnyPaymentWhileItsInvoiceIsOpen(t *testing.T) {
	for _, tc := range []struct {
		name, eventType, reason string
		sub                     map[string]any
	}{
		{"subscription canceled with its last invoice open", "customer.subscription.deleted", "subscription deleted",
			map[string]any{"id": "sub_1", "object": "subscription", "status": "canceled", "customer": "cus_acme",
				"metadata": map[string]string{"vbilling_tenant_id": "acme"}, "latest_invoice": "in_oct"}},
		{"unpaid subscription created outside vBilling", "customer.subscription.updated", "subscription unpaid",
			map[string]any{"id": "sub_1", "object": "subscription", "status": "unpaid", "customer": "cus_acme",
				"metadata": map[string]string{}, "latest_invoice": "in_oct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acct := &r4StripeAccount{
				subs: []map[string]any{tc.sub},
				invoices: map[string]map[string]any{
					"in_oct":   {"id": "in_oct", "object": "invoice", "customer": "cus_acme", "subscription": "sub_1", "status": "open", "amount_remaining": 1000000},
					"in_small": {"id": "in_small", "object": "invoice", "customer": "cus_acme", "status": "paid"},
				},
			}
			srv := httptest.NewServer(http.HandlerFunc(acct.serve))
			defer srv.Close()

			// The state v0.3.0 wrote: a billing hold, no causes.
			since := now.Add(-20 * 24 * time.Hour)
			v030, _ := json.Marshal(TenantState{Tenant: "acme", State: Suspended, Reason: tc.reason, Source: "stripe",
				EventType: tc.eventType, Since: since, Hold: HoldBilling,
				Holds:      map[string]Hold{HoldBilling: {State: Suspended, Reason: tc.reason, Source: "stripe", EventType: tc.eventType, Since: since}},
				EventTimes: map[string]time.Time{HoldBilling: since}})
			state := kv.Memory()
			if err := state.Set("state/acme", string(v030)); err != nil {
				t.Fatal(err)
			}
			e, kube, _ := setup(t, "enforce", nil)
			opts := e.opts
			opts.State = state
			opts.Resolvers = map[string]destinations.CustomerResolver{"stripe": stripe.New(stripe.NewClient(srv.URL, "sk_test_123", "2026-09-30.endive", 0),
				&config.Config{BillingCurrency: "usd", DefaultPlanCode: "standard"}, kv.Memory())}
			e = New(kube, opts)
			if st := e.StateOf("acme"); st.State != Suspended {
				t.Fatalf("test setup: v0.3.0 state not loaded: %+v", st)
			}

			if err := sendStripe(t, e, stripeBody("e1", "invoice.paid", now.Add(-time.Minute), `"object":"invoice","id":"in_small","subscription":null`)); err != nil {
				t.Fatal(err)
			}
			if st := e.StateOf("acme"); st.State != Suspended {
				t.Fatalf("a small one-off invoice paid after the upgrade lifted the suspension v0.3.0 set (%s) while sub_1's invoice in_oct "+
					"(10,000.00) is still open: the read for a hold without causes checks the statuses of vBilling's own subscriptions only "+
					"(state %s, %q)", tc.reason, st.State, st.Reason)
			}
		})
	}
}

// docs/stripe.html gives billing.alert.triggered=suspended as the way to
// turn a Stripe usage alert into a hard cap, and says of a suspension "A
// paid invoice lifts it". A cause "alert:<id>" made blocking by a rule is
// never settled by a Stripe payment: settlesItself settles warnings only,
// and Stripe's Owed answers every alert as still owed. The tenant pays its
// invoice and stays suspended until an operator PUTs it active; every
// later payment only logs "still held".
func TestReview4StripeHardCapIsNeverLiftedByAPayment(t *testing.T) {
	// Stripe shows nothing owed: the subscription is active and its
	// invoices are paid.
	acct := &r4StripeAccount{
		subs: []map[string]any{{"id": "sub_1", "object": "subscription", "status": "active", "customer": "cus_acme",
			"metadata": map[string]string{"vbilling_tenant_id": "acme"}, "latest_invoice": "in_next"}},
		invoices: map[string]map[string]any{
			"in_period": {"id": "in_period", "object": "invoice", "customer": "cus_acme", "subscription": "sub_1", "status": "paid"},
			"in_next":   {"id": "in_next", "object": "invoice", "customer": "cus_acme", "subscription": "sub_1", "status": "paid"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(acct.serve))
	defer srv.Close()
	rules, _ := ParseRules("billing.alert.triggered=suspended")
	e, _, _ := setup(t, "observe", rules)
	e.opts.Resolvers["stripe"] = stripe.New(stripe.NewClient(srv.URL, "sk_test_123", "2026-09-30.endive", 0),
		&config.Config{BillingCurrency: "usd", DefaultPlanCode: "standard"}, kv.Memory())
	at := now.Add(-time.Hour)
	if err := sendStripe(t, e, stripeBody("e1", "billing.alert.triggered", at, `"object":"billing.alert_triggered","alert":{"id":"alrt_cap"}`)); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended {
		t.Fatalf("test setup: state %s after the cap alert, want suspended", st.State)
	}
	for i, id := range []string{"in_period", "in_next"} {
		if err := sendStripe(t, e, stripeBody(fmt.Sprintf("p%d", i), "invoice.paid", at.Add(time.Duration(i+1)*time.Minute), `"object":"invoice","id":"`+id+`","subscription":"sub_1"`)); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.StateOf("acme"); st.State == Suspended {
		t.Fatalf("two paid invoices, nothing owed at Stripe, and the hard-cap suspension stays (hold %+v): no payment can settle a Stripe "+
			"alert cause, contrary to \"A paid invoice lifts it\" (docs/stripe.html)", st.Holds[HoldBilling])
	}
}

// r4AskedReader is a billing platform read again that, like Stripe, answers
// with what it still shows open for the customer it is asked about.
// (Adapted to v0.3.1's stateless read: it is asked about a customer, not
// about recorded causes, and answers with all that customer still owes.)
type r4AskedReader struct {
	resolver
	mu    sync.Mutex
	open  map[string]bool
	asked [][]string
}

func (r *r4AskedReader) Owed(_ context.Context, tenant, customer string, _ func(string) bool) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, []string{tenant, customer})
	var out []string
	for c, open := range r.open {
		if open {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ENFORCEMENT_RULES maps any event type (docs/stripe.html, metronome.html:
// "Map any event type to another state"). An event type HandleStripe does
// not know (invoice.overdue, invoice.marked_uncollectible,
// charge.dispute.created, customer.subscription.paused, ...) gets no Object,
// so the hold it raises records no cause. The next payment, of anything,
// then lowers the hold to its recorded causes, or clears it:
//   - with no cause at all, Stripe is asked about nothing, and a one-off
//     invoice is never among a tenant's subscriptions in arrears;
//   - with a usage alert recorded earlier (a warning, settled by any
//     payment), the suspension is lifted without any read;
//   - with a failed payment recorded earlier and still owed, a payment that
//     settles nothing lowers the suspension to delinquent, which blocks
//     nothing.
func TestReview4SuspensionFromAnEventWithoutACauseIsLiftedByAnyPayment(t *testing.T) {
	for _, earlier := range []string{"none", "usage alert", "failed payment"} {
		t.Run("earlier cause "+earlier, func(t *testing.T) {
			rules, _ := ParseRules("invoice.overdue=suspended")
			e, kube, _ := setup(t, "enforce", rules)
			// What Stripe still shows open: the overdue invoice, and the
			// subscription whose payment failed (in that variant).
			stripeAcct := &r4AskedReader{resolver: e.opts.Resolvers["stripe"].(resolver),
				open: map[string]bool{"invoice:in_big": true, "subscription:sub_1": earlier == "failed payment"}}
			e.opts.Resolvers["stripe"] = stripeAcct
			at := now.Add(-time.Hour)
			switch earlier {
			case "usage alert": // a warning: any payment of the platform settles it, unread
				if err := sendStripe(t, e, stripeBody("e1", "billing.alert.triggered", at, `"object":"billing.alert_triggered","alert":{"id":"alrt_1"}`)); err != nil {
					t.Fatal(err)
				}
			case "failed payment": // delinquent, still owed: the payment below settles nothing
				if err := sendStripe(t, e, stripeBody("e1", "invoice.payment_failed", at, `"object":"invoice","id":"in_sub","subscription":"sub_1"`)); err != nil {
					t.Fatal(err)
				}
			}
			if err := sendStripe(t, e, stripeBody("e2", "invoice.overdue", at.Add(time.Minute), `"object":"invoice","id":"in_big","subscription":null`)); err != nil {
				t.Fatal(err)
			}
			if st := e.StateOf("acme"); st.State != Suspended {
				t.Fatalf("test setup: state %s after the overdue invoice, want suspended", st.State)
			}
			if err := sendStripe(t, e, stripeBody("e3", "invoice.paid", at.Add(2*time.Minute), `"object":"invoice","id":"in_small","subscription":null`)); err != nil {
				t.Fatal(err)
			}
			if st := e.StateOf("acme"); st.State != Suspended || nsLabels(t, kube)[LabelSuspended] != "true" {
				t.Fatalf("paying a small, unrelated invoice lifted the suspension for the overdue invoice in_big, which Stripe still shows open "+
					"(state %s; hold %+v; Stripe asked about %v)", st.State, e.StateOf("acme").Holds[HoldBilling], stripeAcct.asked)
			}
			if len(stripeAcct.asked) == 0 {
				t.Fatalf("the payment kept the suspension without reading Stripe again: the hold stays for the wrong reason")
			}
		})
	}
}

// r4StartMiner is the tenant, cluster admin inside its cluster, while the
// cluster's API still answers: it lifts the pause policy and starts an
// 8-GPU pod that tolerates every taint on its own node gpu-1.
func r4StartMiner(t *testing.T, ctx context.Context, inside *fake.Clientset) {
	t.Helper()
	api := inside.AdmissionregistrationV1()
	if err := api.ValidatingAdmissionPolicyBindings().Delete(ctx, PausedPolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("tenant: delete the pause policy binding: %v", err)
	}
	if err := api.ValidatingAdmissionPolicies().Delete(ctx, PausedPolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("tenant: delete the pause policy: %v", err)
	}
	miner := podOn("miner", "default", "gpu-1", corev1.Toleration{Operator: corev1.TolerationOpExists})
	miner.Spec.Containers = []corev1.Container{{Name: "m", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}
	if _, err := inside.CoreV1().Pods("default").Create(ctx, miner, metav1.CreateOptions{}); err != nil {
		t.Fatalf("tenant: start the pod: %v", err)
	}
}

// r4APIGone makes every call to the cluster's API fail the way a stopped
// control plane does: connection refused.
func r4APIGone(inside *fake.Clientset) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	inside.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, transportErr(refused) })
}

// A pause is verified only on evidence that nothing ran after the last
// check: confirmStopped watches the cluster's own nodes and the pause policy
// until the API goes away with the control plane. keepPaused, at the next
// pass, trusts what the last pass recorded about the cluster's own nodes
// (e.checked) whenever that pass did not record a failure, even if it never
// watched the control plane stop:
//   - the control plane takes longer than the drain timeout (2 minutes) to
//     stop: confirmStopped ends at its deadline with the API still answering,
//     no error, so the own nodes are recorded as checked;
//   - the pods the cluster synced could not be stopped once, after the
//     control plane was scaled down: the drain's result is recorded as
//     checked and confirmStopped never runs.
//
// Meanwhile the API still answers and nothing checks it: the tenant removes
// the pause policy and starts a pod on its own node. Once the control plane
// is down, the next pass (Resync, every 2 minutes) verifies the pause, and
// the node stops billing while the pod runs on.
func TestReview4PauseVerifiedOnACheckThatNeverSawTheAPIGo(t *testing.T) {
	ctx := context.Background()
	stsGVR := appsv1.SchemeGroupVersion.WithResource("statefulsets")

	t.Run("control plane outlives the drain timeout", func(t *testing.T) {
		cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
		cp := controlPlane(cl.Name, cl.Namespace, 1, nil)
		cp.Status.ReadyReplicas = 1 // running, and still ready while it stops
		host := fake.NewSimpleClientset(cp, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
		inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}}, podOn("inference", "default", "gpu-1"))
		spans := pause.Memory()
		e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
			Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
		if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		e.Wait()
		if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "not stopped yet") {
			t.Fatalf("test setup: status %+v, want unverified while the control plane still stops", st)
		}

		r4StartMiner(t, ctx, inside)
		obj, err := host.Tracker().Get(stsGVR, cl.Namespace, cl.Name)
		if err != nil {
			t.Fatal(err)
		}
		stopped := obj.(*appsv1.StatefulSet).DeepCopy()
		stopped.Status.ReadyReplicas = 0 // the control plane is down at last
		if err := host.Tracker().Update(stsGVR, stopped, cl.Namespace); err != nil {
			t.Fatal(err)
		}
		r4APIGone(inside)

		e.Resync(ctx)
		e.Wait()
		if _, open := spans.Current(cl.ExternalID()); open {
			t.Fatalf("pause verified (gpu-1 no longer billed) while default/miner with 8 GPUs runs on it: it started after the first pass's "+
				"last check, while the control plane was still up, and no check ever saw the API go away (status %+v)", e.Pauses())
		}
	})

	t.Run("synced pods could not be stopped once", func(t *testing.T) {
		cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
		var mu sync.Mutex
		stopping, failed := false, false
		host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
			if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
				mu.Lock()
				stopping = true
				mu.Unlock()
			}
			return false, nil, nil
		})
		host.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
			mu.Lock()
			defer mu.Unlock()
			if stopping && !failed {
				failed = true
				return true, nil, errors.New("etcdserver: request timed out")
			}
			return false, nil, nil
		})
		spans := pause.Memory()
		e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
			Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
		if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		e.Wait()
		if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "synced pods could not be stopped") {
			t.Fatalf("test setup: status %+v, want unverified because the synced pods could not be listed", st)
		}

		r4StartMiner(t, ctx, inside)
		r4APIGone(inside)

		e.Resync(ctx)
		e.Wait()
		if _, open := spans.Current(cl.ExternalID()); open {
			t.Fatalf("pause verified (gpu-1 no longer billed) while default/miner with 8 GPUs runs on it: the own nodes were never "+
				"checked while the control plane stopped (status %+v)", e.Pauses())
		}
	})
}

// The other direction: one check that fails with something other than
// the API going away (a 504 from the proxy, an i/o timeout, "http2:
// client connection lost", etcd losing quorum in an HA control plane as
// its replicas stop) ends confirmStopped at once, and the failure is
// recorded for good: keepPaused can never look again, since the API is
// gone by the next pass. A legitimate cluster's pause then stays
// unverified for as long as it lasts, and its nodes and workloads bill
// as if it ran.
func TestReview4OneTransientErrorLeavesACleanPauseUnverifiedForGood(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	var mu sync.Mutex
	stopping, blipped := false, false
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			mu.Lock()
			stopping = true
			mu.Unlock()
		}
		return false, nil, nil
	})
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	stopped := func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case !stopping:
			return false, nil, nil
		case !blipped:
			blipped = true
			return true, nil, apierrors.NewGenericServerResponse(http.StatusGatewayTimeout, "list", schema.GroupResource{Resource: "pods"}, "", "the vCluster did not answer in time", 0, true)
		}
		return true, nil, transportErr(refused) // gone with its control plane, for good
	}
	inside.PrependReactor("list", "*", stopped)
	inside.PrependReactor("get", "*", stopped)
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	for pass := 0; pass < 3; pass++ { // Resync, every 2 minutes
		e.Resync(ctx)
		e.Wait()
	}
	if _, open := spans.Current(cl.ExternalID()); !open {
		t.Fatalf("pause never verified although the drain emptied gpu-1 and the API then went away for good: one 504 while the control "+
			"plane stopped is recorded as a failed check that no later pass can redo, so the paused cluster bills as running (status %+v)", e.Pauses())
	}
}

// An operator's billing hold records no cause, and is meant to be lowered
// only once the billing platform, read again, confirms nothing is owed
// ("Metronome cannot confirm that without the notification, so an
// operator lifts such a hold", docs/private-nodes.html). But a lesser
// webhook that arrives afterwards is recorded as a cause of that hold, and
// settle then takes the causes for the whole hold: once they are settled,
// the hold goes, the operator's reason with it, without any read.
//
// The operator pauses a prepaid tenant whose wallet reached zero (PUT, the
// documented way). The tenant tries a top-up with a card that is declined
// (payment_gate failed: delinquent, a lesser signal, recorded as a cause),
// then tops up 1.00 with a card that works: the successful payment settles
// the failed one by itself, no cause is left, and the paused tenant resumes.
// With Stripe, a failed invoice of a subscription that is paid again does
// the same, while Stripe still shows another subscription in arrears.
func TestReview4OperatorPauseIsLiftedByAPaymentAfterALesserSignal(t *testing.T) {
	t.Run("metronome", func(t *testing.T) {
		e, _, _ := setup(t, "observe", nil)
		if err := e.Set(context.Background(), "acme", Paused, "prepaid balance reached zero", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		if err := sendMetronome(t, e, "m1", "payment_gate.payment_status", `,"payment_status":"failed","invoice_id":"inv-declined"`, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := sendMetronome(t, e, "m2", "payment_gate.payment_status", `,"payment_status":"paid","invoice_id":"inv-1-dollar"`, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if st := e.StateOf("acme"); st.State != Paused {
			t.Fatalf("the operator's pause was lifted by a 1.00 top-up after a declined one (state %s, %q): nothing read the wallet, "+
				"and the docs say only an operator lifts a Metronome hold without a recorded cause", st.State, st.Reason)
		}
	})
	t.Run("stripe", func(t *testing.T) {
		e, _, _ := setup(t, "observe", nil)
		acct := readable(e, "stripe", "subscription:sub_other") // still in arrears
		if err := e.Set(context.Background(), "acme", Paused, "unpaid since October", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		if err := sendStripe(t, e, stripeBody("e1", "invoice.payment_failed", now.Add(time.Second), `"object":"invoice","id":"in_1","subscription":"sub_1"`)); err != nil {
			t.Fatal(err)
		}
		if err := sendStripe(t, e, stripeBody("e2", "customer.subscription.updated", now.Add(2*time.Second), `"object":"subscription","id":"sub_1","status":"active"`)); err != nil {
			t.Fatal(err)
		}
		if st := e.StateOf("acme"); st.State != Paused {
			t.Fatalf("the operator's pause was lifted by sub_1 becoming active after a lesser failure of its own (state %s, %q; Stripe read "+
				"%d times, and it still shows sub_other in arrears)", st.State, st.Reason, acct.reads)
		}
	})
}
