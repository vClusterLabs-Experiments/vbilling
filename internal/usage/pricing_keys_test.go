package usage

import (
	"slices"
	"strings"
	"testing"
)

// setPricingKeys sets PRICING_KEYS for one test and clears it after.
func setPricingKeys(t *testing.T, spec string) {
	t.Helper()
	keys, err := ParsePricingKeys(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPricingKeys(keys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetPricingKeys(nil) })
}

// Every built-in metric a billing platform prices has its own list of keys
// PRICING_KEYS may add; informational metrics have none.
func TestEveryBilledBuiltInMetricHasPricingKeyChoices(t *testing.T) {
	for _, d := range builtin {
		choices := PricingKeyChoices(d.Code)
		if !d.Billable {
			if choices != nil {
				t.Errorf("%s is informational but has pricing key choices %v", d.Code, choices)
			}
			continue
		}
		if len(choices) <= len(d.GroupKeys) {
			t.Errorf("%s: no key to add besides its own (%v)", d.Code, choices)
		}
		// Its own group keys come first, and no key names one object.
		if !slices.Equal(choices[:len(d.GroupKeys)], d.GroupKeys) {
			t.Errorf("%s: choices %v do not start with its group keys %v", d.Code, choices, d.GroupKeys)
		}
		for _, k := range choices {
			if objectKeys[k] {
				t.Errorf("%s: %s names one object and cannot be a pricing key", d.Code, k)
			}
		}
	}
	if PricingKeyChoices("inference_output_tokens") != nil {
		t.Error("a custom metric has pricing key choices: custom metrics declare their own group keys")
	}
}

func TestParsePricingKeys(t *testing.T) {
	got, err := ParsePricingKeys(" vcluster_private_node_hours:zone|public_ip|region , vcluster_gpu_hours:gpu_type,vcluster_private_node_hours:vcpus|zone,vcluster_lb_hours:region ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		// Sorted, without duplicates, and without keys that are its own group keys.
		MetricPrivateNodeHours: {DimPublicIP, DimVCPUs, DimZone},
		MetricGPUHours:         {DimGPUType},
		// vcluster_lb_hours:region adds nothing: region is one of its keys.
	}
	if FormatPricingKeys(got) != FormatPricingKeys(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	if empty, err := ParsePricingKeys(""); err != nil || len(empty) != 0 {
		t.Fatalf("empty spec: %v, %v", empty, err)
	}

	for _, c := range []struct{ spec, want string }{
		{"vcluster_gpu_hours", `entry "vcluster_gpu_hours" must be metric:key|key`},
		{"vcluster_gpu_hours:", `entry "vcluster_gpu_hours:" must be metric:key|key`},
		{"vcluster_gpu_hours:gpu_type||zone", `entry "vcluster_gpu_hours:gpu_type||zone" has an empty key`},
		{"inference_output_tokens:model", `"inference_output_tokens" is not a built-in metric: PRICING_KEYS adds keys to built-in metrics, and custom metrics declare their own group keys`},
		{"vcluster_gpu_utilization:zone", "vcluster_gpu_utilization is informational: no billing platform prices it"},
		{"vcluster_gpu_downtime_hours:zone", "vcluster_gpu_downtime_hours is informational"},
		{"vcluster_lb_hours:zone", `vcluster_lb_hours cannot be priced on "zone": its events do not carry it. It can be priced on region, tenant_class, project`},
		{"vcluster_storage_gb_hours:gpu_type", `vcluster_storage_gb_hours cannot be priced on "gpu_type": its events do not carry it. It can be priced on region, sku, tenant_class, project`},
		{"vcluster_gpu_hours:node", `vcluster_gpu_hours cannot be priced on "node": it names one object`},
		{"vcluster_private_node_hours:tenant_cluster", `vcluster_private_node_hours cannot be priced on "tenant_cluster": it names one object`},
	} {
		if _, err := ParsePricingKeys(c.spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: error %v, want one containing %q", c.spec, err, c.want)
		}
	}
	// The message lists every metric PRICING_KEYS can name.
	_, err = ParsePricingKeys("vcluster_tokens:sku")
	for _, code := range []string{MetricGPUHours, MetricPublicIPHours, MetricLBHours} {
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("message %v does not name %s", err, code)
		}
	}
	if err != nil && strings.Contains(err.Error(), MetricGPUUtilization) {
		t.Errorf("message %v names an informational metric", err)
	}
}

// The keys PRICING_KEYS adds become group keys of the metric everywhere the
// catalog is read, after its own, until they are cleared.
func TestSetPricingKeysExtendsTheCatalog(t *testing.T) {
	setPricingKeys(t, "vcluster_private_node_hours:zone|public_ip,vcluster_cpu_core_hours:sku")
	def, _ := Lookup(MetricPrivateNodeHours)
	if want := []string{"region", "sku", DimCapacityType, DimPublicIP, DimZone}; !slices.Equal(def.GroupKeys, want) {
		t.Fatalf("group keys %v, want %v", def.GroupKeys, want)
	}
	for _, d := range Catalog() {
		if d.Code == MetricCPUCoreHours && !slices.Contains(d.GroupKeys, "sku") {
			t.Errorf("catalog: %v", d.GroupKeys)
		}
	}
	if !HasPricingKey(MetricCPUCoreHours, "sku") || HasPricingKey(MetricMemoryGBHours, "sku") || !HasPricingKey(MetricGPUHours, "sku") {
		t.Error("HasPricingKey")
	}
	if got := ExtraPricingKeys(MetricPrivateNodeHours); !slices.Equal(got, []string{DimPublicIP, DimZone}) {
		t.Errorf("extra keys %v", got)
	}
	if got := ExtraPricingKeys(MetricGPUHours); len(got) != 0 {
		t.Errorf("extra keys of a metric PRICING_KEYS does not name: %v", got)
	}
	// Setting them again replaces them.
	if err := SetPricingKeys(map[string][]string{MetricGPUHours: {DimGPUType}}); err != nil {
		t.Fatal(err)
	}
	if HasPricingKey(MetricCPUCoreHours, "sku") || !HasPricingKey(MetricGPUHours, DimGPUType) {
		t.Error("keys set before were kept")
	}
	if err := SetPricingKeys(map[string][]string{MetricLBHours: {DimZone}}); err == nil {
		t.Error("SetPricingKeys accepted a key the metric's events do not carry")
	}
	SetPricingKeys(nil)
	for _, d := range builtin {
		if !slices.Equal(d.GroupKeys, ownKeys[d.Code]) {
			t.Errorf("%s keeps %v after the keys were cleared", d.Code, d.GroupKeys)
		}
	}
}

// Public IP hours are billable, exact, from the collector, and priced per
// region unless PRICING_KEYS adds more.
func TestPublicIPHoursInTheCatalog(t *testing.T) {
	def, ok := Lookup(MetricPublicIPHours)
	if !ok || !def.Billable || def.Unit != "ip-hours" || def.Name != "Public IP Hours" || def.Accuracy != AccuracyExact || def.Source != SourceKubernetes {
		t.Fatalf("%+v", def)
	}
	if !slices.Equal(def.GroupKeys, []string{"region"}) {
		t.Errorf("group keys %v", def.GroupKeys)
	}
	if !slices.Contains(PricingKeyChoices(MetricPublicIPHours), DimIPFamily) {
		t.Error("public IP hours cannot be priced per address family")
	}
}
