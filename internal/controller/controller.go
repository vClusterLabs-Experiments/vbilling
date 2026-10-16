// Package controller runs vBilling's two loops: tenant reconciliation and
// window-aligned metering into the durable ledger.
package controller

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Discoverer abstracts discovery for tests.
type Discoverer interface {
	Discover(ctx context.Context) ([]discovery.TenantCluster, error)
}

// Collector abstracts metering for tests.
type Collector interface {
	Collect(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error)
}

// tenantFiller re-meters windows a tenant cluster's own API missed.
type tenantFiller interface {
	CollectTenantAPI(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error)
}

// SourceTenantFill is the ledger source of windows filled after a tenant API outage.
const SourceTenantFill = "tenant-api-fill"

// Controller owns tenant reconciliation and metering.
type Controller struct {
	cfg     *config.Config
	disc    Discoverer
	coll    Collector
	spool   *spool.Spool
	tenants *pipeline.TenantRegistry
	dests   []destinations.Destination
	tel     *telemetry.Registry
	now     func() time.Time

	mu         sync.Mutex
	clusters   []discovery.TenantCluster
	lastSeen   map[string]time.Time       // tenant -> last time it had a tenant cluster
	offboarded map[string]map[string]bool // tenant -> destinations done offboarding it
	discovered bool
	lastWindow time.Time

	// gaps: tenant cluster external ID -> end of the first window its own
	// API could not be read for. Persisted, so a restart still fills them.
	gaps *kv.Store
}

func New(cfg *config.Config, disc Discoverer, coll Collector, sp *spool.Spool, tenants *pipeline.TenantRegistry,
	dests []destinations.Destination, tel *telemetry.Registry) *Controller {
	if tel == nil {
		tel = telemetry.New()
	}
	gaps := kv.Memory()
	if cfg.DataDir != "" {
		if s, err := kv.Open(filepath.Join(cfg.DataDir, "tenant-api-gaps.json")); err != nil {
			log.Printf("[controller] tenant API gaps kept in memory only: %v", err)
		} else {
			gaps = s
		}
	}
	return &Controller{cfg: cfg, disc: disc, coll: coll, spool: sp, tenants: tenants, dests: dests, tel: tel,
		now: time.Now, lastSeen: map[string]time.Time{}, offboarded: map[string]map[string]bool{}, gaps: gaps}
}

// Run reconciles tenants every ReconcileInterval and closes a metering
// window at every CollectionInterval boundary until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	log.Printf("[controller] starting (adapters=%v, window=%s, reconcile=%s, region=%s)",
		c.cfg.Adapters, c.cfg.CollectionInterval, c.cfg.ReconcileInterval, c.cfg.Region)
	c.Reconcile(ctx)
	c.CollectDue(ctx)

	reconcile := time.NewTicker(c.cfg.ReconcileInterval)
	defer reconcile.Stop()
	collect := time.NewTimer(c.untilNextWindow())
	defer collect.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-reconcile.C:
			c.Reconcile(ctx)
		case <-collect.C:
			c.CollectDue(ctx)
			collect.Reset(c.untilNextWindow())
		}
	}
}

// settle gives the API server a moment to reflect state at the boundary.
const settle = 2 * time.Second

func (c *Controller) untilNextWindow() time.Duration {
	now := c.now()
	next := now.Truncate(c.cfg.CollectionInterval).Add(c.cfg.CollectionInterval).Add(settle)
	return next.Sub(now)
}

// Ready reports whether discovery has succeeded at least once.
func (c *Controller) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.discovered
}

// Clusters returns the tenant clusters from the last successful discovery.
func (c *Controller) Clusters() []discovery.TenantCluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]discovery.TenantCluster(nil), c.clusters...)
}

// ClustersOf returns the tenant clusters billed to a tenant.
func (c *Controller) ClustersOf(tenant string) []discovery.TenantCluster {
	var out []discovery.TenantCluster
	for _, cl := range c.Clusters() {
		if cl.TenantID(discovery.TenantSource(c.cfg.TenantSource)) == tenant {
			out = append(out, cl)
		}
	}
	return out
}

