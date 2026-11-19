package usage

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"sigs.k8s.io/yaml"
)

// Canonical metric codes emitted by the collector.
const (
	MetricCPUCoreHours     = "vcluster_cpu_core_hours"
	MetricMemoryGBHours    = "vcluster_memory_gb_hours"
	MetricStorageGBHours   = "vcluster_storage_gb_hours"
	MetricInstanceHours    = "vcluster_instance_hours"
	MetricGPUHours         = "vcluster_gpu_hours"
	MetricGPUUtilization   = "vcluster_gpu_utilization"
	MetricNetworkEgressGB  = "vcluster_network_egress_gb"
	MetricLBHours          = "vcluster_lb_hours"
	MetricPrivateNodeHours = "vcluster_private_node_hours"
	// MetricGPUDowntimeHours records GPU time that was allocated but not
	// billed because the node was unhealthy. It is informational: it makes
	// downtime credits auditable without ever reaching an invoice.
	MetricGPUDowntimeHours = "vcluster_gpu_downtime_hours"
)

// MetricDef describes a metric so adapters can create the matching meter,
// billable metric or plan charge.
type MetricDef struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
	// Billable metrics are created in billing backends. Informational ones
	// only reach the ledger, webhooks and exports.
	Billable bool `json:"billable"`
	// GroupKeys are the dimensions a rating engine should be able to price
	// on. Every event of the metric carries all of them (tenant_cluster is
	// added for invoice presentation), so backends can require them.
	GroupKeys []string `json:"group_keys,omitempty"`
	// Custom marks operator-declared metrics (ingest API).
	Custom bool `json:"custom,omitempty"`
	// Accuracy says how a quantity is measured. "exact" quantities come from
	// object lifetimes, counter deltas or pushed usage and add up across
	// windows; "estimate" quantities come from PromQL functions that
	// extrapolate between samples (increase, rate, avg_over_time) and should
	// not be the only evidence for anything a customer may dispute.
	Accuracy string `json:"accuracy,omitempty"`
	// Source names where the quantity comes from: collector (Kubernetes
	// objects), prometheus, records (per-request usage) or ingest.
	Source string `json:"source,omitempty"`
}

// Accuracy values.
const (
	AccuracyExact    = "exact"
	AccuracyEstimate = "estimate"
)

// Metric sources.
const (
	SourceKubernetes = "collector"
	SourcePrometheus = "prometheus"
	SourceRecords    = "records"
	SourceIngest     = "ingest"
)

var builtin = []MetricDef{
	{Code: MetricGPUHours, Name: "GPU Hours", Unit: "gpu-hours", Billable: true,
		Description: "GPU-equivalent hours allocated to running workloads. MIG and time-sliced GPUs count at their fraction.",
		GroupKeys:   []string{"region", "sku", DimCapacityType, DimBillingMode}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricCPUCoreHours, Name: "CPU Core Hours", Unit: "core-hours", Billable: true,
		Description: "vCPU core-hours (usage, requests or the larger of both, per configuration).",
		GroupKeys:   []string{"region", DimCapacityType, DimBillingMode}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricMemoryGBHours, Name: "Memory GiB Hours", Unit: "gib-hours", Billable: true,
		Description: "Memory GiB-hours (usage, requests or the larger of both, per configuration).",
		GroupKeys:   []string{"region", DimCapacityType, DimBillingMode}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricStorageGBHours, Name: "Storage GiB Hours", Unit: "gib-hours", Billable: true,
		Description: "Requested capacity of bound persistent volume claims, per storage class.",
		GroupKeys:   []string{"region", "sku"}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricInstanceHours, Name: "Tenant Cluster Hours", Unit: "hours", Billable: true,
		Description: "Hours a tenant cluster control plane was running and ready.",
		GroupKeys:   []string{"region", DimTenantClass}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricPrivateNodeHours, Name: "Dedicated Node Hours", Unit: "node-hours", Billable: true,
		Description: "Whole nodes allocated to one tenant, billed for full capacity.",
		GroupKeys:   []string{"region", "sku", DimCapacityType}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricNetworkEgressGB, Name: "Network Egress GiB", Unit: "gib", Billable: true,
		Description: "Transmitted bytes from tenant workloads (requires Prometheus).",
		GroupKeys:   []string{"region"}, Accuracy: AccuracyExact, Source: SourcePrometheus},
	{Code: MetricLBHours, Name: "Load Balancer Hours", Unit: "hours", Billable: true,
		Description: "Hours of Service type=LoadBalancer.",
		GroupKeys:   []string{"region"}, Accuracy: AccuracyExact, Source: SourceKubernetes},
	{Code: MetricGPUUtilization, Name: "GPU Utilization Hours", Unit: "util-hours", Billable: false,
		Description: "Average DCGM GPU utilization (0-100) multiplied by hours. Informational (requires Prometheus + DCGM).",
		GroupKeys:   []string{"region", DimGPUType}, Accuracy: AccuracyEstimate, Source: SourcePrometheus},
	{Code: MetricGPUDowntimeHours, Name: "GPU Downtime Hours", Unit: "gpu-hours", Billable: false,
		Description: "Allocated GPU time excluded from billing because the node was not ready or unhealthy.",
		GroupKeys:   []string{"region", DimGPUType}, Accuracy: AccuracyExact, Source: SourceKubernetes},
}

var (
	catMu  sync.RWMutex
	custom = map[string]MetricDef{}
)

