package metrics

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestReview2ExternalPauseBillsPodsBeforeThePause: PR #9 fixed the window a
// pause cuts in two under PRIVATE_NODE_BILLING=usage by saving the pods with
// the inventory vBilling's own pause (Freeze) stores. A pause vBilling did
// not make (vcluster pause, an operator) has no frozen inventory, so the
// collector falls back to the last inventory it read from the cluster's API
// (collectTenantAPI), which never includes pods. The paused window is then
// metered "successfully" without the pods (no gap is recorded, so no fill
// recovers them): their usage between the window start and the pause is
// lost.
//
// v0.3.1: billing stops only on evidence vBilling verified itself. Nothing
// confirms the pods of an external pause stopped (stopping the control
// plane leaves the kubelets on the cluster's own nodes running them), so
// they bill through the pause too, not only up to it: the whole window.
func TestReview2ExternalPauseBillsPodsBeforeThePause(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, nil)
	c.opts.PrivateNodeBilling = "usage"
	tg := target("gpu", true)
	conn := tenantConn(
		node("private-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 1, true),
		pod("infer", "private-gpu", withGPU("nvidia.com/gpu", 1), withRequests("2", "8Gi")),
	)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	// The previous window is collected while the cluster runs.
	if _, _, err := c.Collect(context.Background(), []Target{tg}, []Window{{Start: ws.Add(-time.Minute), End: ws, Latest: true}}); err != nil {
		t.Fatal(err)
	}
	// An operator runs `vcluster pause` 30 seconds into the next window.
	tg.Cluster.Paused, tg.Cluster.Ready = true, false
	tg.Paused = []pause.Span{{From: ws.Add(30 * time.Second), By: "external"}}
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	out, st, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TenantAPIFailed) != 0 {
		t.Logf("recorded as a gap: %v", st.TenantAPIFailed)
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(60.0/3600)) {
		t.Errorf("GPU hours = %v, want the 30 seconds before the external pause and the 30 after it (nothing confirms the pod stopped)", got)
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(2*60.0/3600)) {
		t.Errorf("CPU core-hours = %v, want 2 cores for the whole window, before and after the external pause", got)
	}
}

// TestReview2ClustersSharingANamespaceDoNotBillEachOthersCounters: the
// collector supports several clusters in one control plane cluster
// namespace (ownedFilter uses the syncer's managed-by label strictly), but
// the Prometheus scope of a cluster on shared nodes is only
// namespace="<namespace>". Every cluster in the namespace then reads every
// series in it: egress (on by default with PROMETHEUS_URL) and any counter:
// metric are billed once per cluster, to each cluster's tenant.
func TestReview2ClustersSharingANamespaceDoNotBillEachOthersCounters(t *testing.T) {
	// Cluster a's synced pod is the only one that sent anything: 10 tokens
	// per scrape, 4 scrapes in the window.
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total",
		labels:  map[string]string{"namespace": "shared", "pod": "engine-x-default-x-a", "model_name": "llama"},
		samples: counterSamples(ws.Add(-time.Minute), we, 10, time.Time{})}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	a := Target{Cluster: discovery.TenantCluster{Name: "a", Namespace: "shared"}, Tenant: "tenant-a"}
	b := Target{Cluster: discovery.TenantCluster{Name: "b", Namespace: "shared"}, Tenant: "tenant-b"}
	res := collectOK(t, p, []Target{a, b}, latest())
	byTenant := map[string]float64{}
	for _, e := range res[0].Events {
		byTenant[e.Tenant] += e.Quantity
	}
	if byTenant["tenant-b"] != 0 || byTenant["tenant-a"] != 40 {
		t.Fatalf("billed %v: want 40 tokens to tenant-a (cluster a's pod) and none to tenant-b", byTenant)
	}
}