// LastWindow is the end of the last committed metering window.
func (c *Controller) LastWindow() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastWindow
}

// Reconcile refreshes tenant clusters and tenants and offboards tenants that
// have been gone for longer than OFFBOARD_GRACE. A discovery error changes
// nothing: an API hiccup must never look like deleted tenants.
func (c *Controller) Reconcile(ctx context.Context) {
	clusters, err := c.disc.Discover(ctx)
	if err != nil {
		log.Printf("[controller] discovery failed, keeping previous view: %v", err)
		c.tel.Add("vbilling_discovery_failures_total", "Failed tenant-cluster discovery runs.", nil, 1)
		return
	}
	now := c.now()
	tenants := c.tenantsFrom(clusters)
	for _, t := range tenants {
		c.tenants.Upsert(t)
	}

	c.mu.Lock()
	c.clusters = clusters
	c.discovered = true
	for _, t := range tenants {
		c.lastSeen[t.ID] = now
		delete(c.offboarded, t.ID) // back before offboarding finished: start over next time
	}
	var gone []usage.Tenant
	current := map[string]bool{}
	for _, t := range tenants {
		current[t.ID] = true
	}
	for id, seen := range c.lastSeen {
		if !current[id] && now.Sub(seen) >= c.cfg.OffboardGrace {
			t, _ := c.tenants.Get(id)
			gone = append(gone, t)
		}
	}
	c.mu.Unlock()

	for _, t := range gone {
		// Each destination offboards a tenant once. A permanent rejection is
		// final too: retrying it every pass would never succeed.
		c.mu.Lock()
		done := c.offboarded[t.ID]
		if done == nil {
			done = map[string]bool{}
			c.offboarded[t.ID] = done
		}
		c.mu.Unlock()
		ok := true
		for _, d := range c.dests {
			if done[d.Name()] {
				continue
			}
			switch err := d.RemoveTenant(ctx, t); {
			case err == nil:
				done[d.Name()] = true
			case destinations.IsPermanent(err):
				log.Printf("[controller] offboard %s from %s: %v (rejected, not retried)", t.ID, d.Name(), err)
				done[d.Name()] = true
			default:
				log.Printf("[controller] offboard %s from %s: %v (will retry)", t.ID, d.Name(), err)
				ok = false
			}
		}
		if ok {
			log.Printf("[controller] tenant %s offboarded after %s without tenant clusters", t.ID, c.cfg.OffboardGrace)
			c.tenants.Delete(t.ID)
			c.mu.Lock()
			delete(c.lastSeen, t.ID)
			delete(c.offboarded, t.ID)
			c.mu.Unlock()
		}
	}
	c.tel.Set("vbilling_tenant_clusters", "Tenant clusters being metered.", nil, float64(len(clusters)))
	c.tel.Set("vbilling_tenants", "Billing tenants with at least one tenant cluster.", nil, float64(len(tenants)))
}

