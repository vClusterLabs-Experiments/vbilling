package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// The definitions are what vBilling's events need, as Metronome's create
// bodies (checked against its OpenAPI schema), for every billable metric,
// custom ones included, and none for informational metrics.
func TestMetronomeBillableMetrics(t *testing.T) {
	t.Setenv("CUSTOM_METRICS", "test_cli_tokens:tokens:model|region")
	t.Cleanup(func() { usage.RemoveCustom("test_cli_tokens") })
	var out, errb bytes.Buffer
	if code := metronomeMain([]string{"billable-metrics"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var defs []map[string]any
	if err := json.Unmarshal(out.Bytes(), &defs); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	schema, err := metronometest.RequestSchema("create_billable_metric")
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]map[string]any{}
	for _, d := range defs {
		for _, e := range metronometest.Validate(schema, d, "billable metric") {
			t.Errorf("schema violation: %s", e)
		}
		in := d["event_type_filter"].(map[string]any)["in_values"].([]any)
		byType[in[0].(string)] = d
	}
	for _, def := range usage.Billable() {
		if byType[def.Code] == nil {
			t.Errorf("no billable metric for %s", def.Code)
		}
	}
	if byType[usage.MetricGPUUtilization] != nil || byType[usage.MetricGPUDowntimeHours] != nil {
		t.Error("informational metrics are never billed")
	}
	gpu := byType[usage.MetricGPUHours]
	keys, _ := json.Marshal(gpu["group_keys"])
	if gpu["aggregation_type"] != "SUM" || gpu["aggregation_key"] != "value" || string(keys) != `[["region","sku","capacity_type","billing_mode","tenant_cluster"]]` {
		t.Errorf("GPU hours: %v", gpu)
	}
	filters, _ := json.Marshal(gpu["property_filters"])
	if !strings.Contains(string(filters), `{"exists":true,"name":"value"}`) || !strings.Contains(string(filters), `{"exists":true,"name":"sku"}`) {
		t.Errorf("GPU hours must require value and every group key: %s", filters)
	}
	if keys, _ := json.Marshal(byType["test_cli_tokens"]["group_keys"]); string(keys) != `[["model","region"]]` {
		t.Errorf("custom metric group keys %s", keys)
	}

	if code := metronomeMain([]string{"bogus"}, &out, &errb); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
}

// With PRICING_KEYS the printed billable metrics group on the extra keys
// and require them, under names that say so: what Finance adds before the
// change (a billable metric's group keys cannot be changed). Keys a
// metric's events do not carry are refused.
func TestMetronomeBillableMetricsWithPricingKeys(t *testing.T) {
	t.Cleanup(func() { usage.SetPricingKeys(nil) })
	t.Setenv("PRICING_KEYS", "vcluster_private_node_hours:public_ip|zone,vcluster_public_ip_hours:ip_family")
	var out, errb bytes.Buffer
	if code := metronomeMain([]string{"billable-metrics"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var defs []map[string]any
	if err := json.Unmarshal(out.Bytes(), &defs); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ name, keys string }{
		usage.MetricPrivateNodeHours: {"Dedicated Node Hours [vcluster_private_node_hours] by public_ip, zone", `[["region","sku","capacity_type","public_ip","zone","tenant_cluster"]]`},
		usage.MetricPublicIPHours:    {"Public IP Hours [vcluster_public_ip_hours] by ip_family", `[["region","ip_family","tenant_cluster"]]`},
		usage.MetricGPUHours:         {"GPU Hours [vcluster_gpu_hours]", `[["region","sku","capacity_type","billing_mode","tenant_cluster"]]`},
	}
	for _, d := range defs {
		code := d["event_type_filter"].(map[string]any)["in_values"].([]any)[0].(string)
		w, ok := want[code]
		if !ok {
			continue
		}
		delete(want, code)
		keys, _ := json.Marshal(d["group_keys"])
		filters, _ := json.Marshal(d["property_filters"])
		if d["name"] != w.name || string(keys) != w.keys {
			t.Errorf("%s: %v %s, want %q %s", code, d["name"], keys, w.name, w.keys)
		}
		if strings.Contains(w.keys, "zone") && !strings.Contains(string(filters), `{"exists":true,"name":"zone"}`) {
			t.Errorf("%s does not require zone: %s", code, filters)
		}
	}
	if len(want) != 0 {
		t.Errorf("not printed: %v", want)
	}

	t.Setenv("PRICING_KEYS", "vcluster_lb_hours:zone")
	out.Reset()
	errb.Reset()
	if code := metronomeMain([]string{"billable-metrics"}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), `PRICING_KEYS: vcluster_lb_hours cannot be priced on "zone"`) || out.Len() != 0 {
		t.Errorf("exit %d\nstdout: %s\nstderr: %s", code, out.String(), errb.String())
	}
}
