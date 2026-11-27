package lago

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pricingKeys sets PRICING_KEYS for one test and clears it after.
func pricingKeys(t *testing.T, spec string) {
	t.Helper()
	keys, err := usage.ParsePricingKeys(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := usage.SetPricingKeys(keys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.SetPricingKeys(nil) })
}

// priceFilter adds a charge filter key=value at amount to a metric's charge
// on the default plan, as Finance would in the Lago UI (the billable metric
// must list the value).
func priceFilter(f *fakeLago, metric, key, value, amount string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.plans["vcluster-standard"].Charges {
		if c["billable_metric_code"] == metric {
			fs, _ := c["filters"].([]any)
			c["filters"] = append(fs, map[string]any{"invoice_display_name": nil,
				"properties": map[string]any{"amount": amount}, "values": map[string]any{key: []any{value}}})
		}
	}
}

func filterValues(f *fakeLago, metric, key string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, mf := range f.metrics[metric].Filters {
		if mf.Key == key {
			return slices.Sorted(slices.Values(mf.Values))
		}
	}
	return nil
}

// The keys PRICING_KEYS adds reach Lago: series split by them, their values
// join the billable metric's filters (Lago prices a value only through a
// charge filter, for a value its billable metric lists), and a charge
// filter on such a key prices a metric that has no SKUs. vBilling adds no
// charge filter and no price for them.
func TestLagoPricesOnTheKeysPricingKeysAdds(t *testing.T) {
	pricingKeys(t, "vcluster_public_ip_hours:ip_family,vcluster_private_node_hours:public_ip")
	f := newFakeLago(t)
	seedLago(f, nil)
	a := newTestAdapter(t, f, nil)
	ctx := context.Background()

	if dims := a.PriceDims(usage.MetricPublicIPHours); !slices.Equal(dims, []string{usage.DimIPFamily}) {
		t.Errorf("public IP hours dims %v", dims)
	}
	if dims := a.PriceDims(usage.MetricGPUHours); len(dims) != 0 {
		t.Errorf("a metric PRICING_KEYS does not name: dims %v", dims)
	}

	ip := func(family string) destinations.PriceSeries {
		return destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricPublicIPHours, Dims: map[string]string{usage.DimIPFamily: family}}
	}
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{ip("ipv4"), ip("ipv6")}); err != nil {
		t.Fatal(err)
	}
	if got := filterValues(f, usage.MetricPublicIPHours, usage.DimIPFamily); !slices.Equal(got, []string{"ipv4", "ipv6"}) {
		t.Errorf("billable metric ip_family values %v", got)
	}
	if len(f.requests("PUT", "/api/v1/plans")) != 0 {
		t.Error("preparing a series changed a plan")
	}

	// Finance prices IPv4 addresses; IPv6 falls to the charge's price of 0
	// while IPv4 has one: a rate miss, named with its value.
	priceFilter(f, usage.MetricPublicIPHours, usage.DimIPFamily, "ipv4", "0.005")
	got := lagoCheck(t, a, ip("ipv4"), ip("ipv6"))
	if c := got[ip("ipv4").Key()]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, "ip_family=ipv4") {
		t.Errorf("IPv4: %+v", c)
	}
	c := got[ip("ipv6").Key()]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, "price ip_family=ipv6") || strings.Contains(c.Hint, "sku") {
		t.Errorf("IPv6 billed at 0 while IPv4 is priced: %+v", c)
	}
	// Priced at 0 on purpose is information, as for a SKU.
	priceFilter(f, usage.MetricPublicIPHours, usage.DimIPFamily, "ipv6", "0")
	if c := lagoCheck(t, a, ip("ipv6"))[ip("ipv6").Key()]; c.Status != destinations.PriceZero {
		t.Errorf("IPv6 at 0: %+v", c)
	}

	// Delivery adds the values it carries too (once), after the events.
	ev := usage.Event{ID: "vb1_x", Tenant: "acme", Metric: usage.MetricPrivateNodeHours, Quantity: 1, SKU: "gpu-h100-8",
		WindowStart: time.Now().Add(-time.Minute), WindowEnd: time.Now(), Dimensions: map[string]string{usage.DimPublicIP: "true"}}
	for i := 0; i < 2; i++ {
		if err := a.SendEvents(ctx, []usage.Event{ev}); err != nil {
			t.Fatal(err)
		}
	}
	if got := filterValues(f, usage.MetricPrivateNodeHours, usage.DimPublicIP); !slices.Equal(got, []string{"true"}) {
		t.Errorf("billable metric public_ip values %v", got)
	}
	if got := filterValues(f, usage.MetricPrivateNodeHours, "sku"); !slices.Equal(got, []string{"gpu-h100-8"}) {
		t.Errorf("the SKU filter values are kept: %v", got)
	}
	updates := 0
	for _, r := range f.requests("PUT", "/api/v1/billable_metrics/"+usage.MetricPrivateNodeHours) {
		if strings.Contains(string(r.Body), usage.DimPublicIP) {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("public_ip values were sent %d times, want once", updates)
	}
}

// Public IP hours have their own billable metric and charge in Lago.
func TestLagoHasPublicIPHours(t *testing.T) {
	var m *BillableMetric
	for _, bm := range lagoMetrics(nil) {
		if bm.Code == usage.MetricPublicIPHours {
			m = &bm
		}
	}
	if m == nil || m.FieldName != "public_ip_hours" || fieldNameFor(usage.MetricPublicIPHours) != m.FieldName || m.AggregationType != "sum_agg" {
		t.Fatalf("%+v", m)
	}
	e := usage.Event{Metric: usage.MetricPublicIPHours, Quantity: 2.5}
	if q := ToEvent(&e).Properties["public_ip_hours"]; q != 2.5 {
		t.Errorf("quantity under the field name: %v", q)
	}
}
