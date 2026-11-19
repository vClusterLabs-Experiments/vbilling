package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A tenant deletes its last cluster but keeps usage that arrives as window
// totals (POST /api/v1/events): reserved GPU machines billed hourly from the
// machine inventory, as security.html recommends for untrusted tenants, or a
// storage exporter. Each hour's total is posted 5 minutes after the hour.
// 61f2a40 refreshes a tenant's last-seen time from usage, but to the end of
// its latest window, not to when it arrived: between two posts the tenant
// looks idle for more than OFFBOARD_GRACE (1 hour), so it is offboarded
// (Lago terminates its subscription and ignores the next hour's usage)
// although its usage arrives every hour.
func TestReview4TenantWhoseUsageArrivesAsHourlyTotalsIsNotOffboarded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})}
	h.c.Reconcile(ctx)
	h.sp.Cursor("fake")
	post := func(end time.Time) {
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 8, SKU: "H100", WindowStart: end.Add(-time.Hour), WindowEnd: end,
			ResourceID: "bm-gpu-17"}
		ev.Finalize()
		if _, _, err := h.sp.AppendIngest("ingest:inventory", []usage.Event{ev}); err != nil {
			t.Fatal(err)
		}
		if err := h.sp.Ack("fake", h.sp.Committed()); err != nil { // delivered within seconds
			t.Fatal(err)
		}
	}
	// Reconcile every minute from 10:00:30 to 14:00:30. The cluster is deleted
	// at 10:30:30; the totals of 10:00-11:00, 11:00-12:00, 12:00-13:00 and
	// 13:00-14:00 are posted at 11:05:30, 12:05:30, 13:05:30 and 14:05:30.
	for m := 1; m <= 4*60; m++ {
		h.now = t0.Add(time.Duration(m) * time.Minute)
		if m == 30 {
			h.disc.clusters = nil
		}
		if h.now.Sub(h.now.Truncate(time.Hour)) == 5*time.Minute+30*time.Second {
			post(h.now.Truncate(time.Hour))
		}
		h.c.Reconcile(ctx)
		if len(h.dest.removed) > 0 {
			t.Fatalf("tenant %v offboarded at %s although its hourly usage keeps arriving (the last total, for the window ending %s, "+
				"arrived at %s): its last-seen time is the end of its latest window, not when that usage arrived",
				h.dest.removed, h.now.Format("15:04:05"), h.now.Truncate(time.Hour).Add(-time.Hour).Format("15:04"),
				h.now.Truncate(time.Hour).Add(-55*time.Minute+30*time.Second).Format("15:04:05"))
		}
	}
}
