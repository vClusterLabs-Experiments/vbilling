package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func inNamespace(namespace string) podOpt { return func(p *corev1.Pod) { p.Namespace = namespace } }

// syncedNamespace is a namespace vCluster's namespace syncing created for
// the cluster name in namespace cpNS, labeled as vCluster labels it.
func syncedNamespace(name, cluster, cpNS string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name,
		Labels: map[string]string{discovery.LabelSyncedName: cluster, discovery.LabelSyncedNamespace: cpNS}}}
}

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
	host := []runtime.Object{node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true), pvc, lb, other,
		syncedNamespace("gpu-x-ml", "gpu", ns), syncedNamespace("web-x-ml", "web", "team-w")}
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

// vCluster labels the namespaces it syncs a cluster into with the cluster's
// name and namespace, so a second vCluster named gpu (another project's)
// changes nothing: what team-a's gpu synced to gpu-x-ml is billed to it,
// what team-b's synced to gpu-x-data is billed to team-b's.
func TestLabeledNamespacesBillTheirClusterWhoeverHasTheName(t *testing.T) {
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	mine := pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 4), inNamespace("gpu-x-ml"))
	theirs := pod("trainer-b", "h100-1", withGPU("nvidia.com/gpu", 2), inNamespace("gpu-x-data"))
	a, b := target("gpu", true), target("gpu", true)
	b.Cluster.Namespace, b.Tenant = "team-b", "globex"
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	c := NewCollector(fake.NewSimpleClientset(h100, mine, theirs, syncedNamespace("gpu-x-ml", "gpu", ns), syncedNamespace("gpu-x-data", "gpu", "team-b")),
		nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Now: func() time.Time { return we.Add(2 * time.Second) }})
	c.UseHealth(h)
	out, _, err := c.Collect(context.Background(), []Target{a, b}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-a-gpu"); !near(got, usage.Round(4.0/60)) {
		t.Errorf("team-a's gpu billed %v GPU-hours, want the 4 GPUs it synced to gpu-x-ml", got)
	}
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-b-gpu"); !near(got, usage.Round(2.0/60)) {
		t.Errorf("team-b's gpu billed %v GPU-hours, want the 2 GPUs it synced to gpu-x-data", got)
	}
	for _, s := range h.List() {
		if s.Status == health.Failing {
			t.Errorf("source %s failing: %s", s.Name, s.Message)
		}
	}
}

// A deleted namesake's leftovers as they are on a real control plane
// cluster: its namespace exists, without vCluster's labels (it is where the
// namesake ran, not one it synced to). Its pod outlives its StatefulSet,
// deleted with --cascade=orphan, so discovery no longer lists it and the
// survivor's name looks unique. Its control plane Service (app=vcluster)
// is still there; once that is gone too, vBilling still remembers the
// namespace was the namesake's.
func TestLeftoversInTheNamespaceANamesakeRanInAreNeverTheSurvivors(t *testing.T) {
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	survivor := target("dev", true)
	namesake := target("dev", true)
	namesake.Cluster.Namespace = "team-b"
	orphan := pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 4), inNamespace("team-b"), labels(LabelManagedBy, "dev"))
	teamB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "team-b", Labels: map[string]string{"app": "vcluster", "release": "dev"}}}
	survivorGPUs := func(t *testing.T, c *Collector, targets ...Target) float64 {
		t.Helper()
		out, _, err := c.Collect(context.Background(), targets, latest())
		if err != nil {
			t.Fatal(err)
		}
		got, _ := find(out[we], usage.MetricGPUHours, usage.DimTenantCluster, "vcluster-team-a-dev")
		return got
	}
	newC := func(objs ...runtime.Object) *Collector {
		return NewCollector(fake.NewSimpleClientset(objs...), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
			Now: func() time.Time { return we.Add(2 * time.Second) }})
	}

	if got := survivorGPUs(t, newC(h100, orphan, teamB, svc), survivor); got != 0 {
		t.Errorf("the survivor billed %v GPU-hours for a pod in team-b, where the namesake's control plane Service still is", got)
	}
	// Seen as the namesake's namespace first, then the namesake and its
	// Service are gone: the pod is not the survivor's either.
	c := newC(h100, orphan, teamB)
	survivorGPUs(t, c, survivor, namesake)
	if got := survivorGPUs(t, c, survivor); got != 0 {
		t.Errorf("the survivor billed %v GPU-hours for a pod in team-b, where vBilling saw the namesake run", got)
	}
	// Never seen, nothing left that names it: a namespace without vCluster's
	// labels where no cluster ran is the cluster's, as before.
	if got := survivorGPUs(t, newC(h100, orphan, teamB), survivor); !near(got, usage.Round(4.0/60)) {
		t.Errorf("a pod named dev in an unlabeled namespace no cluster ran in billed %v GPU-hours, want its 4 GPUs (the name is unique)", got)
	}
}

// A synced namespace deleted with its pods: the pods deleted since the last
// collection are billed until they were deleted, to the cluster vCluster's
// labels named while the namespace existed.
func TestPodsDeletedWithTheirSyncedNamespaceAreBilledAsItWas(t *testing.T) {
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	job := pod("job", "h100-1", withGPU("nvidia.com/gpu", 2), inNamespace("gpu-x-ml"))
	job.UID = "job"
	kube := fake.NewSimpleClientset(h100, job, syncedNamespace("gpu-x-ml", "gpu", ns))
	g := NewGraveyard(time.Hour, func() time.Time { return we.Add(time.Minute) })
	now := we.Add(2 * time.Second)
	c := NewCollector(kube, nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Now: func() time.Time { return now }})
	c.UseGraveyard(g)
	tg := target("gpu", true)
	if _, _, err := c.Collect(context.Background(), []Target{tg}, latest()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := kube.CoreV1().Pods("gpu-x-ml").Delete(ctx, "job", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := kube.CoreV1().Namespaces().Delete(ctx, "gpu-x-ml", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	g.RecordPod(job, we.Add(30*time.Second))
	now = we.Add(time.Minute + 2*time.Second)
	out, _, err := c.Collect(ctx, []Target{tg}, []Window{{Start: we, End: we.Add(time.Minute), Latest: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we.Add(time.Minute)], usage.MetricGPUHours); !near(got, usage.Round(2*30.0/3600)) {
		t.Fatalf("the job deleted with gpu-x-ml billed %v GPU-hours, want its 30s of 2 GPUs", got)
	}
}
