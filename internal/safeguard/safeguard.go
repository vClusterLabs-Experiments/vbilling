// Package safeguard keeps vBilling from billing what does not exist. Usage
// vBilling measures itself and usage emitters push to it can be wrong in
// different ways, so they are guarded differently: measured usage is
// checked after it is committed, pushed usage is refused before it is
// stored.
//
// # Measured usage: checked after every collection
//
// Measured usage is every ledger event whose source is not ingest:<client>,
// records or records-late: what the collector, Prometheus and the fills of
// a cluster's own API wrote. After every collection, the Checker takes the
// windows that received such events since its last pass, leaves out those
// that ended more than 24 hours before the newest of them, and checks each
// window whole, on its measured events only:
//
//   - node_hours: the vcluster_private_node_hours billed for a node (its
//     node dimension, or else its resource ID) are at most the window's
//     length;
//   - gpu_hours: the vcluster_gpu_hours billed for a node are at most its
//     physical GPUs (the physical_gpus property) times the window's length.
//     Events that name no node (a pod's GPUs on a shared node), and nodes
//     whose events carry no physical_gpus, are not checked;
//   - resource_tenants: no resource ID is billed to two tenants.
//
// A violation turns the subject's source health "diverged" and counts in
// vbilling_safeguard_violations_total; a later window within bounds clears
// it. The Checker never changes, caps, holds or drops usage: it says where
// to look, and a held invoice would hide the cause.
//
// # Pushed usage: refused at ingest
//
// Pushed usage is what emitters report: events sent to POST /api/v1/events,
// records sent to POST /api/v1/records, and rows vBilling reads back from a
// gateway's log. The Checker leaves it out. Its windows are the emitter's
// (up to 34 days, any alignment), so one event could make every pass read a
// month of ledger, and its node and resource names are whatever the emitter
// says, so they prove nothing about vBilling's own bills and would raise
// findings against other tenants. Instead the ingest API applies the rules
// of this package and its own to every event and record, and refuses what
// breaks them with the reason, before anything is stored. They guarantee:
//
//   - No pushed usage under a measured metric (PushedMetric). A metric the
//     catalog says vBilling measures itself (any source but ingest and
//     records: today every built-in metric, and those read from Prometheus)
//     takes no pushed usage from any ingest token, on either path, so a
//     ghost node's node-hours or GPU-hours cannot be pushed next to the
//     measured ones. A metric built from records takes records only.
//     Emitters write the metrics an operator declared for pushed usage.
//   - No usage for a tenant vBilling does not know, with
//     RECORDS_UNKNOWN_TENANTS=reject: both push paths resolve the tenant the
//     same way (a tenant vBilling knows, or an alias of one) and refuse any
//     other value, so no tenant of that name reaches a destination. With
//     accept, the default, both paths store such usage under the value as
//     sent and report it.
//   - No usage for a cluster that does not exist (Clusters). Pushed usage
//     that names a cluster (tenant_cluster) names one of its tenant, for a
//     window in which the cluster existed: it runs now and was created
//     before the window ended, or vBilling metered it for that tenant and
//     the ledger still holds that usage. Usage that names no cluster is the
//     tenant's as a whole. A row read back from a gateway's log is kept
//     without its cluster rather than refused.
//   - No usage a destination is certain to dead-letter: usage older than
//     the oldest a configured destination accepts is refused.
//
// They do not bound quantities: vBilling has no measurement to hold an
// emitter's own metric against, so what a gateway reports under it is
// billed as reported (the new SKUs it may bring in are capped by the API).
//
// The operator can correct any metric, a measured one included, by pushing
// events with the API token (an operator adjustment). The rules on tenants,
// clusters and age apply to it too. It is pushed usage: its events keep an
// ingest:<client> source, carry the property operator_adjustment, which no
// emitter can set, and are not part of the Checker's checks.
package safeguard

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Checks.
const (
	NodeHours       = "node_hours"
	GPUHours        = "gpu_hours"
	ResourceTenants = "resource_tenants"
)

// tolerance absorbs rounding: every quantity is rounded to nine decimals.
const tolerance = 1e-6

// readLimit bounds the ledger records one pass reads; the next continues.
const readLimit = 200_000

// checkWithin is how far back from the newest window a pass checks: past
// MAX_BACKFILL (6h), the reach of Kubernetes and Prometheus backfill and of
// tenant API fills.
const checkWithin = 24 * time.Hour

