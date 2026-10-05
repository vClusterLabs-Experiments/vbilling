package metrics

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var ws = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
var we = ws.Add(time.Minute)

const ns = "team-a"

func node(name string, labels map[string]string, gpus int64, ready bool, mods ...func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, CreationTimestamp: metav1.NewTime(ws.Add(-24 * time.Hour))},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("64"),
				corev1.ResourceMemory: resource.MustParse("512Gi"),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	if gpus > 0 {
		n.Status.Capacity["nvidia.com/gpu"] = *resource.NewQuantity(gpus, resource.DecimalSI)
	}
	if !ready {
		n.Status.Conditions[0].Status = corev1.ConditionFalse
	}
	for _, m := range mods {
		m(n)
	}
	return n
}

type podOpt func(*corev1.Pod)

func pod(name, nodeName string, opts ...podOpt) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu", LabelVirtualNamespace: "training"}},
		Spec: corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "main", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(ws.Add(-time.Hour))}}}}},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withGPU(res string, n int64) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Limits[corev1.ResourceName(res)] = *resource.NewQuantity(n, resource.DecimalSI)
	}
}

func withRequests(cpu, mem string) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpu)
		p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse(mem)
	}
}

func startedAt(t time.Time) podOpt {
	return func(p *corev1.Pod) {
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(t)}}
	}
}

func finished(start, end time.Time) podOpt {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			StartedAt: metav1.NewTime(start), FinishedAt: metav1.NewTime(end)}}
	}
}

func labels(kv ...string) podOpt {
	return func(p *corev1.Pod) {
		for i := 0; i < len(kv); i += 2 {
			p.Labels[kv[i]] = kv[i+1]
		}
	}
}

func target(name string, ready bool) Target {
	return Target{Cluster: discovery.TenantCluster{Name: name, Namespace: ns, Ready: ready, CreatedAt: ws.Add(-48 * time.Hour)}, Tenant: "acme", Project: "research"}
}

func collect(t *testing.T, opts Options, objs []runtime.Object, windows []Window, targets ...Target) map[time.Time][]usage.Event {
	t.Helper()
	if opts.Region == "" {
		opts.Region = "ap-southeast-2"
	}
	if opts.GPUResources == nil {
		opts.GPUResources = []string{"nvidia.com/gpu"}
	}
	if opts.Basis == "" {
		opts.Basis = "requests"
	}
	opts.Now = func() time.Time { return we.Add(2 * time.Second) }
	c := NewCollector(fake.NewSimpleClientset(objs...), nil, opts)
	if len(targets) == 0 {
		targets = []Target{target("gpu", true)}
	}
	out, _, err := c.Collect(context.Background(), targets, windows)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func latest() []Window { return []Window{{Start: ws, End: we, Latest: true}} }

// find returns the summed quantity of events matching metric and the
// given dimension/field filters.
func find(evs []usage.Event, metric string, filters ...string) (float64, int) {
	var sum float64
	n := 0
outer:
	for _, e := range evs {
		if e.Metric != metric {
			continue
		}
		for i := 0; i < len(filters); i += 2 {
			k, v := filters[i], filters[i+1]
			got := e.Dim(k)
			switch k {
			case "sku":
				got = e.SKU
			case "resource_id":
				got = e.ResourceID
			}
			if got != v {
				continue outer
			}
		}
		sum += e.Quantity
		n++
	}
	return sum, n
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-8 }

func TestGPUSecondsPrecisionAndLifecycle(t *testing.T) {
	h100 := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA H100 80GB HBM3", "topology.kubernetes.io/zone": "syd-1a"}, 8, true)
	objs := []runtime.Object{h100,
		pod("full-window", "h100-1", withGPU("nvidia.com/gpu", 2)),
		pod("started-mid", "h100-1", withGPU("nvidia.com/gpu", 1), startedAt(ws.Add(20*time.Second))),
		pod("done-mid", "h100-1", withGPU("nvidia.com/gpu", 1), finished(ws.Add(-time.Hour), ws.Add(30*time.Second))),
		pod("done-before", "h100-1", withGPU("nvidia.com/gpu", 4), finished(ws.Add(-time.Hour), ws.Add(-time.Second))),
		pod("pulling-image", "h100-1", withGPU("nvidia.com/gpu", 8), func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		}),
	}
	evs := collect(t, Options{}, objs, latest())[we]
	got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H100-80GB-HBM3", usage.DimBillingMode, "shared")
	want := (2*60 + 1*40 + 1*30) / 3600.0 // GPU-seconds -> hours; nothing for unstarted or finished pods
	if n != 1 || !near(got, usage.Round(want)) {
		t.Fatalf("GPU hours = %v (%d events), want %v", got, n, want)
	}
	for _, e := range evs {
		if e.Metric == usage.MetricGPUHours {
			if e.Region != "ap-southeast-2" || e.Dim(usage.DimZone) != "syd-1a" || e.Dim(usage.DimCapacityType) != usage.CapacityOnDemand ||
				e.Tenant != "acme" || e.Project != "research" || e.Dim(usage.DimTenantCluster) != "vcluster-team-a-gpu" {
				t.Fatalf("GPU event attribution: %+v", e)
			}
		}
	}
}

