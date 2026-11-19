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
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
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

// rejectDest permanently rejects every offboarding, like a receiver that
// will never accept the event.
type rejectDest struct {
	fakeDest
	calls int
}

func (d *rejectDest) Name() string { return "picky" }
func (d *rejectDest) RemoveTenant(context.Context, usage.Tenant) error {
	d.calls++
	return destinations.Permanent(errors.New("HTTP 400: unknown event type"))
}

func TestOffboardingIsFinalPerDestination(t *testing.T) {
	h := newHarness(t)
	picky := &rejectDest{}
	h.c.dests = append(h.c.dests, picky)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)

	h.disc.clusters = nil
	h.now = h.now.Add(2 * time.Hour)
	h.dest.failRm = 1
	h.c.Reconcile(ctx) // fake is down (retried); picky rejects for good (final)
	h.c.Reconcile(ctx) // fake succeeds; picky is not asked again
	h.c.Reconcile(ctx)
	if picky.calls != 1 {
		t.Fatalf("a permanent rejection was retried: %d calls", picky.calls)
	}
	if len(h.dest.removed) != 1 {
		t.Fatalf("removed = %v", h.dest.removed)
	}
	if _, ok := h.reg.Get("vcluster-team-a-train"); ok {
		t.Fatal("tenant still registered after offboarding")
	}
}

// Discovery reports a cluster paused only while its control plane is down.
// A pause vBilling did not make is recorded from when it is seen; one
// vBilling marked is left to its enforcer, which records it once it
// verified the cluster's workloads are gone, never from the paused-at
// annotation (anyone who writes vcluster.yaml can back-date it). Any pause
// ends when the cluster runs again.
func TestPausesSeenInDiscoveryReachMetering(t *testing.T) {
	h := newHarness(t)
	spans := pause.Memory()
	h.c.UsePauses(spans)
	ctx := context.Background()
	paused := cluster("vc-a", "a", map[string]string{
		discovery.AnnotationPausedBy: "vbilling",
		discovery.AnnotationPausedAt: t0.Add(-90 * time.Second).Format(time.RFC3339Nano),
	})
	paused.Paused = true
	external := cluster("vc-b", "b", nil)
	external.Paused = true // vcluster pause by an operator: no vBilling annotations
	h.disc.clusters = []discovery.TenantCluster{paused, external}
	h.c.Reconcile(ctx)

	a, b := paused.ExternalID(), external.ExternalID()
	if sp, open := spans.Current(a); open {
		t.Fatalf("the controller recorded vBilling's pause itself: %+v (the enforcer records it once verified, never from paused-at)", sp)
	}
	if sp, open := spans.Current(b); !open || !sp.From.Equal(t0) || sp.By != pause.ByExternal {
		t.Fatalf("external pause span = %+v %t, want it from when it was seen", sp, open)
	}
	h.now = t0.Add(time.Minute)
	h.c.CollectDue(ctx)
	for _, tg := range h.coll.targets {
		want := 0
		if tg.Cluster.ExternalID() == b {
			want = 1
		}
		if len(tg.Paused) != want {
			t.Fatalf("target %s carries %d pause spans, want %d", tg.Cluster.ExternalID(), len(tg.Paused), want)
		}
	}

	// Resumed outside vBilling: the span ends when discovery sees it.
	h.now = t0.Add(5 * time.Minute)
	external.Paused = false
	h.disc.clusters = []discovery.TenantCluster{paused, external}
	h.c.Reconcile(ctx)
	got := spans.Spans(b)
	if len(got) != 1 || !got[0].To.Equal(h.now) {
		t.Fatalf("external spans after resume = %+v", got)
	}
	// vBilling's own pause, once verified, also ends when discovery sees the
	// cluster run again (scaled up by hand, woken by the Platform).
	if _, err := spans.Begin(a, t0.Add(2*time.Minute), pause.ByVBilling); err != nil {
		t.Fatal(err)
	}
	h.now = t0.Add(6 * time.Minute)
	paused.Paused = false
	h.disc.clusters = []discovery.TenantCluster{paused, external}
	h.c.Reconcile(ctx)
	if got := spans.Spans(a); len(got) != 1 || !got[0].To.Equal(h.now) {
		t.Fatalf("vBilling's span of a cluster that runs again = %+v, want it ended when seen running", got)
	}
	// vBilling's own pause and resume are reflected at once.
	h.c.SetPaused(a, true)
	for _, cl := range h.c.Clusters() {
		if cl.ExternalID() == a && !cl.Paused {
			t.Fatal("SetPaused did not update the cluster")
		}
	}
}

