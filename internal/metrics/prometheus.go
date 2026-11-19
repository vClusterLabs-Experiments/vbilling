package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/safehttp"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// promTransport sends one Prometheus HTTP API request and returns the body.
// Two implementations: plain HTTP to any Prometheus-compatible server, and
// the Kubernetes API service proxy of a tenant's cluster, which reaches a
// Prometheus that only exists inside that cluster without exposing it.
type promTransport interface {
	get(ctx context.Context, path string, params url.Values) ([]byte, int, error)
	String() string
}

// httpTransport talks to any server with the Prometheus HTTP API
// (Prometheus, Thanos, Mimir, VictoriaMetrics). Basic auth can be given in
// the URL (https://user:pass@host); headers and a bearer token file cover
// token-based and multi-tenant endpoints.
type httpTransport struct {
	baseURL    string
	httpClient *http.Client
	headers    map[string]string
	tokenFile  string // re-read on every query so rotated tokens keep working
}

func (h *httpTransport) String() string { return redactURL(h.baseURL) }

func (h *httpTransport) get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	if h.tokenFile != "" && req.Header.Get("Authorization") == "" {
		token, err := os.ReadFile(h.tokenFile)
		if err != nil {
			return nil, 0, fmt.Errorf("read prometheus bearer token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return body, resp.StatusCode, err
}

// kubeProxyTransport reaches a Service inside a tenant's cluster through
// that cluster's API server (/api/v1/namespaces/NS/services/NAME:PORT/proxy).
// The tenant API credentials need get on services/proxy for that Service.
type kubeProxyTransport struct {
	kube      kubernetes.Interface
	namespace string
	service   string // name:port
	prefix    string // optional path prefix, e.g. /prometheus
}

func (k *kubeProxyTransport) String() string {
	return "tenant-service://" + k.namespace + "/" + k.service + k.prefix
}

func (k *kubeProxyTransport) get(ctx context.Context, path string, params url.Values) ([]byte, int, error) {
	req := k.kube.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces", k.namespace, "services", k.service, "proxy", strings.TrimPrefix(k.prefix+path, "/"))
	for key, vals := range params {
		for _, v := range vals {
			req = req.Param(key, v)
		}
	}
	res := req.Do(ctx)
	var status int
	res.StatusCode(&status)
	body, err := res.Raw()
	if err != nil && len(body) == 0 {
		return nil, status, fmt.Errorf("prometheus via %s: %w", k, err)
	}
	return body, status, nil
}

// parseTenantService parses tenant-service://NAMESPACE/NAME:PORT[/PREFIX].
func parseTenantService(raw string) (ns, svc, prefix string, ok bool) {
	rest, found := strings.CutPrefix(raw, "tenant-service://")
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	if len(parts) == 3 && parts[2] != "" {
		prefix = "/" + strings.TrimSuffix(parts[2], "/")
	}
	return parts[0], parts[1], prefix, true
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
}

// prometheusClient runs Prometheus HTTP API queries over a transport.
type prometheusClient struct {
	t promTransport
}

func newPrometheusClient(baseURL string, headers map[string]string, tokenFile string) *prometheusClient {
	return &prometheusClient{t: &httpTransport{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
		headers:    headers,
		tokenFile:  tokenFile,
	}}
}

// newClusterPrometheusClient reaches a Prometheus a cluster named in its
// own settings (vbilling.vcluster.com/prometheus-url). Its answers can
// redirect anywhere, so redirects are never followed.
func newClusterPrometheusClient(baseURL string, headers map[string]string, tokenFile string) *prometheusClient {
	c := newPrometheusClient(baseURL, headers, tokenFile)
	c.t.(*httpTransport).httpClient.CheckRedirect = safehttp.RefuseRedirect
	return c
}

type promResult struct {
	Labels map[string]string
	Value  float64
}

// promSample is one raw sample.
type promSample struct {
	T time.Time
	V float64
}

// promSeries is a series with its samples in time order.
type promSeries struct {
	Labels  map[string]string
	Samples []promSample
}

type promResponse struct {
	Status    string `json:"status"`
	Error     string `json:"error"`
	ErrorType string `json:"errorType"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (p *prometheusClient) call(ctx context.Context, path string, params url.Values) (*promResponse, error) {
	body, status, err := p.t.get(ctx, path, params)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("prometheus query: HTTP %d from %s (check PROMETHEUS_HEADERS, PROMETHEUS_BEARER_TOKEN_FILE, the URL credentials, or RBAC for services/proxy)", status, p.t)
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode prometheus response (HTTP %d from %s): %w", status, p.t, err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", pr.Error)
	}
	return &pr, nil
}

// Query runs an instant query that returns a vector.
func (p *prometheusClient) Query(ctx context.Context, query string, at time.Time) ([]promResult, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("time", strconv.FormatInt(at.Unix(), 10))
	pr, err := p.call(ctx, "/api/v1/query", params)
	if err != nil {
		return nil, err
	}
	switch pr.Data.ResultType {
	case "vector":
	case "scalar":
		var sc []interface{}
		if err := json.Unmarshal(pr.Data.Result, &sc); err != nil {
			return nil, err
		}
		v, ok := parseValue(sc)
		if !ok {
			return nil, nil
		}
		return []promResult{{Labels: map[string]string{}, Value: v}}, nil
	default:
		return nil, fmt.Errorf("query returned a %s, expected an instant vector", pr.Data.ResultType)
	}
	var res []struct {
		Metric map[string]string `json:"metric"`
		Value  []interface{}     `json:"value"`
	}
	if err := json.Unmarshal(pr.Data.Result, &res); err != nil {
		return nil, err
	}
	out := make([]promResult, 0, len(res))
	for _, r := range res {
		v, ok := parseValue(r.Value)
		if !ok {
			continue
		}
		out = append(out, promResult{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// QueryRaw returns the raw samples of a series selector over (at-rng, at].
// It is an instant query of a range vector, which Prometheus answers with
// the stored samples, unextrapolated.
func (p *prometheusClient) QueryRaw(ctx context.Context, selector string, rng time.Duration, at time.Time) ([]promSeries, error) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("%s[%ds]", selector, int(rng.Seconds())))
	params.Set("time", strconv.FormatInt(at.Unix(), 10))
	pr, err := p.call(ctx, "/api/v1/query", params)
	if err != nil {
		return nil, err
	}
	if pr.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("counter %q returned a %s, expected raw samples (use a plain series selector)", selector, pr.Data.ResultType)
	}
	return decodeMatrix(pr.Data.Result)
}

// QueryRange evaluates a query at every step from start to end inclusive.
func (p *prometheusClient) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]promSeries, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start.Unix(), 10))
	params.Set("end", strconv.FormatInt(end.Unix(), 10))
	params.Set("step", strconv.Itoa(int(step.Seconds())))
	pr, err := p.call(ctx, "/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	if pr.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("query_range returned a %s", pr.Data.ResultType)
	}
	return decodeMatrix(pr.Data.Result)
}

func decodeMatrix(raw json.RawMessage) ([]promSeries, error) {
	var res []struct {
		Metric map[string]string `json:"metric"`
		Values [][]interface{}   `json:"values"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := make([]promSeries, 0, len(res))
	for _, r := range res {
		s := promSeries{Labels: r.Metric}
		for _, pair := range r.Values {
			if len(pair) < 2 {
				continue
			}
			ts, ok := pair[0].(float64)
			if !ok {
				continue
			}
			v, ok := parseValue(pair)
			if !ok {
				continue
			}
			s.Samples = append(s.Samples, promSample{T: time.UnixMilli(int64(ts * 1000)).UTC(), V: v})
		}
		sort.Slice(s.Samples, func(i, j int) bool { return s.Samples[i].T.Before(s.Samples[j].T) })
		out = append(out, s)
	}
	return out, nil
}

func parseValue(pair []interface{}) (float64, bool) {
	if len(pair) < 2 {
		return 0, false
	}
	s, _ := pair[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// queryVars fill the placeholders of query templates:
//
//	{{matchers}}       label matchers that scope series to one tenant's cluster
//	                   (see scopeMatchers), without braces
//	{{selector}}       the same matchers in braces, e.g. {namespace="team-a"}
//	{{namespace}}      the cluster's namespace on the control plane cluster
//	{{vcluster}}       the cluster's name
//	{{instance}}       its vCluster Platform instance
//	{{project}}, {{tenant}} (the billing customer), {{tenant_cluster}} (its external ID)
//	{{window}}         the window length, e.g. 60s; {{window_seconds}}, e.g. 60
type queryVars struct {
	Namespace, VCluster, Instance, Project, Tenant, TenantCluster string
	Matchers                                                      string
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

// noopMatcher matches every series. It stands in for {{matchers}} when the
// source itself is the scope (a Prometheus that only sees one tenant).
const noopMatcher = `__name__!=""`

// expand fills a query template. Values are escaped for PromQL string literals.
func expand(tmpl string, v queryVars) string {
	q := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	m := v.Matchers
	if m == "" {
		m = noopMatcher
	}
	return strings.NewReplacer(
		"{{selector}}", "{"+m+"}",
		"{{matchers}}", m,
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

// Egress counts transmitted bytes of the cluster's pods. By default it is a
// counter, so each window bills the exact byte delta between samples. Set
// EGRESS_QUERY to count only internet-bound traffic (for example from
// Cilium/Hubble or your fabric's flow metrics); a query is an estimate.
const defaultEgressCounter = `container_network_transmit_bytes_total{{selector}}`

// GPU utilization comes from the DCGM exporter, which labels each GPU with
// the workload's namespace. Prometheus keeps that label as `namespace` when
// it honors the exporter's labels, and renames it to `exported_namespace`
// when it does not (the kube-prometheus-stack default), so the defaults
// match either. GPU_UTIL_QUERY and GPU_COUNT_QUERY replace them.
var (
	defaultGPUUtilQuery  = dcgmUtilQuery(`namespace="{{namespace}}"`, `exported_namespace="{{namespace}}"`)
	defaultGPUCountQuery = dcgmCountQuery(`namespace="{{namespace}}"`, `exported_namespace="{{namespace}}"`)
	// Clusters with their own nodes run their own DCGM exporter; its series
	// are scoped by source, not by the control plane cluster's namespace.
	scopedGPUUtilQuery  = dcgmUtilQuery(`{{matchers}}`)
	scopedGPUCountQuery = dcgmCountQuery(`{{matchers}}`)
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

// PresetVClusterPlatform reads vCluster Platform fleet observability:
// clusters' OpenTelemetry collectors push metrics through the Platform's
// write gateway, which stamps every series with tamper-proof
// vcluster_platform_* labels from the collector's access key. Metrics from
// private nodes (DCGM, kubelet/cAdvisor) arrive the same way.
const PresetVClusterPlatform = "vcluster-platform"

const platformSelector = `vcluster_platform_instance="{{instance}}",vcluster_platform_project="{{project}}"`

// PromMetric is a usage metric read from Prometheus for every cluster at
// every window (PROMETHEUS_METRICS_FILE). Give exactly one of Counter or
// Query.
//
// Counter is a plain series selector of a monotonic counter, such as
// vllm:generation_tokens_total{{selector}}. vBilling reads its raw samples
// and bills the exact increase between window boundaries, handling counter
// resets, so consecutive windows add up to the true total and an outage only
// delays usage, never drops or double counts it. Use it for anything billed.
//
// Query is any PromQL expression returning one value per series at the
// window end. Functions such as increase() and rate() extrapolate between
// samples, so query results are estimates: they are marked as such on every
// event and in the catalog.
type PromMetric struct {
	Code        string   `json:"code"`
	Name        string   `json:"name,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Description string   `json:"description,omitempty"`
	Billable    *bool    `json:"billable,omitempty"` // default true
	Counter     string   `json:"counter,omitempty"`
	Query       string   `json:"query,omitempty"`
	Scale       float64  `json:"scale,omitempty"`      // multiply quantities, e.g. 1/1073741824 for bytes to GiB
	Dimensions  []string `json:"dimensions,omitempty"` // result labels kept as event dimensions (and pricing group keys)
	// SKULabel is the result label whose value becomes the event's SKU.
	SKULabel string `json:"skuLabel,omitempty"`
	// SKU is the v0.2 name of SKULabel. Deprecated: it read as a SKU value
	// while it names a label.
	SKU string `json:"sku,omitempty"`
}

func (m PromMetric) billable() bool { return m.Billable == nil || *m.Billable }

func (m PromMetric) scale() float64 {
	if m.Scale == 0 {
		return 1
	}
	return m.Scale
}

func (m PromMetric) skuLabel() string {
	if m.SKULabel != "" {
		return m.SKULabel
	}
	return m.SKU
}

// Accuracy reports whether the metric's quantities are exact.
func (m PromMetric) Accuracy() string {
	if m.Counter != "" {
		return usage.AccuracyExact
	}
	return usage.AccuracyEstimate
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
	return ParsePromMetrics(data, path)
}

// ParsePromMetrics parses and registers PromQL-defined metrics.
func ParsePromMetrics(data []byte, name string) ([]PromMetric, error) {
	var out []PromMetric
	if err := yaml.UnmarshalStrict(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	for _, m := range out {
		switch {
		case m.Query == "" && m.Counter == "":
			return nil, fmt.Errorf("%s: metric %q needs a counter (exact) or a query (estimate)", name, m.Code)
		case m.Query != "" && m.Counter != "":
			return nil, fmt.Errorf("%s: metric %q has both counter and query; pick one", name, m.Code)
		case m.Counter != "" && strings.ContainsAny(m.Counter, "()[]"):
			return nil, fmt.Errorf("%s: metric %q: counter must be a plain series selector such as vllm:generation_tokens_total{{selector}}, not %q", name, m.Code, m.Counter)
		}
		if m.SKU != "" && m.SKULabel == "" {
			fmt.Fprintf(os.Stderr, "WARNING: %s: metric %q uses sku: %q; rename it to skuLabel (it names a label)\n", name, m.Code, m.SKU)
		}
		var keys []string
		for _, d := range m.Dimensions {
			keys = append(keys, dimKey(d))
		}
		if m.skuLabel() != "" {
			keys = append(keys, "sku")
		}
		if err := usage.RegisterCustom(usage.MetricDef{Code: m.Code, Name: m.Name, Unit: m.Unit, Description: m.Description,
			Billable: m.billable(), GroupKeys: keys, Accuracy: m.Accuracy(), Source: usage.SourcePrometheus}); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
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
