package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type fakeTenantAPI struct {
	conns map[string]*TenantConn
	err   error
}

func (f fakeTenantAPI) Conn(_ context.Context, cl discovery.TenantCluster) (*TenantConn, error) {
	if f.err != nil {
		return nil, f.err
	}
	if c, ok := f.conns[cl.Name]; ok {
		return c, nil
	}
	return nil, ErrNoTenantAPI
}

func tenantConn(objs ...runtime.Object) *TenantConn {
	return &TenantConn{Kube: fake.NewSimpleClientset(objs...)}
}

// collectWith is collect with a hook to attach tenant APIs or a graveyard.
func collectWith(t *testing.T, opts Options, host []runtime.Object, windows []Window, setup func(*Collector)) (map[time.Time][]usage.Event, Stats) {
	t.Helper()
	if opts.GPUResources == nil {
		opts.GPUResources = []string{"nvidia.com/gpu"}
	}
	if opts.Basis == "" {
		opts.Basis = "requests"
	}
	opts.Region = "ap-southeast-2"
	opts.Now = func() time.Time { return we.Add(2 * time.Second) }
	c := NewCollector(fake.NewSimpleClientset(host...), nil, opts)
	setup(c)
	out, st, err := c.Collect(context.Background(), []Target{target("gpu", true)}, windows)
	if err != nil {
		t.Fatal(err)
	}
	return out, st
}

func lbService(name string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference", CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
		Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
		Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "10.0.0.9"}}}}}
}

func boundPVC(name, size string) *corev1.PersistentVolumeClaim {
	class := "fast-nvme"
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "training", CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
		Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}
}

func TestPrivateNodesBilledWholeFromTheTenantAPI(t *testing.T) {
	host := []runtime.Object{node("cp-worker-1", nil, 0, true)}
	gpuNode := node("gpu-a100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB", "node.kubernetes.io/instance-type": "a2-ultragpu-8g"}, 8, true)
	tenant := tenantConn(
		node("cp-worker-1", nil, 0, true), // a synced copy of a control plane cluster node: never billed here
		node("vcluster-fake", map[string]string{LabelFakeNode: "true"}, 0, true),
		gpuNode,
		node("byo-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4", LabelBillable: "false"}, 1, true),
		pod("trainer", "gpu-a100-1", withGPU("nvidia.com/gpu", 8)),
		boundPVC("ckpt", "100Gi"),
		lbService("serve"),
	)
	out, st := collectWith(t, Options{}, host, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	})
	evs := out[we]
	if st.PrivateNodes != 2 {
		t.Errorf("private nodes = %d, want gpu-a100-1 and byo-gpu", st.PrivateNodes)
	}
	if got, n := find(evs, usage.MetricPrivateNodeHours, "resource_id", "gpu-a100-1", "sku", "a2-ultragpu-8g", usage.DimBillingMode, "private_node"); n != 1 || !near(got, usage.Round(1.0/60)) {
		t.Fatalf("private node hours = %v (%d events)", got, n)
	}
	if got, _ := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "private_node"); !near(got, usage.Round(8.0/60)) {
		t.Fatalf("private node GPU hours = %v, want the node's 8 GPUs", got)
	}
	if _, n := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "shared"); n != 0 {
		t.Fatal("pods on a private node were billed again per pod")
	}
	for _, name := range []string{"cp-worker-1", "vcluster-fake", "byo-gpu"} {
		if _, n := find(evs, usage.MetricPrivateNodeHours, "resource_id", name); n != 0 {
			t.Errorf("node %s must not be billed as a private node", name)
		}
	}
	if got, _ := find(evs, usage.MetricStorageGBHours, "sku", "fast-nvme"); !near(got, usage.Round(100.0/60)) {
		t.Errorf("tenant storage GiB-hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricLBHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("tenant load balancer hours = %v", got)
	}
}

