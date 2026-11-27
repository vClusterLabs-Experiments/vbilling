package metrics

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// owned returns the target of one owner of cluster gpu, billed to tenant
// except during elsewhere.
func owned(tenant string, elsewhere ...pause.Span) Target {
	tg := target("gpu", true)
	tg.Tenant = tenant
	tg.Elsewhere = elsewhere
	return tg
}

// splitAt returns the targets of a cluster that moved from tenant a to
// tenant b at the given moment.
func splitAt(a, b string, at time.Time) []Target {
	return []Target{owned(a, pause.Span{From: at}), owned(b, pause.Span{To: at})}
}

func byTenant(evs []usage.Event, metric string) map[string]float64 {
	out := map[string]float64{}
	for _, e := range evs {
		if e.Metric == metric {
			out[e.Tenant] += e.Quantity
		}
	}
	return out
}

func uniqueIDs(t *testing.T, evs []usage.Event) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range evs {
		if seen[e.ID] {
			t.Errorf("event ID %s appears twice (%s %s)", e.ID, e.Tenant, e.Metric)
		}
		seen[e.ID] = true
	}
}

var ownerObjects = func() []runtime.Object {
	class := "fast"
	return []runtime.Object{
		node("n1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}, 8, true),
		pod("train", "n1", withGPU("nvidia.com/gpu", 2), withRequests("2", "4Gi")),
		// Starts 30 seconds in, after the move: none of it is the old owner's.
		pod("eval", "n1", withGPU("nvidia.com/gpu", 1), startedAt(ws.Add(30*time.Second))),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "lb", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}}},
		},
	}
}

// A Project handed to another Tenant 15 seconds into a window: the old
// Tenant is billed the first 15 seconds of everything the cluster had, the
// new one the last 45, and the two add up to what the window bills unsplit.
// Before, the whole window went to one of them.
func TestOwnerChangeSplitsTheWindowAtTheSecond(t *testing.T) {
	moved := ws.Add(15 * time.Second)
	whole := collect(t, Options{}, ownerObjects(), latest(), owned("tenant-acme"))[we]
	split := collect(t, Options{}, ownerObjects(), latest(), splitAt("tenant-acme", "tenant-globex", moved)...)[we]
	uniqueIDs(t, split)
	for _, metric := range []string{usage.MetricGPUHours, usage.MetricCPUCoreHours, usage.MetricMemoryGBHours,
		usage.MetricStorageGBHours, usage.MetricLBHours, usage.MetricInstanceHours} {
		w := byTenant(whole, metric)["tenant-acme"]
		got := byTenant(split, metric)
		if w == 0 {
			t.Fatalf("%s: the unsplit window bills nothing", metric)
		}
		if math.Abs(got["tenant-acme"]+got["tenant-globex"]-w) > 1e-8 {
			t.Errorf("%s: %v + %v, want the unsplit %v", metric, got["tenant-acme"], got["tenant-globex"], w)
		}
	}
	// GPU-hours: train's 2 GPUs for the whole window, eval's 1 from 30s.
	gpu := byTenant(split, usage.MetricGPUHours)
	if want := usage.Round(2 * 15.0 / 3600); !near(gpu["tenant-acme"], want) {
		t.Errorf("old owner GPU-hours = %v, want %v (train's 2 GPUs for 15s, none of eval)", gpu["tenant-acme"], want)
	}
	if want := usage.Round((2*45.0 + 30.0) / 3600); !near(gpu["tenant-globex"], want) {
		t.Errorf("new owner GPU-hours = %v, want %v (train's 2 GPUs for 45s, eval's 1 for 30s)", gpu["tenant-globex"], want)
	}
	if got := byTenant(split, usage.MetricStorageGBHours); !near(got["tenant-acme"], usage.Round(30*15.0/3600)) || !near(got["tenant-globex"], usage.Round(30*45.0/3600)) {
		t.Errorf("storage GiB-hours = %v, want 15s and 45s of 30 GiB", got)
	}
}

