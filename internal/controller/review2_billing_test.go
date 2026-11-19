package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestReview2WindowsRealignAfterACollectionIntervalChange: CollectDue walks
// windows from the ledger watermark in steps of COLLECTION_INTERVAL. After
// the interval grows (60s to 5m), a watermark that is not a multiple of the
// new size (10:03) keeps every later window off the wall-clock grid
// ([10:03, 10:08), [10:08, 10:13), ...), so no window ever equals the
// latest due window and none is marked Latest. With CPU_MEMORY_BASIS=usage
// (or max) the collector then treats every window as collected late and
// bills no metrics-server CPU or memory at all, for as long as it runs (the
// records aggregator loses its records the same way, see
// records.TestReview2RecordsSurviveACollectionIntervalChange).
func TestReview2WindowsRealignAfterACollectionIntervalChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	base := t0.Truncate(time.Hour) // 10:00
	h.now = base.Add(3*time.Minute + 10*time.Second)
	h.c.CollectDue(ctx)
	if wm, _ := h.sp.Watermark(spool.SourceCollector); !wm.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("setup: watermark %s", wm)
	}
	// Restarted with COLLECTION_INTERVAL=5m.
	h.c.cfg.CollectionInterval = 5 * time.Minute
	h.coll.calls = nil
	for _, at := range []time.Duration{10, 15, 20, 25} {
		h.now = base.Add(at*time.Minute + 10*time.Second)
		h.c.CollectDue(ctx)
	}
	latest := 0
	var ends []string
	for _, call := range h.coll.calls {
		for _, w := range call {
			ends = append(ends, w.End.Format("15:04"))
			if w.Latest {
				latest++
			}
		}
	}
	if latest == 0 {
		t.Fatalf("no window collected after the interval change was the live (Latest) one; window ends %v", ends)
	}
}

// TestReview2OffboardingWaitsForUndeliveredUsage: a tenant is offboarded
// from every destination OFFBOARD_GRACE (1h) after its last cluster is
// gone, whether or not the destination has received its usage yet. Lago's
// RemoveTenant terminates the subscription, which issues the final invoice,
// and Lago ingests events of a terminated subscription "but ignores them
// during post-processing" (Lago's ingestion guide). Usage still waiting in
// vBilling is then never invoiced: a delivery backlog after a Lago outage
// longer than the grace period, or events held under PRICE_CHECK=hold
// (up to PRICE_CHECK_MAX_HOLD, 28 days), released after the termination.
func TestReview2OffboardingWaitsForUndeliveredUsage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	tenant := "vcluster-team-a-train"

	// The destination is down: its cursor stays before the tenant's last
	// window, which the controller commits to the ledger.
	before := h.sp.Cursor("fake")
	ev := usage.Event{Tenant: tenant, Metric: usage.MetricInstanceHours, Quantity: 1.0 / 60,
		WindowStart: t0.Truncate(time.Minute).Add(-time.Minute), WindowEnd: t0.Truncate(time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: tenant}}
	ev.Finalize()
	if err := h.sp.AppendWindow(spool.SourceCollector, ev.WindowEnd, []usage.Event{ev}, ""); err != nil {
		t.Fatal(err)
	}
	if h.sp.Cursor("fake") != before || before >= h.sp.Committed() {
		t.Fatal("setup: the destination must not have the tenant's usage yet")
	}

	// The cluster is deleted; an hour later the destination is still behind.
	h.disc.clusters = nil
	h.now = h.now.Add(61 * time.Minute)
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 0 {
		t.Fatalf("offboarded %v from a destination that has not received the tenant's last usage (cursor %d, last event at seq %d)",
			h.dest.removed, h.sp.Cursor("fake"), h.sp.Committed())
	}
}
