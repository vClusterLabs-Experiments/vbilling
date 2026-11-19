package metronome

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Metronome documents that billable metrics are not retroactive: "If usage
// is sent before a streaming metric is defined, it isn't attributed to the
// metric by default" (a reflow needs Metronome support), and "billable
// metrics only match usage events sent after the billable metric is
// created" (guides/implement-metronome/core-concepts/create-billable-metrics,
// guides/get-started/developer-sdks). Without METRONOME_PROVISION, Bootstrap
// creates nothing and never holds delivery ("Metronome keeps that usage but
// bills none of it until one does"), and the install steps in
// docs/metronome.html start vBilling first and add the billable metrics
// afterwards. With the default PRICE_CHECK=warn, the usage of every metric
// Finance has not configured yet (at rollout, or a metric added later, such
// as a new custom or token metric) goes out and is never billed. The fake
// attributes it to a billable metric created later, so the tests that cover
// this path (TestDefaultModeNeverWritesToMetronome) pass.
func TestReview3UsageWaitsForABillableMetricThatCountsIt(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f)
	ctx := context.Background()
	if _, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme"); err != nil {
		t.Fatal(err)
	}
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}
	sentBefore := len(f.Events())
	// Finance applies the definition `vbilling metronome billable-metrics` prints.
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	if sentBefore > 0 {
		t.Errorf("%d event(s) of %s reached Metronome before any billable metric summed them: Metronome never attributes them to a billable metric created later",
			sentBefore, usage.MetricGPUHours)
	}
	day := now.Truncate(24 * time.Hour)
	totals, err := a.RecordedTotals(ctx, destinations.TotalsQuery{From: day, To: day.Add(24 * time.Hour),
		Tenants: []usage.Tenant{{ID: "tenant-acme"}}, Metrics: []string{usage.MetricGPUHours}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tot := range totals {
		if tot.Quantity > 0 && sentBefore > 0 {
			t.Errorf("the fake bills %v GPU-hours sent before their billable metric existed; Metronome documents 0 (the fake is retroactive, Metronome is not)", tot.Quantity)
		}
	}
}

// ToIngest builds the event's properties from the quantity ("value", which
// every billable metric sums), then copies the event's dimensions over them.
// Dimension keys only have to match [a-z][a-z0-9_]*, and an emitter chooses
// them: the native records format and POST /api/v1/events take a
// "dimensions" object as sent (with a tenant-bound token, the tenant's own
// gateway sends it). A dimension named "value" then replaces the quantity
// Metronome sums, while vBilling's ledger, usage API and safeguard keep the
// real one; "unit", "window_start" or "sku" are replaced the same way.
func TestReview3DimensionsNeverReplaceTheQuantitySentToMetronome(t *testing.T) {
	a := New(nil, &config.Config{}, kv.Memory(), nil)
	ws := now.Add(-time.Minute)
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, WindowStart: ws, WindowEnd: now,
		SKU: "NVIDIA-H100-80GB-HBM3", Dimensions: map[string]string{"value": "0", "sku": "free-tier"}}
	ev.Finalize()
	props := a.ToIngest(&ev).Properties
	if props["value"] != "0.133333333" {
		t.Errorf("Metronome sums value=%q for an event of %v GPU-hours", props["value"], ev.Quantity)
	}
	if props["sku"] != "NVIDIA-H100-80GB-HBM3" {
		t.Errorf("Metronome prices sku=%q for usage of SKU %s", props["sku"], ev.SKU)
	}
}
