package controller

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Owners knows which owner a cluster had in the past (TENANT_SOURCE=platform
// keeps a history of moves between projects and tenants).
type Owners interface {
	// OwnerAt returns cl as it was bound during the window ending at end;
	// ok is false when its current owner applies.
	OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool)
}

// ownerChanges is implemented by Owners that date changes inside a window.
type ownerChanges interface {
	// ChangesIn returns the moments in (start, end) at which cl's owner
	// changed, in order.
	ChangesIn(cl discovery.TenantCluster, start, end time.Time) []time.Time
}

// UseOwners bills each window to the owner each cluster had during it,
// rather than its owner now, and splits a window at the moment its owner
// changed: windows metered as they close, backfilled after a restart, and
// filled after a tenant API outage alike.
func (c *Controller) UseOwners(o Owners) { c.owners = o }

// historyOf returns the owner history that decides cl's past owners: the
// vCluster Platform's for a cluster bound to its objects, else the one
// discovery keeps (ownerLog), or nil.
func (c *Controller) historyOf(cl *discovery.TenantCluster) Owners {
	if cl.Platform != nil && c.owners != nil {
		return c.owners
	}
	if c.ownerLog != nil && cl.Platform == nil {
		return c.ownerLog
	}
	return nil
}

type collectFunc func(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error)

// collectAttributed meters windows with collect, split into runs wherever a
// cluster's owner changed, and stamps vCluster Platform UIDs on the events.
// Without owner history, or without a move in the range, it is one call.
// failed maps each cluster whose own API could not be read to the windows
// of the runs that missed it: only those, not the whole range.
func (c *Controller) collectAttributed(ctx context.Context, targets []metrics.Target, windows []metrics.Window, collect collectFunc) (
	out map[time.Time][]usage.Event, st metrics.Stats, failed map[string][]metrics.Window, err error) {
	runs := c.ownerRuns(targets, windows)
	out = map[time.Time][]usage.Event{}
	failed = map[string][]metrics.Window{}
	for _, r := range runs {
		byWindow, rst, err := collect(ctx, r.targets, r.windows)
		if err != nil {
			return nil, rst, nil, err
		}
		props := platformProps(r.targets)
		for end, evs := range byWindow {
			stampPlatform(evs, props)
			out[end] = evs
		}
		for _, id := range rst.TenantAPIFailed {
			failed[id] = append(failed[id], r.windows...)
		}
		st = rst // counters of the latest run, for logs
	}
	if len(runs) > 1 {
		st.TenantAPIFailed = st.TenantAPIFailed[:0]
		for id := range failed {
			st.TenantAPIFailed = append(st.TenantAPIFailed, id)
		}
		sort.Strings(st.TenantAPIFailed)
	}
	return out, st, failed, nil
}

// ownerRun is a run of consecutive windows during which every cluster had
// the same owner, with the targets as they were billed then.
type ownerRun struct {
	targets []metrics.Target
	windows []metrics.Window
	sig     string
}

// ownerRuns splits windows into runs wherever a cluster's owner changed.
// Without owner history, or without a move in the range, it is one run. A
// window a move falls in is a run of its own, with one target per owner of
// the moved cluster. A window of another length (the partial window that
// realigns an old watermark with the grid) is a run of its own too:
// Prometheus queries take the window length from a run's first window.
func (c *Controller) ownerRuns(targets []metrics.Target, windows []metrics.Window) []ownerRun {
	var runs []ownerRun
	for _, w := range windows {
		ts, sig := c.targetsFor(targets, w)
		// A cluster excluded from metering or deleted for the whole window
		// is not read at all.
		ts, skipped := meteredIn(ts, w)
		sig += "\x02" + w.End.Sub(w.Start).String() + "\x02" + skipped
		if n := len(runs); n > 0 && runs[n-1].sig == sig {
			runs[n-1].windows = append(runs[n-1].windows, w)
			continue
		}
		runs = append(runs, ownerRun{targets: ts, windows: []metrics.Window{w}, sig: sig})
	}
	return runs
}

// targetsFor resolves targets as they were billed during the window w. A
// cluster whose owner changed inside w is split at each change: it becomes
// one target per owner, each with the times the others had it as
// Elsewhere, so each owner is billed for its own part of the window and
// the parts add up to the whole (see metrics/owners.go). sig identifies
// the attribution, so windows with the same owners share one collection
// run.
func (c *Controller) targetsFor(targets []metrics.Target, w metrics.Window) ([]metrics.Target, string) {
	if c.owners == nil && c.ownerLog == nil {
		return targets, ""
	}
	out := make([]metrics.Target, 0, len(targets))
	var sig strings.Builder
	for _, t := range targets {
		for _, o := range c.ownersIn(t, w) {
			out = append(out, o)
			sig.WriteString(o.Tenant + "\x00" + o.Project + "\x00" + o.TenantClass)
			for _, sp := range o.Elsewhere {
				sig.WriteString("\x00" + sp.From.Format(time.RFC3339Nano) + "/" + sp.To.Format(time.RFC3339Nano))
			}
			sig.WriteString("\x01")
		}
	}
	return out, sig.String()
}

