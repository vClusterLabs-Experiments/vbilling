package enforcement

// Second independent review (v0.3.0). Every TestReview2* test here fails on
// v0.3.0 and describes the expected behavior.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// review2RunsUnbilled: a workload on the cluster's own node still running
// with the control plane stopped and a pause span open runs unbilled
// (nodes, pods and the control plane stop billing at the pause by default,
// PAUSE_KEEP_BILLING=storage,load_balancers).
func review2RunsUnbilled(t *testing.T, host, inside kubernetes.Interface, spans *pause.Store, cl discovery.TenantCluster, ns, name string) bool {
	t.Helper()
	_, err := inside.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	running := err == nil
	_, open := spans.Current(cl.ExternalID())
	stopped := !cl.External && *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas == 0
	if cl.External {
		stopped = true // its API stays up, but the span alone stops billing
	}
	return running && open && stopped
}

// Prior finding 2 was fixed for a failing drain only. When the connection
// to the cluster's own API fails (a transient error reading the
// vbilling-kubeconfig Secret, the Platform proxy refusing an instance it
// reports as sleeping), pauseCluster logs "pause without draining" and stops
// the control plane anyway. The workloads on the cluster's own nodes keep
// running, the pause span stops their billing, and every later pass returns
// early because replicas == 0, so they are never drained.
func TestReview2TenantAPIErrorMustNotSkipTheDrain(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("inference", "default", "private-gpu"))
	var mu sync.Mutex
	connErr := true
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) {
			mu.Lock()
			defer mu.Unlock()
			if connErr {
				return nil, errors.New("read secret tenant/vbilling-kubeconfig: etcdserver: request timed out")
			}
			return inside, nil
		},
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	mu.Lock()
	connErr = false // the API answers again
	mu.Unlock()
	e.Guard(ctx)
	e.Wait()
	e.Resync(ctx)
	e.Wait()
	if review2RunsUnbilled(t, host, inside, spans, cl, "default", "inference") {
		sp, _ := spans.Current(cl.ExternalID())
		t.Fatalf("private-node workload runs with the control plane stopped and pause span %+v open (unbilled), even after the cluster's API answers again; status=%+v", sp, e.Pauses())
	}
}

// The same path in the default configuration: ENFORCEMENT_IN_CLUSTER=false
// (TenantClient nil) with TENANT_API=true, which private-node billing needs.
// The pause skips the drain without a word, Freeze saves the inventory with
// the private GPU node (so vBilling knows the cluster has its own nodes),
// the control plane stops, and the node stops billing at the pause
// (docs: "Dedicated and private nodes: Stop at the second of the pause")
// while every workload on it keeps running.
func TestReview2PauseWithoutInClusterAccessMustNotStopOwnNodeBilling(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("inference", "default", "private-gpu"))
	spans := pause.Memory()
	frozenNodes := 0
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		// metrics.Collector.Freeze through TENANT_API: it sees the private node.
		Freeze: func(ctx context.Context, _ discovery.TenantCluster) error {
			nodes, err := inside.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			if err == nil {
				frozenNodes = len(nodes.Items)
			}
			return err
		},
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if frozenNodes > 0 && review2RunsUnbilled(t, host, inside, spans, cl, "default", "inference") {
		t.Fatalf("paused without in-cluster access: inventory with %d own node(s) saved, control plane stopped, pause span open, workload still running on the private node; status=%+v", frozenNodes, e.Pauses())
	}
}

// pauseExternal opens the pause span before it installs the policy and
// drains. When either fails (a cluster admin inside a Standalone cluster
// can deny pod deletion with an admission webhook), the error is returned
// but the span stays open: the cluster keeps running its workloads and its
// nodes and pods are not billed. The internal path lifts its mark on a
// failed drain (PR #9); the external path does not.
func TestReview2ExternalDrainFailureMustNotLeaveAnUnbilledSpan(t *testing.T) {
	ctx := context.Background()
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}}, podOn("infer", "ml", "edge-1"))
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "infer", errors.New("admission webhook \"keep.tenant.example\" denied the request"))
	})
	cl := discovery.TenantCluster{Name: "edge", External: true}
	spans := pause.Memory()
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: time.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	e.Resync(ctx)
	e.Wait()
	if review2RunsUnbilled(t, nil, inside, spans, cl, "ml", "infer") {
		t.Fatalf("external cluster's workload still runs (drain denied, status=%+v) while pause span %+v stops its billing", e.Pauses(), spans.Spans(cl.ExternalID()))
	}
}

