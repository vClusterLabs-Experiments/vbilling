package safeguard

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// The rules for pushed usage (see the package comment). The ingest API
// applies them to every event and record before it stores anything, so a
// refusal is the answer the emitter gets.

// Path is a way an emitter pushes usage.
type Path string

const (
	// PathEvents is window totals: POST /api/v1/events.
	PathEvents Path = "events"
	// PathRecords is per-request usage: POST /api/v1/records, and the
	// gateway logs vBilling reads back as records.
	PathRecords Path = "records"
)

// metricsFile is where an operator declares the metrics emitters push under.
const metricsFile = "CUSTOM_METRICS_FILE (Helm metering.customMetrics)"

// PushedMetric reports why usage pushed over path may not be written under
// the metric def, or nil when it may. The catalog decides, by the source it
// names for the metric's quantities:
//
//   - ingest: an operator declared the metric for pushed usage. Both paths
//     may write it.
//   - records: a records format declared it, and the records aggregator
//     builds its events from per-request records. Only records may write
//     it: every unit under it is then backed by a request in the records
//     ledger.
//   - anything else (collector, prometheus, a source added later): vBilling
//     measures the metric itself, and no emitter may write it.
//
// A metric added to the catalog is covered by its source: no list of codes
// here has to follow it.
func PushedMetric(def usage.MetricDef, path Path) error {
	switch {
	case def.Source == usage.SourceIngest:
		return nil
	case def.Source == usage.SourceRecords && path == PathRecords:
		return nil
	case def.Source == usage.SourceRecords:
		return fmt.Errorf("metric %s is built from per-request records, so it takes no window totals: send records to POST /api/v1/records, "+
			"or push under a metric code of your own, declared in %s", def.Code, metricsFile)
	}
	by := ""
	if def.Source != "" {
		by = " (source " + def.Source + ")"
	}
	return fmt.Errorf("metric %s is measured by vBilling itself%s, so it takes no pushed usage: push under a metric code of your own, declared in %s",
		def.Code, by, metricsFile)
}

// Age writes a limit on the age of usage in days and hours ("34 days",
// "33 days 23 hours"), and one that is not whole hours as Go writes a
// duration ("1h30m0s").
func Age(d time.Duration) string {
	if d < time.Hour || d%time.Hour != 0 {
		return d.String()
	}
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	switch {
	case days == 0:
		return plural(hours, "hour")
	case hours == 0:
		return plural(days, "day")
	}
	return plural(days, "day") + " " + plural(hours, "hour")
}

// ErrNotReady is what Clusters.Check answers while vBilling cannot tell yet
// whether a cluster exists: it has not discovered clusters since it
// started, it is still reading its ledger, or the ledger cannot be read.
// Nothing is refused on it: the emitter is asked to retry.
var ErrNotReady = errors.New("vBilling cannot check clusters yet (it is starting, or its ledger cannot be read)")

// LiveCluster is a cluster discovery lists now.
type LiveCluster struct {
	// ID is the cluster's ID, as the tenant_cluster dimension of its
	// metered usage carries it (vcluster-<namespace>-<name>, external-<name>).
	ID string
	// Tenant is the tenant the cluster bills to.
	Tenant string
	// Created is when its control plane was created; zero when unknown.
	Created time.Time
}

// ClusterOptions configure Clusters.
type ClusterOptions struct {
	// Live returns the clusters discovery lists now. settled is false while
	// that list cannot be complete yet (vBilling is starting and has not
	// discovered clusters): a cluster it does not hold is then not refused,
	// the emitter is asked to retry (ErrNotReady). Nil: no cluster runs.
	Live func() (clusters []LiveCluster, settled bool)
	// LiveFor is how long one answer of Live is used (default one second),
	// so that checking every event of a request does not copy the list of
	// clusters every time. Negative: ask every time.
	LiveFor time.Duration
	// Grace is how long after the end of its last metered window a cluster
	// may still have existed (default one minute): a window is metered once
	// it has ended and its collection delay has passed, so a cluster deleted
	// before that leaves its last windows unmetered. It is the metering
	// window plus the collection delay.
	Grace time.Duration
	// Remember is how long a cluster that no longer runs stays known after
	// its last metered window, and how far back its windows can be checked:
	// the retention of the usage ledger, which is what remembers it. Zero:
	// for as long as the ledger holds its usage.
	Remember time.Duration
	Now      func() time.Time
}