func TestFractionalGPUs(t *testing.T) {
	a100 := node("a100-mig", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}, 0, true)
	shared := node("l40s-ts", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L40S", "nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing"}, 4, true)
	objs := []runtime.Object{a100, shared,
		pod("mig", "a100-mig", withGPU("nvidia.com/mig-1g.10gb", 2)),
		pod("big-mig", "a100-mig", withGPU("nvidia.com/mig-3g.40gb", 1)),
		pod("ts", "l40s-ts", withGPU("nvidia.com/gpu", 1)),
	}
	evs := collect(t, Options{}, objs, latest())[we]
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-A100-SXM4-80GB-mig-1g.10gb", usage.DimGPUProfile, "mig-1g.10gb"); !near(got, usage.Round(2*(1.0/7)/60)) {
		t.Errorf("1g MIG GPU-equivalent hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-A100-SXM4-80GB-mig-3g.40gb"); !near(got, usage.Round((3.0/7)/60)) {
		t.Errorf("3g MIG GPU-equivalent hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-L40S-timeslice-4", usage.DimGPUProfile, "timeslice-4"); !near(got, usage.Round(0.25/60)) {
		t.Errorf("time-sliced GPU hours = %v", got)
	}
}

func TestUnhealthyNodeTimeIsCreditedNotBilled(t *testing.T) {
	down := node("h100-down", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, false, func(n *corev1.Node) {
		n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ws.Add(15 * time.Second))
	})
	tainted := node("h100-xid", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true, func(n *corev1.Node) {
		n.Spec.Taints = []corev1.Taint{{Key: "nvidia.com/gpu.unhealthy", Effect: corev1.TaintEffectNoSchedule}} // no timeAdded: whole window
	})
	objs := []runtime.Object{down, tainted,
		pod("a", "h100-down", withGPU("nvidia.com/gpu", 4)),
		pod("b", "h100-xid", withGPU("nvidia.com/gpu", 2)),
	}
	opts := Options{UnhealthyTaints: []string{"node.kubernetes.io/not-ready", "nvidia.com/gpu.unhealthy"}}
	evs := collect(t, opts, objs, latest())[we]
	if got, _ := find(evs, usage.MetricGPUHours); !near(got, usage.Round(4*15/3600.0)) {
		t.Errorf("billable GPU hours = %v, want only the 15s before the node went NotReady", got)
	}
	dt, _ := find(evs, usage.MetricGPUDowntimeHours)
	if !near(dt, usage.Round(4*45/3600.0)+usage.Round(2*60/3600.0)) {
		t.Errorf("downtime GPU hours = %v", dt)
	}
}

func TestControlPlaneAndOtherTenantClustersExcluded(t *testing.T) {
	n := node("cpu-1", nil, 0, true)
	objs := []runtime.Object{n,
		pod("vcluster-0", "cpu-1", withRequests("2", "4Gi"), labels("app", "vcluster", "release", "gpu"), func(p *corev1.Pod) { delete(p.Labels, LabelManagedBy) }),
		pod("mine", "cpu-1", withRequests("1", "2Gi")),
		pod("neighbour", "cpu-1", withRequests("8", "16Gi"), labels(LabelManagedBy, "other")),
	}
	evs := collect(t, Options{}, objs, latest())[we]
	if got, _ := find(evs, usage.MetricCPUCoreHours); !near(got, usage.Round(1.0/60)) {
		t.Fatalf("CPU core-hours = %v, want only the tenant's own synced pod", got)
	}
	if got, _ := find(evs, usage.MetricMemoryGBHours); !near(got, usage.Round(2.0/60)) {
		t.Fatalf("memory GiB-hours = %v", got)
	}
	evs = collect(t, Options{MeterControlPlane: true}, objs, latest())[we]
	if got, _ := find(evs, usage.MetricCPUCoreHours); !near(got, usage.Round(3.0/60)) {
		t.Fatalf("with METER_CONTROL_PLANE CPU core-hours = %v", got)
	}
}

