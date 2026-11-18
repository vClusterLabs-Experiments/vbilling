package metrics

import (
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A node whose kubelet went silent (a preempted spot VM, a host that lost
// power) is marked Unknown only a grace period after its last heartbeat.
// Seen live on GCP: an A100 spot VM last heard from at 11:08:29 (Lease
// 11:08:24.9), preempted 11:08:38, marked Unknown 11:09:28, was billed until
// the mark.
func TestSilentNodeIsDownFromItsLastProofOfLife(t *testing.T) {
	silent := func(heartbeat time.Time) func(*corev1.Node) {
		return func(n *corev1.Node) {
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
			n.Status.Conditions[0].Reason = "NodeStatusUnknown"
			n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(heartbeat)
			n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ws.Add(50 * time.Second))
		}
	}
	lease := func(renewed time.Time) *coordinationv1.Lease {
		rt := metav1.NewMicroTime(renewed)
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "a100-spot", Namespace: corev1.NamespaceNodeLease},
			Spec: coordinationv1.LeaseSpec{RenewTime: &rt}}
	}
	for _, tc := range []struct {
		name      string
		heartbeat time.Time
		lease     *coordinationv1.Lease
		billed    time.Duration
	}{
		{"lease renewed after the last status heartbeat", ws.Add(5 * time.Second), lease(ws.Add(12 * time.Second)), 12 * time.Second},
		{"no lease readable: the status heartbeat", ws.Add(5 * time.Second), nil, 5 * time.Second},
		{"stale heartbeat, no lease: the Unknown mark", ws.Add(-4 * time.Minute), nil, 50 * time.Second},
		{"stale lease: the Unknown mark", ws.Add(-4 * time.Minute), lease(ws.Add(-3 * time.Minute)), 50 * time.Second},
	} {
		n := node("a100-spot", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB"}, 1, true, silent(tc.heartbeat))
		objs := []runtime.Object{n, pod("vllm", "a100-spot", withGPU("nvidia.com/gpu", 1))}
		if tc.lease != nil {
			objs = append(objs, tc.lease)
		}
		evs := collect(t, Options{UnhealthyTaints: []string{"node.kubernetes.io/unreachable"}}, objs, latest())[we]
		gpu, _ := find(evs, usage.MetricGPUHours)
		down, _ := find(evs, usage.MetricGPUDowntimeHours)
		if !near(gpu, usage.Round(tc.billed.Hours())) {
			t.Errorf("%s: GPU hours %v, want %s", tc.name, gpu, tc.billed)
		}
		if !near(down, usage.Round((time.Minute - tc.billed).Hours())) {
			t.Errorf("%s: downtime %v, want %s", tc.name, down, time.Minute-tc.billed)
		}
	}
}

func TestLastProofOfLife(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339Nano, "2026-11-18T"+s+"Z"); return v }
	marked := at("11:09:28")
	var zero time.Time
	for _, tc := range []struct {
		name               string
		heartbeat, renewed time.Time
		want               time.Time
	}{
		{"the live preemption", at("11:08:29"), at("11:08:24.9"), at("11:08:29")},
		{"lease is later", at("11:06:00"), at("11:08:24.9"), at("11:08:24.9")},
		{"nothing known", zero, zero, marked},
		{"heartbeat after the mark", at("11:09:30"), zero, marked},
		{"too old to say when it died", at("11:04:00"), zero, marked},
	} {
		if got := lastProofOfLife(tc.heartbeat, tc.renewed, marked); !got.Equal(tc.want) {
			t.Errorf("%s: %s, want %s", tc.name, got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
		}
	}
}

// The same on a Private Node, read through the tenant cluster's API: the
// preempted A100 of the live run was one.
func TestSilentPrivateNodeIsDownFromItsLastProofOfLife(t *testing.T) {
	n := node("pilot-a100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB", "node.kubernetes.io/instance-type": "a2-highgpu-1g"}, 1, true,
		func(n *corev1.Node) {
			n.Status.Conditions[0].Status = corev1.ConditionUnknown
			n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(ws.Add(5 * time.Second))
			n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ws.Add(55 * time.Second))
		})
	rt := metav1.NewMicroTime(ws.Add(9 * time.Second))
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pilot-a100-1", Namespace: corev1.NamespaceNodeLease},
		Spec: coordinationv1.LeaseSpec{RenewTime: &rt}}
	out, _ := collectWith(t, Options{}, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenantConn(n, l)}})
	})
	evs := out[we]
	if got, _ := find(evs, usage.MetricGPUHours, "resource_id", "pilot-a100-1"); !near(got, usage.Round(9.0/3600)) {
		t.Errorf("GPU hours %v, want the 9s until the last Lease renewal", got)
	}
	if got, _ := find(evs, usage.MetricGPUDowntimeHours, "resource_id", "pilot-a100-1"); !near(got, usage.Round(51.0/3600)) {
		t.Errorf("GPU downtime %v, want 51s", got)
	}
}
