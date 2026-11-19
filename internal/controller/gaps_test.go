package controller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// countMetric counts the ledger's events of a metric.
func countMetric(t *testing.T, sp *spool.Spool, metric string) int {
	t.Helper()
	recs, err := sp.Read(0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Event != nil && r.Event.Metric == metric {
			n++
		}
	}
	return n
}

// The windows a cluster's own API missed are saved before the windows are
// committed. When they cannot be saved, nothing is committed (the windows
// are read again), the source fails, and once they can be, the windows
// commit with their gap.
func TestTenantAPIGapsAreSavedBeforeTheWindowsCommit(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "gaps")
	store, err := kv.Open(filepath.Join(dir, "tenant-api-gaps.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.c.gaps = store
	reg := health.New(nil)
	reg.SetLogger(func(string, ...any) {})
	h.c.UseHealth(reg)
	gc := &gapColl{tenantDown: true}
	h.c.coll = gc
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-p", "private", nil)}
	h.c.Reconcile(ctx)

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	h.c.CollectDue(ctx)
	if _, ok := h.sp.Watermark(spool.SourceCollector); ok {
		t.Fatal("a window was committed although the gap of its cluster's API could not be saved")
	}
	failing := false
	for _, s := range reg.List() {
		failing = failing || s.Name == "tenant-api-gaps" && s.Status == health.Failing
	}
	if !failing {
		t.Fatalf("sources %+v, want tenant-api-gaps failing", reg.List())
	}

	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	h.c.CollectDue(ctx)
	wm, ok := h.sp.Watermark(spool.SourceCollector)
	if !ok {
		t.Fatal("the window was not committed once its gap could be saved")
	}
	if g, ok := parseGaps(h.c.gaps, "vcluster-team-p-private", time.Minute); !ok || !g[len(g)-1].end.Equal(wm) {
		t.Fatalf("gap %v after the commit through %s", g, wm)
	}
	for _, s := range reg.List() {
		if s.Name == "tenant-api-gaps" && s.Status != health.OK {
			t.Fatalf("tenant-api-gaps %s after it could be saved again", s.Status)
		}
	}
}

// A gap saved for windows whose commit never happened (vBilling stopped
// between the two) is dropped: those windows are read again, and with the
// cluster's API answering they commit whole and are never filled.
func TestGapOfAWindowNeverCommittedIsNotFilled(t *testing.T) {
	h := newHarness(t)
	gc := &gapColl{}
	h.c.coll = gc
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-p", "private", nil)}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx) // W1 committed, API up
	wm, _ := h.sp.Watermark(spool.SourceCollector)
	// Saved by a run that stopped before committing W2.
	if err := h.c.gaps.Set("vcluster-team-p-private", tenantGaps{{start: wm, end: wm.Add(time.Minute)}}.String()); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx) // W2 collected again, API up
	if len(gc.fills) != 0 {
		t.Fatalf("filled %v, a window committed with its cluster's API read", gc.fills)
	}
	if _, ok := h.c.gaps.Get("vcluster-team-p-private"); ok {
		t.Fatal("the gap of a window never committed was kept")
	}
}

// A gap v0.3.0 saved ("first|last") whose windows v0.3.0 already filled
// (it stopped between the fill and forgetting the gap) is filled with the
// IDs v0.3.0 gave: the ledger drops them as duplicates, nothing bills twice.
func TestGapFromV030FilledAlreadyBillsOnce(t *testing.T) {
	h := newHarness(t)
	gc := &gapColl{}
	h.c.coll = gc
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-p", "private", nil)}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	h.now = h.now.Add(2 * time.Minute)
	h.c.CollectDue(ctx)
	w1, w2 := t0.Truncate(time.Minute), t0.Truncate(time.Minute).Add(time.Minute)
	// v0.3.0's fill of W1 and W2: unsalted IDs.
	tg := metrics.Target{Cluster: cluster("team-p", "private", nil), Tenant: "vcluster-team-p-private"}
	filled, _, _ := gc.CollectTenantAPI(ctx, []metrics.Target{tg}, []metrics.Window{{Start: w1.Add(-time.Minute), End: w1}, {Start: w1, End: w2}})
	var evs []usage.Event
	for _, w := range []time.Time{w1, w2} {
		evs = append(evs, filled[w]...)
	}
	if _, _, err := h.sp.AppendIngest(SourceTenantFill, evs); err != nil {
		t.Fatal(err)
	}
	gc.fills = nil
	before := countMetric(t, h.sp, usage.MetricPrivateNodeHours)
	if err := h.c.gaps.Set("vcluster-team-p-private", w1.Format(time.RFC3339)+"|"+w2.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx)
	if len(gc.fills) != 1 {
		t.Fatalf("fills %v, want the v0.3.0 gap filled once", gc.fills)
	}
	if n := countMetric(t, h.sp, usage.MetricPrivateNodeHours); n != before {
		t.Fatalf("the ledger has %d private-node events after the upgrade's fill, want still %d: v0.3.0 had filled them", n, before)
	}
	if _, ok := h.c.gaps.Get("vcluster-team-p-private"); ok {
		t.Fatal("the filled gap was kept")
	}
}
