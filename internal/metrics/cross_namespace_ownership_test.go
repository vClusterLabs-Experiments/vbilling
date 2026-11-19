package metrics

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestObjectsAndCountersKeepTheirOwnerAcrossControlPlaneNamespaces(t *testing.T) {
	alpha := Target{Cluster: discovery.TenantCluster{Name: "alpha", Namespace: "a", Ready: true}, Tenant: "acme"}
	beta := Target{Cluster: discovery.TenantCluster{Name: "beta", Namespace: "b", Ready: true}, Tenant: "globex"}
	engine := func(name, ns, cluster string) *corev1.Pod {
		return pod(name, "gpu-node", inNamespace(ns), labels(LabelManagedBy, cluster), withGPU("nvidia.com/gpu", 1))
	}
	series := func(ns, pod string, inc float64) fakeSeries {
		return fakeSeries{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "pod": pod, "model_name": "llama", "metering_source": pod + "-claimed"},
			samples: counterSamples(ws.Add(-30*time.Minute), we.Add(time.Minute), inc, time.Time{})}
	}
	fp := &fakeProm{series: []fakeSeries{series("a", "same-name", 2), series("b", "same-name", 5), series("b", "foreign", 3)}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	kube := fake.NewSimpleClientset(
		node("gpu-node", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "a"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "b"}},
		engine("same-name", "a", "alpha"), engine("same-name", "b", "beta"), engine("foreign", "b", "alpha"),
	)
	metric := tokens
	metric.Dimensions = append(append([]string{}, tokens.Dimensions...), "metering_source")
	c := NewCollector(kube, nil, Options{Region: "r", MeteringSource: "real-host", GPUResources: []string{"nvidia.com/gpu"}, PrometheusURL: srv.URL, EgressQuery: "none", GPUUtilQuery: "none", PromMetrics: []PromMetric{metric},
		Now: func() time.Time { return we.Add(5 * time.Second) }})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p, err := c.NewPromSource(PromOptions{Health: h})
	if err != nil {
		t.Fatal(err)
	}
	targets := []Target{alpha, beta}
	out, _, err := c.Collect(context.Background(), targets, latest())
	if err != nil {
		t.Fatal(err)
	}
	gpus := map[string]float64{}
	for _, e := range out[we] {
		if e.Metric == usage.MetricGPUHours {
			gpus[e.Tenant] += e.Quantity
		}
	}
	for tenant, want := range map[string]float64{"acme": usage.Round(2.0 / 60), "globex": usage.Round(1.0 / 60)} {
		if got := gpus[tenant]; !near(got, want) {
			t.Fatalf("tenant %s GPU-hours %v, want %v", tenant, got, want)
		}
	}
	res, err := p.Collect(context.Background(), targets, latest())
	if err != nil {
		t.Fatal(err)
	}
	totals := map[string]float64{}
	ids := map[string]bool{}
	for _, e := range res[0].Events {
		if ids[e.ID] || e.Dimensions[usage.DimMeteringSource] != "real-host" {
			t.Fatalf("series partitioned or replaced collector identity: %+v", e)
		}
		ids[e.ID] = true
		totals[e.Tenant] += e.Quantity
	}
	if totals["acme"] != 20 || totals["globex"] != 20 {
		t.Fatalf("cross-namespace token totals: %v, want 20 each", totals)
	}
	if source, ok := sourceNamed(h, "prometheus/tokens_out/vcluster-b-beta"); !ok || source.Status != health.Failing {
		t.Fatalf("mixed namespace is not reported: %+v", source)
	}
}

func TestMixedNamespaceRequiresUnambiguousPodAndNamespace(t *testing.T) {
	target := Target{Cluster: discovery.TenantCluster{Name: "alpha", Namespace: "a"}}
	v := &syncedView{contaminated: map[string]bool{"b": true},
		podOwners:     map[string]string{"b\x00mine": target.Cluster.ExternalID(), "b\x00other": "another-cluster", "b\x00reused": ""},
		defaultOwners: map[string]string{"b": "another-cluster"}}
	filter, mixed := v.seriesFilter(target, []string{"b"})
	if !mixed || filter == nil {
		t.Fatal("mixed namespace has no ownership filter")
	}
	for _, labels := range []map[string]string{{"pod": "mine"}, {"namespace": "b"}, {"namespace": "b", "pod": "reused"}, {"namespace": "b", "pod": "unknown"}} {
		if _, err := filter(labels); err == nil {
			t.Fatalf("ambiguous series accepted: %v", labels)
		}
	}
	for pod, want := range map[string]bool{"mine": true, "other": false} {
		if got, err := filter(map[string]string{"namespace": "b", "pod": pod}); err != nil || got != want {
			t.Fatalf("ownership of %s: %t %v, want %t", pod, got, err, want)
		}
	}
}
