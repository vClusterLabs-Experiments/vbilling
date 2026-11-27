package enforcement

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// What vBilling says: reasons, log lines and events that the live
// validation of v0.3.2 found wrong or misleading.

// A rule on an event type vBilling gives no reason of its own (any type
// that is not an alert or a payment) recorded the reason " (rule: warning)":
// a leading space, and nothing naming the event. The reason is what the
// API, the annotations and the denial inside a cluster show.
func TestReasonOfARuledEventNamesTheEventType(t *testing.T) {
	rules, err := ParseRules("vbtest.custom_event=warning,invoice.overdue=suspended,alerts.spend_threshold_reached=delinquent")
	if err != nil {
		t.Fatal(err)
	}
	e, _, _ := setup(t, "observe", rules)
	date := now.Format(http.TimeFormat)
	send := func(id, typ, props string) Signal {
		t.Helper()
		body := []byte(fmt.Sprintf(`{"id":%q,"type":%q,"properties":{"customer_id":"5f794d50-085a-4db6-8d15-286e518b7225"%s}}`, id, typ, props))
		sig, err := e.HandleMetronome(context.Background(), body, date, metronome.Sign(body, date, "mtr_secret"))
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	if sig := send("c1", "vbtest.custom_event", ""); sig.Reason != "vbtest.custom_event (rule: warning)" {
		t.Fatalf("reason of a ruled custom event = %q, want it to name the event type and the rule", sig.Reason)
	}
	if got := e.StateOf("acme").Reason; got != "vbtest.custom_event (rule: warning)" {
		t.Fatalf("the tenant's reason = %q", got)
	}
	// Events that carry a reason of their own keep it.
	if sig := send("c2", "alerts.spend_threshold_reached", `,"alert_name":"Spend above budget"`); sig.Reason != "Spend above budget (rule: delinquent)" {
		t.Fatalf("reason of a ruled alert = %q", sig.Reason)
	}
	if err := sendStripe(t, e, stripeBody("s1", "invoice.overdue", now, `"object":"invoice","id":"in_1"`)); err != nil {
		t.Fatal(err)
	}
	if got := e.StateOf("acme").Reason; got != "invoice.overdue (rule: suspended)" {
		t.Fatalf("reason of a ruled Stripe event = %q", got)
	}
}

// Clearing the operator's pause while a failed payment keeps the tenant
// delinquent logged "paused -> delinquent (threshold billing payment failed
// via api manual)": the remaining hold with the source of the call that
// lowered the state, as if the operator had set the delinquency. The line
// names the remaining hold with its own source, and the call that lifted
// the other one.
func TestLogOfALoweredStateNamesTheRemainingHoldsOwnSource(t *testing.T) {
	e, _, _ := setup(t, "observe", nil)
	ctx := context.Background()
	if err := sendMetronome(t, e, "m1", "payment_gate.payment_status", `,"payment_status":"failed"`, now); err != nil {
		t.Fatal(err)
	}
	if err := e.SetHold(ctx, "acme", HoldOperator, Paused, "vbtest manual pause", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	if err := e.SetHold(ctx, "acme", HoldOperator, Active, "cleared", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if n := logs.count("tenant acme: paused -> delinquent", "the billing hold remains: threshold billing payment failed, set via metronome payment_gate.payment_status",
		"the operator hold was lifted via api manual"); n != 1 {
		t.Fatalf("the log of the lowered state:\n%s", logs)
	}
	if logs.count("threshold billing payment failed via api manual") != 0 {
		t.Fatalf("the remaining hold is logged with the source of the call that lowered the state:\n%s", logs)
	}
	// A state the call itself sets reads as before.
	if err := e.SetHold(ctx, "acme", "abuse", Suspended, "outbound spam", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if logs.count("tenant acme: delinquent -> suspended (abuse: outbound spam via api manual, mode=observe)") != 1 {
		t.Fatalf("the log of a raised state:\n%s", logs)
	}
	// A hold that is lowered, not lifted, says so.
	if err := e.SetHold(ctx, "acme", HoldOperator, Paused, "again", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.SetHold(ctx, "acme", HoldOperator, Warning, "watching", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if logs.count("tenant acme: paused -> suspended", "the abuse hold remains: outbound spam, set via api manual", "the operator hold was lowered via api manual") != 1 {
		t.Fatalf("the log of a hold that was lowered:\n%s", logs)
	}
}

// A state that is reflected again unchanged (a second PUT, a repeated
// webhook, a hold set under a more severe one) creates the same Kubernetes
// Event again. It is there already: that is not an error, and not a log
// line (34 of them in one validation run).
func TestReflectingAnUnchangedStateAgainLogsNoEventError(t *testing.T) {
	e, kube, _ := setup(t, "enforce", nil)
	ctx := context.Background()
	logs := captureLog(t)
	for i := 0; i < 3; i++ {
		if err := e.SetHold(ctx, "acme", HoldOperator, Suspended, "set by operator", "api", "manual"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.SetHold(ctx, "acme", "support", Warning, "ticket 11", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if n := logs.count("event for"); n != 0 {
		t.Fatalf("%d log line(s) about an event that exists already:\n%s", n, logs)
	}
	events, err := kube.CoreV1().Events("team-a").List(ctx, metav1.ListOptions{})
	if err != nil || len(events.Items) != 1 {
		t.Fatalf("events = %d (%v), want the one event of the one state change", len(events.Items), err)
	}
}

// While a pause is still draining, its cluster's control plane runs. The
// guard pass, every 15 seconds, logged "running while its tenant is paused,
// pausing it again" for it, as if the cluster had come back, and queued a
// second pass. A cluster whose pause is in progress is left to it.
func TestGuardLeavesAPauseInProgressAlone(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(podOn("trainer", "ml", "gpu-1"))
	stopsWithItsControlPlane(host, inside)
	draining, release := make(chan struct{}), make(chan struct{})
	first := true
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		if first {
			first = false
			close(draining)
			<-release // the pod takes its grace period
		}
		return false, nil, nil
	})
	passes := 0
	e := New(host, Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside),
		Freeze: func(context.Context, discovery.TenantCluster) error { passes++; return nil }})
	logs := captureLog(t)
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	<-draining
	if st := onlyStatus(t, e); st.Phase != "pausing" {
		t.Fatalf("test setup: status %+v, want pausing", st)
	}
	e.Guard(ctx)
	e.Guard(ctx)
	e.wmu.Lock()
	queued := e.workers[cl.ExternalID()].dirty
	e.wmu.Unlock()
	close(release)
	e.Wait()
	if n := logs.count(cl.ExternalID(), "running while its tenant is paused"); n != 0 {
		t.Fatalf("the guard reported a pause in progress as a cluster that came back, %d time(s):\n%s", n, logs)
	}
	if queued {
		t.Fatal("the guard queued another pass for a cluster whose pause is in progress")
	}
	if st := onlyStatus(t, e); st.Phase != "paused" || !st.Verified || passes != 1 {
		t.Fatalf("status = %+v after %d pass(es), want one pass and a verified pause", st, passes)
	}
	// A cluster that does come back is still caught, and said so.
	if _, err := host.AppsV1().StatefulSets(cl.Namespace).Patch(ctx, cl.Name, "application/merge-patch+json", []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Guard(ctx)
	e.Wait()
	if n := logs.count(cl.ExternalID(), "running while its tenant is paused, pausing it again"); n != 1 {
		t.Fatalf("a cluster scaled back up was reported %d time(s), want once:\n%s", n, logs)
	}
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d: the cluster that came back was not paused again", r)
	}
}

// A control plane someone else scaled to zero, without any mark, is kept
// down quietly: it is not running, and the guard does not say so every 15
// seconds.
func TestGuardDoesNotCallAStoppedClusterRunning(t *testing.T) {
	ctx := context.Background()
	ns := "vc-q"
	host := fake.NewSimpleClientset(controlPlane("q", ns, 0, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, syncedPod("job-x-default-x-q", ns, "q"))
	cl := discovery.TenantCluster{Name: "q", Namespace: ns, Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", State: kvWith(t, "acme", Paused), DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	logs := captureLog(t)
	for i := 0; i < 3; i++ {
		e.Guard(ctx)
		e.Wait()
	}
	if n := logs.count("running while its tenant is paused"); n != 0 {
		t.Fatalf("a cluster with no replicas is called running %d time(s):\n%s", n, logs)
	}
	if pods := podNames(t, host, ns); len(pods) != 0 {
		t.Fatalf("its synced pods were left running: %v", pods)
	}
	if st := onlyStatus(t, e); !strings.Contains(st.Unverified, "paused outside vBilling") {
		t.Fatalf("status = %+v", st)
	}
}

// The MetalLB speaker of the live run runs on the host network in
// metallb-system, and the drain stopped it, so a paused cluster's load
// balancer addresses were reserved (and billed) but not announced. Whether
// a DaemonSet keeps running through a pause is decided by the operator's
// list of namespaces, never by what a pod says about itself: a pod on the
// host network that requests nothing is exactly what a tenant's own
// workload can look like.
func TestHostNetworkDaemonSetOutsideTheKeptNamespacesStopsOnPause(t *testing.T) {
	speaker := podOn("speaker-x", "metallb-system", "gpu-1")
	speaker.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "speaker"}}
	speaker.Spec.HostNetwork = true
	speaker.Spec.Containers = []corev1.Container{{Name: "speaker"}}
	if New(nil, Options{}).keptOnPause(speaker) {
		t.Fatal("a DaemonSet pod is kept through a pause because it is on the host network: any tenant workload can be")
	}
	if !New(nil, Options{KeepNamespaces: append([]string{"metallb-system"}, DefaultKeepNamespaces...)}).keptOnPause(speaker) {
		t.Fatal("PAUSE_KEEP_NAMESPACES with metallb-system does not keep the speaker")
	}
}
