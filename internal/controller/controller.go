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
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
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

// PromCollector reads Prometheus usage under its own ledger source.
type PromCollector interface {
	Collect(ctx context.Context, targets []metrics.Target, windows []metrics.Window) ([]metrics.PromWindow, error)
	Commit(results []metrics.PromWindow, commit func(w metrics.Window, evs []usage.Event) error) error
}

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
	// lastUnrequested is the last logged count of pods without requests.
	lastUnrequested int

	// gaps: tenant cluster external ID -> end of the first window its own
	// API could not be read for. Persisted, so a restart still fills them.
	gaps *kv.Store

	prom    PromCollector
	aliases map[string]string // gateway or external identity -> tenant ID
	// ambiguous are aliases more than one tenant claims: billing uses the
	// last claim, but the gateway never blocks them for either tenant.
	ambiguous   map[string]bool
	aliasWarned map[string]string
	owners      Owners // optional: past owners of clusters (TENANT_SOURCE=platform)

	pauses *pause.Store // pause spans; nil = pauses are not tracked
}

// UsePauses tracks when clusters are paused, so capacity is not billed for
// that time (PAUSE_KEEP_BILLING decides what keeps billing).
func (c *Controller) UsePauses(p *pause.Store) { c.pauses = p }

// SetPaused updates the known state of a cluster right after vBilling
// paused or resumed it, ahead of the next discovery.
func (c *Controller) SetPaused(id string, paused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.clusters {
		if c.clusters[i].ExternalID() == id {
			c.clusters[i].Paused = paused
		}
	}
}

type pauseForgetter interface{ ForgetPaused(id string) }

// observePauses records pauses seen in discovery: ones vBilling did not
// make (vcluster pause, an operator) and resumes made outside vBilling.
// vBilling's own pauses start at their paused-at annotation, to the second.
func (c *Controller) observePauses(clusters []discovery.TenantCluster, now time.Time) {
	if c.pauses == nil {
		return
	}
	forget, _ := c.coll.(pauseForgetter)
	for _, cl := range clusters {
		if cl.External {
			continue // no control plane here to see paused: vBilling's enforcer records its pauses
		}
		id := cl.ExternalID()
		_, open := c.pauses.Current(id)
		switch {
		case cl.Paused && !open:
			at, by := now, "external"
			if cl.Annotations[discovery.AnnotationPausedBy] == "vbilling" {
				by = "vbilling"
				if t, err := time.Parse(time.RFC3339Nano, cl.Annotations[discovery.AnnotationPausedAt]); err == nil && !t.After(now) {
					at = t
				}
			}
			if _, err := c.pauses.Begin(id, at, by); err != nil {
				log.Printf("[controller] %s: record pause: %v", id, err)
			} else {
				log.Printf("[controller] %s: paused since %s (%s)", id, at.UTC().Format(time.RFC3339), by)
			}
		case !cl.Paused && open:
			if _, err := c.pauses.End(id, now); err != nil {
				log.Printf("[controller] %s: record resume: %v", id, err)
			} else {
				log.Printf("[controller] %s: resumed", id)
			}
		}
		if !cl.Paused && forget != nil {
			forget.ForgetPaused(id)
		}
	}
	// Pauses that ended before any window that can still be metered.
	_ = c.pauses.Prune(now.Add(-c.cfg.MaxBackfill - time.Hour))
}

// UsePrometheus enables Prometheus-read usage.
func (c *Controller) UsePrometheus(p PromCollector) { c.prom = p }

// ResolveTenant maps a value an emitter sends (a tenant ID, or an alias
// from the vbilling.vcluster.com/aliases setting of a cluster) to a known
// tenant. Unknown values are returned as they are, with known=false.
func (c *Controller) ResolveTenant(v string) (string, bool) {
	if _, ok := c.tenants.Get(v); ok {
		return v, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.aliases[v]; ok {
		return t, true
	}
	return v, false
}

// GatewayIdentities lists a tenant's gateway identities: its ID and every
// alias (vbilling.vcluster.com/aliases) that ResolveTenant maps to it. An
// alias that is another tenant's ID bills to that tenant, so it is not one
// of this tenant's identities; neither is an alias two tenants claim.
func (c *Controller) GatewayIdentities(tenant string) []string {
	out := []string{tenant}
	c.mu.Lock()
	defer c.mu.Unlock()
	for alias, t := range c.aliases {
		if t != tenant || c.ambiguous[alias] {
			continue
		}
		if _, other := c.tenants.Get(alias); other {
			continue
		}
		out = append(out, alias)
	}
	sort.Strings(out[1:])
	return out
}

// warnAliasConflicts logs, once per change, aliases that are another
// tenant's ID or that two tenants claim.
func (c *Controller) warnAliasConflicts(aliases map[string]string, ambiguous map[string]bool) {
	warn := map[string]string{}
	for a, t := range aliases {
		switch {
		case ambiguous[a]:
			warn[a] = "is claimed by more than one tenant; usage bills to " + t + " and the gateway never blocks it"
		default:
			if _, ok := c.tenants.Get(a); ok {
				warn[a] = "is tenant " + a + "'s own ID; usage sent as " + a + " bills to " + a + ", not to " + t
			}
		}
	}
	c.mu.Lock()
	prev := c.aliasWarned
	c.aliasWarned = warn
	c.mu.Unlock()
	for a, msg := range warn {
		if prev[a] != msg {
			log.Printf("[controller] WARNING: alias %q %s. Fix the %saliases setting", a, msg, discovery.AnnotationPrefix)
		}
	}
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
	c.CollectPrometheusDue(ctx)

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
			c.CollectPrometheusDue(ctx)
			collect.Reset(c.untilNextWindow())
		}
	}
}

