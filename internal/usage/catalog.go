package usage

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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
}

var builtin = []MetricDef{
	{Code: MetricGPUHours, Name: "GPU Hours", Unit: "gpu-hours", Billable: true,
		Description: "GPU-equivalent hours allocated to running workloads. MIG and time-sliced GPUs count at their fraction.",
		GroupKeys:   []string{"region", "sku", DimCapacityType, DimBillingMode}},
	{Code: MetricCPUCoreHours, Name: "CPU Core Hours", Unit: "core-hours", Billable: true,
		Description: "vCPU core-hours (usage, requests or the larger of both, per configuration).",
		GroupKeys:   []string{"region", DimCapacityType, DimBillingMode}},
	{Code: MetricMemoryGBHours, Name: "Memory GiB Hours", Unit: "gib-hours", Billable: true,
		Description: "Memory GiB-hours (usage, requests or the larger of both, per configuration).",
		GroupKeys:   []string{"region", DimCapacityType, DimBillingMode}},
	{Code: MetricStorageGBHours, Name: "Storage GiB Hours", Unit: "gib-hours", Billable: true,
		Description: "Requested capacity of bound persistent volume claims, per storage class.",
		GroupKeys:   []string{"region", "sku"}},
	{Code: MetricInstanceHours, Name: "Tenant Cluster Hours", Unit: "hours", Billable: true,
		Description: "Hours a tenant cluster control plane was running and ready.",
		GroupKeys:   []string{"region", DimTenantClass}},
	{Code: MetricPrivateNodeHours, Name: "Dedicated Node Hours", Unit: "node-hours", Billable: true,
		Description: "Whole nodes allocated to one tenant, billed for full capacity.",
		GroupKeys:   []string{"region", "sku", DimCapacityType}},
	{Code: MetricNetworkEgressGB, Name: "Network Egress GiB", Unit: "gib", Billable: true,
		Description: "Transmitted bytes from tenant workloads (requires Prometheus).",
		GroupKeys:   []string{"region"}},
	{Code: MetricLBHours, Name: "Load Balancer Hours", Unit: "hours", Billable: true,
		Description: "Hours of Service type=LoadBalancer.",
		GroupKeys:   []string{"region"}},
	{Code: MetricGPUUtilization, Name: "GPU Utilization Hours", Unit: "util-hours", Billable: false,
		Description: "Average DCGM GPU utilization (0-100) multiplied by hours. Informational (requires Prometheus + DCGM).",
		GroupKeys:   []string{"region", DimGPUType}},
	{Code: MetricGPUDowntimeHours, Name: "GPU Downtime Hours", Unit: "gpu-hours", Billable: false,
		Description: "Allocated GPU time excluded from billing because the node was not ready or unhealthy.",
		GroupKeys:   []string{"region", DimGPUType}},
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
// through the ingest API by a model gateway.
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
	d.Custom = true
	d.Billable = true
	catMu.Lock()
	custom[d.Code] = d
	catMu.Unlock()
	return nil
}

// ParseCustomMetrics parses "code:unit[:group_key|group_key],..." as used by
// the CUSTOM_METRICS env var, e.g.
// "inference_input_tokens:tokens:model|region,slurm_gpu_hours:gpu-hours".
func ParseCustomMetrics(spec string) ([]MetricDef, error) {
	var out []MetricDef
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		d := MetricDef{Code: parts[0]}
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
