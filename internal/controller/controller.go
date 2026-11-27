// Package controller runs vBilling's two loops: tenant reconciliation and
// window-aligned metering into the durable ledger.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
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

	mu              sync.Mutex
	clusters        []discovery.TenantCluster
	lastSeen        map[string]time.Time       // tenant -> last time it had a tenant cluster or new usage
	offboarded      map[string]map[string]bool // tenant -> destinations done offboarding it
	off             offboarding                // where each departed tenant's usage ends (offboard.go)
	held            heldUsage                  // optional: usage held from destinations until priced
	safeguard       *safeguard.Checker         // optional: flags windows that bill more than exists
	discovered      bool
	lastWindow      time.Time
	collectionRetry time.Time // heartbeat uncertainty is retried before the next boundary
	// promRetry is when the newest Prometheus window is looked at again:
	// its samples had not all reached Prometheus (CollectPrometheusDue).
	promRetry time.Time
	// promWaiting is when a waiting Prometheus window was last logged.
	promWaiting time.Time
	// liveSince is when this controller started metering (Run). A window
	// that ended before it was not watched as it happened: it is metered
	// from what the API still shows, like any backfilled window.
	liveSince time.Time
	// lastUnrequested is the last logged count of pods without requests.
	lastUnrequested int

	// usageRead is the ledger sequence read through for recent usage
	// (usageSince); usageStarted is false until the first reconcile;
	// usageThrough is whether the last reconcile read the ledger through.
	usageRead    uint64
	usageStarted bool
	usageThrough bool

	// gaps: a cluster's external ID -> the windows its own API could not be
	// read for (tenantGaps). Persisted, so a restart still fills them.
	gaps *kv.Store
	// gapsFailing: the last write of gaps failed (reported once it works).
	gapsFailing bool
	health      *health.Registry // optional: where gap writes are reported

	prom    PromCollector
	aliases map[string]string // gateway or external identity -> tenant ID
	// ambiguous are aliases more than one tenant claims: billing uses the
	// last claim, but the gateway never blocks them for either tenant.
	ambiguous   map[string]bool
	aliasWarned map[string]string
	owners      Owners // optional: past owners of clusters (TENANT_SOURCE=platform)

	pauses *pause.Store // pause spans; nil = pauses are not tracked
	// exclusions are the times clusters were excluded from metering;
	// excluded the clusters that are now; departed those that were metered
	// and are gone (lifecycle.go).
	exclusions *pause.Store
	excluded   []discovery.TenantCluster
	departed   map[string]departed
	// missing are clusters discovery stopped listing whose deletion the
	// watch has not confirmed yet (at: when they were first missed).
	missing map[string]departed

	// bound are the tenants INGEST_TOKENS are bound to; known reports
	// tenants vBilling knows from elsewhere (UseKnownTenants).
	bound map[string]bool
	known func(id string) bool
	// shared are external IDs that more than one discovered cluster maps
	// to (see sharedIDs); sharedWarned is what was last logged about them.
	shared       map[string]bool
	sharedWarned string
}

// UsePauses tracks when clusters are paused, so capacity is not billed for
// that time (PAUSE_KEEP_BILLING decides what keeps billing).
func (c *Controller) UsePauses(p *pause.Store) { c.pauses = p }

// UseHealth reports the controller's own sources (tenant-api-gaps).
func (c *Controller) UseHealth(h *health.Registry) { c.health = h }

