package lago

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func lagoCheck(t *testing.T, a *Adapter, series ...destinations.PriceSeries) map[string]destinations.PriceCoverage {
	t.Helper()
	out, err := a.CheckPrices(context.Background(), series)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]destinations.PriceCoverage{}
	for _, c := range out {
		m[c.Series.Key()] = c
	}
	return m
}

func gpu(sku string) destinations.PriceSeries {
	return destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricGPUHours, SKU: sku}
}

// priceSKU sets a charge filter for sku on the default plan's GPU charge, as
// Finance would in the Lago UI.
func priceSKU(f *fakeLago, sku, amount string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.metrics[usage.MetricGPUHours]
	found := false
	for i := range m.Filters {
		if m.Filters[i].Key == "sku" {
			m.Filters[i].Values = append(m.Filters[i].Values, sku)
			found = true
		}
	}
	if !found {
		m.Filters = append(m.Filters, MetricFilter{Key: "sku", Values: []string{sku}})
	}
	for _, c := range f.plans["vcluster-standard"].Charges {
		if c["billable_metric_code"] == usage.MetricGPUHours {
			fs, _ := c["filters"].([]any)
			c["filters"] = append(fs, map[string]any{"invoice_display_name": nil,
				"properties": map[string]any{"amount": amount}, "values": map[string]any{"sku": []any{sku}}})
		}
	}
}

func TestLagoPriceCheckSKUs(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, nil)
	priceSKU(f, h100, "2.49")
	priceSKU(f, l4, "0")
	a := newTestAdapter(t, f, nil)

	cpu := destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricCPUCoreHours}
	got := lagoCheck(t, a, gpu(h100), gpu(l4), gpu(a100), cpu)
	if c := got[gpu(h100).Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("H100: %+v", c)
	}
	if c := got[gpu(l4).Key()]; c.Status != destinations.PriceZero || !strings.Contains(c.Detail, "priced at 0") {
		t.Errorf("L4 with its own filter at 0 is intentional: %+v", c)
	}
	c := got[gpu(a100).Key()]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "sku="+h100) {
		t.Fatalf("A100 falls to the default price 0 while H100 is priced: %+v", c)
	}
	if !strings.Contains(c.Hint, "price sku="+a100) || !strings.Contains(c.Hint, `plan "vcluster-standard"`) {
		t.Errorf("hint: %q", c.Hint)
	}
	if c := got[cpu.Key()]; c.Status != destinations.PriceZero {
		t.Errorf("a whole metric at 0 is information: %+v", c)
	}

	// Delivery splits the SKU out with the charge's price (0): still a gap.
	ws := time.Now().Add(-time.Hour).Truncate(time.Minute)
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: a100, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	ev.Finalize()
	if err := a.SendEvents(context.Background(), []usage.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if fs := f.chargeFor("vcluster-standard", usage.MetricGPUHours)["filters"].([]any); len(fs) != 3 {
		t.Fatalf("delivery should have split out %s: %v", a100, fs)
	}
	c = lagoCheck(t, a, gpu(a100))[gpu(a100).Key()]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "copied the default price 0") {
		t.Errorf("a split-out filter that copies 0 is still a gap: %+v", c)
	}

	// A price card entry of 0 makes the zero deliberate.
	carded := New(f.client(), a.cfg, a.state, PriceCard{usage.MetricGPUHours: {SKU: map[string]Amount{a100: "0"}}})
	if c := lagoCheck(t, carded, gpu(a100))[gpu(a100).Key()]; c.Status != destinations.PriceZero {
		t.Errorf("a SKU the price card prices at 0 is zero, not a gap: %+v", c)
	}

	// Finance prices it: the gap clears.
	f.mu.Lock()
	for _, c := range f.plans["vcluster-standard"].Charges {
		if c["billable_metric_code"] != usage.MetricGPUHours {
			continue
		}
		for _, fl := range c["filters"].([]any) {
			m := fl.(map[string]any)
			if m["values"].(map[string]any)["sku"].([]any)[0] == a100 {
				m["properties"] = map[string]any{"amount": "1.89"}
			}
		}
	}
	f.mu.Unlock()
	if c := lagoCheck(t, a, gpu(a100))[gpu(a100).Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("after the price was set: %+v", c)
	}
}