// Finding is one violation in one window.
type Finding struct {
	Check   string    `json:"check"`
	Subject string    `json:"subject"` // node or resource ID
	Window  time.Time `json:"window_end"`
	Message string    `json:"message"`
}

// Checker checks the windows new ledger records touch.
type Checker struct {
	spool  *spool.Spool
	health *health.Registry
	tel    *telemetry.Registry

	mu      sync.Mutex
	read    uint64                     // ledger read through this seq
	scanned window                     // the ledger range the last pass read
	flagged map[string]time.Time       // health source -> end of the window last over
	counted map[string]time.Time       // check|subject|window counted already -> window end
	open    map[string]map[string]bool // check -> flagged subjects
}

// New returns a checker that starts with the records appended from now on.
func New(sp *spool.Spool, h *health.Registry, tel *telemetry.Registry) *Checker {
	if tel == nil {
		tel = telemetry.New()
	}
	if h == nil {
		h = health.New(tel)
	}
	return &Checker{spool: sp, health: h, tel: tel, read: sp.Committed(),
		flagged: map[string]time.Time{}, counted: map[string]time.Time{}, open: map[string]map[string]bool{}}
}

type window struct{ start, end time.Time }

// measured reports whether vBilling measured an event itself, rather than
// an emitter reporting it: ingested events (ingest:<client>) and records
// aggregated per window (records, records-late) are reported.
func measured(e *usage.Event) bool {
	return !strings.HasPrefix(e.Source, "ingest:") && e.Source != "records" && e.Source != "records-late"
}

// Once checks every window that received events since the last call.
func (c *Checker) Once() []Finding {
	c.mu.Lock()
	defer c.mu.Unlock()
	touched := map[window]bool{}
	for n := 0; n < readLimit; {
		recs, err := c.spool.Read(c.read, 5000)
		if err != nil {
			log.Printf("[safeguard] read ledger: %v", err)
			return nil
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			if r.Event != nil && measured(r.Event) {
				touched[window{r.Event.WindowStart, r.Event.WindowEnd}] = true
			}
		}
		c.read = recs[len(recs)-1].Seq
		n += len(recs)
	}
	var newest time.Time
	for w := range touched {
		if w.end.After(newest) {
			newest = w.end
		}
	}
	for w := range touched {
		// A window that old is reached only by a straggling late record;
		// checking it would read a day of ledger for one event.
		if w.end.Before(newest.Add(-checkWithin)) {
			delete(touched, w)
		}
	}
	if len(touched) == 0 {
		return nil
	}
	var from, to time.Time
	for w := range touched {
		if from.IsZero() || w.start.Before(from) {
			from = w.start
		}
		if w.end.After(to) {
			to = w.end
		}
	}
	byWindow := map[window][]usage.Event{}
	c.scanned = window{from, to}
	err := c.spool.Scan(from, to, func(e *usage.Event) error {
		if w := (window{e.WindowStart, e.WindowEnd}); touched[w] && measured(e) {
			byWindow[w] = append(byWindow[w], *e)
		}
		return nil
	})
	if err != nil {
		log.Printf("[safeguard] scan ledger: %v", err)
		return nil
	}
	windows := make([]window, 0, len(byWindow))
	for w := range byWindow {
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].end.Before(windows[j].end) })
	var all []Finding
	for _, w := range windows {
		findings, checked := Check(w.start, w.end, byWindow[w])
		c.report(w.end, findings, checked)
		all = append(all, findings...)
	}
	c.prune(to.Add(-7 * time.Hour))
	return all
}

