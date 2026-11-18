package controller

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Owners knows which owner a cluster had in the past (TENANT_SOURCE=platform
// keeps a history of moves between projects and tenants).
type Owners interface {
	// OwnerAt returns cl as it was bound during the window ending at end;
	// ok is false when its current owner applies.
	OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool)
}

// UseOwners bills windows metered after the fact (backfill after a restart,
// fills after a tenant API outage) to the owner each cluster had during
// them, rather than its owner now.
func (c *Controller) UseOwners(o Owners) { c.owners = o }

type collectFunc func(ctx context.Context, targets []metrics.Target, windows []metrics.Window) (map[time.Time][]usage.Event, metrics.Stats, error)

// collectAttributed meters windows with collect, split into runs wherever a
// cluster's owner changed, and stamps vCluster Platform UIDs on the events.
// Without owner history, or without a move in the range, it is one call.
func (c *Controller) collectAttributed(ctx context.Context, targets []metrics.Target, windows []metrics.Window, collect collectFunc) (map[time.Time][]usage.Event, metrics.Stats, error) {
	type run struct {
		targets []metrics.Target
		windows []metrics.Window
		sig     string
	}
	var runs []run
	for _, w := range windows {
		ts, sig := c.targetsFor(targets, w.End)
		if n := len(runs); n > 0 && runs[n-1].sig == sig {
			runs[n-1].windows = append(runs[n-1].windows, w)
			continue
		}
		runs = append(runs, run{targets: ts, windows: []metrics.Window{w}, sig: sig})
	}
	out := map[time.Time][]usage.Event{}
	var st metrics.Stats
	failed := map[string]bool{}
	for _, r := range runs {
		byWindow, rst, err := collect(ctx, r.targets, r.windows)
		if err != nil {
			return nil, rst, err
		}
		props := platformProps(r.targets)
		for end, evs := range byWindow {
			stampPlatform(evs, props)
			out[end] = evs
		}
		for _, id := range rst.TenantAPIFailed {
			failed[id] = true
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
	return out, st, nil
}

// targetsFor resolves targets as they were billed during the window ending
// at end. sig identifies the attribution, so windows with the same owners
// share one collection run.
func (c *Controller) targetsFor(targets []metrics.Target, end time.Time) ([]metrics.Target, string) {
	if c.owners == nil {
		return targets, ""
	}
	src := discovery.TenantSource(c.cfg.TenantSource)
	out := make([]metrics.Target, len(targets))
	var sig strings.Builder
	for i, t := range targets {
		if cl, ok := c.owners.OwnerAt(t.Cluster, end); ok {
			tenant, _ := c.tenants.Get(cl.TenantID(src))
			class := tenant.Class
			for _, v := range []string{cl.Meta("tenant-class"), c.cfg.TenantClass} {
				if class == "" {
					class = v
				}
			}
			// The owner changes who pays, never what ran: keep the cluster's
			// pause state and spans.
			cl.Paused = t.Cluster.Paused
			t = metrics.Target{Cluster: cl, Tenant: tenant.ID, Project: cl.Project, TenantClass: class, Paused: t.Paused}
		}
		out[i] = t
		sig.WriteString(t.Tenant + "\x00" + t.Project + "\x00" + t.TenantClass + "\x01")
	}
	return out, sig.String()
}

// platformProps maps cluster external IDs to the Platform properties their
// events carry.
func platformProps(targets []metrics.Target) map[string]map[string]any {
	var out map[string]map[string]any
	for i := range targets {
		if p := targets[i].Cluster.PlatformProperties(); p != nil {
			if out == nil {
				out = map[string]map[string]any{}
			}
			out[targets[i].Cluster.ExternalID()] = p
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
		p := props[evs[i].Dim(usage.DimTenantCluster)]
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
