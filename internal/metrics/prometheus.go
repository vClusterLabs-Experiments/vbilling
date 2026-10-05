package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// prometheusClient runs instant queries. Queries are evaluated at the
// window end, so results line up with metering windows. Any server with the
// Prometheus HTTP API works (Prometheus, Thanos, Mimir, VictoriaMetrics).
// Basic auth can be given in the URL (https://user:pass@host); headers and a
// bearer token file cover token-based and multi-tenant endpoints.
type prometheusClient struct {
	baseURL    string
	httpClient *http.Client
	headers    map[string]string
	tokenFile  string // re-read on every query so rotated tokens keep working
}

func newPrometheusClient(baseURL string, headers map[string]string, tokenFile string) *prometheusClient {
	return &prometheusClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
		headers:    headers,
		tokenFile:  tokenFile,
	}
}

type promResult struct {
	Labels map[string]string
	Value  float64
}

func (p *prometheusClient) Query(ctx context.Context, query string, at time.Time) ([]promResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("time", strconv.FormatInt(at.Unix(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/v1/query?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	if p.tokenFile != "" && req.Header.Get("Authorization") == "" {
		token, err := os.ReadFile(p.tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read prometheus bearer token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("prometheus query: HTTP %d (check PROMETHEUS_HEADERS, PROMETHEUS_BEARER_TOKEN_FILE or the URL credentials)", resp.StatusCode)
	}
	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []interface{}     `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode prometheus response (HTTP %d): %w", resp.StatusCode, err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", pr.Error)
	}
	out := make([]promResult, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		if len(r.Value) < 2 {
			continue
		}
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out = append(out, promResult{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// queryVars fill the placeholders of query templates: {{namespace}} (the
// tenant cluster's namespace on the control plane cluster), {{vcluster}}
// (its name), {{instance}} (its vCluster Platform instance), {{project}},
// {{tenant}} (the billing customer), {{tenant_cluster}} (its external ID),
// {{window}} (the window length, e.g. 60s) and {{window_seconds}} (e.g. 60,
// to integrate a gauge over the window).
type queryVars struct {
	Namespace, VCluster, Instance, Project, Tenant, TenantCluster string
	Window                                                        time.Duration
}

func varsFor(t Target, window time.Duration) queryVars {
	instance := t.Cluster.Instance
	if instance == "" {
		instance = t.Cluster.Name
	}
	return queryVars{Namespace: t.Cluster.Namespace, VCluster: t.Cluster.Name, Instance: instance,
		Project: t.Project, Tenant: t.Tenant, TenantCluster: t.Cluster.ExternalID(), Window: window}
}

// expand fills a query template. Values are escaped for PromQL string literals.
func expand(tmpl string, v queryVars) string {
	q := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	return strings.NewReplacer(
		"{{namespace}}", q(v.Namespace),
		"{{vcluster}}", q(v.VCluster),
		"{{instance}}", q(v.Instance),
		"{{project}}", q(v.Project),
		"{{tenant}}", q(v.Tenant),
		"{{tenant_cluster}}", q(v.TenantCluster),
		"{{window}}", strconv.Itoa(int(v.Window.Seconds()))+"s",
		"{{window_seconds}}", strconv.Itoa(int(v.Window.Seconds())),
	).Replace(tmpl)
}

// Default queries. Egress counts transmitted bytes of the tenant cluster's
// pods; set EGRESS_QUERY to count only internet-bound traffic (for example
// from Cilium/Hubble or your fabric's flow metrics).
//
// GPU utilization comes from the DCGM exporter, which labels each GPU with
// the workload's namespace. Prometheus keeps that label as `namespace` when
// it honors the exporter's labels, and renames it to `exported_namespace`
// when it does not (the kube-prometheus-stack default), so the defaults
// match either. GPU_UTIL_QUERY and GPU_COUNT_QUERY replace them.
const defaultEgressQuery = `sum(increase(container_network_transmit_bytes_total{namespace="{{namespace}}"}[{{window}}]))`

var (
	defaultGPUUtilQuery  = dcgmUtilQuery(`namespace="{{namespace}}"`, `exported_namespace="{{namespace}}"`)
	defaultGPUCountQuery = dcgmCountQuery(`namespace="{{namespace}}"`, `exported_namespace="{{namespace}}"`)
)

// dcgmUtilQuery averages DCGM GPU utilization (percent) per model for any of
// the selectors. DCGM reports no DCGM_FI_DEV_GPU_UTIL for MIG instances, so
// GPUs in MIG mode use each instance's graphics engine activity
// (DCGM_FI_PROF_GR_ENGINE_ACTIVE, 0-1) instead.
func dcgmUtilQuery(selectors ...string) string {
	var parts []string
	for _, s := range selectors {
		parts = append(parts, `avg_over_time(DCGM_FI_DEV_GPU_UTIL{`+s+`}[{{window}}])`)
	}
	for _, s := range selectors {
		parts = append(parts, `100 * avg_over_time(DCGM_FI_PROF_GR_ENGINE_ACTIVE{`+s+`,GPU_I_ID!=""}[{{window}}])`)
	}
	return `avg by (modelName) (` + strings.Join(parts, " or ") + `)`
}

// dcgmCountQuery counts physical GPUs per model. The MIG instances of a GPU
// are separate series that share its UUID, so they count once.
func dcgmCountQuery(selectors ...string) string {
	var parts []string
	for _, s := range selectors {
		parts = append(parts, `DCGM_FI_DEV_GPU_UTIL{`+s+`}`)
	}
	for _, s := range selectors {
		parts = append(parts, `DCGM_FI_PROF_GR_ENGINE_ACTIVE{`+s+`,GPU_I_ID!=""}`)
	}
	return `count by (modelName) (group by (modelName, UUID, gpu, Hostname, hostname) (` + strings.Join(parts, " or ") + `))`
}

// PresetVClusterPlatform reads vCluster Platform fleet observability: tenant
// clusters' OpenTelemetry collectors push metrics through the Platform's
// write gateway, which stamps every series with tamper-proof
// vcluster_platform_* labels from the collector's access key. Metrics from
// private nodes (DCGM, kubelet/cAdvisor) arrive the same way.
const PresetVClusterPlatform = "vcluster-platform"

const platformSelector = `vcluster_platform_instance="{{instance}}",vcluster_platform_project="{{project}}"`

var platformQueries = struct{ egress, util, count string }{
	egress: `sum(increase(container_network_transmit_bytes_total{` + platformSelector + `}[{{window}}]))`,
	util:   dcgmUtilQuery(platformSelector),
	count:  dcgmCountQuery(platformSelector),
}

// PromMetric is a usage metric defined by a PromQL query, evaluated for each
// tenant cluster at every window end (PROMETHEUS_METRICS_FILE). Billable
// ones are created in the billing backends like built-in metrics.
type PromMetric struct {
	Code        string   `json:"code"`
	Name        string   `json:"name,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Description string   `json:"description,omitempty"`
	Billable    bool     `json:"billable"`
	Query       string   `json:"query"`
	Dimensions  []string `json:"dimensions,omitempty"` // result labels kept as event dimensions (and pricing group keys)
	SKU         string   `json:"sku,omitempty"`        // result label used as the event SKU
}

// LoadPromMetrics reads PROMETHEUS_METRICS_FILE and registers its metrics.
func LoadPromMetrics(path string) ([]PromMetric, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []PromMetric
	if err := yaml.UnmarshalStrict(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, m := range out {
		if m.Query == "" {
			return nil, fmt.Errorf("%s: metric %q has no query", path, m.Code)
		}
		var keys []string
		for _, d := range m.Dimensions {
			keys = append(keys, dimKey(d))
		}
		if err := usage.RegisterCustom(usage.MetricDef{Code: m.Code, Name: m.Name, Unit: m.Unit, Description: m.Description,
			Billable: m.Billable, GroupKeys: keys}); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return out, nil
}

// dimKey turns a Prometheus label (modelName, model-name) into a dimension
// key (model_name).
func dimKey(label string) string {
	var b strings.Builder
	for i, r := range label {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	k := strings.Trim(b.String(), "_")
	if k == "" || k[0] < 'a' || k[0] > 'z' {
		k = "l_" + k
	}
	if len(k) > 64 {
		k = k[:64]
	}
	return k
}