// A move reverted inside one window bills three parts: the first and the
// last to the old owner, as one event per metric (the same tenant and
// window), the middle to the other.
func TestMoveRevertedInsideAWindowBillsThreeParts(t *testing.T) {
	out, back := ws.Add(10*time.Second), ws.Add(40*time.Second)
	targets := []Target{
		owned("tenant-acme", pause.Span{From: out, To: back}),
		owned("tenant-globex", pause.Span{To: out}, pause.Span{From: back}),
	}
	evs := collect(t, Options{}, ownerObjects(), latest(), targets...)[we]
	uniqueIDs(t, evs)
	cp := byTenant(evs, usage.MetricInstanceHours)
	if !near(cp["tenant-acme"], usage.Round(30.0/3600)) || !near(cp["tenant-globex"], usage.Round(30.0/3600)) {
		t.Errorf("control plane hours = %v, want 10s + 20s for acme, 30s for globex", cp)
	}
	if _, n := find(evs, usage.MetricInstanceHours); n != 2 {
		t.Errorf("control plane events = %d, want one per tenant", n)
	}
	gpu := byTenant(evs, usage.MetricGPUHours)
	// train: 2 GPUs all window; eval: 1 GPU from 30s (10s of it acme's, from 40s).
	if want := usage.Round((2*30.0 + 20.0) / 3600); !near(gpu["tenant-acme"], want) {
		t.Errorf("acme GPU-hours = %v, want %v", gpu["tenant-acme"], want)
	}
	if want := usage.Round((2*30.0 + 10.0) / 3600); !near(gpu["tenant-globex"], want) {
		t.Errorf("globex GPU-hours = %v, want %v", gpu["tenant-globex"], want)
	}
}

// failingTenantAPI fails the calls numbered in fail (from 1).
type failingTenantAPI struct {
	mu    sync.Mutex
	conn  *TenantConn
	fail  map[int]bool
	calls int
}

func (f *failingTenantAPI) Conn(context.Context, discovery.TenantCluster) (*TenantConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail[f.calls] {
		return nil, errors.New("connection refused")
	}
	return f.conn, nil
}

// A cluster's own API is read for each of its owners. When one read fails,
// none is kept: the window is filled later for both owners, never billed
// for one now and filled again for both.
func TestOwnAPIReadsAreKeptForEveryOwnerOrNone(t *testing.T) {
	gpuNode := node("gpu-a100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}, 8, true)
	moved := ws.Add(15 * time.Second)
	for _, tc := range []struct {
		name string
		fail map[int]bool
	}{{"both reads answer", nil}, {"the second read fails", map[int]bool{2: true}}, {"the first read fails", map[int]bool{1: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			api := &failingTenantAPI{conn: tenantConn(gpuNode), fail: tc.fail}
			c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
				Now: func() time.Time { return we.Add(2 * time.Second) }})
			c.UseTenantAPI(api)
			out, st, err := c.collect(context.Background(), splitAt("tenant-acme", "tenant-globex", moved), latest(), true)
			if err != nil {
				t.Fatal(err)
			}
			got := byTenant(out[we], usage.MetricPrivateNodeHours)
			if tc.fail == nil {
				if !near(got["tenant-acme"], usage.Round(15.0/3600)) || !near(got["tenant-globex"], usage.Round(45.0/3600)) || len(st.TenantAPIFailed) != 0 {
					t.Fatalf("node-hours %v, failed %v: want 15s and 45s", got, st.TenantAPIFailed)
				}
				if st.PrivateNodes != 1 {
					t.Errorf("private nodes = %d, want the node counted once", st.PrivateNodes)
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("billed %v although one owner's read failed: that part would be filled again later", got)
			}
			if len(st.TenantAPIFailed) != 1 || st.TenantAPIFailed[0] != "vcluster-team-a-gpu" {
				t.Errorf("failed = %v, want the cluster once", st.TenantAPIFailed)
			}
		})
	}
}

