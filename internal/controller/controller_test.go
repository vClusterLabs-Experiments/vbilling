package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type fakeDisc struct {
	clusters []discovery.TenantCluster
	err      error
}

func (f *fakeDisc) Discover(context.Context) ([]discovery.TenantCluster, error) {
	return f.clusters, f.err
}

type fakeColl struct {
	mu      sync.Mutex
	calls   [][]metrics.Window
	targets []metrics.Target
	err     error
}

func (f *fakeColl) Collect(_ context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, windows)
	f.targets = targets
	if f.err != nil {
		return nil, metrics.Stats{}, f.err
	}
	out := map[time.Time][]usage.Event{}
	for _, w := range windows {
		for _, tg := range targets {
			e := usage.Event{Tenant: tg.Tenant, Metric: usage.MetricInstanceHours, Quantity: w.End.Sub(w.Start).Hours(),
				WindowStart: w.Start, WindowEnd: w.End, Dimensions: map[string]string{usage.DimTenantCluster: tg.Cluster.ExternalID()}}
			e.Finalize()
			out[w.End] = append(out[w.End], e)
		}
	}
	return out, metrics.Stats{}, nil
}

type fakeDest struct {
	mu      sync.Mutex
	removed []string
	failRm  int
}

func (d *fakeDest) Name() string                                       { return "fake" }
func (d *fakeDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (d *fakeDest) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (d *fakeDest) SendEvents(context.Context, []usage.Event) error    { return nil }
func (d *fakeDest) RemoveTenant(_ context.Context, t usage.Tenant) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRm > 0 {
		d.failRm--
		return errors.New("backend down")
	}
	d.removed = append(d.removed, t.ID)
	return nil
}

var t0 = time.Date(2026, 10, 5, 10, 0, 30, 0, time.UTC)

func cluster(ns, name string, meta map[string]string) discovery.TenantCluster {
	return discovery.TenantCluster{Name: name, Namespace: ns, Ready: true, CreatedAt: t0.Add(-time.Hour), Annotations: meta}
}

type harness struct {
	c    *Controller
	disc *fakeDisc
	coll *fakeColl
	dest *fakeDest
	sp   *spool.Spool
	reg  *pipeline.TenantRegistry
	now  time.Time
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := &config.Config{CollectionInterval: time.Minute, ReconcileInterval: 30 * time.Second, MaxBackfill: time.Hour,
		OffboardGrace: time.Hour, Region: "ap-southeast-2", TenantSource: "cluster", Adapters: []string{"fake"}}
	for _, m := range mutate {
		m(cfg)
	}
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	h := &harness{disc: &fakeDisc{}, coll: &fakeColl{}, dest: &fakeDest{}, sp: sp, reg: pipeline.NewTenantRegistry(), now: t0}
	h.c = New(cfg, h.disc, h.coll, sp, h.reg, []destinations.Destination{h.dest}, nil)
	h.c.now = func() time.Time { return h.now }
	return h
}

func TestTenantsGroupClustersByLabelAndProject(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{
		cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme", discovery.AnnotationPrefix + "display-name": "Acme AI",
			discovery.AnnotationPrefix + "stripe-customer-id": "cus_123", discovery.AnnotationPrefix + "plan": "gpu-pro"}),
		cluster("team-b", "infer", map[string]string{discovery.AnnotationPrefix + "tenant": "acme", discovery.AnnotationPrefix + "email": "billing@acme.test"}),
		cluster("solo", "dev", nil),
	}
	h.c.Reconcile(context.Background())
	all := h.reg.All()
	if len(all) != 2 {
		t.Fatalf("tenants: %+v", all)
	}
	acme, _ := h.reg.Get("acme")
	if acme.DisplayName != "Acme AI" || acme.Email != "billing@acme.test" || acme.Plan != "gpu-pro" || acme.ProviderIDs["stripe"] != "cus_123" ||
		strings.Join(acme.Clusters, ",") != "vcluster-team-a-train,vcluster-team-b-infer" || acme.Region != "ap-southeast-2" {
		t.Fatalf("acme: %+v", acme)
	}
	if solo, ok := h.reg.Get("vcluster-solo-dev"); !ok || solo.DisplayName != "Tenant cluster solo-dev" {
		t.Fatalf("default per-cluster tenant: %+v", solo)
	}

	// Project mode: clusters in one vCluster Platform project share a customer.
	p := newHarness(t, func(c *config.Config) { c.TenantSource = "project" })
	a, b := cluster("loft-p1-v-a", "a", nil), cluster("loft-p1-v-b", "b", nil)
	a.Project, b.Project = "research", "research"
	p.disc.clusters = []discovery.TenantCluster{a, b}
	p.c.Reconcile(context.Background())
	if got := p.reg.All(); len(got) != 1 || got[0].ID != "project-research" || len(got[0].Clusters) != 2 {
		t.Fatalf("project tenants: %+v", got)
	}
}

