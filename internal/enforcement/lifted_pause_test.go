package enforcement

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A hold lifted while its pause is under way ends the pause there. In the
// live validation of v0.3.2 the hold was lifted 27 seconds into a pause,
// and vBilling still deleted every workload pod, stopped the control plane
// and verified the pause before it resumed: the customer lost its workloads
// and got a control plane restart after it no longer owed anything.

// scaleDowns records the patches that scale a control plane to zero.
func scaleDowns(host *fake.Clientset) func() int {
	var mu sync.Mutex
	n := 0
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return false, nil, nil
	})
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// Lifted while the drain deletes pods: the pods it had not reached keep
// running, the control plane is never stopped, and the pause mark, the
// pause policy and the taint are gone again. Nothing was paused, so no
// pause span exists: every second bills.
func TestHoldLiftedDuringTheDrainStopsThePause(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(
		ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"),
		ownedPod("web-2", "shop", "gpu-1", "ReplicaSet", "web"),
		ownedPod("web-3", "shop", "gpu-1", "ReplicaSet", "web"))
	stopped := scaleDowns(host)
	spans := pause.Memory()
	logs := captureLog(t)
	var e *Enforcer
	var mu sync.Mutex
	var deleted []string
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		deleted = append(deleted, a.(ktesting.DeleteActionImpl).Name)
		first := len(deleted) == 1
		mu.Unlock()
		if first {
			// The customer pays while the first pod is being stopped.
			if err := e.Set(ctx, "acme", Active, "top-up paid", "api", "manual"); err != nil {
				t.Errorf("lift the hold: %v", err)
			}
		}
		return false, nil, nil
	})
	e = New(host, Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery = 5 * time.Millisecond
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()

	mu.Lock()
	got := append([]string(nil), deleted...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("pods deleted: %v, want the drain to stop after the one it was deleting when the hold was lifted", got)
	}
	if left := podNames(t, inside, "shop"); len(left) != 2 {
		t.Fatalf("pods left: %v, want the two the drain had not reached", left)
	}
	if n := stopped(); n != 0 {
		t.Fatalf("the control plane was scaled to zero %d time(s) after the hold was lifted", n)
	}
	s := sts(t, host, cl.Namespace, cl.Name)
	if *s.Spec.Replicas != 1 || s.Annotations[discovery.AnnotationPausedBy] != "" || s.Annotations[discovery.AnnotationPausedReplicas] != "" {
		t.Fatalf("control plane: replicas %d, annotations %v; want it running without a pause mark", *s.Spec.Replicas, s.Annotations)
	}
	if hasPolicy(t, inside, PausedPolicyName) || hasPolicy(t, inside, PolicyName) || tainted(t, inside, "gpu-1") {
		t.Fatalf("left inside the cluster: pause policy %t, suspension policy %t, taint %t; want none",
			hasPolicy(t, inside, PausedPolicyName), hasPolicy(t, inside, PolicyName), tainted(t, inside, "gpu-1"))
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v: a pause that was lifted before it stopped anything must not stop any billing", got)
	}
	if _, stopped := spans.Stopped(cl.ExternalID()); stopped {
		t.Fatal("metering is still told the control plane was stopped")
	}
	if st := onlyStatus(t, e); st.Phase != "running" || st.Error != "" || st.Verified {
		t.Fatalf("status = %+v, want running", st)
	}
	if e.hasRecord(cl.ExternalID()) {
		t.Fatal("the pause is still on record after it was undone")
	}
	if logs.count(cl.ExternalID(), "no longer paused") != 1 {
		t.Fatalf("the log does not say the pause stopped:\n%s", logs)
	}
}

