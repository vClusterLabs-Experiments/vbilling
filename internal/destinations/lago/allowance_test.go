package lago

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// With storage allowances (ALLOWANCES_FILE) storage is billed as
// vcluster_storage_overage_gb_hours: Lago gets a billable metric and a plan
// charge for it, and only its events; the whole capacity is informational.
func TestTheStorageOverageIsBilledInLago(t *testing.T) {
	usage.UseStorageAllowances(true)
	t.Cleanup(func() { usage.UseStorageAllowances(false) })
	f := newFakeLago(t)
	a := newTestAdapter(t, f, nil)
	if err := a.Bootstrap(context.Background(), builtinCatalog()); err != nil {
		t.Fatal(err)
	}
	if f.metrics[usage.MetricStorageOverageGBHours] == nil {
		t.Fatal("no billable metric for the storage overage")
	}
	if c := f.chargeFor("vcluster-standard", usage.MetricStorageOverageGBHours); c == nil {
		t.Error("the default plan has no charge for the storage overage")
	}
	if !a.Accepts(usage.MetricStorageOverageGBHours) || a.Accepts(usage.MetricStorageGBHours) {
		t.Error("Lago must take the overage and skip the whole capacity")
	}
	ws := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricStorageOverageGBHours, Quantity: 0.5, SKU: "fast", WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	ev.Finalize()
	if got := ToEvent(&ev).Properties[usage.MetricStorageOverageGBHours]; got != 0.5 {
		t.Errorf("the quantity must be under the field name of the billable metric: %v", ToEvent(&ev).Properties)
	}
	for _, m := range lagoMetrics(usage.Catalog()) {
		if m.Code == usage.MetricStorageOverageGBHours && (m.FieldName != m.Code || m.AggregationType != "sum_agg") {
			t.Errorf("overage billable metric = %+v", m)
		}
	}

	// Without allowances nothing is created for it.
	usage.UseStorageAllowances(false)
	for _, m := range lagoMetrics(usage.Catalog()) {
		if m.Code == usage.MetricStorageOverageGBHours {
			t.Error("a billable metric for the overage without an allowance")
		}
	}
}
