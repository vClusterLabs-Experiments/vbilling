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