func TestDedicatedNodesBilledWholeWithoutDoubleCounting(t *testing.T) {
	dedicated := node("bm-01", map[string]string{"vcluster.loft.sh/managed-by": ns, "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
		"node.kubernetes.io/instance-type": "bm.gpu.h100.8", "karpenter.sh/capacity-type": "reserved"}, 8, true)
	objs := []runtime.Object{dedicated,
		pod("on-dedicated", "bm-01", withGPU("nvidia.com/gpu", 8), withRequests("32", "256Gi")),
	}
	evs := collect(t, Options{}, objs, latest())[we]
	if got, n := find(evs, usage.MetricPrivateNodeHours, "resource_id", "bm-01", "sku", "bm.gpu.h100.8", usage.DimCapacityType, usage.CapacityReserved); n != 1 || !near(got, usage.Round(1.0/60)) {
		t.Fatalf("node hours = %v (%d)", got, n)
	}
	if got, _ := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "dedicated_node"); !near(got, usage.Round(8.0/60)) {
		t.Fatalf("dedicated GPU hours = %v", got)
	}
	if _, n := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "shared"); n != 0 {
		t.Fatal("pod on the tenant's dedicated node was billed again as shared usage")
	}
	if got, _ := find(evs, usage.MetricCPUCoreHours, usage.DimBillingMode, "dedicated_node"); !near(got, usage.Round(64.0/60)) {
		t.Fatalf("dedicated CPU capacity hours = %v", got)
	}
}

func TestStorageLoadBalancersAndControlPlaneHours(t *testing.T) {
	fast := "fast-nvme"
	objs := []runtime.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &fast},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-gpu-0", Namespace: ns, Labels: map[string]string{"app": "vcluster", "release": "gpu"}},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}, Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.10"}}}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "no-ip-yet", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
	}
	evs := collect(t, Options{}, objs, latest())[we]
	if got, _ := find(evs, usage.MetricStorageGBHours, "sku", "fast-nvme"); !near(got, usage.Round(100.0/60)) {
		t.Errorf("storage GiB-hours = %v", got)
	}
	if _, n := find(evs, usage.MetricStorageGBHours); n != 1 {
		t.Errorf("storage events = %d (pending and control-plane PVCs must be skipped)", n)
	}
	if got, _ := find(evs, usage.MetricLBHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("LB hours = %v (only LBs with an address bill)", got)
	}
	if got, _ := find(evs, usage.MetricInstanceHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("instance hours = %v", got)
	}
	evs = collect(t, Options{}, objs, latest(), target("gpu", false))[we]
	if _, n := find(evs, usage.MetricInstanceHours); n != 0 {
		t.Error("a tenant cluster that is not ready (provisioning or asleep) must not bill instance hours")
	}
}

func TestSpotCapacityAndNamespaceAttribution(t *testing.T) {
	spot := node("spot-1", map[string]string{"cloud.google.com/gke-spot": "true", "nvidia.com/gpu.product": "NVIDIA-L4"}, 1, true)
	pre := node("dev-1", map[string]string{"vbilling.vcluster.com/capacity-type": "preemptible", "nvidia.com/gpu.product": "NVIDIA-L4"}, 1, true)
	objs := []runtime.Object{spot, pre,
		pod("a", "spot-1", withGPU("nvidia.com/gpu", 1)),
		pod("b", "dev-1", withGPU("nvidia.com/gpu", 1), labels(LabelVirtualNamespace, "inference")),
	}
	evs := collect(t, Options{MeterByNamespace: true, CapacityTypeLabel: "vbilling.vcluster.com/capacity-type"}, objs, latest())[we]
	if _, n := find(evs, usage.MetricGPUHours, usage.DimCapacityType, usage.CapacitySpot, usage.DimNamespace, "training"); n != 1 {
		t.Error("spot GPU event with virtual namespace missing")
	}
	if _, n := find(evs, usage.MetricGPUHours, usage.DimCapacityType, usage.CapacityPreemptible, usage.DimNamespace, "inference"); n != 1 {
		t.Error("preemptible GPU event missing")
	}
}