// Check returns the violations among one window's events and, per check,
// the subjects it looked at.
func Check(start, end time.Time, events []usage.Event) ([]Finding, map[string][]string) {
	hours := end.Sub(start).Hours()
	nodeHours, gpuHours, physical := map[string]float64{}, map[string]float64{}, map[string]float64{}
	tenants := map[string]map[string]bool{}
	for i := range events {
		e := &events[i]
		if e.ResourceID != "" {
			if tenants[e.ResourceID] == nil {
				tenants[e.ResourceID] = map[string]bool{}
			}
			tenants[e.ResourceID][e.Tenant] = true
		}
		node := e.Dim(usage.DimNode)
		switch e.Metric {
		case usage.MetricPrivateNodeHours: // a whole node: dedicated or private
			if node == "" {
				node = e.ResourceID
			}
			nodeHours[node] += e.Quantity
		case usage.MetricGPUHours:
			if node == "" {
				continue // a pod's GPUs on a shared node: its events name the cluster, not the node
			}
			gpuHours[node] += e.Quantity
			if g, ok := e.Properties["physical_gpus"].(float64); ok && g > physical[node] {
				physical[node] = g
			}
		}
	}
	span := start.UTC().Format("2006-01-02 15:04:05") + " to " + end.UTC().Format("15:04:05")
	var out []Finding
	checked := map[string][]string{}
	for node, h := range nodeHours {
		checked[NodeHours] = append(checked[NodeHours], node)
		if h > hours+tolerance {
			out = append(out, Finding{NodeHours, node, end, fmt.Sprintf("%s: node %s billed %.6f node-hours, more than the window's %.6f", span, node, h, hours)})
		}
	}
	for node, h := range gpuHours {
		g := physical[node]
		if g <= 0 {
			continue
		}
		checked[GPUHours] = append(checked[GPUHours], node)
		if h > g*hours+tolerance {
			out = append(out, Finding{GPUHours, node, end, fmt.Sprintf("%s: node %s billed %.6f GPU-hours, more than its %v physical GPUs for the window (%.6f)", span, node, h, g, g*hours)})
		}
	}
	for id, ts := range tenants {
		checked[ResourceTenants] = append(checked[ResourceTenants], id)
		if len(ts) > 1 {
			names := make([]string, 0, len(ts))
			for t := range ts {
				names = append(names, t)
			}
			sort.Strings(names)
			out = append(out, Finding{ResourceTenants, id, end, fmt.Sprintf("%s: %s billed to %d tenants (%s)", span, id, len(names), strings.Join(names, ", "))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Check != out[j].Check {
			return out[i].Check < out[j].Check
		}
		return out[i].Subject < out[j].Subject
	})
	return out, checked
}

var hints = map[string]string{
	NodeHours: "a node is billed more than once in a window: two clusters bill it whole (a node seen as private by two clusters, or a dedicated-node label " +
		"matching two), or a fill repeats a window. GET /api/v1/events?metric=vcluster_private_node_hours for the window shows which",
	GPUHours: "more GPU-hours than the node's GPUs can give: the node is billed whole by two clusters, or its GPU count changed. " +
		"GET /api/v1/events?metric=vcluster_gpu_hours for the window shows which",
	ResourceTenants: "one resource billed to two tenants in one window: a cluster moved owners and two sources attributed the window " +
		"differently, or two clusters bill the same node. Check the cluster's tenant settings and history",
}

// report flags the findings and clears subjects a later window shows within
// bounds. c.mu held.
func (c *Checker) report(end time.Time, findings []Finding, checked map[string][]string) {
	over := map[string]bool{}
	for _, f := range findings {
		name := health.Name("safeguard", f.Check, f.Subject)
		over[name] = true
		if last, ok := c.flagged[name]; !ok || end.After(last) {
			c.flagged[name] = end
		}
		if c.open[f.Check] == nil {
			c.open[f.Check] = map[string]bool{}
		}
		c.open[f.Check][f.Subject] = true
		c.health.Set(name, "safeguard", f.Subject, health.Diverged, f.Message, hints[f.Check])
		key := f.Check + "|" + f.Subject + "|" + end.Format(time.RFC3339Nano)
		if _, done := c.counted[key]; !done {
			c.counted[key] = end
			log.Printf("[safeguard] %s", f.Message)
			c.tel.Add("vbilling_safeguard_violations_total", "Windows in which vBilling billed more than exists (a node over its hours or GPUs, a resource billed to two tenants).",
				map[string]string{"check": f.Check}, 1)
		}
	}
	for check, subjects := range checked {
		for _, s := range subjects {
			name := health.Name("safeguard", check, s)
			if last, ok := c.flagged[name]; ok && !over[name] && end.After(last) {
				delete(c.flagged, name)
				delete(c.open[check], s)
				c.health.Set(name, "safeguard", s, health.OK, "within bounds again from the window ending "+end.UTC().Format(time.RFC3339), "")
			}
		}
	}
	for _, check := range []string{NodeHours, GPUHours, ResourceTenants} {
		c.tel.Set("vbilling_safeguard_flagged", "Nodes and resources whose last checked window billed more than exists.",
			map[string]string{"check": check}, float64(len(c.open[check])))
	}
}

// prune forgets counted violations of windows long gone. c.mu held.
func (c *Checker) prune(cutoff time.Time) {
	for k, end := range c.counted {
		if end.Before(cutoff) {
			delete(c.counted, k)
		}
	}
}
