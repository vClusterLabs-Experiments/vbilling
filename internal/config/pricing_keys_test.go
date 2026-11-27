package config

import (
	"strings"
	"testing"
)

// PRICING_KEYS is checked with the rest of the configuration: built-in
// billable metrics only, keys their events carry, and never with the
// stripe adapter, whose meters cannot price on dimensions.
func TestPricingKeysSettings(t *testing.T) {
	t.Setenv("ADAPTERS", "metronome,lago,webhook")
	t.Setenv("PRICING_KEYS", "vcluster_private_node_hours:public_ip|zone,vcluster_gpu_hours:gpu_type")
	c := Load()
	if c.PricingKeys != "vcluster_private_node_hours:public_ip|zone,vcluster_gpu_hours:gpu_type" {
		t.Fatalf("PricingKeys %q", c.PricingKeys)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []struct{ spec, want string }{
		{"vcluster_lb_hours:zone", `PRICING_KEYS: vcluster_lb_hours cannot be priced on "zone": its events do not carry it. It can be priced on region, tenant_class, project`},
		{"inference_output_tokens:model", `PRICING_KEYS: "inference_output_tokens" is not a built-in metric`},
		{"vcluster_gpu_hours", `PRICING_KEYS: entry "vcluster_gpu_hours" must be metric:key|key`},
	} {
		t.Setenv("PRICING_KEYS", bad.spec)
		if err := Load().Validate(); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s: %v, want an error containing %q", bad.spec, err, bad.want)
		}
	}

	t.Setenv("PRICING_KEYS", "vcluster_gpu_hours:gpu_type")
	t.Setenv("ADAPTERS", "stripe")
	err := Load().Validate()
	if err == nil || !strings.Contains(err.Error(), "PRICING_KEYS does not work with the stripe adapter") || !strings.Contains(err.Error(), "STRIPE_SPLIT_METERS_BY") {
		t.Fatalf("PRICING_KEYS with the stripe adapter: %v", err)
	}
	// Without keys to add (unset, or only keys a metric is priced on already)
	// the stripe adapter is fine.
	for _, spec := range []string{"", "vcluster_gpu_hours:sku|region"} {
		t.Setenv("PRICING_KEYS", spec)
		if err := Load().Validate(); err != nil {
			t.Errorf("%q with the stripe adapter: %v", spec, err)
		}
	}
}