func TestSharedTenantClusterIsNeverBilledTwice(t *testing.T) {
	// Shared tenant clusters see the control plane cluster's nodes (synced or
	// as placeholders); with a tenant API configured, nothing extra is billed.
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	host := []runtime.Object{h100, pod("a", "h100-1", withGPU("nvidia.com/gpu", 2))}
	tenant := tenantConn(node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true),
		node("h100-2", map[string]string{LabelFakeNode: "true"}, 8, true))
	withAPI, st := collectWith(t, Options{}, host, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	})
	without := collect(t, Options{}, host, latest())
	if st.PrivateNodes != 0 || len(withAPI[we]) != len(without[we]) {
		t.Fatalf("tenant API changed a shared tenant cluster's bill: %d vs %d events, %d private nodes", len(withAPI[we]), len(without[we]), st.PrivateNodes)
	}
	if got, _ := find(withAPI[we], usage.MetricGPUHours); !near(got, usage.Round(2.0/60)) {
		t.Errorf("GPU hours = %v", got)
	}
}

func TestPrivateNodeUsageModeBillsPods(t *testing.T) {
	gpuNode := node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	dcgm := pod("dcgm-exporter", "gpu-1", withRequests("1", "1Gi"))
	dcgm.Namespace = "kube-system"
	trainer := pod("trainer", "gpu-1", withGPU("nvidia.com/gpu", 2), withRequests("4", "8Gi"))
	trainer.Namespace = "research"
	tenant := tenantConn(gpuNode, dcgm, trainer)
	out, _ := collectWith(t, Options{PrivateNodeBilling: "usage", MeterByNamespace: true}, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	})
	evs := out[we]
	if got, _ := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "private_node", usage.DimNamespace, "research"); !near(got, usage.Round(2.0/60)) {
		t.Errorf("usage-mode GPU hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricCPUCoreHours, usage.DimBillingMode, "private_node"); !near(got, usage.Round(4.0/60)) {
		t.Errorf("usage-mode CPU core-hours = %v, want only the tenant's own pods (kube-system excluded)", got)
	}
	if _, n := find(evs, usage.MetricPrivateNodeHours); n != 0 {
		t.Error("usage mode must not also bill the whole node")
	}
}

func TestTenantAPIOutageIsReportedAndAddsNothing(t *testing.T) {
	host := []runtime.Object{node("cpu-1", nil, 0, true), pod("web", "cpu-1", withRequests("1", "1Gi"))}
	out, st := collectWith(t, Options{}, host, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	})
	if len(st.TenantAPIFailed) != 1 || st.TenantAPIFailed[0] != "vcluster-team-a-gpu" {
		t.Fatalf("failed tenant APIs = %v", st.TenantAPIFailed)
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("control plane cluster metering must continue during a tenant API outage, CPU = %v", got)
	}
}

func TestCollectTenantAPIOnlyMetersTenantSources(t *testing.T) {
	host := []runtime.Object{node("cpu-1", nil, 0, true), pod("web", "cpu-1", withRequests("1", "1Gi"))}
	tenant := tenantConn(node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4"}, 1, true))
	opts := Options{GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Region: "r", Now: func() time.Time { return we }}
	c := NewCollector(fake.NewSimpleClientset(host...), nil, opts)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	out, _, err := c.CollectTenantAPI(context.Background(), []Target{target("gpu", true)}, []Window{{Start: ws, End: we}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range out[we] {
		if e.Dim(usage.DimBillingMode) != "private_node" {
			t.Errorf("unexpected %s event from a tenant-API-only collection", e.Metric)
		}
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("GPU hours = %v", got)
	}
}

func TestExternalTenantClustersShareNoNodes(t *testing.T) {
	host := []runtime.Object{node("node-1", nil, 0, true)}
	tenant := tenantConn(node("node-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L40S"}, 4, true))
	tenant.External = true
	out, _ := collectWith(t, Options{}, host, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	})
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimBillingMode, "private_node"); !near(got, usage.Round(4.0/60)) {
		t.Errorf("standalone tenant cluster node with a colliding name: GPU hours = %v", got)
	}
}

