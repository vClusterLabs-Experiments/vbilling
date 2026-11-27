package safeguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// The catalog decides which metrics take pushed usage: every metric it
// says vBilling measures (the built-in ones, and any source it names
// besides ingest and records) takes none, a metric of the records
// aggregator takes records only, and a metric an operator declared for
// pushed usage takes both.
func TestPushedMetricFollowsTheCatalog(t *testing.T) {
	// What the collector and Prometheus measure today. A metric added to the
	// catalog with one of their sources is refused without being listed here.
	for _, code := range []string{usage.MetricGPUHours, usage.MetricCPUCoreHours, usage.MetricMemoryGBHours, usage.MetricPrivateNodeHours,
		usage.MetricInstanceHours, usage.MetricStorageGBHours, usage.MetricLBHours, usage.MetricNetworkEgressGB,
		usage.MetricGPUDowntimeHours, usage.MetricGPUUtilization} {
		def, ok := usage.Lookup(code)
		if !ok {
			t.Fatalf("%s is not in the catalog", code)
		}
		for _, path := range []Path{PathEvents, PathRecords} {
			err := PushedMetric(def, path)
			if err == nil || !strings.Contains(err.Error(), code+" is measured by vBilling itself") || !strings.Contains(err.Error(), "CUSTOM_METRICS_FILE") {
				t.Errorf("%s pushed as %s: %v, want it refused as measured, with a pointer to the custom metrics file", code, path, err)
			}
		}
	}
	// Every metric of the catalog follows its source, the built-in ones too.
	for _, def := range usage.Catalog() {
		events := def.Source == usage.SourceIngest
		records := events || def.Source == usage.SourceRecords
		if got := PushedMetric(def, PathEvents) == nil; got != events {
			t.Errorf("%s (source %q) pushed as events: allowed %v, want %v", def.Code, def.Source, got, events)
		}
		if got := PushedMetric(def, PathRecords) == nil; got != records {
			t.Errorf("%s (source %q) pushed as records: allowed %v, want %v", def.Code, def.Source, got, records)
		}
	}
	for _, c := range []struct {
		source          string
		events, records bool // allowed
	}{
		{usage.SourceIngest, true, true},
		{usage.SourceRecords, false, true},
		{usage.SourcePrometheus, false, false},
		{usage.SourceKubernetes, false, false},
		{"a-source-added-later", false, false},
		{"", false, false},
	} {
		def := usage.MetricDef{Code: "some_metric", Custom: true, Source: c.source}
		if got := PushedMetric(def, PathEvents) == nil; got != c.events {
			t.Errorf("a metric of source %q pushed as events: allowed %v, want %v", c.source, got, c.events)
		}
		if got := PushedMetric(def, PathRecords) == nil; got != c.records {
			t.Errorf("a metric of source %q pushed as records: allowed %v, want %v", c.source, got, c.records)
		}
	}
	if err := PushedMetric(usage.MetricDef{Code: "tokens", Source: usage.SourceRecords}, PathEvents); err == nil || !strings.Contains(err.Error(), "POST /api/v1/records") {
		t.Errorf("window totals under a records metric: %v, want a pointer to the records API", err)
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		34 * 24 * time.Hour:             "34 days",
		34*24*time.Hour - time.Hour:     "33 days 23 hours",
		24 * time.Hour:                  "1 day",
		25 * time.Hour:                  "1 day 1 hour",
		7 * time.Hour:                   "7 hours",
		90 * time.Minute:                "1h30m0s",
		7*24*time.Hour + 30*time.Minute: "168h30m0s",
	} {
		if got := Age(d); got != want {
			t.Errorf("Age(%s) = %q, want %q", d, got, want)
		}
	}
}

// clusterRig is a ledger, a clock and a list of running clusters.
type clusterRig struct {
	t   *testing.T
	dir string
	sp  *spool.Spool

	mu      sync.Mutex
	now     time.Time
	live    []LiveCluster
	settled bool
}

func newClusterRig(t *testing.T) *clusterRig {
	t.Helper()
	r := &clusterRig{t: t, dir: t.TempDir(), now: ws.Add(time.Hour), settled: true}
	sp, err := spool.Open(r.dir, spool.Options{NoSync: true, Now: r.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	r.sp = sp
	return r
}

func (r *clusterRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *clusterRig) set(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *clusterRig) running() ([]LiveCluster, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]LiveCluster(nil), r.live...), r.settled
}

func (r *clusterRig) clusters(remember time.Duration) *Clusters {
	return NewClusters(r.sp, ClusterOptions{Live: r.running, LiveFor: -1, Grace: time.Minute, Remember: remember, Now: r.clock})
}

