package controller

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// ownerLog dates owner changes of clusters billed by their own settings:
// a vbilling.vcluster.com/tenant label, their project (TENANT_SOURCE=project),
// or neither (one customer per cluster). Discovery lists clusters every
// reconcile (30 seconds) and nothing watches those settings, so a change is
// dated at the discovery that first sees it, and the window it falls in is
// split there (targetsFor). Clusters bound to vCluster Platform objects keep
// their history in the Platform resolver instead.
//
// The history is persisted (DATA_DIR/owner-history.json): after a restart,
// a change made while vBilling was down is dated at the first discovery
// after the start, and backfilled windows before it stay with the old
// owner.
type ownerLog struct {
	src discovery.TenantSource

	mu      sync.Mutex
	store   *kv.Store
	entries map[string]*ownerEntry // by cluster external ID
}

// ownerEntry is one cluster's owner and its earlier owners.
type ownerEntry struct {
	Owner    ownerRef     `json:"owner"`
	Since    time.Time    `json:"since"`
	LastSeen time.Time    `json:"lastSeen"`
	History  []ownerEpoch `json:"history,omitempty"`
}

// ownerRef is who a cluster is billed to: the tenant and the project its
// events carry.
type ownerRef struct {
	Tenant  string `json:"tenant"`
	Project string `json:"project,omitempty"`
}

// ownerEpoch is an earlier owner, for windows metered after the fact.
type ownerEpoch struct {
	Owner ownerRef  `json:"owner"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// maxOwnerHistory bounds the earlier owners kept per cluster.
const maxOwnerHistory = 8

func newOwnerLog(store *kv.Store, src discovery.TenantSource) *ownerLog {
	l := &ownerLog{src: src, store: store, entries: map[string]*ownerEntry{}}
	for id, v := range store.WithPrefix("") {
		var e ownerEntry
		if json.Unmarshal([]byte(v), &e) == nil && e.Owner.Tenant != "" {
			l.entries[id] = &e
		}
	}
	return l
}

func (l *ownerLog) refOf(cl *discovery.TenantCluster) ownerRef {
	return ownerRef{Tenant: cl.TenantID(l.src), Project: cl.Project}
}

// observe records the owners discovery saw at now. A cluster whose owner
// changed moved at now, never before committed (windows already in the
// ledger stay with the owner they were billed to). Clusters bound to
// vCluster Platform objects and IDs that more than one cluster maps to are
// not kept here. Entries not seen for longer than keep are dropped.
func (l *ownerLog) observe(clusters []discovery.TenantCluster, shared map[string]bool, now, committed time.Time, keep time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now = now.UTC()
	for i := range clusters {
		cl := &clusters[i]
		id := cl.ExternalID()
		if cl.Platform != nil || shared[id] {
			if l.entries[id] != nil {
				l.forget(id)
			}
			continue
		}
		ref := l.refOf(cl)
		e := l.entries[id]
		switch {
		case e == nil:
			e = &ownerEntry{Owner: ref, Since: now, LastSeen: now.Truncate(time.Hour)}
			l.entries[id] = e
		case e.Owner != ref:
			at := now.Truncate(time.Second) // as vCluster Platform moves are dated
			if committed.After(at) {
				at = committed // windows up to it are in the ledger under the old owner
			}
			if at.Before(e.Since) {
				at = e.Since
			}
			log.Printf("[controller] %s moved from tenant %s (project %q) to tenant %s (project %q) at %s, seen by discovery: usage up to then is billed to %s, from then on to %s",
				id, e.Owner.Tenant, e.Owner.Project, ref.Tenant, ref.Project, at.Format(time.RFC3339), e.Owner.Tenant, ref.Tenant)
			e.History = append(e.History, ownerEpoch{Owner: e.Owner, Since: e.Since, Until: at})
			if len(e.History) > maxOwnerHistory {
				e.History = e.History[len(e.History)-maxOwnerHistory:]
			}
			e.Owner, e.Since = ref, at
		case now.Truncate(time.Hour).After(e.LastSeen):
			e.LastSeen = now.Truncate(time.Hour)
		default:
			continue // nothing to save
		}
		l.persist(id, e)
	}
	for id, e := range l.entries {
		if now.Sub(e.LastSeen) > keep {
			l.forget(id)
		}
	}
}

func (l *ownerLog) persist(id string, e *ownerEntry) {
	b, err := json.Marshal(e)
	if err == nil {
		err = l.store.Set(id, string(b))
	}
	if err != nil {
		log.Printf("[controller] %s: save its owner history: %v", id, err)
	}
}

func (l *ownerLog) forget(id string) {
	delete(l.entries, id)
	_ = l.store.Delete(id)
}

// ownerAt returns who cl was billed to at the instant t: its current owner
// from Since on, before that the earlier owner whose epoch holds t (the
// oldest one known before any of them).
func (e *ownerEntry) ownerAt(t time.Time) ownerRef {
	if len(e.History) == 0 || !t.Before(e.Since) {
		return e.Owner
	}
	ref := e.History[0].Owner
	for _, h := range e.History {
		if !t.Before(h.Since) && t.Before(h.Until) {
			ref = h.Owner
		}
	}
	return ref
}

// OwnerAt returns cl as it was billed during the window ending at end;
// ok is false when cl's own settings apply.
func (l *ownerLog) OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool) {
	if cl.Platform != nil {
		return cl, false
	}
	l.mu.Lock()
	e := l.entries[cl.ExternalID()]
	var ref ownerRef
	if e != nil {
		ref = e.ownerAt(end.Add(-time.Nanosecond)) // the window's last instant
	}
	l.mu.Unlock()
	if e == nil || ref == l.refOf(&cl) {
		return cl, false
	}
	out := cl
	out.Annotations = copyMap(cl.Annotations)
	out.Annotations[discovery.AnnotationPrefix+"tenant"] = ref.Tenant
	out.Project = ref.Project
	return out, true
}

// ChangesIn returns the moments in (start, end) at which cl's owner
// changed, in order.
func (l *ownerLog) ChangesIn(cl discovery.TenantCluster, start, end time.Time) []time.Time {
	if cl.Platform != nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[cl.ExternalID()]
	if e == nil {
		return nil
	}
	var out []time.Time
	for _, h := range e.History {
		if h.Until.After(start) && h.Until.Before(end) {
			out = append(out, h.Until)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// observeOwners records the owners of the clusters discovery found, metered
// or excluded, at now (ownerLog.observe).
func (c *Controller) observeOwners(metered, excluded []discovery.TenantCluster, shared map[string]bool, now time.Time) {
	if c.ownerLog == nil {
		return
	}
	var committed time.Time
	if c.spool != nil {
		committed, _ = c.spool.Watermark(spool.SourceCollector)
	}
	all := append(append(make([]discovery.TenantCluster, 0, len(metered)+len(excluded)), metered...), excluded...)
	// Kept while a window could still be metered for the cluster: backfill
	// reaches MAX_BACKFILL back, and a deleted cluster's last window is
	// metered within that.
	c.ownerLog.observe(all, shared, now, committed, c.cfg.MaxBackfill+2*time.Hour)
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