// Clusters knows which clusters exist for which tenant, and when they did,
// so that pushed usage can only name a cluster of its tenant for a window
// in which that cluster existed.
//
// It knows a cluster from two things vBilling already has. Discovery lists
// the clusters that run now, each with its tenant and when it was created.
// The usage ledger holds what vBilling measured itself: every event of the
// collector, Prometheus and fills carries its cluster's ID and the tenant
// it was billed to, so the windows a cluster was metered in are the time it
// existed for that tenant, even after it was deleted or moved to another
// tenant. Clusters reads the retained ledger once and then follows it;
// nothing else is stored. A cluster that no longer runs is therefore
// remembered for as long as the ledger keeps its usage (ClusterOptions.Remember).
// Usage an emitter pushed is never evidence of a cluster.
type Clusters struct {
	spool *spool.Spool
	opts  ClusterOptions

	mu      sync.Mutex
	read    uint64                      // ledger read through this seq
	metered map[string]map[string]*span // cluster ID -> tenant -> its metered windows
	live    map[string][]LiveCluster    // cluster ID -> clusters running under it, as of liveAt
	liveAt  time.Time
	failed  string    // the last ledger read error logged
	retry   time.Time // after a read error, the ledger is not read again before this
}

// span is the first metered window's start and the last one's end.
type span struct{ start, end time.Time }

// readBatch is how many ledger records one read takes. syncReads bounds
// what a check reads itself to catch up with the ledger: past it, the
// ledger is still being read after a start, and the check answers
// ErrNotReady. After a read error the ledger is left alone for readRetry,
// so that a ledger that cannot be read is not read again for every check.
const (
	readBatch = 5000
	syncReads = 20000
	readRetry = 10 * time.Second
)

var errUnreadable = errors.New("the ledger could not be read a moment ago")

