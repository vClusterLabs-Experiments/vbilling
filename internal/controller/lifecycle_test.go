package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// runColl records the targets of every collection run.
type runColl struct {
	fakeColl
	mu   sync.Mutex
	runs []collRun
}

type collRun struct {
	windows []metrics.Window
	targets []metrics.Target
}

func (r *runColl) Collect(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error) {
	r.mu.Lock()
	r.runs = append(r.runs, collRun{windows: windows, targets: append([]metrics.Target(nil), targets...)})
	r.mu.Unlock()
	return r.fakeColl.Collect(ctx, targets, windows)
}

// targetIn returns the target with the given ID in the run that metered the
// window starting at start.
func (r *runColl) targetIn(start time.Time, id string) (metrics.Target, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.runs {
		for _, w := range run.windows {
			if !w.Start.Equal(start) {
				continue
			}
			for _, t := range run.targets {
				if t.Cluster.ExternalID() == id {
					return t, true
				}
			}
			return metrics.Target{}, false
		}
	}
	return metrics.Target{}, false
}

func lifecycleHarness(t *testing.T) (*harness, *runColl, time.Time) {
	t.Helper()
	h := newHarness(t)
	coll := &runColl{}
	h.c.coll = coll
	base := t0.Truncate(time.Hour)
	if err := h.sp.AppendWindow(spool.SourceCollector, base, nil, ""); err != nil {
		t.Fatal(err)
	}
	return h, coll, base
}

