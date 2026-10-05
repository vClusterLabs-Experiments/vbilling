package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestQueryPlaceholdersAreFilledAndEscaped(t *testing.T) {
	v := queryVars{Namespace: "team-a", VCluster: "gpu", Instance: "gpu-prod", Project: "research", Tenant: `ac"me`, TenantCluster: "vcluster-team-a-gpu", Window: time.Minute}
	got := expand(`x{a="{{namespace}}",b="{{vcluster}}",c="{{instance}}",d="{{project}}",e="{{tenant}}",f="{{tenant_cluster}}"}[{{window}}] * {{window_seconds}}`, v)
	want := `x{a="team-a",b="gpu",c="gpu-prod",d="research",e="ac\"me",f="vcluster-team-a-gpu"}[60s] * 60`
	if got != want {
		t.Errorf("expand = %s\nwant     %s", got, want)
	}
}

func TestPlatformPresetQueriesFleetObservabilityLabels(t *testing.T) {
	prom, queries := promServer(t, nil, func(q string) (string, map[string]string) {
		if strings.Contains(q, "DCGM_FI_DEV_GPU_UTIL") && strings.Contains(q, `vcluster_platform_instance="gpu"`) && strings.Contains(q, "avg_over_time") {
			return "40", map[string]string{"modelName": "NVIDIA A100-SXM4-40GB"}
		}
		return "", nil
	})
	evs := collect(t, Options{PrometheusURL: prom.URL, PrometheusPreset: PresetVClusterPlatform}, nil, latest())[we]
	if got, _ := find(evs, usage.MetricGPUUtilization, usage.DimGPUType, "NVIDIA-A100-SXM4-40GB"); !near(got, usage.Round(40.0/60)) {
		t.Errorf("utilization from fleet observability = %v", got)
	}
	for _, q := range *queries {
		if !strings.Contains(q, `vcluster_platform_instance="gpu",vcluster_platform_project="research"`) {
			t.Errorf("preset query without the tamper-proof tenant selector: %s", q)
		}
	}
}

func TestPrometheusDefinedBillableMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.yaml")
	body := `- code: test_output_tokens
  name: Output tokens
  unit: tokens
  billable: true
  dimensions: [model_name, modelTier]
  sku: model_name
  query: sum by (model_name, modelTier) (increase(vllm:generation_tokens_total{namespace="{{namespace}}"}[{{window}}]))
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pm, err := LoadPromMetrics(path)
	if err != nil {
		t.Fatal(err)
	}
	def, ok := usage.Lookup("test_output_tokens")
	if !ok || !def.Billable || strings.Join(def.GroupKeys, ",") != "model_name,model_tier" {
		t.Fatalf("registered metric = %+v", def)
	}
	prom, queries := promServer(t, nil, func(q string) (string, map[string]string) {
		if strings.Contains(q, "vllm:generation_tokens_total") {
			return "125000", map[string]string{"model_name": "llama-3.1-70b", "modelTier": "premium"}
		}
		return "", nil
	})
	evs := collect(t, Options{PrometheusURL: prom.URL, PromMetrics: pm, GPUUtilQuery: "none"}, []runtime.Object{}, latest())[we]
	got, n := find(evs, "test_output_tokens", "sku", "llama-3.1-70b", "model_name", "llama-3.1-70b", "model_tier", "premium")
	if n != 1 || got != 125000 {
		t.Fatalf("token events: %v (%d)", got, n)
	}
	if !strings.Contains(strings.Join(*queries, "\n"), `vllm:generation_tokens_total{namespace="team-a"}[60s]`) {
		t.Errorf("queries = %q", *queries)
	}
	if _, err := LoadPromMetrics(path); err == nil {
		t.Error("registering the same metric twice must fail")
	}
}

func TestDimKey(t *testing.T) {
	for in, want := range map[string]string{"modelName": "model_name", "model-name": "model_name", "GPU": "g_p_u", "9lives": "l_9lives"} {
		if got := dimKey(in); got != want {
			t.Errorf("dimKey(%s) = %s, want %s", in, got, want)
		}
	}
}
