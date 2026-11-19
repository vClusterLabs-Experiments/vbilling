package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func withUID(uid string) func(*corev1.Node) {
	return func(n *corev1.Node) { n.UID = types.UID(uid) }
}

// readPrivate meters the window that ends at end, read 2s after it, and
// returns its events.
func readPrivate(t *testing.T, c *Collector, now *time.Time, end time.Time) []usage.Event {
	t.Helper()
	*now = end.Add(2 * time.Second)
	out, st, err := c.Collect(context.Background(), []Target{target("gpu", true)}, []Window{{Start: end.Add(-time.Minute), End: end, Latest: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TenantAPIFailed) > 0 {
		t.Fatalf("cluster API read failed: %v", st.TenantAPIFailed)
	}
	return out[end]
}

func setNode(t *testing.T, conn *TenantConn, n *corev1.Node) {
	t.Helper()
	if _, err := conn.Kube.CoreV1().Nodes().Update(context.Background(), n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func h100Node(mods ...func(*corev1.Node)) *corev1.Node {
	return node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "node.kubernetes.io/instance-type": "bm.gpu.h100.8",
		"karpenter.sh/capacity-type": "on-demand", "topology.kubernetes.io/zone": "syd-1a"}, 8, true, append([]func(*corev1.Node){withUID("uid-gpu-1")}, mods...)...)
}

// What a cluster's own node bills as is what vBilling first saw of it: a
// relabel inside the cluster changes neither its GPU model, instance type,
// capacity type nor zone, and it bills for no fewer GPUs than it had. The
// change is reported as diverged, and clears once the node is back.
func TestPrivateNodeBillsAsFirstSeen(t *testing.T) {
	now := we
	conn := tenantConn(h100Node())
	h := health.New(nil)
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	c.UseHealth(h)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	readPrivate(t, c, &now, we)
	readPrivate(t, c, &now, we.Add(time.Minute)) // seen twice: 8 GPUs is its high-water mark

	cheaper := h100Node(func(n *corev1.Node) {
		n.Labels["nvidia.com/gpu.product"] = "Tesla-T4"
		n.Labels["node.kubernetes.io/instance-type"] = "g4dn.xlarge"
		n.Labels["karpenter.sh/capacity-type"] = "spot"
		n.Labels["topology.kubernetes.io/zone"] = "cheap-1a"
		// Time-slicing labels that add up: 1 physical GPU x 8 replicas.
		n.Labels["nvidia.com/gpu.count"] = "1"
		n.Labels["nvidia.com/gpu.replicas"] = "8"
	})
	setNode(t, conn, cheaper)
	evs := readPrivate(t, c, &now, we.Add(2*time.Minute))
	if got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H100-80GB-HBM3", usage.DimGPUType, "NVIDIA-H100-80GB-HBM3",
		usage.DimCapacityType, usage.CapacityOnDemand, usage.DimZone, "syd-1a"); n != 1 || !near(got, usage.Round(8.0/60)) {
		t.Fatalf("relabeled node billed %v GPU-hours in %d events as first seen, want 8 GPUs of H100 on demand in syd-1a: %+v", got, n, evs)
	}
	if got, n := find(evs, usage.MetricPrivateNodeHours, "sku", "bm.gpu.h100.8", usage.DimInstanceType, "bm.gpu.h100.8"); n != 1 || !near(got, usage.Round(1.0/60)) {
		t.Fatalf("relabeled node billed %v node-hours (%d events) under its first instance type", got, n)
	}
	src, ok := sourceNamed(h, "private-nodes/vcluster-team-a-gpu")
	if !ok || src.Status != health.Diverged {
		t.Fatalf("health = %+v, want the cluster's private-nodes source diverged", h.List())
	}
	for _, want := range []string{"GPU model Tesla-T4 (first seen: NVIDIA-H100-80GB-HBM3)", "capacity type spot", "advertises 1 GPUs, billed for 8"} {
		if !strings.Contains(src.Message, want) {
			t.Errorf("diverged message %q lacks %q", src.Message, want)
		}
	}

	setNode(t, conn, h100Node())
	readPrivate(t, c, &now, we.Add(3*time.Minute))
	if src, _ := sourceNamed(h, "private-nodes/vcluster-team-a-gpu"); src.Status != health.OK {
		t.Fatalf("health = %+v, want the source to recover once the node is back", h.List())
	}
}

func sourceNamed(h *health.Registry, name string) (health.Source, bool) {
	for _, s := range h.List() {
		if s.Name == name {
			return s, true
		}
	}
	return health.Source{}, false
}

// A count only one read saw (a device plugin that advertises time-sliced
// devices before GPU Feature Discovery labels them) does not become the
// node's high-water mark.
func TestPrivateNodeGPUHighWaterMarkNeedsTwoReads(t *testing.T) {
	now := we
	racing := h100Node(func(n *corev1.Node) { n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(64) })
	conn := tenantConn(racing)
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	if got, _ := find(readPrivate(t, c, &now, we), usage.MetricGPUHours); !near(got, usage.Round(64.0/60)) {
		t.Fatalf("first read: %v GPU-hours, want what it advertised", got)
	}
	labeled := h100Node(func(n *corev1.Node) {
		n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(64)
		n.Labels["nvidia.com/gpu.count"] = "8"
		n.Labels["nvidia.com/gpu.replicas"] = "8"
	})
	setNode(t, conn, labeled)
	for i := 1; i <= 3; i++ {
		if got, _ := find(readPrivate(t, c, &now, we.Add(time.Duration(i)*time.Minute)), usage.MetricGPUHours); !near(got, usage.Round(8.0/60)) {
			t.Fatalf("read %d: %v GPU-hours, want 8 physical GPUs (64 time-sliced devices / 8)", i+1, got)
		}
	}
}

func resourceQty(n int64) *resource.Quantity { return resource.NewQuantity(n, resource.DecimalSI) }

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The history is kept in DATA_DIR: a restart does not forget what a node
// first was, nor its GPUs.
func TestPrivateNodeHistorySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-nodes.json")
	now := we
	conn := tenantConn(h100Node())
	newC := func() *Collector {
		c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
			Now: func() time.Time { return now }})
		if err := c.UsePrivateNodeHistory(path); err != nil {
			t.Fatal(err)
		}
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
		return c
	}
	c := newC()
	readPrivate(t, c, &now, we)
	readPrivate(t, c, &now, we.Add(time.Minute))

	setNode(t, conn, h100Node(func(n *corev1.Node) {
		n.Labels["nvidia.com/gpu.product"] = "Tesla-T4"
		n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(1)
	}))
	evs := readPrivate(t, newC(), &now, we.Add(2*time.Minute))
	if got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H100-80GB-HBM3"); n != 1 || !near(got, usage.Round(8.0/60)) {
		t.Fatalf("after a restart the relabeled node billed %v GPU-hours (%d events), want 8 H100s", got, n)
	}
	var saved map[string]nodeRecord
	if err := json.Unmarshal(readFile(t, path), &saved); err != nil {
		t.Fatal(err)
	}
	if r := saved["uid-gpu-1"]; r.GPUs != 8 || r.GPUType != "NVIDIA-H100-80GB-HBM3" || r.Cluster != "vcluster-team-a-gpu" {
		t.Fatalf("saved history = %+v", saved)
	}
}

