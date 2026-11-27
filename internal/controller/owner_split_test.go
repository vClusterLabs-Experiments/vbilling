package controller

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// billedColl bills each target the part of each window it is billed for
// (outside Target.NotBilled), as control plane hours: what the collectors
// do with every metric.
type billedColl struct {
	mu    sync.Mutex
	calls [][]metrics.Target
}

func (f *billedColl) Collect(_ context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error) {
	f.mu.Lock()
	f.calls = append(f.calls, targets)
	f.mu.Unlock()
	out := map[time.Time][]usage.Event{}
	for _, w := range windows {
		for _, tg := range targets {
			billed := w.End.Sub(w.Start) - pause.Overlap(tg.NotBilled(), time.Time{}, time.Time{}, w.Start, w.End)
			if billed <= 0 {
				continue
			}
			e := usage.Event{Tenant: tg.Tenant, Project: tg.Project, Metric: usage.MetricInstanceHours, Quantity: billed.Hours(),
				WindowStart: w.Start, WindowEnd: w.End, Dimensions: map[string]string{usage.DimTenantCluster: tg.Cluster.ExternalID()}}
			e.Finalize()
			out[w.End] = append(out[w.End], e)
		}
	}
	return out, metrics.Stats{}, nil
}

// datedOwners is a cluster's owner history as vCluster Platform mode keeps
// it: earlier bindings, each until the moment it ended, then the cluster's
// own.
type datedOwners []datedEpoch

type datedEpoch struct {
	until time.Time
	b     discovery.PlatformBinding
}

func (o datedOwners) OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool) {
	last := end.Add(-time.Nanosecond)
	for _, e := range o {
		if !last.Before(e.until) {
			continue
		}
		if e.b.Same(cl.Platform) {
			return cl, false
		}
		out := cl
		out.Annotations = map[string]string{}
		out.ApplyPlatform(e.b)
		return out, true
	}
	return cl, false
}

func (o datedOwners) ChangesIn(_ discovery.TenantCluster, start, end time.Time) []time.Time {
	var out []time.Time
	for _, e := range o {
		if e.until.After(start) && e.until.Before(end) {
			out = append(out, e.until)
		}
	}
	return out
}

var (
	ownedByAcme   = discovery.PlatformBinding{Instance: "mover", InstanceUID: "uid-vci", Project: "train", ProjectUID: "uid-project", Tenant: "acme", TenantUID: "uid-acme"}
	ownedByGlobex = discovery.PlatformBinding{Instance: "mover", InstanceUID: "uid-vci", Project: "train", ProjectUID: "uid-project", Tenant: "globex", TenantUID: "uid-globex"}
)

func boundCluster(b discovery.PlatformBinding) discovery.TenantCluster {
	cl := cluster("vc-mover", "mover", nil)
	cl.Annotations = map[string]string{}
	cl.ApplyPlatform(b)
	return cl
}

// ledgerWindow returns the events the ledger holds for the window ending
// at end.
func ledgerWindow(t *testing.T, h *harness, end time.Time) []usage.Event {
	t.Helper()
	var out []usage.Event
	if err := h.sp.Scan(time.Time{}, end.Add(time.Hour), func(ev *usage.Event) error {
		if ev.WindowEnd.Equal(end) {
			out = append(out, *ev)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func seconds(evs []usage.Event) map[string]float64 {
	out := map[string]float64{}
	for _, e := range evs {
		out[e.Tenant] += math.Round(e.Quantity * 3600)
	}
	return out
}

func checkUnique(t *testing.T, evs []usage.Event) {
	t.Helper()
	seen := map[string]bool{}
	for _, e := range evs {
		if seen[e.ID] {
			t.Errorf("event ID %s twice (%s)", e.ID, e.Tenant)
		}
		seen[e.ID] = true
	}
}

// The tester's case: a Project is handed to another Tenant 15 seconds into
// a window. The window is split there: 15 seconds for the old Tenant, 45
// for the new one, each with its own Platform UIDs, adding up to the
// window. Before, the whole window stayed with the old Tenant.
func TestMoveInTheMiddleOfAWindowSplitsIt(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TenantSource = "platform" })
	coll := &billedColl{}
	h.c.coll = coll
	ctx := context.Background()
	start := t0.Truncate(time.Minute)
	moved := start.Add(15 * time.Second)
	h.c.UseOwners(datedOwners{{until: moved, b: ownedByAcme}})
	h.disc.clusters = []discovery.TenantCluster{boundCluster(ownedByGlobex)}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx) // the window before the move
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx)

	if got := seconds(ledgerWindow(t, h, start)); got["tenant-acme"] != 60 || len(got) != 1 {
		t.Errorf("the window before the move: %v, want 60s for tenant-acme", got)
	}
	evs := ledgerWindow(t, h, start.Add(time.Minute))
	checkUnique(t, evs)
	if got := seconds(evs); got["tenant-acme"] != 15 || got["tenant-globex"] != 45 {
		t.Errorf("the window of the move: %v, want 15s for tenant-acme and 45s for tenant-globex", got)
	}
	for _, e := range evs {
		want := map[string]string{"tenant-acme": "uid-acme", "tenant-globex": "uid-globex"}[e.Tenant]
		if e.Properties["platform_tenant_uid"] != want {
			t.Errorf("%s's event carries tenant UID %v, want %s", e.Tenant, e.Properties["platform_tenant_uid"], want)
		}
	}
	// One collection run for the split window, with one target per owner.
	last := coll.calls[len(coll.calls)-1]
	if len(last) != 2 || last[0].Tenant != "tenant-acme" || last[1].Tenant != "tenant-globex" {
		t.Fatalf("targets of the split window: %+v", last)
	}
}