func TestBackfillAndDeterminism(t *testing.T) {
	n := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	pm := &metricsv1beta1.PodMetrics{ObjectMeta: metav1.ObjectMeta{Name: "train", Namespace: ns},
		Containers: []metricsv1beta1.ContainerMetrics{{Name: "main", Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("8Gi")}}}}
	objs := []runtime.Object{n, pod("train", "h100-1", withGPU("nvidia.com/gpu", 1), withRequests("1", "1Gi"))}
	windows := []Window{{Start: ws.Add(-2 * time.Minute), End: ws.Add(-time.Minute)}, {Start: ws.Add(-time.Minute), End: ws}, {Start: ws, End: we, Latest: true}}

	run := func() map[time.Time][]usage.Event {
		mc := metricsfake.NewSimpleClientset()
		// The tracker would guess "podmetricses"; the client lists "pods".
		if err := mc.Tracker().Create(schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}, pm, ns); err != nil {
			t.Fatal(err)
		}
		c := NewCollector(fake.NewSimpleClientset(objs...), mc, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "usage",
			Now: func() time.Time { return we.Add(time.Second) }})
		out, _, err := c.Collect(context.Background(), []Target{target("gpu", true)}, windows)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	for i, w := range windows {
		evs := out[w.End]
		if got, _ := find(evs, usage.MetricGPUHours); !near(got, usage.Round(1.0/60)) {
			t.Errorf("window %d GPU hours = %v", i, got)
		}
		_, cpuEvents := find(evs, usage.MetricCPUCoreHours)
		if w.Latest && cpuEvents != 1 {
			t.Error("usage-based CPU missing from the live window")
		}
		if !w.Latest && cpuEvents != 0 {
			t.Error("usage-based CPU must not be invented for backfilled windows")
		}
		for _, e := range evs {
			if !w.Latest && e.Properties["backfilled"] != true {
				t.Errorf("backfilled event not flagged: %+v", e)
			}
		}
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(3.0/60)) {
		t.Errorf("usage CPU core-hours = %v, want metrics-server value", got)
	}
	// Every event is stamped with its own window and IDs never repeat across windows.
	seen := map[string]bool{}
	for _, w := range windows {
		for _, e := range out[w.End] {
			if !e.WindowStart.Equal(w.Start) || !e.WindowEnd.Equal(w.End) {
				t.Fatalf("event %s/%s has window [%s, %s), want [%s, %s)", e.Tenant, e.Metric, e.WindowStart, e.WindowEnd, w.Start, w.End)
			}
			if seen[e.ID] {
				t.Fatalf("event ID %s repeats across windows", e.ID)
			}
			seen[e.ID] = true
		}
	}
	again := run()
	for _, w := range windows {
		a, b := out[w.End], again[w.End]
		if len(a) != len(b) {
			t.Fatal("non-deterministic event count")
		}
		for i := range a {
			if a[i].ID != b[i].ID {
				t.Fatalf("event IDs differ between identical runs: %s vs %s", a[i].ID, b[i].ID)
			}
		}
	}
}

func TestPrometheusEgressAndUtilizationAtWindowEnd(t *testing.T) {
	var queries []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		if r.URL.Query().Get("time") != strconv.FormatInt(we.Unix(), 10) {
			t.Errorf("query not evaluated at window end: %s", r.URL.RawQuery)
		}
		val := "0"
		labels := map[string]string{}
		switch {
		case strings.Contains(q, "container_network_transmit_bytes_total"):
			val = strconv.Itoa(3 * gib)
		case strings.Contains(q, "avg_over_time(DCGM_FI_DEV_GPU_UTIL"):
			val, labels = "75", map[string]string{"modelName": "NVIDIA H100 80GB HBM3"}
		case strings.Contains(q, "count by"):
			val, labels = "2", map[string]string{"modelName": "NVIDIA H100 80GB HBM3"}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector",
			"result": []map[string]any{{"metric": labels, "value": []any{float64(we.Unix()), val}}}}})
	}))
	defer prom.Close()
	evs := collect(t, Options{PrometheusURL: prom.URL}, nil, latest())[we]
	if got, _ := find(evs, usage.MetricNetworkEgressGB); !near(got, 3) {
		t.Errorf("egress GiB = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUUtilization, usage.DimGPUType, "NVIDIA-H100-80GB-HBM3"); !near(got, usage.Round(75.0/60)) {
		t.Errorf("utilization hours = %v", got)
	}
	if !strings.Contains(queries[0], `increase(container_network_transmit_bytes_total{namespace="team-a"}[60s])`) {
		t.Errorf("egress query = %s", queries[0])
	}
}