// tenantsFrom groups tenant clusters into billing tenants.
func (c *Controller) tenantsFrom(clusters []discovery.TenantCluster) []usage.Tenant {
	src := discovery.TenantSource(c.cfg.TenantSource)
	byID := map[string]*usage.Tenant{}
	var ids []string
	for i := range clusters {
		cl := &clusters[i]
		id := cl.TenantID(src)
		t, ok := byID[id]
		if !ok {
			t = &usage.Tenant{ID: id, Region: c.cfg.Region, CreatedAt: cl.CreatedAt, Metadata: map[string]string{}, ProviderIDs: map[string]string{}}
			byID[id] = t
			ids = append(ids, id)
		}
		t.Clusters = append(t.Clusters, cl.ExternalID())
		if cl.CreatedAt.Before(t.CreatedAt) {
			t.CreatedAt = cl.CreatedAt
		}
		first := func(cur *string, vals ...string) {
			for _, v := range vals {
				if *cur == "" && v != "" {
					*cur = v
				}
			}
		}
		first(&t.DisplayName, cl.Meta("display-name"), cl.DisplayName)
		first(&t.Email, cl.Meta("email"))
		first(&t.Currency, cl.Meta("currency"))
		first(&t.Project, cl.Project)
		first(&t.Class, cl.Meta("tenant-class"), c.cfg.TenantClass)
		first(&t.Plan, cl.Meta("plan"))
		if v := cl.Meta("stripe-customer-id"); v != "" {
			t.ProviderIDs["stripe"] = v
		}
		if v := cl.Meta("metronome-customer-id"); v != "" {
			t.ProviderIDs["metronome"] = v
		}
		if v := cl.Meta("cost-center"); v != "" {
			t.Metadata["cost_center"] = v
		}
	}
	out := make([]usage.Tenant, 0, len(ids))
	sort.Strings(ids)
	for _, id := range ids {
		t := byID[id]
		sort.Strings(t.Clusters)
		if t.DisplayName == "" {
			t.DisplayName = id
			if len(t.Clusters) == 1 && strings.HasPrefix(id, "vcluster-") {
				t.DisplayName = "Tenant cluster " + strings.TrimPrefix(id, "vcluster-")
			}
		}
		t.Metadata["region"] = t.Region
		t.Metadata["clusters"] = strings.Join(t.Clusters, ",")
		if t.Project != "" {
			t.Metadata["project"] = t.Project
		}
		if len(t.ProviderIDs) == 0 {
			t.ProviderIDs = nil
		}
		out = append(out, *t)
	}
	return out
}

// CollectDue meters every closed window since the watermark (bounded by
// MAX_BACKFILL) and commits each one atomically to the ledger.
func (c *Controller) CollectDue(ctx context.Context) {
	if !c.Ready() {
		return
	}
	size := c.cfg.CollectionInterval
	latestEnd := c.now().UTC().Truncate(size)
	firstEnd := latestEnd
	note := ""
	if wm, ok := c.spool.Watermark(spool.SourceCollector); ok {
		if !latestEnd.After(wm) {
			return
		}
		firstEnd = wm.Add(size)
		// The oldest window we backfill starts MAX_BACKFILL before the latest end.
		if earliest := latestEnd.Add(-c.cfg.MaxBackfill).Truncate(size).Add(size); firstEnd.Before(earliest) {
			gap := earliest.Sub(firstEnd)
			note = "gap: " + gap.String() + " older than MAX_BACKFILL not metered"
			log.Printf("[controller] %d window(s) before %s are older than MAX_BACKFILL=%s and will not be metered",
				int(gap/size), earliest.Add(-size).Format(time.RFC3339), c.cfg.MaxBackfill)
			firstEnd = earliest
		}
	}
	var windows []metrics.Window
	for end := firstEnd; !end.After(latestEnd); end = end.Add(size) {
		windows = append(windows, metrics.Window{Start: end.Add(-size), End: end, Latest: end.Equal(latestEnd)})
	}

	src := discovery.TenantSource(c.cfg.TenantSource)
	var targets []metrics.Target
	for _, cl := range c.Clusters() {
		t, _ := c.tenants.Get(cl.TenantID(src))
		targets = append(targets, metrics.Target{Cluster: cl, Tenant: t.ID, Project: cl.Project, TenantClass: t.Class})
	}

	started := time.Now()
	byWindow, st, err := c.coll.Collect(ctx, targets, windows)
	if err != nil {
		log.Printf("[controller] metering failed for %d window(s), will retry: %v", len(windows), err)
		c.tel.Add("vbilling_collection_failures_total", "Failed metering runs (windows are retried).", nil, 1)
		return
	}
	total := 0
	for i, w := range windows {
		n := ""
		if i == 0 {
			n = note
		}
		evs := byWindow[w.End]
		if err := c.spool.AppendWindow(spool.SourceCollector, w.End, evs, n); err != nil {
			log.Printf("[controller] commit window %s: %v", w.End.Format(time.RFC3339), err)
			return
		}
		total += len(evs)
		c.mu.Lock()
		c.lastWindow = w.End
		c.mu.Unlock()
	}
	c.fillTenantGaps(ctx, st, windows, targets)
	if len(windows) > 1 {
		log.Printf("[controller] backfilled %d window(s) after a gap", len(windows)-1)
		c.tel.Add("vbilling_backfilled_windows_total", "Windows metered after a gap (restart or outage).", nil, float64(len(windows)-1))
	}
	log.Printf("[controller] window %s: %d events for %d tenant cluster(s) (%d pods, %d GPU pods, %d nodes down)",
		latestEnd.Format("15:04:05"), total, len(targets), st.Pods, st.GPUPods, st.NodesDown)
	c.tel.Add("vbilling_events_metered_total", "Usage events written to the ledger by the collector.", nil, float64(total))
	c.tel.Set("vbilling_last_window_end_timestamp_seconds", "End of the last committed metering window.", nil, float64(latestEnd.Unix()))
	c.tel.Set("vbilling_collection_duration_seconds", "Duration of the last metering run.", nil, time.Since(started).Seconds())
	c.tel.Set("vbilling_nodes_down", "Nodes excluded from billing as not ready or unhealthy.", nil, float64(st.NodesDown))
}