// Two clusters whose namespace and name join to the same ID share pause
// spans, saved inventories and gaps: their pauses never clip either one.
func TestSharedClusterIDsAreNeverClipped(t *testing.T) {
	h := newHarness(t)
	spans := pause.Memory()
	h.c.UsePauses(spans)
	ctx := context.Background()
	one, other := cluster("a-b", "c", nil), cluster("a", "b-c", nil)
	if one.ExternalID() != other.ExternalID() {
		t.Fatalf("test setup: %s and %s differ", one.ExternalID(), other.ExternalID())
	}
	one.Paused = true // one of them is down: no span may clip the other
	h.disc.clusters = []discovery.TenantCluster{one, other}
	h.c.Reconcile(ctx)
	id := one.ExternalID()
	if sp, open := spans.Current(id); open {
		t.Fatalf("pause span %+v recorded for an ID two clusters share", sp)
	}
	// A span recorded before the second cluster appeared clips neither.
	if _, err := spans.Begin(id, t0.Add(-time.Hour), pause.ByVBilling); err != nil {
		t.Fatal(err)
	}
	h.now = t0.Add(time.Minute)
	h.c.CollectDue(ctx)
	for _, tg := range h.coll.targets {
		if len(tg.Paused) != 0 {
			t.Fatalf("target %s carries pause spans %+v of a shared ID", tg.Cluster.ExternalID(), tg.Paused)
		}
	}
	h.c.SetPaused(id, false)
	paused := 0
	for _, cl := range h.c.Clusters() {
		if cl.Paused {
			paused++
		}
	}
	if paused != 1 {
		t.Fatalf("SetPaused on a shared ID changed what discovery saw: %d paused, want 1", paused)
	}
}

func TestWindowsAreCollectedAfterTheKubeletReportedThem(t *testing.T) {
	h := newHarness(t)
	// Just after a boundary, the next collection is the following boundary
	// plus the settle time, long enough for the kubelet to post the status
	// of a container that exited in the last second of the window.
	h.now = t0.Truncate(time.Minute).Add(500 * time.Millisecond)
	got := h.c.untilNextWindow()
	// The window that just ended is collected once its settle time passed.
	if want := minSettle - 500*time.Millisecond; got != want {
		t.Fatalf("next collection in %v, want %v", got, want)
	}
	h.now = t0.Truncate(time.Minute).Add(minSettle)
	if got, want := h.c.untilNextWindow(), time.Minute; got != want {
		t.Fatalf("at the settle time the next collection is in %v, want %v", got, want)
	}
	if minSettle < 5*time.Second {
		t.Fatalf("settle = %v: a real kubelet took about 3s to report a finished container", minSettle)
	}

	// COLLECTION_DELAY longer than a window (spot capacity): every window
	// is collected that long after it ends, one per window, never early.
	h.c.cfg.CollectionDelay = 75 * time.Second
	b := t0.Truncate(time.Minute)
	h.now = b.Add(15 * time.Second) // window ending at b-60s is due now; b only at b+75s
	if got := h.c.latestDue(); !got.Equal(b.Add(-time.Minute)) {
		t.Fatalf("latest due window ends %s, want %s", got, b.Add(-time.Minute))
	}
	if got, want := h.c.untilNextWindow(), time.Minute; got != want {
		t.Fatalf("with a 75s delay the next collection is in %v, want %v (at b+75s)", got, want)
	}
	h.now = b.Add(74 * time.Second)
	if got := h.c.latestDue(); !got.Equal(b.Add(-time.Minute)) {
		t.Fatalf("window ending at %s collected before its delay", b)
	}
}