func TestDiscoveryFailureNeverOffboards(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(context.Background())

	h.disc.err = errors.New("apiserver timeout")
	h.now = h.now.Add(3 * time.Hour)
	h.c.Reconcile(context.Background())
	if len(h.dest.removed) != 0 || len(h.c.Clusters()) != 1 {
		t.Fatal("a failed discovery must keep the previous view")
	}
}

func TestOffboardingWaitsForGraceAndRetries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)

	h.disc.clusters = nil // tenant cluster deleted
	h.now = h.now.Add(30 * time.Minute)
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 0 {
		t.Fatal("offboarded before the grace period")
	}
	h.dest.failRm = 1
	h.now = h.now.Add(31 * time.Minute)
	h.c.Reconcile(ctx) // backend fails: retried next time
	h.c.Reconcile(ctx)
	if strings.Join(h.dest.removed, ",") != "vcluster-team-a-train" {
		t.Fatalf("removed = %v", h.dest.removed)
	}
	h.c.Reconcile(ctx)
	if len(h.dest.removed) != 1 {
		t.Fatal("tenant offboarded twice")
	}
	if _, ok := h.reg.Get("vcluster-team-a-train"); ok {
		t.Fatal("offboarded tenant still registered")
	}
}

func TestCollectBackfillsGapsAndRetriesFailures(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxBackfill = 10 * time.Minute })
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)

	h.c.CollectDue(ctx) // first run ever: only the last closed window
	if len(h.coll.calls) != 1 || len(h.coll.calls[0]) != 1 || !h.coll.calls[0][0].End.Equal(t0.Truncate(time.Minute)) {
		t.Fatalf("first collection windows: %+v", h.coll.calls)
	}
	h.c.CollectDue(ctx) // same window: nothing to do
	if len(h.coll.calls) != 1 {
		t.Fatal("window collected twice")
	}

	h.now = h.now.Add(5 * time.Minute) // vBilling was down for 5 minutes
	h.c.CollectDue(ctx)
	last := h.coll.calls[len(h.coll.calls)-1]
	if len(last) != 5 || !last[4].Latest || last[0].Latest {
		t.Fatalf("expected 5 windows with only the last live, got %+v", last)
	}

	h.coll.err = errors.New("apiserver unavailable")
	h.now = h.now.Add(2 * time.Minute)
	h.c.CollectDue(ctx)
	h.coll.err = nil
	h.c.CollectDue(ctx)
	last = h.coll.calls[len(h.coll.calls)-1]
	if len(last) != 2 {
		t.Fatalf("failed windows must be retried, got %d windows", len(last))
	}

	h.now = h.now.Add(3 * time.Hour) // longer than MAX_BACKFILL
	h.c.CollectDue(ctx)
	last = h.coll.calls[len(h.coll.calls)-1]
	if len(last) != 10 {
		t.Fatalf("backfill must be capped at MAX_BACKFILL (10 windows), got %d", len(last))
	}
	wm, _ := h.sp.Watermark(spool.SourceCollector)
	if !wm.Equal(h.now.Truncate(time.Minute)) {
		t.Fatalf("watermark %s, want %s", wm, h.now.Truncate(time.Minute))
	}
	if h.coll.targets[0].Tenant != "vcluster-team-a-train" {
		t.Fatalf("collector target tenant: %+v", h.coll.targets[0])
	}
}

// gapColl fails the tenant API part on demand and meters private nodes when
// asked to fill.
type gapColl struct {
	fakeColl
	tenantDown bool
	fills      [][]metrics.Window
}

