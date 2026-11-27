package metrics

import (
	"context"
	"math"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func boundClaim(name string, gi int64, phase corev1.PersistentVolumeClaimPhase, created time.Time) *corev1.PersistentVolumeClaim {
	class := "fast"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name), Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
	if phase == corev1.ClaimBound {
		pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(gi*gib, resource.BinarySI)}
	}
	return pvc
}

func balancer(name string, addressed bool, created time.Time) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name), Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(created)},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	if addressed {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
	}
	return svc
}

// collectWatched meters the default target for one window with a Graveyard
// the test filled, as the watch would have.
func collectWatched(t *testing.T, g *Graveyard, objs ...runtime.Object) []usage.Event {
	t.Helper()
	c := NewCollector(fake.NewSimpleClientset(objs...), nil, Options{Region: "r1", Basis: "requests", Now: func() time.Time { return we.Add(5 * time.Second) }})
	c.UseGraveyard(g)
	out, _, err := c.Collect(context.Background(), []Target{target("gpu", true)}, latest())
	if err != nil {
		t.Fatal(err)
	}
	return out[we]
}

func gibSeconds(evs []usage.Event) float64 {
	q, _ := find(evs, usage.MetricStorageGBHours)
	return math.Round(q*3600*1000) / 1000
}

func lbSeconds(evs []usage.Event) float64 {
	q, _ := find(evs, usage.MetricLBHours)
	return math.Round(q*3600*1000) / 1000
}

// A volume deleted 45 seconds into a window used to have no storage event
// for that window: the list taken when the window closed did not hold it
// any more. It is billed until the second it was deleted, like a pod or a
// node.
func TestDeletedVolumeIsBilledUntilItsDeletion(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return we.Add(5 * time.Second) })
	g.RecordVolume(boundClaim("data", 1, corev1.ClaimBound, ws.Add(-time.Hour)), ws.Add(45*time.Second))
	if got := gibSeconds(collectWatched(t, g)); got != 45 {
		t.Errorf("billed %v GiB-seconds, want 45: 1 GiB until its deletion", got)
	}
	// Another namespace's deleted volume is not this cluster's.
	other := boundClaim("other", 5, corev1.ClaimBound, ws.Add(-time.Hour))
	other.Namespace = "team-b"
	g.RecordVolume(other, ws.Add(30*time.Second))
	if got := gibSeconds(collectWatched(t, g)); got != 45 {
		t.Errorf("billed %v GiB-seconds, want 45: a volume deleted in another namespace is not this cluster's", got)
	}
}

// A volume resized 32 seconds into a window used to be billed at its new
// size for the whole window. It is billed at each size for the time it had
// it.
func TestResizedVolumeIsBilledAtEachSizeForItsTime(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return ws.Add(32 * time.Second) })
	g.claimChanged(boundClaim("data", 1, corev1.ClaimBound, ws.Add(-time.Hour)), boundClaim("data", 3, corev1.ClaimBound, ws.Add(-time.Hour)))
	evs := collectWatched(t, g, boundClaim("data", 3, corev1.ClaimBound, ws.Add(-time.Hour)))
	if got, want := gibSeconds(evs), float64(32*1+28*3); got != want {
		t.Errorf("billed %v GiB-seconds, want %v: 1 GiB for 32 s, then 3 GiB for 28 s (before: 3 GiB for all 60)", got, want)
	}
}

// A claim created 50 seconds before it was bound is billed from when it was
// bound, where the watch saw that. Before, the window it was bound in was
// billed from its creation.
func TestVolumeIsBilledFromWhenItWasBound(t *testing.T) {
	created, boundAt := ws.Add(-20*time.Second), ws.Add(30*time.Second)
	g := NewGraveyard(time.Hour, func() time.Time { return boundAt })
	g.claimChanged(boundClaim("data", 2, corev1.ClaimPending, created), boundClaim("data", 2, corev1.ClaimBound, created))
	if got := gibSeconds(collectWatched(t, g, boundClaim("data", 2, corev1.ClaimBound, created))); got != 60 {
		t.Errorf("billed %v GiB-seconds, want 60: 2 GiB for the 30 s it was bound", got)
	}
	// Not seen by the watch: from its creation, as before.
	if got := gibSeconds(collectWatched(t, NewGraveyard(time.Hour, nil), boundClaim("data", 2, corev1.ClaimBound, ws.Add(20*time.Second)))); got != 80 {
		t.Errorf("billed %v GiB-seconds, want 80: 2 GiB from its creation, 40 s", got)
	}
}

