package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestSuddenNodeFailureWaitsBeforeCommittingItsWindow(t *testing.T) {
	clock := we.Add(5 * time.Second)
	renewal := metav1.NewMicroTime(ws.Add(30 * time.Second))
	n := node("spot", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 1, true)
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "spot", Namespace: corev1.NamespaceNodeLease}, Spec: coordinationv1.LeaseSpec{RenewTime: &renewal}}
	kube := fake.NewSimpleClientset(n, l, pod("engine", "spot", withGPU("nvidia.com/gpu", 1)))
	c := NewCollector(kube, nil, Options{GPUResources: []string{"nvidia.com/gpu"}, Now: func() time.Time { return clock }})
	targets := []Target{{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns, Ready: true}, Tenant: "acme"}}
	_, _, err := c.Collect(context.Background(), targets, latest())
	var pending *HeartbeatPending
	if !errors.As(err, &pending) {
		t.Fatalf("stale Ready node's window was not held: %v", err)
	}
	// Even if the node controller is slow, the wait is bounded and the
	// local snapshot bills only up to the last renewal, not the boundary.
	clock = we.Add(35 * time.Second)
	out, _, err := c.Collect(context.Background(), targets, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(30.0/3600)) {
		t.Fatalf("GPU-hours %v, want only the 30s before the last renewal", got)
	}
	actual, err := kube.CoreV1().Nodes().Get(context.Background(), "spot", metav1.GetOptions{})
	if err != nil || actual.Status.Conditions[0].Status != corev1.ConditionTrue {
		t.Fatalf("metering mutated the Node: %+v %v", actual, err)
	}
}

func TestHealthyLeaseDoesNotDelayAWindow(t *testing.T) {
	nodes := []corev1.Node{*node("healthy", nil, 1, true)}
	proofs := map[string]nodeHeartbeat{"healthy": {At: we.Add(time.Second), Interval: 10 * time.Second}}
	if pending := settleHeartbeats(nodes, proofs, latest(), we.Add(5*time.Second), time.Time{}); pending != nil {
		t.Fatalf("fresh Lease held the window: %v", pending)
	}
}