// An excluded cluster used to vanish from metering at once: the window the
// exclusion was set in and the one it was removed in were not billed at
// all. Now the cluster is billed up to the moment vBilling sees the
// exclusion and again from the moment it sees it gone, nothing in between,
// and the tenant record shows the exclusion.
func TestExcludedClusterIsBilledUpToItsExclusionAndFromItsEnd(t *testing.T) {
	h, coll, base := lifecycleHarness(t)
	ctx := context.Background()
	cl := cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})
	id := cl.ExternalID()
	h.disc.clusters = []discovery.TenantCluster{cl}
	h.now = base.Add(30 * time.Second)
	h.c.Reconcile(ctx)

	// Excluded at 10:00:45, as vBilling sees it.
	setAt := base.Add(45 * time.Second)
	h.disc.clusters, h.disc.excluded = nil, []discovery.TenantCluster{cl}
	h.now = setAt
	h.c.Reconcile(ctx)
	h.now = base.Add(time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	tg, ok := coll.targetIn(base, id)
	if !ok {
		t.Fatal("the window the exclusion was set in did not meter the cluster at all")
	}
	if len(tg.Excluded) != 1 || !tg.Excluded[0].From.Equal(setAt) || !tg.Excluded[0].Open() {
		t.Fatalf("excluded spans %+v, want one open span from %s", tg.Excluded, setAt.Format("15:04:05"))
	}
	if rec, _ := h.reg.Get("acme"); len(rec.Clusters) != 0 || len(rec.ExcludedClusters) != 1 || rec.ExcludedClusters[0] != id {
		t.Errorf("tenant record: clusters %v excluded %v, want none metered and %s excluded", rec.Clusters, rec.ExcludedClusters, id)
	}

	// Fully excluded windows do not meter it.
	h.now = base.Add(3*time.Minute + 5*time.Second)
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	for _, start := range []time.Time{base.Add(time.Minute), base.Add(2 * time.Minute)} {
		if _, ok := coll.targetIn(start, id); ok {
			t.Errorf("window %s metered an excluded cluster", start.Format("15:04"))
		}
	}

	// The setting is removed at 10:03:44, as vBilling sees it.
	clearedAt := base.Add(3*time.Minute + 44*time.Second)
	h.disc.clusters, h.disc.excluded = []discovery.TenantCluster{cl}, nil
	h.now = clearedAt
	h.c.Reconcile(ctx)
	h.now = base.Add(5*time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	tg, ok = coll.targetIn(base.Add(3*time.Minute), id)
	if !ok {
		t.Fatal("the window the exclusion was removed in did not meter the cluster at all")
	}
	if len(tg.Excluded) != 1 || !tg.Excluded[0].From.Equal(setAt) || !tg.Excluded[0].To.Equal(clearedAt) {
		t.Fatalf("excluded spans %+v, want %s to %s", tg.Excluded, setAt.Format("15:04:05"), clearedAt.Format("15:04:05"))
	}
	if tg, ok := coll.targetIn(base.Add(4*time.Minute), id); !ok || len(tg.Excluded) != 1 {
		t.Errorf("the window after: metered %t with spans %+v, want metered (the span is in the past)", ok, tg.Excluded)
	}
	if rec, _ := h.reg.Get("acme"); len(rec.Clusters) != 1 || len(rec.ExcludedClusters) != 0 {
		t.Errorf("tenant record after: clusters %v excluded %v", rec.Clusters, rec.ExcludedClusters)
	}
}

// A cluster vBilling was not metering when it finds it excluded (it just
// started) is excluded from the first window it has not committed yet: no
// window it meters after the fact bills it.
func TestClusterFoundExcludedAtStartIsExcludedFromTheFirstOpenWindow(t *testing.T) {
	h, coll, base := lifecycleHarness(t)
	ctx := context.Background()
	cl := cluster("team-a", "train", nil)
	h.disc.excluded = []discovery.TenantCluster{cl}
	h.now = base.Add(4*time.Minute + 30*time.Second) // down since 10:00, found excluded on start
	h.c.Reconcile(ctx)
	h.now = base.Add(5*time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	for i := 0; i < 5; i++ {
		if _, ok := coll.targetIn(base.Add(time.Duration(i)*time.Minute), cl.ExternalID()); ok {
			t.Errorf("window %d metered a cluster that was excluded when vBilling started", i)
		}
	}
	if sp, open := h.c.exclusionStore().Current(cl.ExternalID()); !open || !sp.From.Equal(base) {
		t.Errorf("exclusion %+v open %t, want open from the watermark %s", sp, open, base.Format("15:04:05"))
	}
}

// A deleted cluster used to vanish from metering at once, so the window it
// was deleted in had no event of any metric. It is now metered one last
// time, up to its deletion, and what is reported about it is dropped once
// that window is committed.
func TestDeletedClusterIsMeteredUpToItsDeletion(t *testing.T) {
	h, coll, base := lifecycleHarness(t)
	ctx := context.Background()
	reg := health.New(nil)
	reg.SetLogger(func(string, ...any) {})
	h.c.UseHealth(reg)
	h.c.UsePauses(pause.Memory())
	cl := cluster("team-a", "ghost", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})
	id := cl.ExternalID()
	h.disc.clusters = []discovery.TenantCluster{cl}
	h.now = base.Add(30 * time.Second)
	h.c.Reconcile(ctx)
	reg.OK(health.Name("tenant-api", id), "tenant-api", id, 1)

	deletedAt := base.Add(44 * time.Second)
	h.disc.clusters = nil
	h.now = deletedAt
	h.c.Reconcile(ctx)
	if rec, known := h.reg.Get("acme"); !known || len(rec.Clusters) != 0 {
		t.Errorf("tenant record lists %v (known %t), want the tenant with no clusters", rec.Clusters, known)
	}

	h.now = base.Add(3*time.Minute + 5*time.Second)
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)
	tg, ok := coll.targetIn(base, id)
	if !ok {
		t.Fatal("the window the cluster was deleted in did not meter it at all")
	}
	if !tg.Cluster.DeletedAt.Equal(deletedAt) || len(tg.Excluded) != 1 || !tg.Excluded[0].From.Equal(deletedAt) || !tg.Excluded[0].Open() {
		t.Fatalf("deleted at %s with spans %+v, want %s and nothing billed after it", tg.Cluster.DeletedAt.Format("15:04:05"), tg.Excluded, deletedAt.Format("15:04:05"))
	}
	if tg.Tenant != "acme" {
		t.Errorf("billed to %q, want its tenant acme", tg.Tenant)
	}
	for _, start := range []time.Time{base.Add(time.Minute), base.Add(2 * time.Minute)} {
		if _, ok := coll.targetIn(start, id); ok {
			t.Errorf("window %s metered a deleted cluster", start.Format("15:04"))
		}
	}
	for _, s := range reg.List() {
		if s.Subject == id {
			t.Errorf("source %s still reported for a deleted cluster", s.Name)
		}
	}
	// It is not metered a second time.
	h.now = base.Add(4*time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	if _, ok := coll.targetIn(base.Add(3*time.Minute), id); ok {
		t.Error("a deleted cluster was metered again after its last window")
	}
}

// goneColl is a collector that watches control plane workloads.
type goneColl struct {
	runColl
	gone map[string]time.Time
}

func (g *goneColl) ControlPlaneGone(cl discovery.TenantCluster) (time.Time, bool, bool) {
	at, ok := g.gone[cl.ExternalID()]
	return at, ok, true
}

// Where control plane workloads are watched, a cluster is deleted when its
// workload was seen being deleted, at that moment. One that discovery
// merely lost sight of is not given a last window.
func TestDeletionIsDatedByTheWatchedWorkload(t *testing.T) {
	h, _, base := lifecycleHarness(t)
	ctx := context.Background()
	coll := &goneColl{gone: map[string]time.Time{}}
	h.c.coll = coll
	deleted, lost := cluster("team-a", "deleted", nil), cluster("team-b", "lost", nil)
	h.disc.clusters = []discovery.TenantCluster{deleted, lost}
	h.now = base.Add(10 * time.Second)
	h.c.Reconcile(ctx)

	seen := base.Add(21 * time.Second) // the watch saw the deletion here
	coll.gone[deleted.ExternalID()] = seen
	h.disc.clusters = nil
	h.now = base.Add(40 * time.Second) // discovery notices 19 seconds later
	h.c.Reconcile(ctx)
	h.now = base.Add(time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	tg, ok := coll.targetIn(base, deleted.ExternalID())
	if !ok || !tg.Cluster.DeletedAt.Equal(seen) {
		t.Errorf("deleted cluster: metered %t, deleted at %s, want metered up to %s", ok, tg.Cluster.DeletedAt.Format("15:04:05"), seen.Format("15:04:05"))
	}
	if _, ok := coll.targetIn(base, lost.ExternalID()); ok {
		t.Error("a cluster whose workload was not deleted was given a last window")
	}
}

// The list that no longer shows a cluster can be ahead of the watch that
// reports its deletion. The cluster is not written off as "lost sight of":
// the next pass asks the watch again and meters its last window.
func TestDeletionTheWatchReportsAMomentLaterStillClosesTheLastWindow(t *testing.T) {
	h, _, base := lifecycleHarness(t)
	ctx := context.Background()
	coll := &goneColl{gone: map[string]time.Time{}}
	h.c.coll = coll
	cl := cluster("team-a", "late", nil)
	h.disc.clusters = []discovery.TenantCluster{cl}
	h.now = base.Add(10 * time.Second)
	h.c.Reconcile(ctx)

	h.disc.clusters = nil
	h.now = base.Add(40 * time.Second) // discovery misses it; the watch has not reported yet
	h.c.Reconcile(ctx)
	deletedAt := base.Add(39 * time.Second)
	coll.gone[cl.ExternalID()] = deletedAt
	h.now = base.Add(55 * time.Second) // the next pass
	h.c.Reconcile(ctx)
	h.now = base.Add(time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	if tg, ok := coll.targetIn(base, cl.ExternalID()); !ok || !tg.Cluster.DeletedAt.Equal(deletedAt) {
		t.Errorf("metered %t, deleted at %s, want its last window metered up to %s", ok, tg.Cluster.DeletedAt.Format("15:04:05"), deletedAt.Format("15:04:05"))
	}
}