// A load balancer is billed from when it got its address (where the watch
// saw that) until it was deleted.
func TestLoadBalancerIsBilledFromItsAddressToItsDeletion(t *testing.T) {
	created := ws.Add(-30 * time.Second)
	g := NewGraveyard(time.Hour, func() time.Time { return ws.Add(10 * time.Second) })
	g.mu.Lock()
	g.addressed["uid-lb"] = ws.Add(10 * time.Second)
	g.mu.Unlock()
	if got := lbSeconds(collectWatched(t, g, balancer("lb", true, created))); got != 50 {
		t.Errorf("billed %v s, want 50: from its address at 10:00:10", got)
	}
	g.RecordLoadBalancer(balancer("lb", true, created), ws.Add(40*time.Second))
	if got := lbSeconds(collectWatched(t, g)); got != 30 {
		t.Errorf("billed %v s, want 30: from its address until it was deleted at 10:00:40 (before: nothing, it was gone when the window closed)", got)
	}
}

func readyNode(name string, ready bool, since time.Time) *corev1.Node {
	n := node(name, map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, true)
	n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(since)
	if !ready {
		n.Status.Conditions[0].Status = corev1.ConditionFalse
	}
	return n
}

// A node that was not ready for the first 15 seconds of a window and is
// ready when the window is collected used to be billed for all of it: a
// ready node shows nothing of the outage it came back from. The watch saw
// it, so those 15 seconds are downtime, not usage.
func TestNodeThatRecoveredInTheWindowIsNotBilledForItsDowntime(t *testing.T) {
	downAt, backAt := ws.Add(-2*time.Minute), ws.Add(15*time.Second)
	g := NewGraveyard(time.Hour, func() time.Time { return backAt })
	g.nodeChanged(readyNode("n1", false, downAt), readyNode("n1", true, backAt))
	evs := collectWatched(t, g, readyNode("n1", true, backAt))
	if q, _ := find(evs, usage.MetricPrivateNodeHours); math.Round(q*3600*1000)/1000 != 45 {
		t.Errorf("billed %v node-seconds, want 45: the node was not ready for the first 15 s", math.Round(q*3600*1000)/1000)
	}
	// Without the watch nothing tells: billed as the node reads now.
	evs = collectWatched(t, nil, readyNode("n1", true, backAt))
	if q, _ := find(evs, usage.MetricPrivateNodeHours); math.Round(q*3600*1000)/1000 != 60 {
		t.Errorf("without the watch: %v node-seconds, want 60", math.Round(q*3600*1000)/1000)
	}
}

// After a pause the kubelets of a cluster's own nodes renew their Leases
// again only once its API is back. A Lease that is stale because the API
// was down says nothing about the node: it has the usual time to renew from
// when the API came up. Before, the first window after a resume marked a
// node whose kubelet had not reconnected yet as down since before the
// pause, and billed that window as downtime although the node never
// stopped.
func TestKubeletSilenceWhileTheAPIWasDownIsNotANodeFailure(t *testing.T) {
	n := node("n1", nil, 8, true)
	n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(ws.Add(-6 * time.Minute))
	proofs := map[string]nodeHeartbeat{"n1": {At: ws.Add(-6 * time.Minute), Interval: 10 * time.Second}} // last renewal before the pause
	resumed := ws.Add(56 * time.Second)                                                                  // the API came back at 10:00:56
	now := we.Add(5 * time.Second)

	nodes := []corev1.Node{*n.DeepCopy()}
	if pending := settleHeartbeats(nodes, proofs, latest(), now, resumed); pending != nil {
		t.Fatalf("held the window for a node whose API only just came back: %v", pending)
	}
	if nodes[0].Status.Conditions[0].Status != corev1.ConditionTrue {
		t.Fatal("marked the node down from before the pause")
	}

	// A node that stays silent after the API is back is still caught: held
	// first, then down from when the API came up.
	later := []Window{{Start: we, End: we.Add(time.Minute), Latest: true}}
	nodes = []corev1.Node{*n.DeepCopy()}
	if pending := settleHeartbeats(nodes, proofs, later, we.Add(30*time.Second).Add(5*time.Second), resumed); pending == nil {
		t.Fatal("a node silent for 39 s after its API came back should hold the window")
	}
	nodes = []corev1.Node{*n.DeepCopy()}
	if pending := settleHeartbeats(nodes, proofs, later, we.Add(time.Minute+5*time.Second), resumed); pending != nil {
		t.Fatalf("still holding after the longest wait: %v", pending)
	}
	if c := nodes[0].Status.Conditions[0]; c.Status != corev1.ConditionUnknown || !c.LastTransitionTime.Time.Equal(resumed) {
		t.Errorf("a node silent since its API came back: status %s since %s, want Unknown since %s", c.Status, c.LastTransitionTime.Format("15:04:05"), resumed.Format("15:04:05"))
	}

	// Without knowing when the API came up, as before: down from its last renewal.
	nodes = []corev1.Node{*n.DeepCopy()}
	_ = settleHeartbeats(nodes, proofs, latest(), now, time.Time{})
	if c := nodes[0].Status.Conditions[0]; c.Status != corev1.ConditionUnknown {
		t.Errorf("a node silent for 7 minutes with its API up: status %s, want Unknown", c.Status)
	}
}
