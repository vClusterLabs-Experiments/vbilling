package stripe

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Coverage must use the same currency constraint as auto-subscription.
func TestReviewPriceCoverageRequiresSubscribableCurrency(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "", func(c *config.Config) {
		c.StripeAutoSubscribe = true
		c.BillingCurrency = "eur"
	})
	ctx := context.Background()
	ser := gpuSeries(a, "H100")
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{ser}); err != nil {
		t.Fatal(err)
	}
	f.addPrice(t, "vcluster_gpu_hours__h100", "price_usd", i64(249), map[string]string{metaPlan: "standard"})
	coverage := checkStripe(t, a, ser)[ser.Key()]
	prices, err := a.planPrices(ctx, "standard", "eur")
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 0 {
		t.Fatalf("fixture unexpectedly has an EUR price: %v", prices)
	}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if len(f.subs) != 0 {
		t.Fatalf("fixture unexpectedly created a subscription: %v", f.subs)
	}
	if coverage.Status != destinations.PriceMissing {
		t.Fatalf("coverage=%s (%s), but no price matches the EUR subscription and no subscription was created", coverage.Status, coverage.Detail)
	}
}

// A tenant whose currency overrides billing.currency is its own series, and
// only prices in its currency cover it.
func TestPriceCoverageFollowsTenantCurrency(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "", func(c *config.Config) { c.StripeAutoSubscribe = true })
	ctx := context.Background()
	eur := usage.Tenant{ID: "acme", Currency: "eur"}
	if got := a.PricePlan(eur); got != "standard (EUR)" {
		t.Fatalf("PricePlan = %q, want the plan with its currency", got)
	}
	if got := a.PricePlan(usage.Tenant{ID: "globex", Currency: "USD"}); got != "standard" {
		t.Fatalf("PricePlan in the default currency = %q, want standard", got)
	}
	ev := usage.Event{Metric: usage.MetricGPUHours, SKU: "H100", Region: "us-east"}
	ser := destinations.SeriesOf(a, a.PricePlan(eur), &ev)
	if err := a.PrepareSeries(ctx, []destinations.PriceSeries{ser}); err != nil {
		t.Fatal(err)
	}
	f.addPrice(t, "vcluster_gpu_hours__h100", "price_usd", i64(249), map[string]string{metaPlan: "standard"})
	if c := checkStripe(t, a, ser)[ser.Key()]; c.Status != destinations.PriceMissing {
		t.Fatalf("EUR tenant covered by a USD price: %s (%s)", c.Status, c.Detail)
	}
	f.addPrice(t, "vcluster_gpu_hours__h100", "price_eur", i64(229), map[string]string{metaPlan: "standard"}, func(p *Price) { p.Currency = "eur" })
	if c := checkStripe(t, a, ser)[ser.Key()]; c.Status != destinations.PriceCovered {
		t.Fatalf("EUR tenant with an EUR price: %s (%s)", c.Status, c.Detail)
	}
}