func (g *gapColl) Collect(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error) {
	out, st, err := g.fakeColl.Collect(ctx, targets, windows)
	if g.tenantDown {
		for _, tg := range targets {
			st.TenantAPIFailed = append(st.TenantAPIFailed, tg.Cluster.ExternalID())
		}
	}
	return out, st, err
}

func (g *gapColl) CollectTenantAPI(_ context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error) {
	g.fills = append(g.fills, windows)
	out := map[time.Time][]usage.Event{}
	if g.tenantDown {
		return out, metrics.Stats{TenantAPIFailed: []string{targets[0].Cluster.ExternalID()}}, nil
	}
	for _, w := range windows {
		e := usage.Event{Tenant: targets[0].Tenant, Metric: usage.MetricPrivateNodeHours, Quantity: w.End.Sub(w.Start).Hours(), ResourceID: "gpu-1",
			WindowStart: w.Start, WindowEnd: w.End, Dimensions: map[string]string{usage.DimTenantCluster: targets[0].Cluster.ExternalID(), usage.DimBillingMode: "private_node"}}
		e.Finalize()
		out[w.End] = append(out[w.End], e)
	}
	return out, metrics.Stats{}, nil
}

func TestTenantAPIOutageWindowsAreFilledExactlyOnce(t *testing.T) {
	h := newHarness(t)
	gc := &gapColl{tenantDown: true}
	h.c.coll = gc
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-p", "private", nil)}
	h.c.Reconcile(ctx)
	id := "vcluster-team-p-private"

	h.c.CollectDue(ctx) // W1: tenant API down
	h.now = h.now.Add(2 * time.Minute)
	h.c.CollectDue(ctx) // W2, W3: still down
	w1, w3 := t0.Truncate(time.Minute), t0.Truncate(time.Minute).Add(2*time.Minute)
	if span, ok := h.c.gaps.Get(id); !ok || span != w1.Format(time.RFC3339)+"|"+w3.Format(time.RFC3339) {
		t.Fatalf("gap = %q, %v; want the failed windows W1..W3", span, ok)
	}
	if len(gc.fills) != 0 {
		t.Fatal("no fill may run while the tenant API is still down")
	}

	gc.tenantDown = false
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx) // W4 live; W1..W3 filled
	if len(gc.fills) != 1 || len(gc.fills[0]) != 3 {
		t.Fatalf("fill windows = %+v, want the 3 windows missed", gc.fills)
	}
	if _, ok := h.c.gaps.Get(id); ok {
		t.Fatal("gap must be cleared once filled")
	}
	countPrivate := func() int {
		n := 0
		recs, err := h.sp.Read(0, 10000)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Event != nil && r.Event.Metric == usage.MetricPrivateNodeHours {
				n++
			}
		}
		return n
	}
	if n := countPrivate(); n != 3 {
		t.Fatalf("ledger has %d filled private-node events, want 3", n)
	}
	// A repeated fill (for example after a crash between append and clearing
	// the gap) must not bill twice.
	_ = h.c.gaps.Set(id, w1.Format(time.RFC3339)+"|"+w3.Format(time.RFC3339))
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx)
	if n := countPrivate(); n != 3 {
		t.Fatalf("after a repeated fill the ledger has %d private-node events, want still 3", n)
	}
}

func TestTenantAPIGapsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{CollectionInterval: time.Minute, MaxBackfill: time.Hour, DataDir: dir}
	a := New(cfg, &fakeDisc{}, &fakeColl{}, nil, pipeline.NewTenantRegistry(), nil, nil)
	if err := a.gaps.Set("vcluster-team-p-private", "2026-10-05T10:00:00Z|2026-10-05T10:02:00Z"); err != nil {
		t.Fatal(err)
	}
	b := New(cfg, &fakeDisc{}, &fakeColl{}, nil, pipeline.NewTenantRegistry(), nil, nil)
	if g, ok := parseGap(b.gaps, "vcluster-team-p-private"); !ok || !g[1].Equal(time.Date(2026, 10, 5, 10, 2, 0, 0, time.UTC)) {
		t.Fatalf("gap after restart = %v, %v", g, ok)
	}
}