// Lifted while the inventory is saved, right before the hard stop: the
// control plane is not scaled down.
func TestHoldLiftedBeforeTheControlPlaneStopsKeepsItRunning(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"))
	stopped := scaleDowns(host)
	spans := pause.Memory()
	var e *Enforcer
	e = New(host, Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside),
		Freeze: func(context.Context, discovery.TenantCluster) error {
			return e.Set(ctx, "acme", Active, "top-up paid", "api", "manual")
		}})
	e.resumeEvery = 5 * time.Millisecond
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if n := stopped(); n != 0 {
		t.Fatalf("the control plane was scaled to zero %d time(s) although the hold was lifted before the stop", n)
	}
	if s := sts(t, host, cl.Namespace, cl.Name); *s.Spec.Replicas != 1 || s.Annotations[discovery.AnnotationPausedBy] != "" {
		t.Fatalf("control plane: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
	if hasPolicy(t, inside, PausedPolicyName) || tainted(t, inside, "gpu-1") {
		t.Fatal("the pause policy or the taint is still there")
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v, want none", got)
	}
}

// Lifted as the control plane is scaled down: the pass does not go on to
// stop the synced pods and watch the stop. The control plane is scaled back
// to its replicas at once, and no span opens, so nothing after the lift
// counts as paused.
func TestHoldLiftedWhileTheControlPlaneStopsScalesItBackUp(t *testing.T) {
	ctx := context.Background()
	ns := "vc-team"
	host := fake.NewSimpleClientset(controlPlane("team", ns, 2, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		syncedPod("api-x-default-x-team", ns, "team"), syncedPod("worker-x-default-x-team", ns, "team"))
	cl := discovery.TenantCluster{Name: "team", Namespace: ns, Source: "statefulset"}
	spans := pause.Memory()
	var e *Enforcer
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			// Lifted in the very call that stops it.
			if err := e.storeHold("acme", Active); err != nil {
				t.Errorf("lift the hold: %v", err)
			}
		}
		return false, nil, nil
	})
	var events pauseLog
	e = New(host, Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		OnPause: func(_ string, paused bool) {
			if paused {
				events.add("paused")
			} else {
				events.add("resumed")
			}
		}})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	s := sts(t, host, ns, "team")
	if *s.Spec.Replicas != 2 || s.Annotations[discovery.AnnotationPausedBy] != "" {
		t.Fatalf("control plane: replicas %d, annotations %v; want its 2 replicas back and no pause mark", *s.Spec.Replicas, s.Annotations)
	}
	if pods := podNames(t, host, ns); len(pods) != 2 {
		t.Fatalf("synced pods left: %v, want both: the pass must not stop them once the hold is lifted", pods)
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v: nothing after the lift may count as paused", got)
	}
	if _, stopped := spans.Stopped(cl.ExternalID()); stopped {
		t.Fatal("metering is still told the control plane is stopped")
	}
	if ev := events.list(); len(ev) != 2 || ev[1] != "resumed" {
		t.Fatalf("metering was told %v, want paused, then resumed", ev)
	}
	if st := onlyStatus(t, e); st.Phase != "running" || st.Verified {
		t.Fatalf("status = %+v, want running", st)
	}
}

// storeHold sets the tenant's billing hold without reflecting it on the
// clusters: the first half of a state change, for tests that change the
// state from inside a call the fake API is serving (it is locked then).
func (e *Enforcer) storeHold(tenant string, state State) error {
	_, _, _, err := e.store(tenant, update{hold: HoldBilling, state: state, reason: "test", source: "api", eventType: "manual"})
	return err
}

// A cluster reached only through its own API: lifted during its drain, the
// pause policy and the taint go, and no span opens.
func TestHoldLiftedDuringTheDrainOfAnExternalCluster(t *testing.T) {
	ctx := context.Background()
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}},
		ownedPod("infer-1", "ml", "edge-1", "ReplicaSet", "infer"), ownedPod("infer-2", "ml", "edge-1", "ReplicaSet", "infer"))
	cl := discovery.TenantCluster{Name: "edge", External: true}
	spans := pause.Memory()
	var e *Enforcer
	var mu sync.Mutex
	deleted := 0
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		deleted++
		first := deleted == 1
		mu.Unlock()
		if first {
			if err := e.storeHold("acme", Active); err != nil {
				t.Errorf("lift the hold: %v", err)
			}
		}
		return false, nil, nil
	})
	e = New(fake.NewSimpleClientset(), Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if left := podNames(t, inside, "ml"); len(left) != 1 {
		t.Fatalf("pods left: %v, want the one the drain had not reached", left)
	}
	if hasPolicy(t, inside, PausedPolicyName) || tainted(t, inside, "edge-1") {
		t.Fatal("the pause policy or the taint is still there")
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v, want none", got)
	}
}

// A tenant that is paused again while its lifted pause is being undone
// ends up paused: undoing one pause never leaves a later one undone.
func TestPausedAgainWhileALiftedPauseIsUndoneIsPaused(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"), ownedPod("web-2", "shop", "gpu-1", "ReplicaSet", "web"))
	stopsWithItsControlPlane(host, inside)
	spans := pause.Memory()
	var e *Enforcer
	var mu sync.Mutex
	deleted, again := 0, false
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		deleted++
		first := deleted == 1
		mu.Unlock()
		if first {
			if err := e.storeHold("acme", Active); err != nil {
				t.Errorf("lift the hold: %v", err)
			}
		}
		return false, nil, nil
	})
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `paused-by":null`) && !again {
			// The wallet is empty again as the pause mark is lifted.
			again = true
			if err := e.storeHold("acme", Paused); err != nil {
				t.Errorf("pause again: %v", err)
			}
		}
		return false, nil, nil
	})
	e = New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery = 5 * time.Millisecond
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	mu.Lock()
	undone := again
	mu.Unlock()
	if !undone {
		t.Fatal("test setup: the lifted pause was never undone")
	}
	e.Guard(ctx) // every 15 seconds
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d: the later pause was not carried out", r)
	}
	if _, open := spans.Current(cl.ExternalID()); !open {
		t.Fatalf("the later pause is not verified: %+v", e.Pauses())
	}
}