// fillTenantGaps records tenant clusters whose own API failed in this run
// and, once it answers again, meters the windows it missed. Those events go
// to the ledger as late events, deduplicated by ID. A gap is the span of
// failed windows only (first|last window end), so a fill repeated after a
// crash never touches windows that were committed normally. Windows older
// than MAX_BACKFILL are not filled.
func (c *Controller) fillTenantGaps(ctx context.Context, st metrics.Stats, windows []metrics.Window, targets []metrics.Target) {
	failed := map[string]bool{}
	first, last := windows[0].End, windows[len(windows)-1].End
	for _, id := range st.TenantAPIFailed {
		failed[id] = true
		from := first
		if g, ok := parseGap(c.gaps, id); ok {
			from = g[0]
		} else {
			log.Printf("[controller] %s: tenant API unreachable from window %s, will fill once it answers", id, first.Format(time.RFC3339))
		}
		_ = c.gaps.Set(id, from.Format(time.RFC3339)+"|"+last.Format(time.RFC3339))
	}
	filler, ok := c.coll.(tenantFiller)
	if !ok {
		return
	}
	size := c.cfg.CollectionInterval
	earliest := last.Add(-c.cfg.MaxBackfill)
	for _, t := range targets {
		id := t.Cluster.ExternalID()
		g, ok := parseGap(c.gaps, id)
		if !ok || failed[id] {
			continue
		}
		var missed []metrics.Window
		for end := g[0]; !end.After(g[1]); end = end.Add(size) {
			if end.After(earliest) {
				missed = append(missed, metrics.Window{Start: end.Add(-size), End: end})
			}
		}
		if len(missed) == 0 {
			_ = c.gaps.Delete(id)
			continue
		}
		byWindow, fst, err := filler.CollectTenantAPI(ctx, []metrics.Target{t}, missed)
		if err != nil || len(fst.TenantAPIFailed) > 0 {
			continue // still unreachable: try again next window
		}
		var evs []usage.Event
		for _, w := range missed {
			evs = append(evs, byWindow[w.End]...)
		}
		accepted, dups, err := c.spool.AppendIngest(SourceTenantFill, evs)
		if err != nil {
			log.Printf("[controller] %s: fill %d window(s): %v", id, len(missed), err)
			continue
		}
		_ = c.gaps.Delete(id)
		log.Printf("[controller] %s: filled %d window(s) missed during a tenant API outage (%d events, %d already in the ledger)",
			id, len(missed), len(accepted), len(dups))
		c.tel.Add("vbilling_tenant_api_filled_windows_total", "Windows filled after a tenant API outage.", nil, float64(len(missed)))
	}
}

// parseGap reads a "first|last" window-end span.
func parseGap(store *kv.Store, id string) ([2]time.Time, bool) {
	v, ok := store.Get(id)
	if !ok {
		return [2]time.Time{}, false
	}
	a, b, found := strings.Cut(v, "|")
	from, err1 := time.Parse(time.RFC3339, a)
	to, err2 := time.Parse(time.RFC3339, b)
	if !found || err1 != nil || err2 != nil || to.Before(from) {
		_ = store.Delete(id)
		return [2]time.Time{}, false
	}
	return [2]time.Time{from, to}, true
}