// Time-slicing replicas divide a cluster's own node's devices only when GPU
// Feature Discovery's physical count adds up with them.
func TestPrivateNodeTimeSlicingNeedsGPUFeatureDiscoverysCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		labels   map[string]string
		devices  int64
		physical float64
	}{
		{"8 GPUs x 4 replicas, counted", map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/gpu.replicas": "4"}, 32, 8},
		{"replicas without a count", map[string]string{"nvidia.com/gpu.replicas": "4"}, 32, 32},
		{"a count that does not add up", map[string]string{"nvidia.com/gpu.count": "2", "nvidia.com/gpu.replicas": "4"}, 32, 32},
		{"GKE time-sharing labels", map[string]string{"cloud.google.com/gke-gpu-sharing-strategy": "time-sharing", "cloud.google.com/gke-max-shared-clients-per-gpu": "4"}, 32, 32},
	} {
		labels := map[string]string{"nvidia.com/gpu.product": "NVIDIA-L40S"}
		for k, v := range tc.labels {
			labels[k] = v
		}
		_, g := privateNodeBill(t, node("gpu-h100-1", labels, tc.devices, true))
		if !near(g, usage.Round(tc.physical/60)) {
			t.Errorf("%s: %v GPU-hours, want %v GPUs", tc.name, g, tc.physical)
		}
	}
}

