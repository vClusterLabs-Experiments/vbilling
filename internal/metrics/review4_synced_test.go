package metrics

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Two vClusters are named dev: team-a's (tenant acme) and team-b's (another
// tenant). Cluster names are chosen by whoever creates a cluster (in
// vCluster Platform, the project user), so the same name in two projects is
// common. team-b's dev is deleted, or its StatefulSet is (an upgrade that
// recreates it with --cascade=orphan). Discovery stops listing it, so
// team-a's dev has no namesake left and its name counts as unique
// (SyncedHere). What team-b's dev left in its own namespace still carries
// vcluster.loft.sh/managed-by=dev, and synced.of and deletedElsewhere (919df10)
// now take it for objects team-a's dev synced to another namespace: acme is
// billed for another tenant's GPUs.
func TestReview4LeftoversOfADeletedNamesakeAreNotBilledToTheSurvivor(t *testing.T) {
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	survivor := Target{Cluster: discovery.TenantCluster{Name: "dev", Namespace: "team-a", Ready: true, CreatedAt: ws.Add(-48 * time.Hour)},
		Tenant: "acme", Project: "research"}
	collectSurvivor := func(t *testing.T, host []runtime.Object, g *Graveyard) float64 {
		t.Helper()
		c := NewCollector(fake.NewSimpleClientset(host...), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
			Now: func() time.Time { return we.Add(2 * time.Second) }})
		if g != nil {
			c.UseGraveyard(g)
		}
		out, _, err := c.Collect(context.Background(), []Target{survivor}, latest())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-a-dev")
		return got
	}

	t.Run("pods deleted with the namesake in this window", func(t *testing.T) {
		// team-b's dev and its namespace are deleted at 10:00:40; the graveyard
		// keeps its 4-GPU trainer for MAX_BACKFILL. The next discovery runs
		// before this window is collected (it runs every 30 seconds).
		gone := pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 4), inNamespace("team-b"), labels(LabelManagedBy, "dev"))
		gone.UID = "team-b-trainer"
		g := NewGraveyard(time.Hour, func() time.Time { return we })
		g.RecordPod(gone, ws.Add(40*time.Second))
		if got := collectSurvivor(t, []runtime.Object{h100}, g); got != 0 {
			t.Errorf("acme's dev (team-a) billed %.6f GPU-hours for the 4-GPU trainer of the deleted team-b/dev, another tenant's cluster "+
				"(40s of 4 GPUs = %.6f): deletedElsewhere takes every deleted pod named dev outside team-a once the name is unique", got, usage.Round(4*40.0/3600))
		}
	})

	t.Run("pods left running by the namesake", func(t *testing.T) {
		// The namesake's synced pod outlives its StatefulSet (deleted with
		// --cascade=orphan for an upgrade, or synced without an owner
		// reference): it keeps running in team-b with managed-by=dev.
		orphan := pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 4), inNamespace("team-b"), labels(LabelManagedBy, "dev"))
		orphan.UID = "team-b-trainer"
		if got := collectSurvivor(t, []runtime.Object{h100, orphan}, nil); got != 0 {
			t.Errorf("acme's dev (team-a) billed %.6f GPU-hours per window for a pod running in team-b, the namespace of another tenant's "+
				"cluster of the same name: synced.of takes it for a pod team-a's dev synced elsewhere", got)
		}
	})
}

// 919df10 bills what a cluster syncs to other namespaces (vcluster.yaml
// sync.toHost.namespaces) from Kubernetes, but Prometheus still selects a
// shared-node cluster's series by namespace="<its own namespace>". A tenant
// writes its vcluster.yaml: turning namespace syncing on moves its inference
// engine out of that namespace, and its token counter (and its egress,
// container_network_transmit_bytes_total{namespace=...}) is never billed.
// Nothing reports it: the counter reads succeed, with no series.
func TestReview4PrometheusUsageOfPodsSyncedToOtherNamespacesIsBilled(t *testing.T) {
	// The engine runs in gpu-x-ml, the namespace gpu's vcluster.yaml syncs
	// its "ml" namespace to; the cluster's own namespace is team-a.
	engine := map[string]string{"namespace": "gpu-x-ml", "model_name": "llama", "pod": "engine-0"}
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: engine,
		samples: counterSamples(ws.Add(-30*time.Minute), ws.Add(2*time.Minute), 3, time.Time{})}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	synced := pod("engine-0", "h100-1", withGPU("nvidia.com/gpu", 1), inNamespace("gpu-x-ml")) // managed-by=gpu
	synced.UID = "engine-0"
	kube := fake.NewSimpleClientset(node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true), synced)
	c := NewCollector(kube, nil, Options{Region: "r1", PrometheusURL: srv.URL, GPUUtilQuery: "none", EgressQuery: "none",
		PromMetrics: []PromMetric{tokens}, GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return we.Add(2 * time.Second) }})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p, err := c.NewPromSource(PromOptions{Health: h})
	if err != nil {
		t.Fatal(err)
	}
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns, Ready: true, CreatedAt: ws.Add(-48 * time.Hour)}, Tenant: "acme"}

	// Kubernetes bills the synced engine's GPU (919df10).
	out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(1.0/60)) {
		t.Fatalf("precondition: the engine synced to gpu-x-ml billed %v GPU-hours, want 1 GPU for the window", got)
	}

	// Its tokens: 4 samples of 3 in each of two windows.
	var billed float64
	for _, w := range windowsFrom(ws, 2) {
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			billed += e.Quantity
		}
	}
	if billed != 24 {
		t.Fatalf("cluster gpu (namespace team-a) syncs its engine to gpu-x-ml: Kubernetes bills the engine's GPU, but %v of its 24 output tokens "+
			"were billed, because Prometheus selects the cluster's series by namespace=%q only", billed, ns)
	}
}