// A move reverted inside one window makes three parts: the old owner's
// before and after, billed as one event, and the other owner's between.
func TestRevertedMoveMakesThreeParts(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TenantSource = "platform" })
	h.c.coll = &billedColl{}
	ctx := context.Background()
	start := t0.Truncate(time.Minute)
	// Handed to globex 10 seconds into the window ending start+1m, back to
	// acme 30 seconds later.
	h.c.UseOwners(datedOwners{
		{until: start.Add(10 * time.Second), b: ownedByAcme},
		{until: start.Add(40 * time.Second), b: ownedByGlobex},
	})
	h.disc.clusters = []discovery.TenantCluster{boundCluster(ownedByAcme)}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx)

	evs := ledgerWindow(t, h, start.Add(time.Minute))
	checkUnique(t, evs)
	if len(evs) != 2 {
		t.Fatalf("events: %+v, want one per tenant", evs)
	}
	if got := seconds(evs); got["tenant-acme"] != 30 || got["tenant-globex"] != 30 {
		t.Errorf("parts: %v, want 10s + 20s for tenant-acme and 30s for tenant-globex", got)
	}
}

// A cluster billed by its tenant label: discovery reads it every 30
// seconds, so a relabel is dated at the discovery that first sees it, and
// the window is split there.
func TestLabelChangeIsDatedAtTheDiscoveryThatSeesIt(t *testing.T) {
	h := newHarness(t)
	h.c.coll = &billedColl{}
	ctx := context.Background()
	start := t0.Truncate(time.Minute)
	label := func(tenant string) []discovery.TenantCluster {
		return []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": tenant})}
	}
	h.disc.clusters = label("acme")
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)

	// Relabeled; the discovery 20 seconds into the next window sees it.
	h.disc.clusters = label("globex")
	h.now = start.Add(80 * time.Second)
	h.c.Reconcile(ctx)
	h.now = start.Add(2*time.Minute + 30*time.Second)
	h.c.CollectDue(ctx)

	if got := seconds(ledgerWindow(t, h, start.Add(time.Minute))); got["acme"] != 60 || len(got) != 1 {
		t.Errorf("the window before the discovery that saw it: %v, want 60s for acme", got)
	}
	evs := ledgerWindow(t, h, start.Add(2*time.Minute))
	checkUnique(t, evs)
	if got := seconds(evs); got["acme"] != 20 || got["globex"] != 40 {
		t.Errorf("the window it was seen in: %v, want 20s for acme and 40s for globex", got)
	}
	if acme, _ := h.reg.Get("acme"); len(acme.Clusters) != 0 {
		t.Errorf("the old tenant still lists the cluster: %v", acme.Clusters)
	}
	if g, _ := h.reg.Get("globex"); len(g.Clusters) != 1 {
		t.Errorf("the new tenant does not list the cluster: %v", g.Clusters)
	}
}

// The history of a cluster billed by its label is persisted: a relabel
// made while vBilling was down is dated at the first discovery after the
// start, the windows backfilled before it stay with the old tenant, and
// the window of that discovery is split there.
func TestLabelHistorySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(c *config.Config) { c.DataDir = dir })
	h.c.coll = &billedColl{}
	ctx := context.Background()
	start := t0.Truncate(time.Minute)
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)

	// Down for five minutes; relabeled meanwhile. A new controller on the
	// same data directory and ledger.
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "globex"})}
	h.now = start.Add(5*time.Minute + 20*time.Second)
	again := New(h.c.cfg, h.disc, &billedColl{}, h.sp, h.reg, []destinations.Destination{h.dest}, nil)
	again.now = func() time.Time { return h.now }
	h.c = again
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	h.now = start.Add(6*time.Minute + 30*time.Second)
	h.c.CollectDue(ctx)

	for i := 1; i <= 5; i++ {
		if got := seconds(ledgerWindow(t, h, start.Add(time.Duration(i)*time.Minute))); got["acme"] != 60 || len(got) != 1 {
			t.Errorf("backfilled window %d: %v, want 60s for acme", i, got)
		}
	}
	if got := seconds(ledgerWindow(t, h, start.Add(6*time.Minute))); got["acme"] != 20 || got["globex"] != 40 {
		t.Errorf("the window of the first discovery after the start: %v, want 20s for acme and 40s for globex", got)
	}
}

// Without a change inside a window nothing is split, and windows with the
// same owners stay one collection run.
func TestNoChangeNoSplit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxBackfill = 10 * time.Minute })
	coll := &billedColl{}
	h.c.coll = coll
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	h.now = h.now.Add(5 * time.Minute)
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	if len(coll.calls) != 2 || len(coll.calls[1]) != 1 || len(coll.calls[1][0].Elsewhere) != 0 {
		t.Fatalf("calls: %d, last %+v", len(coll.calls), coll.calls[len(coll.calls)-1])
	}
}

// ReconcileSoon makes the next commit wait for a reconcile: the tenant
// records follow a move the Platform watch reported before usage is
// billed to the new owner.
func TestReconcileSoonRunsBeforeTheNextCommit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})}
	h.c.reconcileIfAsked(ctx)
	if _, ok := h.reg.Get("acme"); ok {
		t.Fatal("reconciled without being asked")
	}
	h.c.ReconcileSoon()
	h.c.ReconcileSoon() // asking twice is one reconcile
	h.c.reconcileIfAsked(ctx)
	if _, ok := h.reg.Get("acme"); !ok {
		t.Fatal("not reconciled when asked")
	}
}
