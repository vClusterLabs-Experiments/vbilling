package pricing

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// OffNote is what GET /api/v1/pricing and the startup log say with
// PRICE_CHECK=off.
const OffNote = "PRICE_CHECK=off: vBilling checks no billing platform's prices and holds nothing. " +
	"Usage a billing platform has no price for is delivered and rated at 0 there, without a warning from vBilling: " +
	"read the billing platform's draft invoices to see what it does not price"

// Report is GET /api/v1/pricing.
type Report struct {
	Mode string `json:"mode"`
	// Note says in words what the mode means when the rest of the report
	// cannot (PRICE_CHECK=off: nothing is checked).
	Note         string       `json:"note,omitempty"`
	Interval     string       `json:"interval"`
	Lookback     string       `json:"lookback"`
	OK           bool         `json:"ok"` // no destination has an unpriced series or held events
	Unpriced     int          `json:"unpriced"`
	HeldEvents   int          `json:"held_events"`
	Destinations []DestReport `json:"destinations"`
}

// Off is the report of a vBilling that runs with PRICE_CHECK=off.
func Off() Report {
	return Report{Mode: ModeOff, Note: OffNote, OK: true, Destinations: []DestReport{}}
}

// DestReport is one destination's coverage. Priced, Unpriced, Zero and
// Unknown count series: each row of Series is counted once.
type DestReport struct {
	Destination string    `json:"destination"`
	LastCheck   time.Time `json:"last_check,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Priced      int       `json:"priced"`
	Unpriced    int       `json:"unpriced"`
	Zero        int       `json:"zero"`
	Unknown     int       `json:"unknown"`
	HeldEvents  int       `json:"held_events"`
	// Warnings are prices that look like a mistake although every series
	// involved is priced (a whole node charged for its node-hours and again
	// for its GPUs, CPU and memory).
	Warnings []string `json:"warnings,omitempty"`
	Series   []Row    `json:"series"`
}

// Row is one series at a destination: a plan, a metric, a SKU and the values
// of the other keys the platform can price the metric on (Dims), with its own
// status. Two series that differ only in a Dims value (capacity_type=spot and
// capacity_type=on-demand, say) are two rows.
type Row struct {
	Plan   string            `json:"plan,omitempty"`
	Metric string            `json:"metric"`
	SKU    string            `json:"sku,omitempty"`
	Dims   map[string]string `json:"dims,omitempty"` // the other pricing keys; "" for one the usage carries no value for
	Status string            `json:"status"`         // priced | unpriced | zero | unknown
	Detail string            `json:"detail,omitempty"`
	Hint   string            `json:"hint,omitempty"`
	// Note is something to know about where the series is rated, whatever
	// its status.
	Note       string    `json:"note,omitempty"`
	Uncounted  bool      `json:"uncounted,omitempty"`    // the platform would not count this usage at all yet: held in warn mode too
	NoUsageYet bool      `json:"no_usage_yet,omitempty"` // a catalog metric with no recent usage
	LastSeen   time.Time `json:"last_seen,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	HeldEvents int       `json:"held_events,omitempty"`
	// ReleasedUnpriced counts held events released without a price after
	// MaxHold (23 hours) since vBilling started.
	ReleasedUnpriced   int       `json:"released_unpriced_events,omitempty"`
	ReleasedUnpricedAt time.Time `json:"released_unpriced_at,omitempty"`
	HeldSince          time.Time `json:"held_since,omitempty"`
	OldestHeld         time.Time `json:"oldest_held_window,omitempty"`
}

// rank orders statuses from worst to best, for sorting.
var rank = map[string]int{destinations.PriceMissing: 0, destinations.PriceUnknown: 1, destinations.PriceZero: 2, destinations.PriceCovered: 3}

// Report returns the current coverage of every destination.
func (s *Service) Report() Report {
	rep := Report{Mode: s.opts.Mode, Interval: s.opts.Interval.String(), Lookback: s.opts.Lookback.String(), OK: true}
	for _, name := range s.order {
		ds := s.dests[name]
		// Never ahead of what health and /metrics show: a check publishes
		// there before its answers can be read here.
		ds.pubMu.Lock()
		d := s.destReport(ds)
		ds.pubMu.Unlock()
		rep.Unpriced += d.Unpriced
		rep.HeldEvents += d.HeldEvents
		rep.Destinations = append(rep.Destinations, d)
	}
	// Held events of destinations that cannot check prices any more.
	if s.held != nil {
		for _, name := range s.held.Destinations() {
			if _, ok := s.dests[name]; !ok {
				d := DestReport{Destination: name, HeldEvents: s.held.Count(name),
					LastError: "held events for a destination that no longer checks prices; when it is configured again it waits for an operator's start decision (POST /api/v1/destinations/<name>/start), which delivers them or moves them to its dead letters"}
				rep.HeldEvents += d.HeldEvents
				rep.Destinations = append(rep.Destinations, d)
			}
		}
	}
	rep.OK = rep.Unpriced == 0 && rep.HeldEvents == 0
	return rep
}

// CheckNow checks every destination and returns the report.
func (s *Service) CheckNow(ctx context.Context) Report {
	s.CheckAll(ctx)
	return s.Report()
}

