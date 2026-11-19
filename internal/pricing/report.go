package pricing

import (
	"context"
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Report is GET /api/v1/pricing.
type Report struct {
	Mode         string       `json:"mode"`
	Interval     string       `json:"interval"`
	Lookback     string       `json:"lookback"`
	OK           bool         `json:"ok"` // no destination has an unpriced series or held events
	Unpriced     int          `json:"unpriced"`
	HeldEvents   int          `json:"held_events"`
	Destinations []DestReport `json:"destinations"`
}

// DestReport is one destination's coverage.
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
	Series      []Row     `json:"series"`
}

// Row is one (plan, metric, SKU) at a destination. When the platform prices
// on more dimensions (a Metronome pricing group on region, say), the row
// aggregates every combination seen, and Missing lists the unpriced ones.
type Row struct {
	Plan         string              `json:"plan,omitempty"`
	Metric       string              `json:"metric"`
	SKU          string              `json:"sku,omitempty"`
	Status       string              `json:"status"` // priced | unpriced | zero | unknown
	Detail       string              `json:"detail,omitempty"`
	Hint         string              `json:"hint,omitempty"`
	Uncounted    bool                `json:"uncounted,omitempty"`    // the platform would not count this usage at all yet: held in warn mode too
	NoUsageYet   bool                `json:"no_usage_yet,omitempty"` // a catalog metric with no recent usage
	Combinations int                 `json:"combinations,omitempty"` // series with other pricing dimensions
	Missing      []map[string]string `json:"missing,omitempty"`      // unpriced combinations of those dimensions
	LastSeen     time.Time           `json:"last_seen,omitempty"`
	CheckedAt    time.Time           `json:"checked_at,omitempty"`
	HeldEvents   int                 `json:"held_events,omitempty"`
	// ReleasedUnpriced counts held events released without a price after
	// PRICE_CHECK_MAX_HOLD since vBilling started.
	ReleasedUnpriced   int       `json:"released_unpriced_events,omitempty"`
	ReleasedUnpricedAt time.Time `json:"released_unpriced_at,omitempty"`
	HeldSince          time.Time `json:"held_since,omitempty"`
	OldestHeld         time.Time `json:"oldest_held_window,omitempty"`
}

// rank orders statuses from worst to best for aggregation.
var rank = map[string]int{destinations.PriceMissing: 0, "": 1, destinations.PriceUnknown: 2, destinations.PriceZero: 3, destinations.PriceCovered: 4}

// Report returns the current coverage of every destination.
func (s *Service) Report() Report {
	rep := Report{Mode: s.opts.Mode, Interval: s.opts.Interval.String(), Lookback: s.opts.Lookback.String(), OK: true}
	for _, name := range s.order {
		d := s.destReport(s.dests[name])
		rep.Unpriced += d.Unpriced
		rep.HeldEvents += d.HeldEvents
		rep.Destinations = append(rep.Destinations, d)
	}
	// Held events of destinations that cannot check prices any more.
	if s.held != nil {
		for _, name := range s.held.Destinations() {
			if _, ok := s.dests[name]; !ok {
				d := DestReport{Destination: name, HeldEvents: s.held.Count(name),
					LastError: "held events for a destination that no longer checks prices; they are released when it runs again"}
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

type rowKey struct{ plan, metric, sku string }

func (s *Service) destReport(ds *destState) DestReport {
	held := map[string]heldInfo{}
	if s.held != nil {
		for _, h := range s.held.Series(ds.name) {
			held[h.Series] = heldInfo{h.Count, h.Since, h.Oldest, h.Sample}
		}
	}

	s.mu.Lock()
	d := DestReport{Destination: ds.name, LastCheck: ds.lastCheck, LastSuccess: ds.lastSuccess, LastError: ds.lastErr}
	rows := map[rowKey]*Row{}
	for key, st := range ds.series {
		k := rowKey{st.s.Plan, st.s.Metric, st.s.SKU}
		r := rows[k]
		if r == nil {
			r = &Row{Plan: st.s.Plan, Metric: st.s.Metric, SKU: st.s.SKU, NoUsageYet: st.s.Catalog, Status: st.status, Detail: st.detail, Hint: st.hint, CheckedAt: st.checkedAt}
			rows[k] = r
		} else if rank[st.status] < rank[r.Status] {
			r.Status, r.Detail, r.Hint, r.CheckedAt = st.status, st.detail, st.hint, st.checkedAt
		}
		r.Uncounted = r.Uncounted || st.uncounted
		r.ReleasedUnpriced += st.releasedUnpriced
		if st.releasedAt.After(r.ReleasedUnpricedAt) {
			r.ReleasedUnpricedAt = st.releasedAt
		}
		if len(st.s.Dims) > 0 {
			r.Combinations++
			if st.status == destinations.PriceMissing && len(r.Missing) < 20 {
				r.Missing = append(r.Missing, st.s.Dims)
			}
		}
		if st.lastSeen.After(r.LastSeen) && !st.s.Catalog {
			r.LastSeen = st.lastSeen
		}
		if h, ok := held[key]; ok {
			addHeld(r, h)
			delete(held, key)
		}
	}
	s.mu.Unlock()
	// Held series not (or no longer) in the series map: attribute them by
	// their sample event.
	for _, h := range held {
		k := rowKey{"", h.sample.Metric, h.sample.SKU}
		t, _ := s.tenants.Get(h.sample.Tenant)
		k.plan = ds.pc.PricePlan(t)
		r := rows[k]
		if r == nil {
			r = &Row{Plan: k.plan, Metric: k.metric, SKU: k.sku, Detail: "not checked yet"}
			rows[k] = r
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
		return a.SKU < b.SKU
	})
	return d
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
