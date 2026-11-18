package stripe

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func i64(v int64) *int64   { return &v }
func str(v string) *string { return &v }

// addPrice puts a metered price on the meter with this event name.
func (f *fakeStripe) addPrice(t *testing.T, eventName, id string, amount *int64, meta map[string]string, mutate ...func(*Price)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var meterID string
	for _, m := range f.meters {
		if m.EventName == eventName {
			meterID = m.ID
		}
	}
	if meterID == "" {
		t.Fatalf("no meter %s", eventName)
	}
	p := Price{ID: id, Active: true, Currency: "usd", Metadata: meta, BillingScheme: "per_unit", UnitAmount: amount}
	p.Recurring = &PriceRecurring{UsageType: "metered", Meter: meterID}
	for _, m := range mutate {
		m(&p)
	}
	f.prices = append(f.prices, p)
}

func checkStripe(t *testing.T, a *Adapter, series ...destinations.PriceSeries) map[string]destinations.PriceCoverage {
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

func gpuSeries(a *Adapter, sku string) destinations.PriceSeries {
	ev := usage.Event{Metric: usage.MetricGPUHours, SKU: sku, Region: "us-east"}
	return destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: "acme"}), &ev)
}

func TestStripePriceCheckPerSKUMeter(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Billable()); err != nil {
		t.Fatal(err)
	}
	h100, b200, t4 := gpuSeries(a, "NVIDIA-H100"), gpuSeries(a, "NVIDIA-B200"), gpuSeries(a, "NVIDIA-T4")
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{h100, t4}); err != nil {
		t.Fatal(err)
	}
	f.addPrice(t, "vcluster_gpu_hours__nvidia_h100", "price_h100", i64(249), nil)
	f.addPrice(t, "vcluster_gpu_hours__nvidia_t4", "price_t4", nil, nil, func(p *Price) { p.UnitAmountDecimal = str("0.000") })

	got := checkStripe(t, a, h100, b200, t4)
	if c := got[h100.Key()]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, "price_h100") || !strings.Contains(c.Detail, "STRIPE_AUTO_SUBSCRIBE is off") {
		t.Errorf("H100: %+v", c)
	}
	if c := got[b200.Key()]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, `no Stripe meter "vcluster_gpu_hours__nvidia_b200"`) {
		t.Errorf("B200 before its meter exists: %+v", c)
	}
	if c := got[t4.Key()]; c.Status != destinations.PriceZero {
		t.Errorf("T4 priced at 0: %+v", c)
	}

	// The series appears: vBilling creates its meter (never a price), and the
	// gap names the price Finance has to create.
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{b200}); err != nil {
		t.Fatal(err)
	}
	c := checkStripe(t, a, b200)[b200.Key()]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, `create a metered price on meter "vcluster_gpu_hours__nvidia_b200"`) {
		t.Errorf("B200 with a meter and no price: %+v", c)
	}
	for _, p := range f.prices {
		if strings.Contains(p.ID, "b200") {
			t.Fatal("vBilling created a price")
		}
	}
	f.addPrice(t, "vcluster_gpu_hours__nvidia_b200", "price_b200", nil, nil, func(p *Price) { p.UnitAmountDecimal = str("410.5") })
	if c := checkStripe(t, a, b200)[b200.Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("B200 after its price was added: %+v", c)
	}
}