// TestReview2NewlyVisibleCounterDoesNotBillItsHistory: a counter series
// without a baseline that first shows up after vBilling last read the
// metric for the cluster "counts from zero". That is exact for a pod that
// just started, but vBilling cannot tell a new process from one that only
// became visible: an engine Prometheus starts scraping (a ServiceMonitor
// added for a running vLLM), a label change that gives a series a new
// identity (the external label the "empty source" hint asks for, a
// relabel rule), or a series back after the 7-day prune. Its first sample
// then carries the process's whole lifetime count, and all of it is billed
// in one window.
func TestReview2NewlyVisibleCounterDoesNotBillItsHistory(t *testing.T) {
	// A vLLM engine that has served 5,000,000 tokens so far is scraped
	// from ws+15s on, adding 10 tokens per scrape.
	var samples []promSample
	for i := 1; i <= 8; i++ {
		samples = append(samples, promSample{T: ws.Add(time.Duration(i) * 15 * time.Second), V: 5_000_000 + float64(i-1)*10})
	}
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total",
		labels: map[string]string{"namespace": ns, "model_name": "llama"}, samples: samples}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "tenant-a"}

	// vBilling has been reading the metric for this cluster: no series yet.
	var billed float64
	for _, w := range windowsFrom(ws.Add(-2*time.Minute), 4) {
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			billed += e.Quantity
		}
	}
	// The counter grew by 70 between its first and last sample. Whatever
	// it did before its first sample is unknown to vBilling, and for a
	// process that only became visible it is history, not usage.
	if billed > 1000 {
		t.Fatalf("billed %v tokens for a counter that grew by 70 while vBilling could see it: its 5,000,000-token history was billed in one window", billed)
	}
}

// TestReview2TenantFillEventsNeverReuseCommittedIDs: when a tenant
// cluster's own API cannot be read, the window is committed with what the
// control plane cluster shows, and the tenant-API part is appended later by
// fillTenantGaps (spool.AppendIngest, source tenant-api-fill). Both parts
// build storage and load balancer events from the same template (tenant,
// metric, window, region, project, SKU = storage class, resource ID =
// cluster, dimensions), so with METER_CONTROL_PLANE=true (the control
// plane's own PVC and LoadBalancer Service are billed on the control plane
// cluster) a fill event gets the ID of an event already committed for that
// window. AppendIngest only deduplicates against ingested IDs, so the ledger
// takes it; Stripe (identifier), Metronome and Lago (transaction_id) then
// drop it as a duplicate, and the tenant's own volumes and load balancers
// are never invoiced for the outage (or for a sleeping instance, which the
// Platform proxy reports as a tenant API gap).
func TestReview2TenantFillEventsNeverReuseCommittedIDs(t *testing.T) {
	class := "fast-nvme"
	host := []runtime.Object{
		node("cp-worker-1", nil, 0, true),
		// The control plane's own volume and API Service, billed with
		// METER_CONTROL_PLANE=true.
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-gpu-0", Namespace: ns,
			Labels: map[string]string{"app": "vcluster", "release": "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: ns,
			Labels: map[string]string{"app": "vcluster", "release": "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}}}},
	}
	// Inside the cluster: a private GPU node, a 100Gi volume of the same
	// class and a LoadBalancer Service of its own.
	tenant := tenantConn(
		node("gpu-a100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}, 8, true),
		boundPVC("ckpt", "100Gi"),
		lbService("serve"),
	)
	opts := Options{GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Region: "ap-southeast-2",
		MeterControlPlane: true, Now: func() time.Time { return we.Add(2 * time.Second) }}
	c := NewCollector(fake.NewSimpleClientset(host...), nil, opts)
	windows := []Window{{Start: ws, End: we, Latest: true}}
	targets := []Target{target("gpu", true)}

	// The window is collected while the cluster's API is unreachable.
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	committed, st, err := c.Collect(context.Background(), targets, windows)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TenantAPIFailed) != 1 {
		t.Fatalf("tenant API failures = %v, want the cluster", st.TenantAPIFailed)
	}
	// Once it answers, the controller fills the window from the tenant API.
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	fill, _, err := c.CollectTenantAPI(context.Background(), targets, []Window{{Start: ws, End: we}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]usage.Event{}
	for _, e := range committed[we] {
		ids[e.ID] = e
	}
	if len(fill[we]) == 0 {
		t.Fatal("the fill metered nothing")
	}
	for _, e := range fill[we] {
		if prev, dup := ids[e.ID]; dup {
			t.Errorf("fill event %s (%s, %v) has the ID of the %s event already committed for the window (%v): billing platforms drop it as a duplicate",
				e.ID, e.Metric, e.Quantity, prev.Metric, prev.Quantity)
		}
	}
}