// event is usage of a cluster for the minute starting at start.
func clusterEvent(tenant, cluster, source string, start time.Time) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricInstanceHours, Quantity: 1.0 / 60, WindowStart: start, WindowEnd: start.Add(time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: cluster}, Source: source}
	e.Finalize()
	return e
}

// meter commits what the collector measured for a cluster in the minutes
// from start to end.
func (r *clusterRig) meter(tenant, cluster string, start, end time.Time) {
	r.t.Helper()
	for s := start; s.Before(end); s = s.Add(time.Minute) {
		if err := r.sp.AppendWindow("collector/"+cluster, s.Add(time.Minute), []usage.Event{clusterEvent(tenant, cluster, spool.SourceCollector, s)}, ""); err != nil {
			r.t.Fatal(err)
		}
	}
}

func minute(c *Clusters, tenant, cluster string, start time.Time) error {
	return c.Check(tenant, cluster, start, start.Add(time.Minute))
}

// A cluster is known for its tenant while it runs, from when it was
// created, and never for another tenant.
func TestClustersKnowTheRunningClustersOfATenant(t *testing.T) {
	r := newClusterRig(t)
	created := ws.Add(10 * time.Minute)
	r.live = []LiveCluster{{ID: "vcluster-acme-train", Tenant: "acme", Created: created}, {ID: "external-edge", Tenant: "globex"}}
	c := r.clusters(0)
	if err := minute(c, "acme", "vcluster-acme-train", ws.Add(30*time.Minute)); err != nil {
		t.Errorf("a running cluster of the tenant: %v", err)
	}
	if err := minute(c, "acme", "vcluster-acme-train", created.Add(-30*time.Second)); err != nil {
		t.Errorf("the window the cluster was created in: %v", err)
	}
	if err := minute(c, "acme", "vcluster-acme-train", created.Add(-2*time.Minute)); err == nil || !strings.Contains(err.Error(), "was created at") {
		t.Errorf("a window before the cluster was created: %v, want it refused", err)
	}
	if err := minute(c, "globex", "external-edge", ws.Add(-30*24*time.Hour)); err != nil {
		t.Errorf("a cluster whose creation time is not known: %v, want any window accepted", err)
	}
	unknown := minute(c, "acme", "vcluster-acme-ghost", ws)
	other := minute(c, "acme", "external-edge", ws)
	if unknown == nil || other == nil {
		t.Fatalf("a cluster that does not exist: %v; another tenant's cluster: %v; want both refused", unknown, other)
	}
	// The refusal tells a tenant nothing about another tenant's clusters.
	if strings.Replace(unknown.Error(), "vcluster-acme-ghost", "X", 1) != strings.Replace(other.Error(), "external-edge", "X", 1) {
		t.Errorf("the refusals differ:\n%v\n%v", unknown, other)
	}
	if err := (*Clusters)(nil).Check("acme", "vcluster-acme-train", ws, ws.Add(time.Minute)); err == nil {
		t.Error("without any knowledge of clusters, a cluster must be refused")
	}
}

// A cluster that no longer runs is known from the usage vBilling measured
// for it: late usage for a window in which it was metered, or for the
// window after its last one (it was deleted during it), is accepted; a
// later window, an earlier one and another tenant are refused. Usage an
// emitter pushed is never evidence of a cluster.
func TestClustersRememberDeletedClustersFromTheLedger(t *testing.T) {
	r := newClusterRig(t)
	first, last := ws, ws.Add(10*time.Minute)
	r.meter("acme", "vcluster-acme-gone", first, last)
	// The cluster then moved to globex for five minutes and was deleted.
	r.meter("globex", "vcluster-acme-gone", last, last.Add(5*time.Minute))
	// What emitters reported never counts.
	pushed := []usage.Event{clusterEvent("acme", "vcluster-acme-pushed", "ingest:gw", first), clusterEvent("acme", "vcluster-acme-pushed", "records", first.Add(time.Minute))}
	if _, _, err := r.sp.AppendIngest("ingest:gw", pushed[:1]); err != nil {
		t.Fatal(err)
	}
	if err := r.sp.AppendWindow("records", first.Add(2*time.Minute), pushed[1:], ""); err != nil {
		t.Fatal(err)
	}
	c := r.clusters(0)
	for name, tc := range map[string]struct {
		tenant, cluster string
		start           time.Time
		ok              bool
	}{
		"its first metered window":             {"acme", "vcluster-acme-gone", first, true},
		"a window in the middle":               {"acme", "vcluster-acme-gone", first.Add(5 * time.Minute), true},
		"the window after its last one":        {"acme", "vcluster-acme-gone", last, true},
		"two windows after its last one":       {"acme", "vcluster-acme-gone", last.Add(time.Minute), false},
		"the window before its first one":      {"acme", "vcluster-acme-gone", first.Add(-time.Minute), false},
		"the next owner, in its own time":      {"globex", "vcluster-acme-gone", last.Add(2 * time.Minute), true},
		"the next owner, before it owned it":   {"globex", "vcluster-acme-gone", first, false},
		"a tenant that never owned it":         {"initech", "vcluster-acme-gone", first, false},
		"a cluster only pushed usage named":    {"acme", "vcluster-acme-pushed", first, false},
		"a cluster vBilling has never metered": {"acme", "vcluster-acme-ghost", first, false},
	} {
		if err := minute(c, tc.tenant, tc.cluster, tc.start); (err == nil) != tc.ok {
			t.Errorf("%s: %v, want accepted %v", name, err, tc.ok)
		} else if errors.Is(err, ErrNotReady) {
			t.Errorf("%s: asked to retry, want a definite answer", name)
		}
	}
	// A longer window is accepted when any part of it overlaps the cluster's time.
	if err := c.Check("acme", "vcluster-acme-gone", first.Add(-time.Hour), first.Add(30*time.Second)); err != nil {
		t.Errorf("an hour that ends inside the cluster's first minute: %v", err)
	}
	// A cluster metered later is known at once, without Run.
	r.meter("acme", "vcluster-acme-new", last.Add(20*time.Minute), last.Add(21*time.Minute))
	if err := minute(c, "acme", "vcluster-acme-new", last.Add(20*time.Minute)); err != nil {
		t.Errorf("a cluster metered after the first check: %v", err)
	}
}