// minSettle gives the kubelet time to report state from before the
// boundary. A container that exits shortly before a window ends shows as
// running until the kubelet posts its status: on a DGX Spark a pod that
// finished at 07:08:59 was still running at 07:09:02, so a 2s settle billed
// it a second past its end. The kubelet relists every second and then
// writes the status, so give it a few seconds.
const minSettle = 5 * time.Second

// settle is how long after a window ends it is collected: COLLECTION_DELAY,
// at least minSettle. A longer delay lets Kubernetes notice a node that died
// without warning (it marks it Unknown 40 to 50 seconds after its last
// heartbeat) before the window it died in is committed.
func (c *Controller) settle() time.Duration { return max(c.cfg.CollectionDelay, minSettle) }

// latestDue is the end of the newest window that is past its settle time.
func (c *Controller) latestDue() time.Time {
	return c.now().UTC().Add(-c.settle()).Truncate(c.cfg.CollectionInterval)
}

func (c *Controller) untilNextWindow() time.Duration {
	now := c.now()
	next := c.latestDue().Add(c.cfg.CollectionInterval).Add(c.settle())
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

	aliases := map[string]string{}
	ambiguous := map[string]bool{}
	src := discovery.TenantSource(c.cfg.TenantSource)
	for i := range clusters {
		owner := clusters[i].TenantID(src)
		for _, a := range strings.Split(clusters[i].Meta("aliases"), ",") {
			if a = strings.TrimSpace(a); a == "" || a == owner {
				continue
			}
			if prev, ok := aliases[a]; ok && prev != owner {
				ambiguous[a] = true
			}
			aliases[a] = owner
		}
	}
	c.warnAliasConflicts(aliases, ambiguous)
	c.observePauses(clusters, now)
	c.mu.Lock()
	c.aliases = aliases
	c.ambiguous = ambiguous
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
		first(&t.Plan, cl.Meta("plan"), c.cfg.PlanByClass[t.Class])
		if v := cl.Meta("stripe-customer-id"); v != "" {
			t.ProviderIDs["stripe"] = v
		}
		if v := cl.Meta("metronome-customer-id"); v != "" {
			t.ProviderIDs["metronome"] = v
		}
		if v := cl.Meta("cost-center"); v != "" {
			t.Metadata["cost_center"] = v
		}
		for k, v := range cl.PlatformMetadata() {
			if _, set := t.Metadata[k]; !set {
				t.Metadata[k] = v
			}
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
	latestEnd := c.latestDue()
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

	targets := c.targets()

	started := time.Now()
	byWindow, st, err := c.collectAttributed(ctx, targets, windows, c.coll.Collect)
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
	c.tel.Set("vbilling_pods_without_requests", "Running pods on shared nodes that request no CPU or memory, so CPU_MEMORY_BASIS=requests bills them nothing.", nil, float64(st.Unrequested))
	if st.Unrequested != c.lastUnrequested {
		if st.Unrequested > 0 {
			log.Printf("[controller] %d running pod(s) request no CPU or memory, so they are not billed for either (CPU_MEMORY_BASIS=requests); give them default requests with a vCluster LimitRange (policies.limitRange)", st.Unrequested)
		}
		c.lastUnrequested = st.Unrequested
	}
	if st.UsageUnmeasured > 0 {
		log.Printf("[controller] CPU_MEMORY_BASIS=usage: CPU and memory of %d pod-window(s) collected late were not billed (metrics-server has no usage for past windows; requests is the exact basis)", st.UsageUnmeasured)
		c.tel.Add("vbilling_cpu_memory_unmeasured_total", "Pod-windows whose CPU and memory were not billed: usage basis, window collected late.", nil, float64(st.UsageUnmeasured))
	}
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
		byWindow, fst, err := c.collectAttributed(ctx, []metrics.Target{t}, missed, filler.CollectTenantAPI)
		if err != nil || len(fst.TenantAPIFailed) > 0 {
			continue // still unreachable: try again next window
		}
		var evs []usage.Event
		for _, w := range missed {
			evs = append(evs, byWindow[w.End]...)
		}
		saltFill(evs)
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

// saltFill gives events filled after a tenant API outage IDs of their own,
// as late records get. The window was committed with what the control plane
// cluster shows, and a cluster's own volumes and load balancers are built
// from the same template as that part's (tenant, metric, window, SKU,
// resource ID, dimensions): unsalted, a fill event can carry the ID of a
// committed one, and billing platforms drop it as a duplicate. A fill
// repeated after a crash gets the same salted IDs, so AppendIngest still
// deduplicates it.
func saltFill(evs []usage.Event) {
	for i := range evs {
		evs[i].ID = usage.DeriveIDWith(&evs[i], "fill")
		if evs[i].Properties == nil {
			evs[i].Properties = map[string]any{}
		}
		evs[i].Properties["filled"] = true
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

// targets resolves every known cluster to its billing identity.
func (c *Controller) targets() []metrics.Target {
	src := discovery.TenantSource(c.cfg.TenantSource)
	var targets []metrics.Target
	for _, cl := range c.Clusters() {
		t, _ := c.tenants.Get(cl.TenantID(src))
		target := metrics.Target{Cluster: cl, Tenant: t.ID, Project: cl.Project, TenantClass: t.Class}
		if c.pauses != nil {
			target.Paused = c.pauses.Spans(cl.ExternalID())
		}
		targets = append(targets, target)
	}
	return targets
}

// CollectPrometheusDue reads Prometheus usage for every window since the
// Prometheus watermark, bounded by MAX_BACKFILL. It runs after the
// Kubernetes windows are committed and never holds them up. When every read
// fails (Prometheus is down), nothing is committed: the windows are read
// again, one by one, once it answers. When only some reads fail, the
// windows are committed without them, and those counters bill their whole
// increase in the first window that reads them again.
func (c *Controller) CollectPrometheusDue(ctx context.Context) {
	if c.prom == nil || !c.Ready() || !c.prometheusConfigured() {
		return
	}
	size := c.cfg.CollectionInterval
	latestEnd := c.latestDue()
	firstEnd := latestEnd
	if wm, ok := c.spool.Watermark(metrics.SourcePrometheus); ok {
		if !latestEnd.After(wm) {
			return
		}
		firstEnd = wm.Add(size)
		if earliest := latestEnd.Add(-c.cfg.MaxBackfill).Truncate(size).Add(size); firstEnd.Before(earliest) {
			log.Printf("[controller] Prometheus windows before %s are older than MAX_BACKFILL=%s; counters bill that usage in the first window read",
				earliest.Add(-size).Format(time.RFC3339), c.cfg.MaxBackfill)
			firstEnd = earliest
		}
	}
	var windows []metrics.Window
	for end := firstEnd; !end.After(latestEnd); end = end.Add(size) {
		windows = append(windows, metrics.Window{Start: end.Add(-size), End: end, Latest: end.Equal(latestEnd)})
	}
	// Windows read after the fact bill each cluster's owner during them,
	// as Kubernetes backfill does. Each run of windows with the same owners
	// is read and committed before the next one is read: counters bill
	// from the baselines the previous run committed.
	total := 0
	for _, r := range c.ownerRuns(c.targets(), windows) {
		results, err := c.prom.Collect(ctx, r.targets, r.windows)
		if err != nil {
			log.Printf("[controller] Prometheus: %v; %d window(s) wait until it answers", err, len(r.windows))
			c.tel.Add("vbilling_prometheus_unavailable_total", "Runs where every Prometheus read failed (windows wait).", nil, 1)
			return
		}
		props := platformProps(r.targets)
		err = c.prom.Commit(results, func(w metrics.Window, evs []usage.Event) error {
			stampPlatform(evs, props)
			total += len(evs)
			return c.spool.AppendWindow(metrics.SourcePrometheus, w.End, evs, "")
		})
		if err != nil {
			log.Printf("[controller] commit Prometheus windows: %v (will retry)", err)
			return
		}
	}
	if total > 0 || len(windows) > 1 {
		log.Printf("[controller] Prometheus: %d event(s) over %d window(s)", total, len(windows))
	}
	c.tel.Add("vbilling_prometheus_events_total", "Usage events read from Prometheus.", nil, float64(total))
}

// prometheusConfigured is true when PROMETHEUS_URL is set or a cluster
// names its own Prometheus.
func (c *Controller) prometheusConfigured() bool {
	if c.cfg.PrometheusURL != "" {
		return true
	}
	for _, cl := range c.Clusters() {
		if cl.Meta("prometheus-url") != "" {
			return true
		}
	}
	return false
}