// A paused cluster on shared nodes is metered from the inventory saved
// before its pause, when its synced pods (which vCluster's placeholders
// stand for) were still running: the placeholders are not its own nodes.
// An inventory saved by an earlier version is decided as that version did.
func TestPausedClusterKeepsItsNodesAsClassifiedWhenSaved(t *testing.T) {
	synced := pod("web", "cp-2", withRequests("1", "1Gi"))
	c := pausedCollector(t, pause.DefaultKeep, []runtime.Object{node("cp-2", nil, 0, true, machine("4c4c4544-0002")), synced})
	tg := target("gpu", false)
	conn := tenantConn(node("cp-2", map[string]string{LabelFakeNode: "true"}, 0, true, machine("a1b2c3d4-0000")))
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	if err := c.Freeze(context.Background(), tg.Cluster); err != nil {
		t.Fatal(err)
	}
	if has, _ := c.OwnNodes(tg.Cluster); has {
		t.Fatal("the placeholder counted as the cluster's own node")
	}
	// The pause stops the synced pods; the cluster's API goes away.
	if err := c.kube.CoreV1().Pods(ns).Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused"), conns: map[string]*TenantConn{"gpu": nil}})
	tg.Cluster.Paused = true
	tg.Paused = []pause.Span{{From: ws.Add(-time.Hour), By: "vbilling"}}
	out, st, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if st.PrivateNodes != 0 {
		t.Fatalf("a placeholder of the saved inventory was metered as a private node: %+v", out[we])
	}

	// Saved by v0.3.0: not classified, so a placeholder is its label.
	var inv tenantInventory
	b, _ := c.pauses.Inventory(tg.Cluster.ExternalID())
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	inv.Views, inv.Classified = nil, false
	if err := c.storeInventory(tg.Cluster.ExternalID(), &inv); err != nil {
		t.Fatal(err)
	}
	if _, st, err := c.Collect(context.Background(), []Target{tg}, latest()); err != nil || st.PrivateNodes != 0 {
		t.Fatalf("legacy inventory: %d private nodes, %v", st.PrivateNodes, err)
	}
}

// A fill (or a pause's Freeze) meters a cluster's API without the list of
// its synced pods the collection made: vBilling reads where they run.
func TestFillKnowsThePlaceholdersOfAClusterOnSharedNodes(t *testing.T) {
	host := []runtime.Object{node("cp-2", nil, 0, true, machine("4c4c4544-0002")), pod("web", "cp-2", withRequests("1", "1Gi"))}
	conn := tenantConn(node("cp-2", map[string]string{LabelFakeNode: "true"}, 0, true, machine("a1b2c3d4-0000")),
		node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4"}, 1, true))
	c := NewCollector(fake.NewSimpleClientset(host...), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return we.Add(2 * time.Second) }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	out, st, err := c.CollectTenantAPI(context.Background(), []Target{target("gpu", true)}, []Window{{Start: ws, End: we}})
	if err != nil {
		t.Fatal(err)
	}
	if st.PrivateNodes != 1 {
		t.Fatalf("private nodes = %d, want gpu-1 only", st.PrivateNodes)
	}
	if _, n := find(out[we], usage.MetricPrivateNodeHours, "resource_id", "cp-2"); n != 0 {
		t.Fatal("a placeholder was filled as a private node")
	}
}