// UseKnownTenants adds tenants vBilling knows without a cluster: those its
// records configuration names (tenant maps, tenant-bound pulls) and those
// holding a billing state. No alias can capture them. Call it before Run.
func (c *Controller) UseKnownTenants(known func(id string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.known = known
}

// KnownTenant reports whether id is a tenant vBilling knows: discovered
// from a cluster, bound to an ingest token, or named by the records
// configuration or a billing state (UseKnownTenants). Such an ID always
// bills to itself: an alias that equals it is ignored.
func (c *Controller) KnownTenant(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := c.tenants.Get(id); ok || c.bound[id] {
		return true
	}
	c.mu.Lock()
	known := c.known
	c.mu.Unlock()
	return known != nil && known(id)
}

// SetPaused updates the known state of a cluster right after vBilling
// paused or resumed it, ahead of the next discovery.
func (c *Controller) SetPaused(id string, paused bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shared[id] {
		return // two clusters answer to this ID: discovery tells them apart
	}
	for i := range c.clusters {
		if c.clusters[i].ExternalID() == id {
			c.clusters[i].Paused = paused
		}
	}
}

type pauseForgetter interface{ ForgetPaused(id string) }

// observePauses records pauses vBilling did not make, and ends the pause
// of any cluster that runs again, whoever paused it.
//
// A cluster is paused only while its control plane is really down
// (discovery.TenantCluster.Paused); annotations never pause billing, and a
// paused-at annotation never dates a pause, since whoever writes
// vcluster.yaml can write both. vBilling's own pauses are recorded by its
// enforcer, from the moment it verified the cluster's workloads were gone,
// so a cluster it marked is left to it here. Any other pause (vcluster
// pause, an operator, vCluster Platform's sleep mode, the tenant scaling
// its control plane down) is recorded from when discovery sees it, as
// external: it stops the control plane's hours only, because vBilling
// cannot verify the cluster's nodes and workloads stopped.
func (c *Controller) observePauses(clusters []discovery.TenantCluster, shared map[string]bool, now time.Time) {
	if c.pauses == nil {
		return
	}
	forget, _ := c.coll.(pauseForgetter)
	for _, cl := range clusters {
		if cl.External {
			continue // no control plane here to see paused: vBilling's enforcer records its pauses
		}
		id := cl.ExternalID()
		if shared[id] {
			continue // two clusters answer to this ID: neither gets a pause span
		}
		_, open := c.pauses.Current(id)
		switch {
		case cl.Paused && !open && cl.Annotations[discovery.AnnotationPausedBy] != pause.ByVBilling:
			if _, err := c.pauses.Begin(id, now, pause.ByExternal); err != nil {
				log.Printf("[controller] %s: record pause: %v", id, err)
			} else {
				log.Printf("[controller] %s: paused outside vBilling since %s: its control plane stops billing, its nodes and workloads keep billing", id, now.UTC().Format(time.RFC3339))
			}
		case !cl.Paused && open:
			if _, err := c.pauses.End(id, now); err != nil {
				log.Printf("[controller] %s: record resume: %v", id, err)
			} else {
				log.Printf("[controller] %s: runs again, billed from %s", id, now.UTC().Format(time.RFC3339))
			}
		}
		if !cl.Paused && forget != nil {
			forget.ForgetPaused(id)
		}
	}
	// Pauses that ended before any window that can still be metered.
	_ = c.pauses.Prune(now.Add(-c.cfg.MaxBackfill - time.Hour))
}

// sharedIDs returns the external IDs more than one cluster maps to.
// vcluster-<namespace>-<name> is not unique (namespace a-b with cluster c
// and namespace a with cluster b-c are both vcluster-a-b-c), and the ID
// is the cluster's identity in the ledger, so it cannot change. Pause
// state, saved inventories and tenant API gaps are kept by it too: for a
// shared ID vBilling keeps no pause span, so neither cluster stops billing
// because of the other's pause.
func sharedIDs(clusters []discovery.TenantCluster) map[string]bool {
	seen := map[string]int{}
	for i := range clusters {
		seen[clusters[i].ExternalID()]++
	}
	out := map[string]bool{}
	for id, n := range seen {
		if n > 1 {
			out[id] = true
		}
	}
	return out
}

func (c *Controller) warnSharedIDs(shared map[string]bool) {
	ids := make([]string, 0, len(shared))
	for id := range shared {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	msg := strings.Join(ids, ", ")
	c.mu.Lock()
	changed := msg != c.sharedWarned
	c.sharedWarned = msg
	c.mu.Unlock()
	if changed && msg != "" {
		log.Printf("[controller] WARNING: more than one cluster has the ID %s (vcluster-<namespace>-<name>): they are metered under one ID, and pausing them never stops their billing. Rename one of them", msg)
	}
}

// UsePrometheus enables Prometheus-read usage.
func (c *Controller) UsePrometheus(p PromCollector) { c.prom = p }

// ResolveTenant maps a value an emitter sends (a tenant ID, or an alias
// from the vbilling.vcluster.com/aliases setting of a cluster) to a known
// tenant. A tenant vBilling knows (KnownTenant) always resolves to itself,
// whatever aliases claim. Unknown values are returned as they are, with
// known=false.
func (c *Controller) ResolveTenant(v string) (string, bool) {
	if c.KnownTenant(v) {
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
// alias that is a tenant vBilling knows bills to that tenant, so it is not
// one of this tenant's identities; neither is an alias two tenants claim.
func (c *Controller) GatewayIdentities(tenant string) []string {
	c.mu.Lock()
	var claimed []string
	for alias, t := range c.aliases {
		if t == tenant && !c.ambiguous[alias] {
			claimed = append(claimed, alias)
		}
	}
	c.mu.Unlock()
	out := []string{tenant}
	for _, alias := range claimed {
		if !c.KnownTenant(alias) {
			out = append(out, alias)
		}
	}
	sort.Strings(out[1:])
	return out
}

// warnAliasConflicts logs, once per change, aliases that are a tenant
// vBilling knows or that two tenants claim.
func (c *Controller) warnAliasConflicts(aliases map[string]string, ambiguous map[string]bool) {
	warn := map[string]string{}
	for a, t := range aliases {
		switch {
		case ambiguous[a]:
			warn[a] = "is claimed by more than one tenant; usage bills to " + t + " and the gateway never blocks it"
		case c.KnownTenant(a):
			warn[a] = "is tenant " + a + "'s own ID; usage sent as " + a + " bills to " + a + ", not to " + t
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
	bound := map[string]bool{}
	for _, t := range cfg.IngestTokens {
		if t != "" {
			bound[t] = true
		}
	}
	return &Controller{cfg: cfg, disc: disc, coll: coll, spool: sp, tenants: tenants, dests: dests, tel: tel,
		now: time.Now, lastSeen: map[string]time.Time{}, offboarded: map[string]map[string]bool{}, gaps: gaps, bound: bound}
}

// Run reconciles tenants every ReconcileInterval and closes a metering
// window at every CollectionInterval boundary until ctx ends.
func (c *Controller) Run(ctx context.Context) error {
	log.Printf("[controller] starting (adapters=%v, window=%s, reconcile=%s, region=%s)",
		c.cfg.Adapters, c.cfg.CollectionInterval, c.cfg.ReconcileInterval, c.cfg.Region)
	c.mu.Lock()
	c.liveSince = c.now()
	c.mu.Unlock()
	c.Reconcile(ctx)
	c.CollectDue(ctx)
	c.CollectPrometheusDue(ctx)
	c.checkSafeguard()

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
			c.checkSafeguard()
			collect.Reset(c.untilNextWindow())
		}
	}
}

// UseSafeguard checks, after every collection, that the windows committed
// since (by any source) bill no more than exists. Health only: it never
// holds delivery.
func (c *Controller) UseSafeguard(s *safeguard.Checker) { c.safeguard = s }

func (c *Controller) checkSafeguard() {
	if c.safeguard != nil {
		c.safeguard.Once()
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
	c.mu.Lock()
	retry := c.collectionRetry
	if !c.promRetry.IsZero() && (retry.IsZero() || c.promRetry.Before(retry)) {
		retry = c.promRetry
	}
	c.mu.Unlock()
	if !retry.IsZero() && retry.Before(next) {
		return max(time.Second, retry.Sub(now))
	}
	return next.Sub(now)
}

// live reports whether a window ending at end is metered as it happened:
// it ended after this controller started. One that ended before is metered
// after the fact and marked backfilled, the newest one too.
func (c *Controller) live(end time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !end.Before(c.liveSince)
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
	var excluded []discovery.TenantCluster
	if l, ok := c.disc.(excludedLister); ok {
		excluded = l.Excluded()
	}
	c.observeExclusions(clusters, excluded, now)
	c.observeDepartures(clusters, excluded, now)
	tenants := c.tenantsFrom(clusters)
	for _, t := range tenants {
		c.tenants.Upsert(t)
	}
	c.noteClusterless(tenants, excluded)

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
	shared := sharedIDs(clusters)
	c.warnSharedIDs(shared)
	c.observePauses(clusters, shared, now)
	var head uint64
	if c.spool != nil {
		head = c.spool.Committed()
	}
	using, readThrough := c.usageSince(head)
	c.mu.Lock()
	c.aliases = aliases
	c.ambiguous = ambiguous
	c.shared = shared
	c.clusters = clusters
	c.excluded = excluded
	c.discovered = true
	for _, t := range tenants {
		c.lastSeen[t.ID] = now
		c.off.noteSeen(t.ID, head)
		delete(c.offboarded, t.ID) // back before offboarding finished: start over next time
	}
	// A tenant whose usage still arrives (gateway records, ingest, Prometheus)
	// is not gone, whether or not it still has a cluster: it was last seen
	// when its latest usage was appended to the ledger (never later than
	// now), whatever window that usage is for.
	for id, at := range using {
		if at.After(now) {
			at = now
		}
		if seen, known := c.lastSeen[id]; known && at.After(seen) {
			c.lastSeen[id] = at
			delete(c.offboarded, id) // back before offboarding finished: start over next time
		}
	}
	var gone []usage.Tenant
	current := map[string]bool{}
	for _, t := range tenants {
		current[t.ID] = true
	}
	for id, seen := range c.lastSeen {
		// Until the ledger is read through, a tenant's latest usage may be
		// in the part not read yet: nobody is offboarded.
		if readThrough && !current[id] && now.Sub(seen) >= c.cfg.OffboardGrace {
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
			// Offboarding ends billing there (Lago terminates the subscription
			// and ignores its later events): only once the destination has the
			// tenant's last usage, delivery backlog and held usage included.
			if ready, why := c.delivered(t.ID, d.Name()); !ready {
				c.logWaiting(t.ID, d.Name(), why)
				ok = false
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
			c.off.forget(t.ID)
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
	start := latestEnd.Add(-size) // first run ever: the last closed window only
	note := ""
	if wm, ok := c.spool.Watermark(spool.SourceCollector); ok {
		if !latestEnd.After(wm) {
			return
		}
		start = wm
		// The oldest window we backfill starts MAX_BACKFILL before the latest end.
		if earliest := latestEnd.Add(-c.cfg.MaxBackfill).Truncate(size); start.Before(earliest) {
			gap := earliest.Sub(start)
			note = "gap: " + gap.String() + " older than MAX_BACKFILL not metered"
			log.Printf("[controller] %d window(s) before %s are older than MAX_BACKFILL=%s and will not be metered",
				int(gap/size), earliest.Format(time.RFC3339), c.cfg.MaxBackfill)
			start = earliest
		}
	}
	windows := windowsBetween(start, latestEnd, size, latestEnd)
	if n := len(windows); n > 0 && !c.live(latestEnd) {
		// The newest window closed before this controller started: nothing
		// watched it, so it is metered like the windows before it.
		windows[n-1].Latest = false
	}

	targets := c.targets()

	started := time.Now()
	byWindow, st, failed, err := c.collectAttributed(ctx, targets, windows, c.coll.Collect)
	if err != nil {
		var pending *metrics.HeartbeatPending
		if errors.As(err, &pending) {
			c.mu.Lock()
			c.collectionRetry = c.now().Add(5 * time.Second)
			c.mu.Unlock()
			log.Printf("[controller] usage window held: %v", pending)
			c.tel.Set("vbilling_collection_held", "1 while a window waits for stale node heartbeat evidence.", nil, 1)
			return
		}
		log.Printf("[controller] metering failed for %d window(s), will retry: %v", len(windows), err)
		c.tel.Add("vbilling_collection_failures_total", "Failed metering runs (windows are retried).", nil, 1)
		return
	}
	c.mu.Lock()
	c.collectionRetry = time.Time{}
	c.mu.Unlock()
	c.tel.Set("vbilling_collection_held", "1 while a window waits for stale node heartbeat evidence.", nil, 0)
	// The windows a cluster's own API missed are recorded before any is
	// committed: a crash in between must not leave a window committed
	// without its cluster's API read and nothing to fill it.
	if err := c.recordGaps(failed, windows); err != nil {
		log.Printf("[controller] record the windows clusters' own APIs missed: %v; %d window(s) are not committed and are read again", err, len(windows))
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
	c.fillTenantGaps(ctx, failed, windows, targets)
	c.forgetDeparted()
	backfilled := 0
	for _, w := range windows {
		if !w.Latest {
			backfilled++
		}
	}
	if backfilled > 0 {
		log.Printf("[controller] backfilled %d window(s) after a gap", backfilled)
		c.tel.Add("vbilling_backfilled_windows_total", "Windows metered after a gap (restart or outage).", nil, float64(backfilled))
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

// recordGaps records, before a run commits its windows, the windows each
// cluster's own API missed in it (failed), so a window committed without
// its cluster's API read is always that cluster's gap, even if vBilling
// stops right after the commit. A gap recorded for windows whose commit
// never happened (vBilling stopped, or a commit failed) is dropped here by
// the next run, which reads those windows again: a gap never holds a window
// that was committed with its cluster's API read. Gaps older than
// MAX_BACKFILL are dropped. An error means a gap could not be saved: the
// run must not commit (its windows are read again next time).
func (c *Controller) recordGaps(failed map[string][]metrics.Window, windows []metrics.Window) error {
	size := c.cfg.CollectionInterval
	wm, _ := c.spool.Watermark(spool.SourceCollector)
	earliest := windows[len(windows)-1].End.Add(-c.cfg.MaxBackfill)
	ids := map[string]bool{}
	for id := range c.gaps.WithPrefix("") {
		ids[id] = true
	}
	for id := range failed {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var errs []error
	for _, id := range sorted {
		g, known := parseGaps(c.gaps, id, size)
		next := g.through(wm)
		if ws := failed[id]; len(ws) > 0 {
			if len(next) == 0 {
				log.Printf("[controller] %s: tenant API unreachable from window %s, will fill once it answers", id, ws[0].End.Format(time.RFC3339))
			}
			next = next.add(ws)
		}
		next = next.after(earliest)
		var err error
		switch {
		case len(next) == 0 && known:
			err = c.gaps.Delete(id)
		case len(next) > 0 && (!known || next.String() != g.String()):
			err = c.gaps.Set(id, next.String())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	err := errors.Join(errs...)
	c.reportGaps(err)
	return err
}

// reportGaps reports whether tenant API gaps can be saved (reported when
// they cannot, and once when they can again).
func (c *Controller) reportGaps(err error) {
	c.mu.Lock()
	was := c.gapsFailing
	c.gapsFailing = err != nil
	c.mu.Unlock()
	if c.health == nil {
		return
	}
	switch {
	case err != nil:
		c.health.Fail("tenant-api-gaps", "tenant-api", "", fmt.Errorf("the windows clusters' own APIs missed cannot be saved, so no window is committed until they can: %w", err),
			"check DATA_DIR (tenant-api-gaps.json): free space and permissions")
	case was:
		c.health.OK("tenant-api-gaps", "tenant-api", "", 0)
	}
}

// fillTenantGaps meters, once a cluster's own API answers again, exactly
// the windows it missed (recordGaps). Those events go to the ledger as late
// events, deduplicated by ID. A gap holds the failed windows themselves
// (tenantGaps), never one committed with the cluster's API side in
// between: fill IDs are salted, so a committed window filled again would
// bill twice. A fill repeated after a crash gets the same IDs and is
// dropped as a duplicate. Windows older than MAX_BACKFILL are not filled.
//
// A gap an earlier version recorded ("first|last") is one span from the
// first failed window to the last, which may hold windows committed with
// the cluster's API read, or filled already: its windows are filled with
// the unsalted IDs those versions gave, so what a billing platform or the
// ledger already has is dropped as a duplicate, and an event an earlier
// fill already gave a salted ID is left out.
func (c *Controller) fillTenantGaps(ctx context.Context, failed map[string][]metrics.Window, windows []metrics.Window, targets []metrics.Target) {
	filler, ok := c.coll.(tenantFiller)
	if !ok {
		return
	}
	size := c.cfg.CollectionInterval
	earliest := windows[len(windows)-1].End.Add(-c.cfg.MaxBackfill)
	for _, t := range targets {
		id := t.Cluster.ExternalID()
		g, ok := parseGaps(c.gaps, id, size)
		if !ok || len(failed[id]) > 0 {
			continue
		}
		missed := g.windows(size, earliest)
		if len(missed) == 0 {
			c.dropGap(id)
			continue
		}
		byWindow, fst, _, err := c.collectAttributed(ctx, []metrics.Target{t}, missed, filler.CollectTenantAPI)
		if err != nil || len(fst.TenantAPIFailed) > 0 {
			continue // still unreachable: try again next window
		}
		legacy := g.legacyEnds(size)
		var evs []usage.Event
		for _, w := range missed {
			for _, ev := range byWindow[w.End] {
				salted := usage.DeriveIDWith(&ev, "fill")
				if legacy[w.End] {
					if c.spool.IngestedRecently(salted) {
						continue // filled already, by a version that salted fills
					}
					salted = ev.ID
				}
				ev.ID = salted
				if ev.Properties == nil {
					ev.Properties = map[string]any{}
				}
				ev.Properties["filled"] = true
				evs = append(evs, ev)
			}
		}
		accepted, dups, err := c.spool.AppendIngest(SourceTenantFill, evs)
		if err != nil {
			log.Printf("[controller] %s: fill %d window(s): %v", id, len(missed), err)
			continue
		}
		c.dropGap(id)
		log.Printf("[controller] %s: filled %d window(s) missed during a tenant API outage (%d events, %d already in the ledger)",
			id, len(missed), len(accepted), len(dups))
		c.tel.Add("vbilling_tenant_api_filled_windows_total", "Windows filled after a tenant API outage.", nil, float64(len(missed)))
	}
}

// dropGap forgets a filled gap. If that cannot be saved, the next fill
// gives the same IDs and AppendIngest drops them as duplicates.
func (c *Controller) dropGap(id string) {
	err := c.gaps.Delete(id)
	if err != nil {
		log.Printf("[controller] %s: forget a filled tenant API gap: %v (filled again next time; the ledger drops the duplicates)", id, err)
		c.reportGaps(err)
		return
	}
	c.mu.Lock()
	failing := c.gapsFailing
	c.mu.Unlock()
	if failing {
		c.reportGaps(nil)
	}
}

// gapSpan is a run of consecutive windows a cluster's own API missed, from
// the first one's start to the last one's end. legacy marks a span an
// earlier version recorded (see fillTenantGaps).
type gapSpan struct {
	start, end time.Time
	legacy     bool
}

// tenantGaps are the windows a cluster's own API missed, as runs in time
// order. Stored as "start/end,start/end" (RFC 3339), a span from an earlier
// version as "start/end/legacy".
type tenantGaps []gapSpan

func (g tenantGaps) String() string {
	parts := make([]string, len(g))
	for i, s := range g {
		parts[i] = s.start.Format(time.RFC3339) + "/" + s.end.Format(time.RFC3339)
		if s.legacy {
			parts[i] += "/legacy"
		}
	}
	return strings.Join(parts, ",")
}

// add returns the gaps with windows added, runs that touch merged (a span
// from an earlier version stays apart).
func (g tenantGaps) add(ws []metrics.Window) tenantGaps {
	all := append(tenantGaps(nil), g...)
	for _, w := range ws {
		all = append(all, gapSpan{start: w.Start.UTC(), end: w.End.UTC()})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	var out tenantGaps
	for _, s := range all {
		if n := len(out); n > 0 && out[n-1].legacy == s.legacy && !s.start.After(out[n-1].end) {
			if s.end.After(out[n-1].end) {
				out[n-1].end = s.end
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// after drops the runs that end at or before t.
func (g tenantGaps) after(t time.Time) tenantGaps {
	var out tenantGaps
	for _, s := range g {
		if s.end.After(t) {
			out = append(out, s)
		}
	}
	return out
}

// through keeps the windows that end at or before t: the committed ones.
func (g tenantGaps) through(t time.Time) tenantGaps {
	var out tenantGaps
	for _, s := range g {
		if !s.start.Before(t) {
			continue
		}
		if s.end.After(t) {
			s.end = t
		}
		out = append(out, s)
	}
	return out
}

// windows returns the missed windows that end after earliest.
func (g tenantGaps) windows(size time.Duration, earliest time.Time) []metrics.Window {
	var out []metrics.Window
	for _, s := range g {
		for _, w := range windowsBetween(s.start, s.end, size, time.Time{}) {
			if w.End.After(earliest) {
				out = append(out, w)
			}
		}
	}
	return out
}

// legacyEnds are the ends of the windows of spans from earlier versions.
func (g tenantGaps) legacyEnds(size time.Duration) map[time.Time]bool {
	out := map[time.Time]bool{}
	for _, s := range g {
		if s.legacy {
			for _, w := range windowsBetween(s.start, s.end, size, time.Time{}) {
				out[w.End] = true
			}
		}
	}
	return out
}

// parseGaps reads a cluster's gaps. Earlier versions stored one span from
// the first to the last failed window, "first|last|start", or "first|last"
// (v0.3.0, whose first window was a whole window of the current size): it
// is read as that span, marked legacy.
func parseGaps(store *kv.Store, id string, size time.Duration) (tenantGaps, bool) {
	v, ok := store.Get(id)
	if !ok {
		return nil, false
	}
	var g tenantGaps
	if strings.Contains(v, "|") {
		parts := strings.Split(v, "|")
		var times []time.Time
		for _, p := range parts {
			t, err := time.Parse(time.RFC3339, p)
			if err != nil {
				break
			}
			times = append(times, t)
		}
		if len(times) == len(parts) && len(times) >= 2 && len(times) <= 3 && !times[1].Before(times[0]) {
			s := gapSpan{start: times[0].Add(-size), end: times[1], legacy: true}
			if len(times) == 3 && times[2].Before(times[0]) {
				s.start = times[2]
			}
			g = tenantGaps{s}
		}
	} else {
		for _, part := range strings.Split(v, ",") {
			fields := strings.Split(part, "/")
			if len(fields) < 2 || len(fields) > 3 || len(fields) == 3 && fields[2] != "legacy" {
				g = nil
				break
			}
			s, errS := time.Parse(time.RFC3339, fields[0])
			e, errE := time.Parse(time.RFC3339, fields[1])
			if errS != nil || errE != nil || !e.After(s) {
				g = nil
				break
			}
			g = append(g, gapSpan{start: s, end: e, legacy: len(fields) == 3})
		}
	}
	if len(g) == 0 {
		_ = store.Delete(id)
		return nil, false
	}
	return g, true
}

// windowsBetween splits start to end into windows on the grid of size, the
// last one marked Latest when it ends at latest. A start off the grid, the
// watermark of an install that used another window size, first gets one
// partial window up to the grid, so every later window lines up again.
func windowsBetween(start, end time.Time, size time.Duration, latest time.Time) []metrics.Window {
	var out []metrics.Window
	for s := start; s.Before(end); {
		e := s.Truncate(size).Add(size)
		if e.After(end) {
			e = end
		}
		out = append(out, metrics.Window{Start: s, End: e, Latest: e.Equal(latest)})
		s = e
	}
	return out
}

// targets resolves every known cluster to its billing identity.
func (c *Controller) targets() []metrics.Target {
	src := discovery.TenantSource(c.cfg.TenantSource)
	c.mu.Lock()
	shared := c.shared
	c.mu.Unlock()
	exclusions := c.exclusionStore()
	var targets []metrics.Target
	for _, cl := range c.Clusters() {
		t, _ := c.tenants.Get(cl.TenantID(src))
		target := metrics.Target{Cluster: cl, Tenant: t.ID, Project: cl.Project, TenantClass: t.Class}
		if c.pauses != nil && !shared[cl.ExternalID()] {
			// A shared ID's spans could be either cluster's: neither is clipped.
			target.Paused = c.pauses.Spans(cl.ExternalID())
		}
		if !shared[cl.ExternalID()] {
			// It was excluded from metering earlier: not billed for that time.
			target.Excluded = exclusions.Spans(cl.ExternalID())
		}
		targets = append(targets, target)
	}
	// Clusters excluded or deleted since: their last window is still closed.
	return append(targets, c.lifecycleTargets()...)
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
	start := latestEnd.Add(-size)
	if wm, ok := c.spool.Watermark(metrics.SourcePrometheus); ok {
		if !latestEnd.After(wm) {
			return
		}
		start = wm
		if earliest := latestEnd.Add(-c.cfg.MaxBackfill).Truncate(size); start.Before(earliest) {
			log.Printf("[controller] Prometheus windows before %s are older than MAX_BACKFILL=%s; counters bill that usage in the first window read",
				earliest.Format(time.RFC3339), c.cfg.MaxBackfill)
			start = earliest
		}
	}
	windows := windowsBetween(start, latestEnd, size, latestEnd)
	targets := c.targets()
	now := c.now().UTC()
	// The newest window is read once its samples have reached Prometheus
	// (see metrics.PromMaxWait): until then it waits, and is looked at again
	// in a few seconds. Older windows are past that wait and are read now.
	c.mu.Lock()
	c.promRetry = time.Time{}
	c.mu.Unlock()
	if s, ok := c.prom.(promSettler); ok && now.Before(latestEnd.Add(metrics.PromMaxWait)) {
		// Only the clusters this window is read for are waited for.
		read, _ := meteredIn(targets, windows[len(windows)-1])
		if settled, waiting := s.Settled(ctx, read, latestEnd); !settled {
			retry := now.Add(promPoll)
			if cap := latestEnd.Add(metrics.PromMaxWait); retry.After(cap) {
				retry = cap
			}
			c.mu.Lock()
			c.promRetry = retry
			// Waiting a few seconds is the normal case: say it now and then,
			// not for every window.
			if now.Sub(c.promWaiting) >= 10*time.Minute {
				c.promWaiting = now
				log.Printf("[controller] Prometheus window %s waits for its samples (%s is still behind), at most %s; windows wait like this whenever samples are still on their way",
					latestEnd.Format("15:04:05"), waiting, metrics.PromMaxWait)
			}
			c.mu.Unlock()
			windows = windows[:len(windows)-1]
			if len(windows) == 0 {
				return
			}
		}
	}
	// The newest window read is the live one when it is read on time (its
	// wait, and one more look); any other (read after an outage, or closed
	// before this controller started) is marked backfilled.
	for i := range windows {
		w := &windows[i]
		w.Latest = i == len(windows)-1 && c.live(w.End) && now.Before(w.End.Add(metrics.PromMaxWait+promPoll))
	}
	// Windows read after the fact bill each cluster's owner during them,
	// as Kubernetes backfill does. Each run of windows with the same owners
	// is read and committed before the next one is read: counters bill
	// from the baselines the previous run committed.
	total, late := 0, 0
	for _, r := range c.ownerRuns(targets, windows) {
		results, err := c.prom.Collect(ctx, r.targets, r.windows)
		if err != nil {
			log.Printf("[controller] Prometheus: %v; %d window(s) wait until it answers", err, len(r.windows))
			c.tel.Add("vbilling_prometheus_unavailable_total", "Runs where every Prometheus read failed (windows wait).", nil, 1)
			return
		}
		props := platformProps(r.targets)
		for _, res := range results {
			late += res.Late
		}
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
	c.tel.Add("vbilling_prometheus_late_samples_total",
		"Counter samples that reached Prometheus too late for their own window and were billed in a later one.", nil, float64(late))
	c.tel.Set("vbilling_prometheus_last_window_end_timestamp_seconds", "End of the last committed Prometheus window.", nil,
		float64(windows[len(windows)-1].End.Unix()))
	c.forgetDeparted()
}

// promSettler is implemented by a PromCollector that can tell whether a
// window's samples have reached Prometheus.
type promSettler interface {
	Settled(ctx context.Context, targets []metrics.Target, end time.Time) (settled bool, waiting string)
}

// promPoll is how often a waiting Prometheus window is looked at again.
const promPoll = 5 * time.Second

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