func TestStripePriceCheckTiersInactiveAndCatalog(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	a.Bootstrap(ctx, usage.Billable())
	free, paid := gpuSeries(a, "FREE"), gpuSeries(a, "PAID")
	a.PrepareSeries(ctx, []destinations.PriceSeries{free, paid})
	f.addPrice(t, "vcluster_gpu_hours__free", "price_free", nil, nil, func(p *Price) {
		p.BillingScheme = "tiered"
		p.Tiers = []PriceTier{{UnitAmount: i64(0)}, {UnitAmountDecimal: str("0"), FlatAmount: i64(0)}}
	})
	f.addPrice(t, "vcluster_gpu_hours__paid", "price_paid", nil, nil, func(p *Price) {
		p.BillingScheme = "tiered"
		p.Tiers = []PriceTier{{UnitAmount: i64(0)}, {FlatAmount: i64(5000)}}
	})
	f.addPrice(t, usage.MetricCPUCoreHours, "price_cpu", i64(3), nil)
	cpuCat := destinations.PriceSeries{Metric: usage.MetricCPUCoreHours, Catalog: true}
	lbCat := destinations.PriceSeries{Metric: usage.MetricLBHours, Catalog: true}
	gpuCat := destinations.PriceSeries{Metric: usage.MetricGPUHours, Catalog: true}

	got := checkStripe(t, a, free, paid, cpuCat, lbCat, gpuCat)
	if got[free.Key()].Status != destinations.PriceZero {
		t.Errorf("all-zero tiers: %+v", got[free.Key()])
	}
	if got[paid.Key()].Status != destinations.PriceCovered {
		t.Errorf("tier with a flat amount: %+v", got[paid.Key()])
	}
	if got[cpuCat.Key()].Status != destinations.PriceCovered || got[lbCat.Key()].Status != destinations.PriceMissing {
		t.Errorf("catalog: cpu %+v lb %+v", got[cpuCat.Key()], got[lbCat.Key()])
	}
	if got[gpuCat.Key()].Status != destinations.PriceCovered {
		t.Errorf("a split meter's price covers the metric: %+v", got[gpuCat.Key()])
	}

	f.mu.Lock()
	for _, m := range f.meters {
		if m.EventName == "vcluster_gpu_hours__paid" {
			m.Status = "inactive"
		}
	}
	f.mu.Unlock()
	if c := checkStripe(t, a, paid)[paid.Key()]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "deactivated") {
		t.Errorf("inactive meter: %+v", c)
	}
}

func TestStripePriceCheckPlanTags(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "", func(c *config.Config) { c.StripeAutoSubscribe = true })
	ctx := context.Background()
	a.Bootstrap(ctx, usage.Billable())
	h100 := gpuSeries(a, "H100")
	if h100.Plan != "standard" {
		t.Fatalf("plan %q", h100.Plan)
	}
	a.PrepareSeries(ctx, []destinations.PriceSeries{h100})
	f.addPrice(t, "vcluster_gpu_hours__h100", "price_enterprise", i64(199), map[string]string{metaPlan: "enterprise"})
	c := checkStripe(t, a, h100)[h100.Key()]
	if c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, "vbilling_plan=standard") {
		t.Errorf("price for another plan only: %+v", c)
	}
	f.addPrice(t, "vcluster_gpu_hours__h100", "price_standard", i64(249), map[string]string{metaPlan: "standard"})
	if c := checkStripe(t, a, h100)[h100.Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("tagged price: %+v", c)
	}
	if !a.RefreshTenantsOnPriceChange() {
		t.Error("auto-subscribe needs tenants refreshed when prices appear")
	}
	// The check refreshed the price list subscriptions are built from.
	ps, err := a.planPrices(ctx, "standard", "usd")
	if err != nil || len(ps) != 1 || ps[0].ID != "price_standard" {
		t.Errorf("plan prices %v %v", ps, err)
	}
}

// TestStripeMockPriceCheck runs the check and series preparation against
// stripe-mock, which validates every request against Stripe's OpenAPI spec:
//
//	docker run -d -p 12111:12111 stripe/stripe-mock
//	STRIPE_MOCK_URL=http://localhost:12111 go test ./internal/destinations/stripe -run StripeMock
func TestStripeMockPriceCheck(t *testing.T) {
	base := os.Getenv("STRIPE_MOCK_URL")
	if base == "" {
		t.Skip("STRIPE_MOCK_URL not set")
	}
	cfg := &config.Config{StripeSplitMetersBy: []string{"sku", "region"}, DefaultPlanCode: "standard", BillingCurrency: "usd", StripeAutoSubscribe: true}
	a := New(NewClient(base, "sk_test_123", "2026-09-30.endive", 0), cfg, kv.Memory())
	ctx := context.Background()
	s := destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: "acme"}), &usage.Event{Metric: usage.MetricGPUHours, SKU: "NVIDIA-H100", Region: "us-east"})
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{s}); err != nil {
		t.Fatalf("prepare (create meter) rejected by stripe-mock: %v", err)
	}
	out, err := a.CheckPrices(ctx, []destinations.PriceSeries{s, {Plan: "standard", Metric: usage.MetricCPUCoreHours, Catalog: true}})
	if err != nil {
		t.Fatalf("check rejected by stripe-mock: %v", err)
	}
	for _, c := range out {
		// stripe-mock returns fixtures, so only the request shapes are meaningful.
		t.Logf("%s: %s %s", destinations.DescribeSeries(c.Series), c.Status, c.Detail)
		if c.Status == "" {
			t.Errorf("no status for %+v", c.Series)
		}
	}
}
