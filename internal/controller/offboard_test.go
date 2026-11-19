package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A departed tenant is offboarded from a destination only once that
// destination has its last usage: the cursor is past the tenant's last
// ledger event, including events committed after the cluster was deleted
// (its last window, late records), and none of its events is held.
func TestOffboardingWaitsForDeliveryAndHeldUsage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	held, err := pipeline.OpenHeld(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	h.c.UseHeld(held)
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	tenant := "vcluster-team-a-train"
	h.sp.Cursor("fake") // the destination's cursor exists, at the ledger head

	// The cluster is deleted; its last window is committed afterwards.
	h.disc.clusters = nil
	h.c.Reconcile(ctx)
	end := t0.Truncate(time.Minute)
	ev := usage.Event{Tenant: tenant, Metric: usage.MetricInstanceHours, Quantity: 0.5 / 60, WindowStart: end.Add(-time.Minute), WindowEnd: end,
		Dimensions: map[string]string{usage.DimTenantCluster: tenant}}
	ev.Finalize()
	if err := h.sp.AppendWindow(spool.SourceCollector, end, []usage.Event{ev}, ""); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(61 * time.Minute)
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 0 {
		t.Fatal("offboarded before the destination read the tenant's last window")
	}

	// The dispatcher reads it, but holds it until its price exists.
	if _, err := held.Add("fake", []pipeline.HeldEntry{{Seq: 1, At: h.now, Series: "instance-hours", Reason: "no price", Event: ev}}); err != nil {
		t.Fatal(err)
	}
	if err := h.sp.Ack("fake", h.sp.Committed()); err != nil {
		t.Fatal(err)
	}
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 0 {
		t.Fatal("offboarded while the tenant's usage is held from the destination")
	}

	// The price appears and the held event is delivered.
	entries, next, err := held.Peek("fake", "instance-hours", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("held: %v %v", entries, err)
	}
	if err := held.Release("fake", "instance-hours", entries[0].Seq, 1, next); err != nil {
		t.Fatal(err)
	}
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 1 {
		t.Fatalf("removed = %v, want the tenant offboarded once its usage is delivered", h.dest.removed)
	}
	if _, ok := h.reg.Get(tenant); ok {
		t.Fatal("tenant still registered after offboarding")
	}
}