// docs/private-nodes.html: "A workload whose pods tolerate every taint is
// stopped, but its controller can schedule it again before the control
// plane stops; ... Its usage is billed while it runs." The second half is
// not true: the drain accepts running == tolerating, the control plane is
// stopped, the recreated pod keeps running on the private node, and the
// pause span stops billing its node (and its pods) at the pause. One line
// in the tenant's pod spec (tolerations: [{operator: Exists}]) keeps a GPU
// workload running for free.
func TestReview2TolerantWorkloadRunsUnbilledAfterPause(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	everything := corev1.Toleration{Operator: corev1.TolerationOpExists}
	first := podOn("vllm-0", "serving", "private-gpu", everything)
	first.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "vllm-7d9"}}
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, first)
	n := 0
	// The ReplicaSet controller (still running: the control plane is up
	// during the drain) replaces each deleted pod on the same node.
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		n++
		next := podOn(fmt.Sprintf("vllm-%d", n), "serving", "private-gpu", everything)
		next.OwnerReferences = first.OwnerReferences
		if err := inside.Tracker().Add(next); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 50 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	running := podNames(t, inside, "serving")
	_, open := spans.Current(cl.ExternalID())
	if *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas == 0 && open && len(running) > 0 {
		t.Fatalf("pods %v tolerate the pause taint and keep running on the private node after the control plane stopped; "+
			"the open pause span stops billing their node and pods, contrary to the documented \"billed while it runs\"", running)
	}
}

// PR #9 narrowed the DaemonSet exemption to PAUSE_KEEP_NAMESPACES and to
// pods without extended-resource requests. Both are chosen by the tenant:
// a tenant is cluster admin inside its own cluster, so it can run a
// DaemonSet in kube-system, and a privileged container sees every GPU of
// the node through NVIDIA_VISIBLE_DEVICES=all without requesting
// nvidia.com/gpu. Such a pod is treated as node infrastructure, keeps
// running while paused, and its node is not billed.
func TestReview2KeepNamespaceDaemonSetRunsUnbilledAfterPause(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	privileged := true
	miner := podOn("batch-infer-x7", "kube-system", "private-gpu")
	miner.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "batch-infer"}}
	miner.Spec.Containers = []corev1.Container{{Name: "infer", Image: "tenant/vllm",
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		Env:             []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"}}}}
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, miner)
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if review2RunsUnbilled(t, host, inside, spans, cl, "kube-system", "batch-infer-x7") {
		t.Fatal("a tenant's DaemonSet in kube-system using every GPU (privileged, NVIDIA_VISIBLE_DEVICES=all) is exempted as node infrastructure: it keeps running after the control plane stopped, and the pause span stops billing its node")
	}
}

// review2FlakyGateway fails its first call, like a LiteLLM restart.
type review2FlakyGateway struct {
	mu    sync.Mutex
	calls int
}

func (g *review2FlakyGateway) Apply(context.Context, string, bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.calls == 1 {
		return errors.New("litellm: dial tcp 10.0.0.9:4000: connect: connection refused")
	}
	return nil
}

// A webhook whose state cannot be fully applied returns 500 so the provider
// retries it (docs: "the API call or webhook that changed the state also
// fails ... so billing platforms retry it"). The state is stored anyway and
// the event is not remembered as applied. A payment that arrives before the
// retry is then undone by the retry: events are applied in arrival order,
// their own creation time is ignored. With a rule that maps the event to
// paused, the paying tenant's clusters are drained and stopped.
func TestReview2RetriedStaleWebhookMustNotUndoAPayment(t *testing.T) {
	ctx := context.Background()
	at := now
	// Stripe, read again when the payment arrives, shows nothing owed.
	e := New(nil, Options{Mode: "enforce", StripeWebhookSecret: "whsec_test", Gateway: &review2FlakyGateway{},
		Resolvers: map[string]destinations.CustomerResolver{"stripe": &account{resolver: resolver{"cus_acme": "acme"}}},
		Now:       func() time.Time { return at }})
	send := func(id, typ, status string, created time.Time) error {
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":{"customer":"cus_acme","status":%q}}}`, id, typ, created.Unix(), status))
		_, err := e.HandleStripe(ctx, body, stripe.SignatureHeader(body, "whsec_test", at))
		return err
	}
	unpaidAt := now
	// 1. The subscription goes unpaid; applying it fails (gateway down): 500.
	if err := send("evt_unpaid", "customer.subscription.updated", "unpaid", unpaidAt); err == nil {
		t.Fatal("expected the first delivery to fail")
	}
	// 2. Twenty minutes later the customer pays.
	at = now.Add(20 * time.Minute)
	if err := send("evt_paid", "invoice.paid", "paid", at); err != nil {
		t.Fatal(err)
	}
	// 3. An hour later Stripe retries the older event.
	at = now.Add(80 * time.Minute)
	err := send("evt_unpaid", "customer.subscription.updated", "unpaid", unpaidAt)
	if st := e.StateOf("acme"); st.State != Active {
		t.Fatalf("a retried event created %s, before the payment at %s, set the paying tenant to %s (%s); retry result: %v",
			unpaidAt.Format(time.RFC3339), now.Add(20*time.Minute).Format(time.RFC3339), st.State, st.Reason, err)
	}
}