// A deleted cluster is remembered for as long as the ledger keeps its
// usage: once its last metered window is older than that, it is unknown,
// and while it is remembered only windows that ended within that time can
// be checked. The answer does not depend on when vBilling last started.
func TestClustersForgetWithTheLedgersRetention(t *testing.T) {
	const keep = 7 * 24 * time.Hour
	r := newClusterRig(t)
	first, last := ws, ws.Add(48*time.Hour)
	// Metered for two days, an event every hour.
	for s := first; s.Before(last); s = s.Add(time.Hour) {
		r.meter("acme", "vcluster-acme-gone", s, s.Add(time.Minute))
	}
	end := last.Add(-time.Hour).Add(time.Minute) // the end of its last metered window
	late := first.Add(time.Hour)                 // a window it was metered in
	for _, restarted := range []bool{false, true} {
		c := r.clusters(keep)
		r.set(func() { r.now = end.Add(24 * time.Hour) })
		if err := minute(c, "acme", "vcluster-acme-gone", late); err != nil {
			t.Fatalf("restarted %v: late usage a day after the cluster was deleted: %v", restarted, err)
		}
		// Six days later the window is older than the ledger keeps.
		r.set(func() { r.now = late.Add(keep).Add(2 * time.Minute) })
		if restarted {
			c = r.clusters(keep)
		}
		if err := minute(c, "acme", "vcluster-acme-gone", late); err == nil || !strings.Contains(err.Error(), "cannot check") {
			t.Fatalf("restarted %v: usage for a window older than the ledger keeps: %v, want it refused", restarted, err)
		}
		if err := minute(c, "acme", "vcluster-acme-gone", end.Add(-time.Minute)); err != nil {
			t.Fatalf("restarted %v: usage for the cluster's last window, still within what the ledger keeps: %v", restarted, err)
		}
		// Once its last metered window is older than that, the cluster is forgotten.
		r.set(func() { r.now = end.Add(keep).Add(time.Minute) })
		if restarted {
			c = r.clusters(keep)
		}
		if err := minute(c, "acme", "vcluster-acme-gone", end.Add(-time.Minute)); err == nil || !strings.Contains(err.Error(), "is not a cluster vBilling knows") {
			t.Fatalf("restarted %v: a cluster whose last metered window is older than the ledger keeps: %v, want it unknown", restarted, err)
		}
		c.prune()
		if len(c.metered) != 0 {
			t.Fatalf("restarted %v: the forgotten cluster is still held in memory: %v", restarted, c.metered)
		}
	}
}

// While vBilling has not discovered clusters since it started, it cannot
// tell a cluster that does not exist from one it has not seen yet: it asks
// the emitter to retry instead of refusing. What the ledger vouches for is
// accepted meanwhile.
func TestClustersAskToRetryWhileVBillingIsStarting(t *testing.T) {
	r := newClusterRig(t)
	r.meter("acme", "vcluster-acme-train", ws, ws.Add(5*time.Minute))
	r.settled = false
	c := r.clusters(0)
	if err := minute(c, "acme", "vcluster-acme-train", ws.Add(2*time.Minute)); err != nil {
		t.Errorf("a window the ledger metered the cluster in: %v", err)
	}
	for _, cluster := range []string{"vcluster-acme-train", "vcluster-acme-ghost"} {
		if err := minute(c, "acme", cluster, ws.Add(30*time.Minute)); !errors.Is(err, ErrNotReady) {
			t.Errorf("%s before the first discovery: %v, want ErrNotReady", cluster, err)
		}
	}
	r.set(func() {
		r.settled = true
		r.live = []LiveCluster{{ID: "vcluster-acme-train", Tenant: "acme", Created: ws}}
	})
	if err := minute(c, "acme", "vcluster-acme-train", ws.Add(30*time.Minute)); err != nil {
		t.Errorf("the running cluster after the first discovery: %v", err)
	}
	if err := minute(c, "acme", "vcluster-acme-ghost", ws.Add(30*time.Minute)); err == nil || errors.Is(err, ErrNotReady) {
		t.Errorf("a cluster that does not exist after the first discovery: %v, want it refused", err)
	}
}