func TestLagoPriceCheckDefaultPriceAndCatalog(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, func(metric string, c map[string]any) {
		if metric == usage.MetricGPUHours {
			c["properties"] = map[string]any{"amount": "1.50"}
		}
		if metric == usage.MetricStorageGBHours {
			c["charge_model"] = "graduated"
			c["properties"] = map[string]any{"graduated_ranges": []any{
				map[string]any{"from_value": 0, "to_value": 100, "per_unit_amount": "0", "flat_amount": "0"},
				map[string]any{"from_value": 101, "to_value": nil, "per_unit_amount": "0.02", "flat_amount": "0"},
			}}
		}
	})
	a := newTestAdapter(t, f, nil)
	storage := destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricStorageGBHours, SKU: "ssd"}
	lbCat := destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricLBHours, Catalog: true}
	got := lagoCheck(t, a, gpu("NEW-SKU"), storage, lbCat)
	if c := got[gpu("NEW-SKU").Key()]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, "default price") {
		t.Errorf("a non-zero default price covers new SKUs: %+v", c)
	}
	if c := got[storage.Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("graduated ranges with a priced range: %+v", c)
	}
	if c := got[lbCat.Key()]; c.Status != destinations.PriceZero {
		t.Errorf("lb catalog: %+v", c)
	}
}

func TestLagoPriceCheckMissingPieces(t *testing.T) {
	f := newFakeLago(t)
	f.addMetric(usage.MetricGPUHours, "GPU")
	f.addMetric(usage.MetricCPUCoreHours, "CPU")
	f.addPlan("vcluster-standard", f.charge(usage.MetricGPUHours, "1"))
	a := newTestAdapter(t, f, nil)
	noMetric := destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricLBHours}
	noCharge := destinations.PriceSeries{Plan: "vcluster-standard", Metric: usage.MetricCPUCoreHours}
	noPlan := destinations.PriceSeries{Plan: "enterprise", Metric: usage.MetricGPUHours, SKU: "x"}
	got := lagoCheck(t, a, noMetric, noCharge, noPlan)
	for _, c := range []struct {
		s    destinations.PriceSeries
		want string
	}{{noMetric, "no billable metric"}, {noCharge, "has no charge for"}, {noPlan, `no plan "enterprise"`}} {
		cov := got[c.s.Key()]
		if cov.Status != destinations.PriceMissing || !strings.Contains(cov.Detail, c.want) || cov.Hint == "" {
			t.Errorf("%s: %+v", c.want, cov)
		}
	}
}

func TestLagoPrepareSeriesAddsMetricValuesOnly(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, nil)
	priceSKU(f, h100, "2.49")
	a := newTestAdapter(t, f, nil)
	before := f.planJSON("vcluster-standard")
	if err := a.PrepareSeries(context.Background(), []destinations.PriceSeries{gpu(a100), gpu(h100)}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	vals := f.metrics[usage.MetricGPUHours].Filters[0].Values
	f.mu.Unlock()
	if strings.Join(vals, ",") != h100+","+a100 {
		t.Errorf("billable metric sku values: %v", vals)
	}
	if len(f.requests("PUT", "/api/v1/plans")) != 0 {
		t.Error("preparing a series changed a plan")
	}
	after := f.planJSON("vcluster-standard")
	if len(after["charges"].([]any)) != len(before["charges"].([]any)) {
		t.Error("charges changed")
	}
	if p := a.PricePlan(usage.Tenant{ID: "x", Plan: "enterprise"}); p != "enterprise" {
		t.Errorf("plan %q", p)
	}
}