// A billing signal replaces the billing hold whatever its severity, so a
// less severe signal lifts a suspension or a pause without any payment.
// Stripe sends invoice.payment_failed for every failed attempt, including
// one the customer makes on the hosted invoice page with a declined card:
// a suspended (or, with a rule, paused) tenant becomes delinquent, which
// blocks nothing, and its clusters and gateway are released. The same
// happens when any warning-level alert (billing.alert.triggered, any
// Metronome alerts.* without a rule) follows the event that blocked it.
func TestReview2LesserBillingSignalMustNotLiftABlock(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	if _, err := stripeEvent(t, e, "evt_unpaid", "customer.subscription.updated", "unpaid"); err != nil {
		t.Fatal(err)
	}
	if l := nsLabels(t, kube); l[LabelSuspended] != "true" {
		t.Fatalf("test setup: unpaid did not suspend: %v", l)
	}
	// The tenant tries a declined card on the hosted invoice page.
	if _, err := stripeEvent(t, e, "evt_declined", "invoice.payment_failed", "open"); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); !st.State.blocks() || nsLabels(t, kube)[LabelSuspended] != "true" {
		t.Fatalf("a failed payment attempt lifted the suspension of an unpaid subscription: state %s, namespace labels %v", st.State, nsLabels(t, kube))
	}
}

// For clusters with their own nodes the in-cluster policy is the only
// suspension ("the only enforcement that reaches clusters with their own
// nodes"). policyInside updates the policy but only creates the binding and
// ignores AlreadyExists. A tenant, cluster admin inside its own cluster,
// pre-creates a binding named vbilling-suspended with validationActions
// [Audit]: every later suspension installs the policy, finds the binding,
// reports success, and denies nothing. (The tenant can also label any
// namespace vbilling.vcluster.com/enforcement-exempt.)
func TestReview2PreCreatedBindingMustNotDisarmTheInClusterPolicy(t *testing.T) {
	host := fake.NewSimpleClientset(
		controlPlane("private-a", "vc-private-a", 1, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-private-a"}},
	)
	inside := fake.NewSimpleClientset(&admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName},
		Spec:       admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: PolicyName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Audit}},
	})
	cl := discovery.TenantCluster{Name: "private-a", Namespace: "vc-private-a", Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", Now: time.Now,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "subscription canceled", "stripe", "customer.subscription.deleted"); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	b, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deny := false
	for _, a := range b.Spec.ValidationActions {
		deny = deny || a == admissionv1.Deny
	}
	if !deny {
		t.Fatalf("suspension reported success but the binding only %v: new pods are not denied", b.Spec.ValidationActions)
	}
}

// Prepaid wallet (the docs' example rule): the low-balance alert pauses;
// another alert without a rule arrives afterwards and resumes the tenant.
func TestReview2LaterAlertMustNotResumeAnEmptyWallet(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_contract_credit_balance_reached=paused")
	m, _, _ := setup(t, "observe", rules)
	send := func(id, typ string) {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"}}`, id, typ))
		date := now.Format(http.TimeFormat)
		if _, err := m.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret")); err != nil {
			t.Fatal(err)
		}
	}
	send("a1", "alerts.low_remaining_contract_credit_balance_reached")
	send("a2", "alerts.spend_threshold_reached")
	if st := m.StateOf("acme"); st.State != Paused {
		t.Fatalf("a later warning-level alert resumed a tenant paused for an empty wallet: state %s (%s)", st.State, st.Reason)
	}
}

// A payment stamped in whole seconds (the signed Date header) right after
// an alert stamped with milliseconds in the same second is newer, not
// stale: it must resume the tenant.
func TestSameSecondEventsApplyInArrivalOrder(t *testing.T) {
	rules, _ := ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	m, _, _ := setup(t, "observe", rules)
	readable(m, "metronome") // the top-up brought the balance back: the alert is ok again
	date := now.Truncate(time.Second).Format(http.TimeFormat)
	send := func(body string) {
		t.Helper()
		if _, err := m.HandleMetronome(context.Background(), []byte(body), date, metronome.Sign([]byte(body), date, "mtr_secret")); err != nil {
			t.Fatal(err)
		}
	}
	alertAt := now.Truncate(time.Second).Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	send(fmt.Sprintf(`{"id":"b1","type":"alerts.low_remaining_commit_balance_reached","timestamp":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"}}`, alertAt))
	if st := m.StateOf("acme"); st.State != Paused {
		t.Fatalf("the commit alert did not pause: %s (%s)", st.State, st.Reason)
	}
	send(`{"id":"b2","type":"payment_gate.payment_status","properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225","payment_status":"paid"}}`)
	if st := m.StateOf("acme"); st.State != Active {
		t.Fatalf("a payment in the same second as the alert must resume: %s (%s)", st.State, st.Reason)
	}
}