// Catalog returns builtin metrics followed by operator-declared custom ones.
func Catalog() []MetricDef {
	catMu.RLock()
	defer catMu.RUnlock()
	out := make([]MetricDef, 0, len(builtin)+len(custom))
	out = append(out, builtin...)
	codes := make([]string, 0, len(custom))
	for c := range custom {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		out = append(out, custom[c])
	}
	return out
}

// Billable returns the subset of the catalog that billing backends create.
func Billable() []MetricDef {
	var out []MetricDef
	for _, d := range Catalog() {
		if d.Billable {
			out = append(out, d)
		}
	}
	return out
}

// SetAccuracy changes a built-in metric's accuracy when configuration makes
// it an estimate (for example EGRESS_QUERY replacing the egress counter).
func SetAccuracy(code, accuracy string) {
	catMu.Lock()
	defer catMu.Unlock()
	for i := range builtin {
		if builtin[i].Code == code {
			builtin[i].Accuracy = accuracy
		}
	}
}

// Estimate reports whether an event's quantity is an estimate: the catalog
// marks its metric as one, or the event says so itself (a metrics-server
// sample, a PromQL query).
func (e *Event) Estimate() bool {
	if v, ok := e.Properties["estimate"].(bool); ok && v {
		return true
	}
	def, ok := Lookup(e.Metric)
	return ok && def.Accuracy == AccuracyEstimate
}

// Lookup finds a metric definition by code.
func Lookup(code string) (MetricDef, bool) {
	for _, d := range builtin {
		if d.Code == code {
			return d, true
		}
	}
	catMu.RLock()
	defer catMu.RUnlock()
	d, ok := custom[code]
	return d, ok
}

// RegisterCustom declares an operator metric, e.g. inference tokens pushed
// through the ingest API by a model gateway. The caller decides whether it
// is billable: informational metrics (a cross-check of a billed one, a
// utilization signal) reach the ledger and exports but never a billing
// platform.
func RegisterCustom(d MetricDef) error {
	if !metricRe.MatchString(d.Code) {
		return fmt.Errorf("custom metric code %q must match %s", d.Code, metricRe)
	}
	if _, ok := Lookup(d.Code); ok {
		return fmt.Errorf("metric %q already exists", d.Code)
	}
	if d.Name == "" {
		d.Name = d.Code
	}
	if d.Unit == "" {
		d.Unit = "units"
	}
	if d.Accuracy == "" {
		d.Accuracy = AccuracyExact
	}
	if d.Source == "" {
		d.Source = SourceIngest
	}
	d.Custom = true
	catMu.Lock()
	custom[d.Code] = d
	catMu.Unlock()
	return nil
}

// RemoveCustom drops a custom metric. Tests use it to undo what they
// registered, so they can run repeatedly in one process (-count).
func RemoveCustom(code string) {
	catMu.Lock()
	delete(custom, code)
	catMu.Unlock()
}

// EnsureCustom registers d unless a metric with its code exists, in which
// case the existing definition wins (an operator's CUSTOM_METRICS entry
// beats a gateway format's default).
func EnsureCustom(d MetricDef) (MetricDef, error) {
	if existing, ok := Lookup(d.Code); ok {
		return existing, nil
	}
	if err := RegisterCustom(d); err != nil {
		return MetricDef{}, err
	}
	got, _ := Lookup(d.Code)
	return got, nil
}

// ParseCustomMetrics parses "code:unit[:group_key|group_key],..." as used by
// the CUSTOM_METRICS env var, e.g.
// "inference_input_tokens:tokens:model|region,slurm_gpu_hours:gpu-hours".
// Metrics declared this way are billable.
func ParseCustomMetrics(spec string) ([]MetricDef, error) {
	var out []MetricDef
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		d := MetricDef{Code: parts[0], Billable: true}
		if len(parts) > 1 {
			d.Unit = parts[1]
		}
		if len(parts) > 2 && parts[2] != "" {
			d.GroupKeys = strings.Split(parts[2], "|")
		}
		if len(parts) > 3 {
			return nil, fmt.Errorf("custom metric %q: expected code:unit[:key|key]", item)
		}
		out = append(out, d)
	}
	return out, nil
}

// resetCustomForTest clears custom metrics (tests only).
func resetCustomForTest() {
	catMu.Lock()
	custom = map[string]MetricDef{}
	catMu.Unlock()
}

// CustomMetricSpec is one entry of CUSTOM_METRICS_FILE, the list form of
// CUSTOM_METRICS that Helm renders from metering.customMetrics.
type CustomMetricSpec struct {
	Code        string   `json:"code"`
	Name        string   `json:"name,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Description string   `json:"description,omitempty"`
	GroupKeys   []string `json:"groupKeys,omitempty"`
	// Billable defaults to true; false keeps the metric informational.
	Billable *bool `json:"billable,omitempty"`
}

// ParseCustomMetricsYAML reads a YAML list of CustomMetricSpec.
func ParseCustomMetricsYAML(data []byte) ([]MetricDef, error) {
	var specs []CustomMetricSpec
	if err := yaml.UnmarshalStrict(data, &specs); err != nil {
		return nil, err
	}
	out := make([]MetricDef, 0, len(specs))
	for _, s := range specs {
		if s.Code == "" {
			return nil, fmt.Errorf("custom metric without a code")
		}
		billable := true
		if s.Billable != nil {
			billable = *s.Billable
		}
		out = append(out, MetricDef{Code: s.Code, Name: s.Name, Unit: s.Unit, Description: s.Description,
			GroupKeys: s.GroupKeys, Billable: billable})
	}
	return out, nil
}