// ownersIn returns t as billed during w: one target, or one per owner when
// its owner changed inside w.
func (c *Controller) ownersIn(t metrics.Target, w metrics.Window) []metrics.Target {
	h := c.historyOf(&t.Cluster)
	if h == nil {
		return []metrics.Target{t}
	}
	var cuts []time.Time
	if ch, ok := h.(ownerChanges); ok {
		cuts = ch.ChangesIn(t.Cluster, w.Start, w.End)
	}
	if len(cuts) == 0 {
		return []metrics.Target{c.billedAs(t, h, w.End)}
	}
	// The window's parts, each billed to the owner at its last instant.
	type part struct {
		from, to time.Time
		target   metrics.Target
	}
	var parts []part
	from := w.Start
	for _, cut := range append(append([]time.Time(nil), cuts...), w.End) {
		if !cut.After(from) || cut.After(w.End) {
			continue
		}
		parts = append(parts, part{from: from, to: cut, target: c.billedAs(t, h, cut)})
		from = cut
	}
	// One target per billing identity: a move and its reversal inside one
	// window make three parts, the first and the last billed as one.
	key := func(t metrics.Target) string { return t.Tenant + "\x00" + t.Project + "\x00" + t.TenantClass }
	var out []metrics.Target
	seen := map[string]bool{}
	for _, p := range parts {
		if k := key(p.target); !seen[k] {
			seen[k] = true
			out = append(out, p.target)
		}
	}
	if len(out) == 1 {
		return out // moved and moved back to the same billing identity
	}
	// Each owner's Elsewhere are the other owners' parts. The first part
	// reaches back and the last one forward, so a counter sample read in
	// this window but taken before it (it reached Prometheus late) goes to
	// the owner at the window's start.
	for i := range out {
		mine := key(out[i])
		var elsewhere []pause.Span
		for j, p := range parts {
			if key(p.target) == mine {
				continue
			}
			sp := pause.Span{From: p.from, To: p.to}
			if j == 0 {
				sp.From = time.Time{}
			}
			if j == len(parts)-1 {
				sp.To = time.Time{}
			}
			elsewhere = pause.Union(elsewhere, []pause.Span{sp})
		}
		out[i].Elsewhere = elsewhere
	}
	return out
}

// billedAs returns t as billed during the window, or part of a window,
// that ends at end.
func (c *Controller) billedAs(t metrics.Target, h Owners, end time.Time) metrics.Target {
	cl, ok := h.OwnerAt(t.Cluster, end)
	if !ok {
		return t
	}
	src := discovery.TenantSource(c.cfg.TenantSource)
	tenant, _ := c.tenants.Get(cl.TenantID(src))
	class := tenant.Class
	for _, v := range []string{cl.Meta("tenant-class"), c.cfg.TenantClass} {
		if class == "" {
			class = v
		}
	}
	// The owner changes who pays, never what ran: keep the cluster's pause
	// state and spans.
	cl.Paused = t.Cluster.Paused
	return metrics.Target{Cluster: cl, Tenant: tenant.ID, Project: cl.Project, TenantClass: class, Paused: t.Paused, Excluded: t.Excluded}
}

// propsKey identifies the events of a cluster split between owners: one
// target, and one set of Platform properties, per owner.
func propsKey(cluster, tenant, project, class string) string {
	return cluster + "\x00" + tenant + "\x00" + project + "\x00" + class
}

// platformProps maps clusters (by external ID) to the Platform properties
// their events carry, and the targets of a cluster split between owners
// by propsKey.
func platformProps(targets []metrics.Target) map[string]map[string]any {
	n := map[string]int{}
	for i := range targets {
		n[targets[i].Cluster.ExternalID()]++
	}
	var out map[string]map[string]any
	for i := range targets {
		t := &targets[i]
		if p := t.Cluster.PlatformProperties(); p != nil {
			if out == nil {
				out = map[string]map[string]any{}
			}
			key := t.Cluster.ExternalID()
			if n[key] > 1 {
				key = propsKey(key, t.Tenant, t.Project, t.TenantClass)
			}
			out[key] = p
		}
	}
	return out
}

// stampPlatform adds Platform UIDs to events. Properties are not part of an
// event's identity, so this never changes an ID.
func stampPlatform(evs []usage.Event, props map[string]map[string]any) {
	if len(props) == 0 {
		return
	}
	for i := range evs {
		ev := &evs[i]
		p := props[ev.Dim(usage.DimTenantCluster)]
		if p == nil {
			p = props[propsKey(ev.Dim(usage.DimTenantCluster), ev.Tenant, ev.Project, ev.Dim(usage.DimTenantClass))]
		}
		if p == nil {
			continue
		}
		if evs[i].Properties == nil {
			evs[i].Properties = map[string]any{}
		}
		for k, v := range p {
			if _, set := evs[i].Properties[k]; !set {
				evs[i].Properties[k] = v
			}
		}
	}
}
