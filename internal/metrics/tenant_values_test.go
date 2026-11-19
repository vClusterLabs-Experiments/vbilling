package metrics

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Label values become SKUs and dimensions. A value with markup or control
// characters is billed under a safe version of it, never dropped.
func TestPrometheusLabelValuesAreMadeSafe(t *testing.T) {
	bad := `<img src=x onerror="alert(1)">`
	fp := &fakeProm{series: []fakeSeries{
		{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": bad},
			samples: counterSamples(ws.Add(-5*time.Minute), we, 10, time.Time{})},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	target := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	res := collectOK(t, p, []Target{target}, latest())
	var billed float64
	for _, pw := range res {
		for _, e := range pw.Events {
			billed += e.Quantity
			if err := usage.CheckValue(e.SKU); err != nil || e.SKU != usage.SafeValue(bad) {
				t.Errorf("SKU %q: %v", e.SKU, err)
			}
			if err := usage.CheckValue(e.Dim("model_name")); err != nil {
				t.Errorf("dimension model_name %q: %v", e.Dim("model_name"), err)
			}
		}
	}
	if billed == 0 {
		t.Fatal("the usage of a series with an unsafe label value was dropped")
	}
}

// Labels on a cluster's own nodes are written by the tenant. GPU types and
// zones read from them are made safe the same way.
func TestNodeLabelValuesAreMadeSafe(t *testing.T) {
	if got := normalizeGPUType("NVIDIA H100 <b>"); got != "NVIDIA-H100-_b_" {
		t.Errorf("GPU type = %q", got)
	}
	if got := firstLabel(map[string]string{"z": "eu-1\"a"}, "z"); got != "eu-1_a" {
		t.Errorf("zone = %q", got)
	}
	if got := normalizeGPUType("NVIDIA A100-SXM4-80GB"); got != "NVIDIA-A100-SXM4-80GB" {
		t.Errorf("a valid GPU type changed: %q", got)
	}
}