func TestDeletedPodsAndNodesBilledUntilDeletion(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return we })
	// A job that started 10s into the window and was deleted 30s in, gone
	// from every list by the time the window closes.
	job := pod("short-job", "h100-1", withGPU("nvidia.com/gpu", 1), startedAt(ws.Add(10*time.Second)))
	job.UID = types.UID("job-1")
	g.RecordPod(job, ws.Add(30*time.Second))
	// A dedicated Auto Node scaled down 45s into the window.
	autoNode := node("auto-gpu-1", map[string]string{"vcluster.loft.sh/managed-by": ns, "nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	g.RecordNode(autoNode, ws.Add(45*time.Second))
	host := []runtime.Object{node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)}
	out, _ := collectWith(t, Options{}, host, latest(), func(c *Collector) { c.UseGraveyard(g) })
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimBillingMode, "shared"); !near(got, usage.Round(20.0/3600)) {
		t.Errorf("deleted job GPU hours = %v, want exactly the 20s it ran", got)
	}
	if got, _ := find(out[we], usage.MetricGPUHours, usage.DimBillingMode, "dedicated_node"); !near(got, usage.Round(8*45/3600.0)) {
		t.Errorf("scaled-down node GPU hours = %v, want 8 GPUs for 45s", got)
	}
	expired := NewGraveyard(time.Minute, func() time.Time { return we.Add(2 * time.Hour) })
	expired.RecordPod(job, ws.Add(30*time.Second))
	if len(expired.Pods("")) != 0 {
		t.Error("deleted pods past retention must be dropped")
	}
}

func TestGKETimeSharingLabels(t *testing.T) {
	l4 := node("gke-l4", map[string]string{"cloud.google.com/gke-accelerator": "nvidia-l4",
		"cloud.google.com/gke-gpu-sharing-strategy": "time-sharing", "cloud.google.com/gke-max-shared-clients-per-gpu": "4"}, 4, true)
	evs := collect(t, Options{}, []runtime.Object{l4, pod("infer", "gke-l4", withGPU("nvidia.com/gpu", 1))}, latest())[we]
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "nvidia-l4-timeslice-4"); !near(got, usage.Round(0.25/60)) {
		t.Errorf("GKE time-shared GPU hours = %v", got)
	}
}

// Seen on GCP: after a reboot or a spot restart, a Ready private node
// advertises no GPUs for minutes while the driver container rebuilds. The
// GPUs are installed (GPU Feature Discovery still labels them), so the time
// is GPU downtime, not usage and not silence.
func TestPrivateNodeGPUsNotAllocatableAreDowntime(t *testing.T) {
	loading := node("gpu-a100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB", "nvidia.com/gpu.count": "1", "node.kubernetes.io/instance-type": "a2-highgpu-1g"}, 0, true)
	mig := node("gpu-a100-2", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB-MIG-1g.5gb", "nvidia.com/gpu.count": "7", "nvidia.com/mig.strategy": "single"}, 0, true)
	cpu := node("cpu-1", nil, 0, true)
	out, _ := collectWith(t, Options{}, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenantConn(loading, mig, cpu)}})
	})
	evs := out[we]
	if _, n := find(evs, usage.MetricGPUHours); n != 0 {
		t.Fatal("GPUs that are not allocatable were billed")
	}
	for _, name := range []string{"gpu-a100-1", "gpu-a100-2"} {
		if got, n := find(evs, usage.MetricGPUDowntimeHours, "resource_id", name, "sku", "NVIDIA-A100-SXM4-40GB"); n != 1 || !near(got, usage.Round(1.0/60)) {
			t.Errorf("%s GPU downtime = %v (%d events), want one GPU for the window", name, got, n)
		}
	}
	if _, n := find(evs, usage.MetricGPUDowntimeHours, "resource_id", "cpu-1"); n != 0 {
		t.Error("a node without GPUs reported GPU downtime")
	}
	if got, _ := find(evs, usage.MetricPrivateNodeHours, "resource_id", "gpu-a100-1"); !near(got, usage.Round(1.0/60)) {
		t.Errorf("the Ready node itself is still billed: node hours = %v", got)
	}
}
