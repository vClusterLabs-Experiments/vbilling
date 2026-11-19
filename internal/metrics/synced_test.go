package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func inNamespace(namespace string) podOpt { return func(p *corev1.Pod) { p.Namespace = namespace } }

// Volumes and load balancers a cluster synced to a namespace of their own
// bill like those in its namespace; so do its pods deleted since the last
// collection, until they were deleted.
func TestObjectsSyncedToOtherNamespacesBillTheirCluster(t *testing.T) {
	pvc := boundPVC("ckpt", "100Gi")
	pvc.Namespace, pvc.Labels = "gpu-x-ml", map[string]string{LabelManagedBy: "gpu"}
	lb := lbService("serve")
	lb.Namespace, lb.Labels = "gpu-x-ml", map[string]string{LabelManagedBy: "gpu"}
	other := pod("other-cluster", "h100-1", withGPU("nvidia.com/gpu", 1), inNamespace("web-x-ml"), labels(LabelManagedBy, "web"))
	gone := pod("short-job", "h100-1", withGPU("nvidia.com/gpu", 1), inNamespace("gpu-x-ml"), startedAt(ws.Add(10*time.Second)))
	g := NewGraveyard(time.Hour, func() time.Time { return we })
	g.RecordPod(gone, ws.Add(40*time.Second))
	host := []runtime.Object{node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true), pvc, lb, other}
	out, _ := collectWith(t, Options{}, host, latest(), func(c *Collector) { c.UseGraveyard(g) })
	evs := out[we]
	if got, _ := find(evs, usage.MetricStorageGBHours, "sku", "fast-nvme"); !near(got, usage.Round(100.0/60)) {
		t.Errorf("storage GiB-hours %v, want the volume synced to gpu-x-ml", got)
	}
	if got, _ := find(evs, usage.MetricLBHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("load balancer hours %v, want the service synced to gpu-x-ml", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours); !near(got, usage.Round(30.0/3600)) {
		t.Errorf("GPU-hours %v, want only the deleted job's 30s (another cluster's pod is not this one's)", got)
	}
}

// With namespace syncing, the label holds only the cluster's name. When
// two vClusters have it, what either synced to another namespace could be
// the other's: neither is billed for it, and the source says why.
func TestObjectsOfANameUsedTwiceAreBilledToNeither(t *testing.T) {
	synced := pod("trainer-2", "h100-1", withGPU("nvidia.com/gpu", 4), inNamespace("gpu-x-ml"))
	host := []runtime.Object{node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true),
		pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 2)), synced}
	a, b := target("gpu", true), target("gpu", true)
	b.Cluster.Namespace = "team-b"
	h := health.New(nil)
	c := NewCollector(fake.NewSimpleClientset(host...), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return we.Add(2 * time.Second) }})
	c.UseHealth(h)
	out, _, err := c.Collect(context.Background(), []Target{a, b}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-a-gpu"); !near(got, usage.Round(2.0/60)) {
		t.Errorf("team-a's gpu billed %v GPU-hours, want its own namespace's 2 GPUs only", got)
	}
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-b-gpu"); got != 0 {
		t.Errorf("team-b's gpu billed %v GPU-hours for a pod that may be team-a's", got)
	}
	src, ok := sourceNamed(h, "synced-namespaces/vcluster-team-a-gpu")
	if !ok || src.Status != health.Failing || !strings.Contains(src.Message, "namespace syncing with a cluster name used twice") {
		t.Fatalf("health = %+v", h.List())
	}
}

// A synced pod carries vcluster.loft.sh/managed-by; its other labels are
// the tenant's. One labeled app=vcluster is still a workload, not the
// cluster's control plane.
func TestSyncedPodLabeledLikeTheControlPlaneIsBilled(t *testing.T) {
	host := []runtime.Object{node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true),
		pod("disguised", "h100-1", withGPU("nvidia.com/gpu", 4), labels("app", "vcluster", "release", "gpu"))}
	if got, _ := find(collect(t, Options{}, host, latest())[we], usage.MetricGPUHours); !near(got, usage.Round(4.0/60)) {
		t.Fatalf("a synced pod labeled app=vcluster billed %v GPU-hours, want 4 GPUs", got)
	}
}

// Without list access to every namespace, what clusters synced elsewhere is
// not billed, and the source says so; the rest is metered as before.
func TestSyncedObjectsThatCannotBeListedAreReported(t *testing.T) {
	kube := fake.NewSimpleClientset(node("cpu-1", nil, 0, true), pod("web", "cpu-1", withRequests("1", "1Gi")))
	kube.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "" && a.GetResource().Resource != "nodes" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, "", errors.New("namespaced role"))
		}
		return false, nil, nil
	})
	h := health.New(nil)
	c := NewCollector(kube, nil, Options{Region: "r", Basis: "requests", Now: func() time.Time { return we.Add(2 * time.Second) }})
	c.UseHealth(h)
	out, _, err := c.Collect(context.Background(), []Target{target("gpu", true)}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("CPU core-hours %v", got)
	}
	if src, ok := sourceNamed(h, "synced-namespaces"); !ok || src.Status != health.Failing {
		t.Fatalf("health = %+v", h.List())
	}
}