func TestMigFraction(t *testing.T) {
	cases := []struct {
		profile, gpu string
		want         float64
	}{{"1g.10gb", "NVIDIA-H100", 1.0 / 7}, {"7g.80gb", "NVIDIA-H100", 1}, {"1g.6gb", "NVIDIA-A30", 0.25}, {"2g.20gb+me", "NVIDIA-A100", 2.0 / 7}, {"weird", "x", 1}}
	for _, c := range cases {
		if got := migFraction(c.profile, c.gpu); !near(got, c.want) {
			t.Errorf("migFraction(%s,%s) = %v, want %v", c.profile, c.gpu, got, c.want)
		}
	}
}

func TestMIGSingleStrategyGKEPartitionsAndSharedProducts(t *testing.T) {
	single := node("a100-single", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-40GB-MIG-1g.5gb", "nvidia.com/mig.strategy": "single"}, 56, true)
	gke := node("gke-a100", map[string]string{"cloud.google.com/gke-accelerator": "nvidia-tesla-a100", "cloud.google.com/gke-gpu-partition-size": "3g.20gb"}, 16, true)
	shared := node("t4-shared", map[string]string{"nvidia.com/gpu.product": "Tesla-T4-SHARED", "nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing"}, 16, true)
	objs := []runtime.Object{single, gke, shared,
		pod("slices", "a100-single", withGPU("nvidia.com/gpu", 2)),
		pod("part", "gke-a100", withGPU("nvidia.com/gpu", 1)),
		pod("ts", "t4-shared", withGPU("nvidia.com/gpu", 1)),
	}
	evs := collect(t, Options{}, objs, latest())[we]
	if got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-A100-SXM4-40GB-mig-1g.5gb", usage.DimGPUType, "NVIDIA-A100-SXM4-40GB", usage.DimGPUProfile, "mig-1g.5gb"); n != 1 || !near(got, usage.Round(2*(1.0/7)/60)) {
		t.Errorf("MIG single strategy: GPU-equivalent hours = %v (%d events), want two 1/7 slices", got, n)
	}
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "nvidia-tesla-a100-mig-3g.20gb", usage.DimGPUProfile, "mig-3g.20gb"); !near(got, usage.Round((3.0/7)/60)) {
		t.Errorf("GKE GPU partition: GPU-equivalent hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "Tesla-T4-timeslice-4", usage.DimGPUType, "Tesla-T4"); !near(got, usage.Round(0.25/60)) {
		t.Errorf("-SHARED product label: time-sliced GPU hours = %v", got)
	}
}

