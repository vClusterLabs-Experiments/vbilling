package controller

import (
	"log"
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A cluster stops being metered in two ways that are not a pause: an
// operator excludes it (vbilling.vcluster.com/exclude), or it is deleted.
// Both used to drop it from metering at once, so the window it happened in
// was never billed, not even for the part before. Both now leave a moment
// behind: the cluster stays a target with the time since then marked as
// not billed (metrics.Target.Excluded), so its last window is billed up to
// that moment and nothing after it.

// excludedLister is implemented by a Discoverer that reports the clusters
// it left out as excluded.
type excludedLister interface {
	Excluded() []discovery.TenantCluster
}

// goneDater is implemented by a Collector that watches control plane
// workloads and saw one being deleted.
type goneDater interface {
	ControlPlaneGone(cl discovery.TenantCluster) (at time.Time, gone, watched bool)
}

// departed is a cluster that was metered and no longer exists.
type departed struct {
	cluster discovery.TenantCluster
	at      time.Time // when it was deleted, as vBilling saw it
}

// UseExclusions records the times clusters are excluded from metering in
// store (pause.OpenSpans), so they survive a restart. Without it they are
// kept in memory.
func (c *Controller) UseExclusions(store *pause.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exclusions = store
}

func (c *Controller) exclusionStore() *pause.Store {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exclusions == nil {
		c.exclusions = pause.Memory()
	}
	return c.exclusions
}

// observeExclusions records since when each excluded cluster is excluded
// and when a cluster is metered again. It runs before the new view replaces
// the previous one. A cluster vBilling was metering is excluded from now; one
// it was not metering (vBilling just started, or the cluster is new) from
// the first window not committed yet.
func (c *Controller) observeExclusions(metered, excluded []discovery.TenantCluster, now time.Time) {
	store := c.exclusionStore()
	c.mu.Lock()
	was := make(map[string]bool, len(c.clusters))
	for i := range c.clusters {
		was[c.clusters[i].ExternalID()] = true
	}
	shared := c.shared
	c.mu.Unlock()
	present := make(map[string]bool, len(metered)+len(excluded))
	for _, cl := range excluded {
		id := cl.ExternalID()
		present[id] = true
		if shared[id] {
			continue // two clusters answer to this ID: no span for either
		}
		if _, open := store.Current(id); open {
			continue
		}
		from := now
		if !was[id] {
			from = cl.CreatedAt
			if c.spool != nil {
				if wm, ok := c.spool.Watermark(spool.SourceCollector); ok && wm.After(from) {
					from = wm
				}
			}
			if from.IsZero() || from.After(now) {
				from = now
			}
		}
		if _, err := store.Begin(id, from, pause.ByExcluded); err != nil {
			log.Printf("[controller] %s: record its exclusion: %v", id, err)
			continue
		}
		log.Printf("[controller] %s is excluded from metering since %s (%sexclude=true): nothing of it is billed until the setting is removed, and that time is never billed afterwards",
			id, from.UTC().Format(time.RFC3339), discovery.AnnotationPrefix)
	}
	for _, cl := range metered {
		id := cl.ExternalID()
		present[id] = true
		if ended, err := store.End(id, now); err != nil {
			log.Printf("[controller] %s: record the end of its exclusion: %v", id, err)
		} else if ended {
			log.Printf("[controller] %s is metered again from %s (%sexclude was removed)", id, now.UTC().Format(time.RFC3339), discovery.AnnotationPrefix)
			if c.health != nil {
				c.health.Remove(health.Name("metering", id))
			}
		}
	}
	// A cluster deleted while excluded: its exclusion ends, and is forgotten
	// once no window can be metered that far back.
	for id, spans := range store.All() {
		if n := len(spans); n > 0 && spans[n-1].Open() && !present[id] {
			_, _ = store.End(id, now)
		}
	}
	_ = store.Prune(now.Add(-c.cfg.MaxBackfill - time.Hour))
	if c.health != nil {
		for _, cl := range excluded {
			id := cl.ExternalID()
			sp, _ := store.Current(id)
			c.health.Set(health.Name("metering", id), "kubernetes", id, health.Paused,
				"excluded from metering since "+sp.From.UTC().Format(time.RFC3339)+" ("+discovery.AnnotationPrefix+"exclude=true): nothing of it is billed",
				"remove the setting to meter it again; the excluded time is never billed afterwards")
		}
	}
	c.tel.Set("vbilling_excluded_clusters", "Tenant clusters excluded from metering (vbilling.vcluster.com/exclude).", nil, float64(len(excluded)))
}

// observeDepartures remembers the clusters that were metered and are gone,
// with when, so the window each was deleted in is still metered up to that
// moment. It runs before the new view replaces the previous one.
//
// Where control plane workloads are watched, a cluster counts as deleted
// only if its workload was seen being deleted (a cluster discovery merely
// lost sight of is not billed a last window it may not have had); the
// moment is that of the deletion. Otherwise it is now, when discovery first
// misses it.
func (c *Controller) observeDepartures(metered, excluded []discovery.TenantCluster, now time.Time) {
	present := make(map[string]bool, len(metered)+len(excluded))
	for i := range metered {
		present[metered[i].ExternalID()] = true
	}
	for i := range excluded {
		present[excluded[i].ExternalID()] = true
	}
	dater, _ := c.coll.(goneDater)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.departed == nil {
		c.departed = map[string]departed{}
	}
	if c.missing == nil {
		c.missing = map[string]departed{}
	}
	// Clusters missed for the first time, and those missed a moment ago
	// whose deletion the watch had not reported yet.
	candidates := make([]departed, 0, len(c.missing))
	for _, cl := range c.clusters {
		if id := cl.ExternalID(); !present[id] && !c.shared[id] && !cl.External {
			candidates = append(candidates, departed{cluster: cl, at: now})
		}
	}
	for id, m := range c.missing {
		delete(c.missing, id)
		if !present[id] && now.Sub(m.at) <= missingGrace {
			candidates = append(candidates, m)
		}
	}
	for _, m := range candidates {
		id, at := m.cluster.ExternalID(), m.at
		if dater != nil {
			if seen, gone, watched := dater.ControlPlaneGone(m.cluster); watched {
				if !gone {
					// The list and the watch are two views: give the watch
					// a moment to report the deletion before concluding that
					// discovery only lost sight of the cluster.
					c.missing[id] = m
					continue
				}
				at = seen
			}
		}
		c.departed[id] = departed{cluster: m.cluster, at: at}
		log.Printf("[controller] %s was deleted at %s: it is metered up to then", id, at.UTC().Format(time.RFC3339))
	}
	for id, d := range c.departed {
		// Back under the same ID, or past every window that could bill it.
		if present[id] || now.Sub(d.at) > c.cfg.MaxBackfill+time.Hour {
			delete(c.departed, id)
		}
	}
}

// missingGrace is how long a cluster discovery no longer lists waits for
// the watch to confirm that its workload was deleted.
const missingGrace = 2 * time.Minute

// forgetDeparted drops what is still reported about clusters that are gone
// once the window they were deleted in is committed: their sources would
// otherwise read as healthy, with a last success from before the deletion,
// for hours.
//
// Committed means by every source that reads the cluster: Prometheus
// windows are read after the Kubernetes ones, and a deleted cluster's last
// counter samples belong to its last window too.
func (c *Controller) forgetDeparted() {
	if c.spool == nil {
		return
	}
	through, _ := c.spool.Watermark(spool.SourceCollector)
	if c.prom != nil && c.prometheusConfigured() {
		if wm, _ := c.spool.Watermark(metrics.SourcePrometheus); wm.Before(through) {
			through = wm
		}
	}
	c.mu.Lock()
	var done []string
	for id, d := range c.departed {
		if !through.Before(d.at) {
			done = append(done, id)
			delete(c.departed, id)
		}
	}
	c.mu.Unlock()
	sort.Strings(done)
	for _, id := range done {
		if c.health != nil {
			c.health.Forget(id)
		}
		if c.pauses != nil {
			// Its pause, if it was paused, ended with it.
			_, _ = c.pauses.End(id, through)
		}
	}
}

// lifecycleTargets returns the clusters that are not metered any more but
// still have a window to close: excluded ones and deleted ones, each with
// the time since then marked as not billed. Windows that lie entirely in
// that time drop them again (ownerRuns).
func (c *Controller) lifecycleTargets() []metrics.Target {
	src := discovery.TenantSource(c.cfg.TenantSource)
	store := c.exclusionStore()
	c.mu.Lock()
	excluded := append([]discovery.TenantCluster(nil), c.excluded...)
	gone := make([]departed, 0, len(c.departed))
	for _, d := range c.departed {
		gone = append(gone, d)
	}
	shared := c.shared
	c.mu.Unlock()
	sort.Slice(gone, func(i, j int) bool { return gone[i].cluster.ExternalID() < gone[j].cluster.ExternalID() })
	var out []metrics.Target
	add := func(cl discovery.TenantCluster, spans []pause.Span) {
		id := cl.ExternalID()
		t, _ := c.tenants.Get(cl.TenantID(src))
		target := metrics.Target{Cluster: cl, Tenant: t.ID, Project: cl.Project, TenantClass: t.Class, Excluded: spans}
		if c.pauses != nil && !shared[id] {
			target.Paused = c.pauses.Spans(id)
		}
		out = append(out, target)
	}
	for _, cl := range excluded {
		if spans := store.Spans(cl.ExternalID()); len(spans) > 0 {
			add(cl, spans)
		}
	}
	for _, d := range gone {
		cl := d.cluster
		cl.DeletedAt = d.at
		add(cl, pause.Union(store.Spans(cl.ExternalID()), []pause.Span{{From: d.at, By: pause.ByExcluded}}))
	}
	return out
}

// meteredIn drops the targets that are excluded from metering for the whole
// window. skipped names them, so runs of windows split where the set of
// targets changes.
func meteredIn(targets []metrics.Target, w metrics.Window) (kept []metrics.Target, skipped string) {
	size := w.End.Sub(w.Start)
	for i := range targets {
		if len(targets[i].Excluded) == 0 {
			continue
		}
		if pause.Overlap(targets[i].Excluded, time.Time{}, time.Time{}, w.Start, w.End) < size {
			continue
		}
		if kept == nil {
			kept = append(make([]metrics.Target, 0, len(targets)), targets[:i]...)
		}
		skipped += targets[i].Cluster.ExternalID() + "\x00"
	}
	if kept == nil {
		return targets, ""
	}
	kept = kept[:0]
	for i := range targets {
		if len(targets[i].Excluded) > 0 && pause.Overlap(targets[i].Excluded, time.Time{}, time.Time{}, w.Start, w.End) >= size {
			continue
		}
		kept = append(kept, targets[i])
	}
	return kept, skipped
}

// noteClusterless keeps the tenant records in step with clusters that left
// metering. A tenant whose last cluster was deleted or excluded stays known
// until it is offboarded, but with the clusters it has now (none), not the
// ones it had: an operator reading the API must never see a deleted
// cluster as live. Clusters excluded from metering are listed apart, so an
// exclusion shows where the cluster used to be.
func (c *Controller) noteClusterless(current []usage.Tenant, excluded []discovery.TenantCluster) {
	src := discovery.TenantSource(c.cfg.TenantSource)
	excludedOf := map[string][]string{}
	for i := range excluded {
		id := excluded[i].TenantID(src)
		excludedOf[id] = append(excludedOf[id], excluded[i].ExternalID())
	}
	for id := range excludedOf {
		sort.Strings(excludedOf[id])
	}
	has := make(map[string]bool, len(current))
	for _, t := range current {
		has[t.ID] = true
		if ex := excludedOf[t.ID]; len(ex) > 0 {
			t.ExcludedClusters = ex
			c.tenants.Upsert(t)
		}
	}
	for _, t := range c.tenants.All() {
		if has[t.ID] {
			continue
		}
		ex := excludedOf[t.ID]
		if len(t.Clusters) == 0 && equalStrings(t.ExcludedClusters, ex) {
			continue
		}
		t.Clusters, t.ExcludedClusters = nil, ex
		if t.Metadata != nil {
			meta := make(map[string]string, len(t.Metadata))
			for k, v := range t.Metadata {
				meta[k] = v
			}
			meta["clusters"] = ""
			t.Metadata = meta
		}
		c.tenants.Upsert(t)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
