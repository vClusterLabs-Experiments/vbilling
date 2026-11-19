package metrics

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TENANT_EXCLUDE_NAMESPACES is gone; config.RemovedSettings and the
// configuration page tell operators to "label a namespace
// vbilling.vcluster.com/exclude=true" instead. Nothing reads such a label
// inside a cluster: the collector only skips kube-system, and
// vbilling.vcluster.com/exclude is a discovery setting read from the
// cluster's StatefulSet, host namespace or VirtualClusterInstance, where it
// drops the whole cluster from metering. With PRIVATE_NODE_BILLING=usage,
// an install that excluded its GPU operator or monitoring namespaces bills
// every tenant for them after the upgrade, whatever it labels.
func TestReview3ExcludedNamespaceInsideAClusterIsNotBilled(t *testing.T) {
	t.Setenv("TENANT_EXCLUDE_NAMESPACES", "kube-system,gpu-operator") // a v0.3.0 value
	cfg := config.Load()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Region: "ap-southeast-2", PrivateNodeBilling: "usage", TenantExcludeNamespaces: cfg.TenantExcludeNamespaces,
		Now: func() time.Time { return we.Add(2 * time.Second) }})
	operator := pod("nvidia-driver-daemonset", "private-gpu", withRequests("4", "16Gi"))
	operator.Namespace = "gpu-operator"
	labeled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator",
		Labels: map[string]string{discovery.AnnotationPrefix + "exclude": "true"}}} // the advice, followed inside the cluster
	conn := tenantConn(node("private-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true), operator, labeled)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	out, _, err := c.Collect(context.Background(), []Target{target("gpu", true)}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); got > 0 {
		t.Errorf("billed the tenant %v CPU core-hours for a pod in excluded namespace gpu-operator", got)
	}
}

// process_start_time_seconds shows when the process that exposes a series
// started, not when its counter started. For a collector-style exporter
// (dcgm-exporter's energy counters, a standalone cAdvisor, node or CNI
// exporters) the counter is the hardware's or the kernel's: it keeps its
// value when the exporter restarts. Scraped through a PodMonitor, the
// series' instance (pod IP) and pod labels change with every restart (a
// GPU operator upgrade restarts every exporter), so each series reappears
// without a baseline, with its whole history, and the new exporter process
// started after vBilling's last read: d3e6116 counts it from zero. The
// energy the GPU used since the driver loaded is billed again in one window.
func TestReview3ExporterRestartDoesNotBillTheCounterAgain(t *testing.T) {
	energy := PromMetric{Code: "gpu_energy_kj", Counter: "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{{selector}}", Scale: 1.0 / 1e6, Dimensions: []string{"gpu"}}
	gpu := func(instance, pod string) map[string]string {
		return map[string]string{"namespace": ns, "gpu": "0", "UUID": "GPU-5f2c", "Hostname": "gpu-node-1",
			"job": "nvidia-dcgm-exporter", "instance": instance, "pod": pod}
	}
	// The GPU's cumulative energy since the driver loaded: 5e12 mJ, plus
	// 1e6 mJ (1 kJ) per 15s scrape. The exporter pod restarts at ws+60s and
	// is scraped again from ws+75s under its new pod IP.
	var before, after []promSample
	for i := 0; i <= 24; i++ {
		at := ws.Add(-3 * time.Minute).Add(time.Duration(i) * 15 * time.Second)
		v := 5e12 + float64(i)*1e6
		switch {
		case !at.After(ws.Add(time.Minute)):
			before = append(before, promSample{T: at, V: v})
		case !at.Before(ws.Add(75 * time.Second)):
			after = append(after, promSample{T: at, V: v})
		}
	}
	fp := &fakeProm{series: []fakeSeries{
		{name: "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", labels: gpu("10.0.0.7:9400", "nvidia-dcgm-exporter-x7k2p"), samples: before},
		{name: "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION", labels: gpu("10.0.0.8:9400", "nvidia-dcgm-exporter-q9w4d"), samples: after},
		// The new exporter process (client_golang exposes this by default).
		createdSeries("process_start_time_seconds", map[string]string{"namespace": ns, "job": "nvidia-dcgm-exporter", "instance": "10.0.0.8:9400"},
			ws.Add(62*time.Second), ws.Add(75*time.Second), we.Add(2*time.Minute)),
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, energy)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	var billed float64
	for _, w := range windowsFrom(ws.Add(-2*time.Minute), 5) { // through ws+3m
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			billed += e.Quantity
		}
	}
	// The GPU used about 1 kJ per scrape over 5 minutes: about 20 kJ.
	if billed > 100 {
		t.Fatalf("billed %.0f kJ for a GPU that used about 20 kJ while vBilling read it: the restarted exporter's counter was counted from zero (%.0f kJ of history)", billed, 5e12/1e6)
	}
}