// NewClusters returns a Clusters over the usage ledger sp. Call Run to keep
// it reading the ledger in the background; without it, checks read what
// they need themselves.
func NewClusters(sp *spool.Spool, opts ClusterOptions) *Clusters {
	if opts.Grace <= 0 {
		opts.Grace = time.Minute
	}
	if opts.LiveFor == 0 {
		opts.LiveFor = time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Clusters{spool: sp, opts: opts, metered: map[string]map[string]*span{}}
}

// Run reads the retained ledger, then every record appended to it, until
// ctx ends.
func (c *Clusters) Run(ctx context.Context) {
	if c == nil || c.spool == nil {
		return
	}
	pruned := time.Now()
	for ctx.Err() == nil {
		changed := c.spool.Changed() // taken before reading, so no append is missed
		for ctx.Err() == nil {
			if done, err := c.advance(readBatch); done || err != nil {
				break
			}
		}
		if time.Since(pruned) > time.Hour {
			c.prune()
			pruned = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(time.Minute):
		}
	}
}

// advance reads up to max ledger records not read yet and reports whether
// everything committed is read then.
func (c *Clusters) advance(max int) (bool, error) {
	if c.spool == nil {
		return true, nil
	}
	for n := 0; ; {
		c.mu.Lock()
		after, retry := c.read, c.retry
		c.mu.Unlock()
		committed := c.spool.Committed()
		if after >= committed {
			return true, nil
		}
		if n >= max {
			return false, nil
		}
		if time.Now().Before(retry) {
			return false, errUnreadable
		}
		recs, err := c.spool.Read(after, min(readBatch, max-n))
		if err != nil {
			c.mu.Lock()
			c.retry = time.Now().Add(readRetry)
			if msg := err.Error(); msg != c.failed {
				c.failed = msg
				log.Printf("[safeguard] read the ledger for the clusters it metered: %v (usage that names a cluster which no longer runs waits until it can be read)", err)
			}
			c.mu.Unlock()
			return false, err
		}
		c.mu.Lock()
		c.failed, c.retry = "", time.Time{}
		for i := range recs {
			c.note(recs[i].Event)
		}
		last := committed // nothing left to read below the committed point
		if len(recs) > 0 {
			last = recs[len(recs)-1].Seq
		}
		if last > c.read {
			c.read = last
		}
		c.mu.Unlock()
		n += len(recs)
	}
}

// note remembers the window of an event vBilling measured for a cluster.
// c.mu held.
func (c *Clusters) note(e *usage.Event) {
	if e == nil || !measured(e) || e.Tenant == "" || !e.WindowEnd.After(e.WindowStart) {
		return
	}
	id := e.Dim(usage.DimTenantCluster)
	if id == "" {
		return
	}
	tenants := c.metered[id]
	if tenants == nil {
		tenants = map[string]*span{}
		c.metered[id] = tenants
	}
	sp := tenants[e.Tenant]
	if sp == nil {
		tenants[e.Tenant] = &span{start: e.WindowStart.UTC(), end: e.WindowEnd.UTC()}
		return
	}
	if e.WindowStart.Before(sp.start) {
		sp.start = e.WindowStart.UTC()
	}
	if e.WindowEnd.After(sp.end) {
		sp.end = e.WindowEnd.UTC()
	}
}

// prune forgets clusters whose last metered window is older than Remember.
func (c *Clusters) prune() {
	if c.opts.Remember <= 0 {
		return
	}
	cutoff := c.opts.Now().Add(-c.opts.Remember)
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, tenants := range c.metered {
		for t, sp := range tenants {
			if sp.end.Before(cutoff) {
				delete(tenants, t)
			}
		}
		if len(tenants) == 0 {
			delete(c.metered, id)
		}
	}
}

// liveNow returns the clusters that run now by ID, and whether that list is
// settled (ClusterOptions.Live).
func (c *Clusters) liveNow() (map[string][]LiveCluster, bool) {
	if c.opts.Live == nil {
		return nil, true
	}
	c.mu.Lock()
	if c.live != nil && c.opts.LiveFor > 0 && time.Since(c.liveAt) < c.opts.LiveFor {
		defer c.mu.Unlock()
		return c.live, true
	}
	c.mu.Unlock()
	list, settled := c.opts.Live()
	byID := make(map[string][]LiveCluster, len(list))
	for _, l := range list {
		byID[l.ID] = append(byID[l.ID], l)
	}
	if settled {
		// An unsettled answer is never kept: the next check asks again.
		c.mu.Lock()
		c.live, c.liveAt = byID, time.Now()
		c.mu.Unlock()
	}
	return byID, settled
}

// Check reports why usage of tenant for the window [start, end) may not
// name cluster, or nil when it may:
//
//   - the cluster runs now for the tenant and was created before the window
//     ended; or
//   - vBilling metered the cluster for the tenant, the window overlaps the
//     time from the start of its first metered window to ClusterOptions.Grace
//     after the end of its last one, and (with ClusterOptions.Remember) both
//     that last window and the usage's own window ended within Remember.
//
// So late usage for a window in which a deleted cluster still existed is
// accepted while the ledger remembers the cluster, and usage for a cluster
// vBilling never metered or discovered, for another tenant's cluster, or
// for a window after the cluster was gone is refused. While vBilling cannot
// tell yet it answers ErrNotReady instead of refusing. A nil Clusters knows
// no cluster.
func (c *Clusters) Check(tenant, cluster string, start, end time.Time) error {
	if c == nil {
		return unknownCluster(tenant, cluster, 0)
	}
	live, settled := c.liveNow()
	var created time.Time // the tenant's cluster of that ID runs, but was created after the window
	for _, l := range live[cluster] {
		if l.Tenant != tenant {
			continue
		}
		if l.Created.IsZero() || end.After(l.Created) {
			return nil
		}
		created = l.Created
	}
	read, _ := c.advance(syncReads)
	now := c.opts.Now().UTC()
	horizon := time.Time{} // windows that ended before it cannot be checked
	if c.opts.Remember > 0 {
		horizon = now.Add(-c.opts.Remember)
	}
	c.mu.Lock()
	var metered span
	sp := c.metered[cluster][tenant]
	remembered := sp != nil && !sp.end.Before(horizon)
	if remembered {
		metered = *sp
	}
	c.mu.Unlock()
	if remembered && start.Before(metered.end.Add(c.opts.Grace)) && end.After(metered.start) && end.After(horizon) {
		return nil
	}
	if !settled || !read {
		return ErrNotReady
	}
	const stamp = time.RFC3339
	switch {
	case remembered && !end.After(horizon):
		return fmt.Errorf("tenant_cluster %q: vBilling keeps %s of the windows it metered for a cluster, so it cannot check usage for a window that ended at %s. "+
			"Leave tenant_cluster out to bill the tenant without a cluster", cluster, Age(c.opts.Remember), end.UTC().Format(stamp))
	case remembered:
		return fmt.Errorf("tenant_cluster %q: vBilling metered this cluster for tenant %q from %s to %s, and this usage is for %s to %s, when the cluster did not exist for the tenant",
			cluster, tenant, metered.start.Format(stamp), metered.end.Format(stamp), start.UTC().Format(stamp), end.UTC().Format(stamp))
	case !created.IsZero():
		return fmt.Errorf("tenant_cluster %q: this cluster of tenant %q was created at %s, after this usage's window ended (%s)",
			cluster, tenant, created.UTC().Format(stamp), end.UTC().Format(stamp))
	}
	return unknownCluster(tenant, cluster, c.opts.Remember)
}

// unknownCluster is the refusal for a cluster vBilling does not know for
// the tenant. It says the same for a cluster that never existed and for
// another tenant's cluster, so it tells a tenant nothing about the others.
func unknownCluster(tenant, cluster string, remember time.Duration) error {
	kept := "while the usage it metered for it is in the ledger"
	if remember > 0 {
		kept = "for " + Age(remember) + " after its last metered window"
	}
	return fmt.Errorf("tenant_cluster %q is not a cluster vBilling knows for tenant %q: it knows a cluster while it runs and %s. "+
		"Name the cluster by its ID (the tenant_cluster of its metered usage), or leave tenant_cluster out to bill the tenant without a cluster", cluster, tenant, kept)
}