// After a start the retained ledger is read in the background. Until it is
// read through, a cluster the ledger may still vouch for is not refused:
// the emitter is asked to retry, and gets a definite answer once it is.
func TestClustersAskToRetryWhileTheLedgerIsStillBeingRead(t *testing.T) {
	r := newClusterRig(t)
	// More measured usage than one check reads itself, and at its very end
	// the only window of a cluster that was deleted since.
	batch := make([]usage.Event, 1000)
	for n := 0; n*len(batch) <= syncReads; n++ {
		start := ws.Add(time.Duration(n) * time.Minute)
		for i := range batch {
			batch[i] = clusterEvent("globex", fmt.Sprintf("vcluster-globex-%d", i), spool.SourceCollector, start)
		}
		if err := r.sp.AppendWindow(spool.SourceCollector, start.Add(time.Minute), batch, ""); err != nil {
			t.Fatal(err)
		}
	}
	last := ws.Add(24 * time.Hour)
	r.meter("acme", "vcluster-acme-gone", last, last.Add(time.Minute))
	c := r.clusters(0)
	if err := minute(c, "acme", "vcluster-acme-gone", last); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a deleted cluster before the ledger is read through: %v, want ErrNotReady", err)
	}
	// The emitter retries. Each check reads on, as Run does in the background.
	var err error
	for i := 0; i < 5; i++ {
		if err = minute(c, "acme", "vcluster-acme-gone", last); !errors.Is(err, ErrNotReady) {
			break
		}
	}
	if err != nil {
		t.Fatalf("late usage of the deleted cluster once the ledger is read: %v", err)
	}
	if err := minute(c, "acme", "vcluster-acme-ghost", last); err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("a cluster the ledger does not hold, once it is read: %v, want it refused", err)
	}
}

// A ledger that cannot be read proves nothing either way: a cluster it may
// hold is neither refused nor accepted, the emitter is asked to retry. A
// running cluster needs no ledger.
func TestClustersAskToRetryWhileTheLedgerCannotBeRead(t *testing.T) {
	r := newClusterRig(t)
	r.meter("acme", "vcluster-acme-gone", ws, ws.Add(time.Minute))
	r.live = []LiveCluster{{ID: "vcluster-acme-train", Tenant: "acme"}}
	segments, err := filepath.Glob(filepath.Join(r.dir, "seg-*.log"))
	if err != nil || len(segments) != 1 {
		t.Fatalf("ledger segments: %v %v", segments, err)
	}
	raw, err := os.ReadFile(segments[0])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff // a damaged record
	if err := os.WriteFile(segments[0], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := r.clusters(0)
	for i := 0; i < 2; i++ {
		if err := minute(c, "acme", "vcluster-acme-gone", ws); !errors.Is(err, ErrNotReady) {
			t.Fatalf("a cluster only the unreadable ledger can vouch for: %v, want ErrNotReady", err)
		}
	}
	if err := minute(c, "acme", "vcluster-acme-train", ws); err != nil {
		t.Fatalf("a running cluster while the ledger cannot be read: %v", err)
	}
}

// Run follows the ledger while checks and commits go on.
func TestClustersFollowTheLedgerWhileItIsChecked(t *testing.T) {
	r := newClusterRig(t)
	c := r.clusters(0)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.Run(ctx) }()
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = minute(c, "acme", "vcluster-acme-train", ws)
				_ = minute(c, "globex", "vcluster-acme-train", ws)
			}
		}()
	}
	for i := 0; i < 50; i++ {
		r.meter("acme", "vcluster-acme-train", ws.Add(time.Duration(i)*time.Minute), ws.Add(time.Duration(i+1)*time.Minute))
	}
	deadline := time.Now().Add(5 * time.Second)
	for minute(c, "acme", "vcluster-acme-train", ws.Add(49*time.Minute)) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the last metered window never became known")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
	if err := minute(c, "globex", "vcluster-acme-train", ws); err == nil {
		t.Fatal("another tenant may name the cluster")
	}
}