// A counter's increase goes to the owner at the time of the sample that
// shows it: a sample counts what happened up to its own time, so one taken
// at the moment of the move is the old owner's. The parts add up to the
// unsplit increase, and the counter's baseline moves on once.
func TestCounterIncreaseGoesToTheOwnerAtItsSampleTime(t *testing.T) {
	labels := map[string]string{"namespace": ns, "model": "a"}
	fp := &fakeProm{series: []fakeSeries{
		// +10 every 15 seconds: at :00, :15, :30, :45 of each minute.
		{name: "app_requests_total", labels: labels, samples: every(ws.Add(-10*time.Minute), ws.Add(10*time.Minute), 15*time.Second, 10)},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, requests)
	first := Window{Start: ws.Add(-time.Minute), End: ws}
	read := func(targets []Target, w Window) map[string]float64 {
		res := collectOK(t, p, targets, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		uniqueIDs(t, res[0].Events)
		return byTenant(res[0].Events, "requests")
	}
	if got := read([]Target{owned("tenant-acme")}, first); got["tenant-acme"] != 40 {
		t.Fatalf("the window before: %v", got)
	}
	// Moved 30 seconds in: the samples at :15 and :30 are acme's, :45 and
	// :00 globex's.
	got := read(splitAt("tenant-acme", "tenant-globex", ws.Add(30*time.Second)), Window{Start: ws, End: we, Latest: true})
	if got["tenant-acme"] != 20 || got["tenant-globex"] != 20 {
		t.Errorf("split window = %v, want 20 and 20", got)
	}
	// The baseline moved on once: the next window bills one minute.
	if got := read([]Target{owned("tenant-globex")}, Window{Start: we, End: we.Add(time.Minute), Latest: true}); got["tenant-globex"] != 40 || len(got) != 1 {
		t.Errorf("the window after = %v, want 40 for globex", got)
	}
}

// One owner's counter read fails, the other's succeeds: neither is kept,
// and the counter's baseline does not move, so the next window bills the
// whole increase. Without that, the successful read would move the
// baseline past what the failed one never billed.
func TestCounterReadsOfOwnersAreKeptTogether(t *testing.T) {
	fp := &fakeProm{series: []fakeSeries{
		{name: "app_requests_total", labels: map[string]string{"namespace": ns, "model": "a"}, samples: every(ws.Add(-10*time.Minute), ws.Add(10*time.Minute), 15*time.Second, 10)},
		{name: "app_requests_total", labels: map[string]string{"namespace": "team-b", "model": "a"}, samples: every(ws.Add(-10*time.Minute), ws.Add(10*time.Minute), 15*time.Second, 1)},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	_, p, h := promSourceFor(t, srv.URL, "", nil, requests)
	// Another cluster, read fine all along: the windows commit.
	web := Target{Cluster: discovery.TenantCluster{Name: "web", Namespace: "team-b", Ready: true, CreatedAt: ws.Add(-48 * time.Hour)}, Tenant: "tenant-initech"}
	commit := func(res []PromWindow) {
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	commit(collectOK(t, p, []Target{owned("tenant-acme"), web}, []Window{{Start: ws.Add(-time.Minute), End: ws}}))

	targets := append(splitAt("tenant-acme", "tenant-globex", ws.Add(30*time.Second)), web)
	// The new owner's settings point at another Prometheus, which is down.
	targets[1].Cluster.Annotations = map[string]string{discovery.AnnotationPrefix + "prometheus-url": down.URL}
	res := collectOK(t, p, targets, []Window{{Start: ws, End: we, Latest: true}})
	if got := byTenant(res[0].Events, "requests"); len(got) != 1 || got["tenant-initech"] != 4 {
		t.Errorf("billed %v; want only the other cluster's 4, as one owner's read failed", got)
	}
	if st := statusOf(h, "prometheus/requests/vcluster-team-a-gpu"); st != health.Failing {
		t.Errorf("source status = %q, want failing", st)
	}
	commit(res)
	next := collectOK(t, p, []Target{owned("tenant-globex")}, []Window{{Start: we, End: we.Add(time.Minute), Latest: true}})
	if got := byTenant(next[0].Events, "requests"); got["tenant-globex"] != 80 {
		t.Errorf("the next window = %v, want both minutes (80): the baseline must not have moved", got)
	}
}

// A query, an estimate over the whole window, is prorated by each owner's
// share of the window, and so is GPU utilization.
func TestQueriesAreProratedBetweenOwners(t *testing.T) {
	prom, _ := promServer(t, nil, func(q string) (string, map[string]string) {
		switch {
		case strings.Contains(q, "power_watts"):
			return "600", map[string]string{}
		case strings.Contains(q, "avg_over_time(DCGM_FI_DEV_GPU_UTIL"):
			return "50", map[string]string{"modelName": "NVIDIA A100-SXM4-80GB"}
		case strings.Contains(q, "count by"):
			return "8", map[string]string{"modelName": "NVIDIA A100-SXM4-80GB"}
		}
		return "", nil
	})
	power := PromMetric{Code: "power_test_watts", Query: "sum(power_watts{{selector}})"}
	evs := collect(t, Options{PrometheusURL: prom.URL, EgressQuery: "none", PromMetrics: []PromMetric{power}}, nil, latest(),
		splitAt("tenant-acme", "tenant-globex", ws.Add(15*time.Second))...)[we]
	uniqueIDs(t, evs)
	if got := byTenant(evs, power.Code); !near(got["tenant-acme"], 150) || !near(got["tenant-globex"], 450) {
		t.Errorf("query = %v, want 150 and 450 (600 prorated 15s and 45s)", got)
	}
	if got := byTenant(evs, usage.MetricGPUUtilization); !near(got["tenant-acme"], usage.Round(50*15.0/3600)) || !near(got["tenant-globex"], usage.Round(50*45.0/3600)) {
		t.Errorf("utilization-hours = %v, want 50%% for 15s and 45s", got)
	}
}

func statusOf(h *health.Registry, name string) health.Status {
	for _, s := range h.List() {
		if s.Name == name {
			return s.Status
		}
	}
	return ""
}
