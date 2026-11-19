package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Earlier versions kept the history by node UID. On load each record moves
// to its cluster and node name; two records of one node (its object was
// created again) become one: what the earlier first saw, and the most GPUs
// either had.
func TestPrivateNodeHistoryByUIDIsMovedToTheNodesName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-nodes.json")
	old := map[string]nodeRecord{
		"uid-gpu-1": {Cluster: "vcluster-team-a-gpu", Node: "gpu-1", GPUs: 8, GPUType: "NVIDIA-H100-80GB-HBM3", InstanceType: "bm.gpu.h100.8",
			CapacityType: "on-demand", Zone: "syd-1a", FirstSeen: ws.Add(-48 * time.Hour), LastSeen: ws.Add(-time.Hour)},
		"uid-gpu-1-recreated": {Cluster: "vcluster-team-a-gpu", Node: "gpu-1", GPUType: "Tesla-T4", CapacityType: "spot",
			FirstSeen: ws.Add(-time.Hour), LastSeen: ws},
	}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	now := we
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	if err := c.UsePrivateNodeHistory(path); err != nil {
		t.Fatal(err)
	}
	conn := tenantConn(h100Node(withUID("uid-gpu-1-recreated"), func(n *corev1.Node) {
		n.Labels["nvidia.com/gpu.product"] = "Tesla-T4"
		n.Labels["karpenter.sh/capacity-type"] = "spot"
		n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(0)
	}))
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	evs := readPrivate(t, c, &now, we)
	if got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H100-80GB-HBM3", usage.DimCapacityType, usage.CapacityOnDemand); n != 1 || !near(got, usage.Round(8.0/60)) {
		t.Fatalf("a node recreated before the upgrade billed %v GPU-hours (%d events), want 8 H100s on demand as first seen: %+v", got, n, evs)
	}
	var saved map[string]nodeRecord
	if err := json.Unmarshal(readFile(t, path), &saved); err != nil {
		t.Fatal(err)
	}
	if r, ok := saved["vcluster-team-a-gpu/gpu-1"]; len(saved) != 1 || !ok || r.GPUs != 8 || r.CapacityType != "on-demand" {
		t.Fatalf("saved history = %+v, want one record by cluster and node name", saved)
	}
}

// Labels a node gets after its first ten minutes do not fill what it was
// first seen without: it bills as first seen.
func TestPrivateNodeAttributesFillOnlyWhileNew(t *testing.T) {
	now := we
	unlabeled := h100Node(func(n *corev1.Node) {
		delete(n.Labels, "node.kubernetes.io/instance-type")
		delete(n.Labels, "karpenter.sh/capacity-type")
		n.CreationTimestamp = metav1.NewTime(we.Add(-11 * time.Minute))
	})
	conn := tenantConn(unlabeled)
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	readPrivate(t, c, &now, we)
	setNode(t, conn, h100Node(func(n *corev1.Node) { n.Labels["karpenter.sh/capacity-type"] = "spot" }))
	evs := readPrivate(t, c, &now, we.Add(time.Minute))
	if got, n := find(evs, usage.MetricPrivateNodeHours, "sku", "8xNVIDIA-H100-80GB-HBM3", usage.DimCapacityType, usage.CapacityOnDemand); n != 1 || !near(got, usage.Round(1.0/60)) {
		t.Fatalf("labels set 12 minutes after the node was created changed what it bills: %+v", evs)
	}
}

// With GPU Feature Discovery's physical count, the floor rises at once, as
// far as that count goes. Without it, a count above the floor raises it
// once it held for gpuRaiseAfter.
func TestPrivateNodeGPUFloorRises(t *testing.T) {
	t.Run("counted", func(t *testing.T) {
		now := we
		conn := tenantConn(h100Node(func(n *corev1.Node) {
			n.Labels["nvidia.com/gpu.count"] = "4"
			n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(8) // a device plugin and a DRA driver both advertise them
		}))
		c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
			Now: func() time.Time { return now }})
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
		readPrivate(t, c, &now, we)
		setNode(t, conn, h100Node(func(n *corev1.Node) { n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(0) }))
		if got, _ := find(readPrivate(t, c, &now, we.Add(time.Minute)), usage.MetricGPUHours); !near(got, usage.Round(4.0/60)) {
			t.Fatalf("%v GPU-hours once the node stopped advertising, want the 4 GPU Feature Discovery counted on the first read", got)
		}
	})
	t.Run("held", func(t *testing.T) {
		now := we
		conn := tenantConn(h100Node(func(n *corev1.Node) { n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(4) }))
		c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
			Now: func() time.Time { return now }})
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
		readPrivate(t, c, &now, we)
		readPrivate(t, c, &now, we.Add(time.Minute)) // floor 4
		setNode(t, conn, h100Node())
		for i := 2; i <= 12; i++ { // 8 GPUs from read 3 on, for 10 minutes
			readPrivate(t, c, &now, we.Add(time.Duration(i)*time.Minute))
		}
		setNode(t, conn, h100Node(func(n *corev1.Node) { n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(0) }))
		if got, _ := find(readPrivate(t, c, &now, we.Add(13*time.Minute)), usage.MetricGPUHours); !near(got, usage.Round(8.0/60)) {
			t.Fatalf("%v GPU-hours once the node stopped advertising, want the 8 it advertised for 10 minutes", got)
		}
	})
}