// destReport builds one destination's report: one row per series. The caller
// holds ds.pubMu, or is the check that publishes.
func (s *Service) destReport(ds *destState) DestReport {
	held := map[string]heldInfo{}
	if s.held != nil {
		for _, h := range s.held.Series(ds.name) {
			held[h.Series] = heldInfo{h.Count, h.Since, h.Oldest, h.Sample}
		}
	}

	s.mu.Lock()
	d := DestReport{Destination: ds.name, LastCheck: ds.lastCheck, LastSuccess: ds.lastSuccess, LastError: ds.lastErr}
	rows := make(map[string]*Row, len(ds.series))
	for key, st := range ds.series {
		r := &Row{Plan: st.s.Plan, Metric: st.s.Metric, SKU: st.s.SKU, Dims: maps.Clone(st.s.Dims), NoUsageYet: st.s.Catalog,
			Status: st.status, Detail: st.detail, Hint: st.hint, Note: st.note, Uncounted: st.uncounted, CheckedAt: st.checkedAt,
			ReleasedUnpriced: st.releasedUnpriced, ReleasedUnpricedAt: st.releasedAt}
		if !st.s.Catalog {
			r.LastSeen = st.lastSeen
		}
		if h, ok := held[key]; ok {
			addHeld(r, h)
			delete(held, key)
		}
		rows[key] = r
	}
	d.Warnings = doubleCharges(ds)
	s.mu.Unlock()
	// Held series not (or no longer) in the series map: they belong to the
	// series their sample event is in now.
	for _, h := range held {
		t, _ := s.tenants.Get(h.sample.Tenant)
		ser := destinations.SeriesOf(ds.pc, ds.pc.PricePlan(t), &h.sample)
		r := rows[ser.Key()]
		if r == nil {
			r = &Row{Plan: ser.Plan, Metric: ser.Metric, SKU: ser.SKU, Dims: ser.Dims, Detail: "not checked yet"}
			rows[ser.Key()] = r
		}
		addHeld(r, h)
	}

	for _, r := range rows {
		if r.Status == "" {
			r.Status = destinations.PriceUnknown
			if r.Detail == "" {
				r.Detail = "not checked yet"
				if d.LastError != "" {
					r.Detail += " (last check failed: " + d.LastError + ")"
				}
			}
		}
		switch r.Status {
		case destinations.PriceCovered:
			d.Priced++
		case destinations.PriceMissing:
			d.Unpriced++
		case destinations.PriceZero:
			d.Zero++
		default:
			d.Unknown++
		}
		d.HeldEvents += r.HeldEvents
		d.Series = append(d.Series, *r)
	}
	sort.Slice(d.Series, func(i, j int) bool {
		a, b := d.Series[i], d.Series[j]
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		if a.Plan != b.Plan {
			return a.Plan < b.Plan
		}
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		if a.SKU != b.SKU {
			return a.SKU < b.SKU
		}
		if la, lb := dimsKey(a.Dims), dimsKey(b.Dims); la != lb {
			return la < lb
		}
		return !a.NoUsageYet && b.NoUsageYet
	})
	return d
}

// dimsKey orders rows that differ only in their other pricing keys.
func dimsKey(dims map[string]string) string {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(dims[k])
		b.WriteByte(0)
	}
	return b.String()
}

// Whole nodes (dedicated nodes of the control plane cluster, private nodes of
// a tenant's cluster) are metered twice on purpose: node-hours for the node,
// and GPU, CPU and memory hours for its full capacity, marked with one of
// these billing modes (metrics.Collector.meterWholeNode). A plan is meant to
// price one of the two.
var wholeNodeModes = map[string]bool{"dedicated_node": true, "private_node": true}

// perResource are the metrics a whole node emits next to its node-hours.
var perResource = []string{usage.MetricGPUHours, usage.MetricCPUCoreHours, usage.MetricMemoryGBHours}

// wholeNodeUsage reports whether an event is the GPU, CPU or memory capacity
// of a whole node.
func wholeNodeUsage(ev *usage.Event) bool {
	if !wholeNodeModes[ev.Dim(usage.DimBillingMode)] {
		return false
	}
	for _, m := range perResource {
		if ev.Metric == m {
			return true
		}
	}
	return false
}

// doubleCharges lists the plans that price, at non-zero rates, both the
// node-hours of whole nodes and the GPU, CPU or memory hours those same nodes
// emit: such a plan charges a node about twice. s.mu held.
func doubleCharges(ds *destState) []string {
	type planUse struct {
		node bool
		res  map[string]bool
	}
	plans := map[string]*planUse{}
	for _, st := range ds.series {
		if st.s.Catalog || st.status != destinations.PriceCovered {
			continue
		}
		p := plans[st.s.Plan]
		if p == nil {
			p = &planUse{res: map[string]bool{}}
			plans[st.s.Plan] = p
		}
		switch {
		case st.s.Metric == usage.MetricPrivateNodeHours:
			p.node = true
		case st.wholeNode:
			p.res[st.s.Metric] = true
		}
	}
	var names []string
	for plan, p := range plans {
		if p.node && len(p.res) > 0 {
			names = append(names, plan)
		}
	}
	sort.Strings(names)
	var out []string
	for _, plan := range names {
		var res []string
		for _, m := range perResource {
			if plans[plan].res[m] {
				res = append(res, m)
			}
		}
		where := "the prices of " + ds.name
		if plan != "" {
			where = "plan " + plan
		}
		out = append(out, fmt.Sprintf("%s prices whole nodes twice: %s and %s of the same dedicated or private nodes are all priced above 0, "+
			"so a node is charged for its node-hours and again for its GPUs, CPU and memory. "+
			"If that is not intended, price either the node-hours or the per-resource usage of whole nodes (billing_mode dedicated_node or private_node) at 0",
			where, usage.MetricPrivateNodeHours, strings.Join(res, ", ")))
	}
	return out
}

type heldInfo struct {
	count         int
	since, oldest time.Time
	sample        usage.Event
}

func addHeld(r *Row, h heldInfo) {
	r.HeldEvents += h.count
	if r.HeldSince.IsZero() || h.since.Before(r.HeldSince) {
		r.HeldSince = h.since
	}
	if r.OldestHeld.IsZero() || h.oldest.Before(r.OldestHeld) {
		r.OldestHeld = h.oldest
	}
}