func TestDedicatedNodesBilledInWholeGPUs(t *testing.T) {
	dedicated := func(extra map[string]string) map[string]string {
		l := map[string]string{"vcluster.loft.sh/managed-by": ns}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	capacity := func(res string, n int64) func(*corev1.Node) {
		return func(nd *corev1.Node) {
			nd.Status.Capacity[corev1.ResourceName(res)] = *resource.NewQuantity(n, resource.DecimalSI)
		}
	}
	cases := []struct {
		name string
		node *corev1.Node
		want float64
	}{
		{"time-sliced, 8 GPUs x 4 replicas", node("ts", dedicated(map[string]string{"nvidia.com/gpu.product": "NVIDIA-L40S-SHARED", "nvidia.com/gpu.replicas": "4"}), 32, true), 8},
		{"MIG single, 8 GPUs x 7 slices", node("mig", dedicated(map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3-MIG-1g.10gb"}), 56, true), 8},
		{"MIG mixed, 6 whole + 2 carved", node("mixed", dedicated(map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}), 6, true,
			capacity("nvidia.com/mig-3g.40gb", 4), capacity("nvidia.com/mig-1g.10gb", 2)), 8},
		{"renamed shared resource", node("renamed", dedicated(map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4", "nvidia.com/gpu.replicas": "2"}), 0, true,
			capacity("nvidia.com/gpu.shared", 8)), 4},
	}
	for _, c := range cases {
		evs := collect(t, Options{}, []runtime.Object{c.node}, latest())[we]
		if got, n := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "dedicated_node"); n != 1 || !near(got, usage.Round(c.want/60)) {
			t.Errorf("%s: dedicated GPU hours = %v (%d events), want %v", c.name, got, n, usage.Round(c.want/60))
		}
	}
}

// promServer answers instant queries through respond and records them.
func promServer(t *testing.T, check func(*http.Request) bool, respond func(q string) (string, map[string]string)) (*httptest.Server, *[]string) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil && !check(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		val, labels := respond(q)
		result := []map[string]any{}
		if val != "" {
			result = append(result, map[string]any{"metric": labels, "value": []any{float64(we.Unix()), val}})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestPrometheusAuthAndExportedNamespaceLabels(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authed := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer s3cret" && r.Header.Get("X-Scope-OrgID") == "tenant-1"
	}
	// kube-prometheus-stack renames the exporter's namespace label.
	prom, queries := promServer(t, authed, func(q string) (string, map[string]string) {
		switch {
		case strings.Contains(q, "avg_over_time(DCGM_FI_DEV_GPU_UTIL") && strings.Contains(q, `exported_namespace="team-a"`):
			return "50", map[string]string{"modelName": "NVIDIA H100 80GB HBM3"}
		case strings.Contains(q, "count by (modelName)"):
			return "4", map[string]string{"modelName": "NVIDIA H100 80GB HBM3"}
		}
		return "", nil
	})
	opts := Options{PrometheusURL: prom.URL, PromHeaders: map[string]string{"X-Scope-OrgID": "tenant-1"}, PromTokenFile: tokenFile}
	evs := collect(t, opts, nil, latest())[we]
	if len(*queries) == 0 {
		t.Fatal("no authenticated queries reached Prometheus")
	}
	got, n := find(evs, usage.MetricGPUUtilization, usage.DimGPUType, "NVIDIA-H100-80GB-HBM3")
	if n != 1 || !near(got, usage.Round(50.0/60)) {
		t.Fatalf("utilization hours = %v (%d events)", got, n)
	}
	for _, e := range evs {
		if e.Metric == usage.MetricGPUUtilization && e.Properties["gpu_devices"] != 4.0 {
			t.Errorf("gpu_devices = %v", e.Properties["gpu_devices"])
		}
	}

	bad := newPrometheusClient(prom.URL, nil, "")
	if _, err := bad.Query(context.Background(), "up", we); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("unauthenticated query error = %v", err)
	}
	basic, _ := promServer(t, func(r *http.Request) bool { u, p, ok := r.BasicAuth(); return ok && u == "vb" && p == "pw" }, func(string) (string, map[string]string) { return "1", nil })
	withCreds := strings.Replace(basic.URL, "http://", "http://vb:pw@", 1)
	if _, err := newPrometheusClient(withCreds, nil, "").Query(context.Background(), "up", we); err != nil {
		t.Errorf("basic auth from the URL: %v", err)
	}
}

func TestCustomAndDisabledGPUUtilQuery(t *testing.T) {
	prom, queries := promServer(t, nil, func(q string) (string, map[string]string) {
		if strings.Contains(q, "amd_gpu_busy") {
			return "30", map[string]string{"gpu_type": "AMD Instinct MI300X"}
		}
		return "", nil
	})
	custom := `avg by (gpu_type) (avg_over_time(amd_gpu_busy{namespace="{{namespace}}"}[{{window}}]))`
	evs := collect(t, Options{PrometheusURL: prom.URL, GPUUtilQuery: custom}, nil, latest())[we]
	if got, _ := find(evs, usage.MetricGPUUtilization, usage.DimGPUType, "AMD-Instinct-MI300X"); !near(got, usage.Round(30.0/60)) {
		t.Errorf("custom utilization hours = %v", got)
	}
	for _, q := range *queries {
		if strings.Contains(q, "DCGM_FI_DEV_GPU_UTIL") {
			t.Errorf("default DCGM query ran alongside a custom one: %s", q)
		}
	}
	*queries = nil
	collect(t, Options{PrometheusURL: prom.URL, GPUUtilQuery: "none"}, nil, latest())
	if len(*queries) != 1 || !strings.Contains((*queries)[0], "container_network_transmit_bytes_total") {
		t.Errorf("with GPU_UTIL_QUERY=none only egress should run, got %q", *queries)
	}
}
