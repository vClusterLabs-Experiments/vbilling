package records

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Ledger sources the aggregator commits under. Each has its own watermark,
// which is all the state the aggregator needs to resume after a crash.
const (
	// SourceBase holds one event per tenant, metric and window, built from
	// the records that arrived by the window's end plus Settle.
	SourceBase = "records"
	// SourceLate holds adjustments for records that arrived after their
	// window was committed, one commit per arrival window.
	SourceLate = "records-late"
)

// Aggregator sums records into usage events. The split between on-time and
// late records depends only on when each record was appended to the
// records ledger, never on when the aggregator runs, so recomputing a
// window after a crash yields the same events with the same IDs.
type Aggregator struct {
	records *spool.Spool // per-request records
	ledger  *spool.Spool // usage ledger
	opts    Options
	health  *health.Registry
	tel     *telemetry.Registry
}

// NewAggregator prepares an aggregator. It raises the records ledger's
// clock past every range already committed, so a record appended after a
// restart can never land inside a closed range, even if the wall clock
// moved back. Call it before the ingest API accepts records.
func NewAggregator(records, ledger *spool.Spool, opts Options, h *health.Registry, tel *telemetry.Registry) *Aggregator {
	opts.defaults()
	if h == nil {
		h = health.New(tel)
	}
	if tel == nil {
		tel = telemetry.New()
	}
	if wm, ok := ledger.Watermark(SourceBase); ok {
		records.AdvanceClock(wm.Add(opts.Settle))
	}
	if wm, ok := ledger.Watermark(SourceLate); ok {
		records.AdvanceClock(wm)
	}
	return &Aggregator{records: records, ledger: ledger, opts: opts, health: h, tel: tel}
}

// Run aggregates every few seconds until ctx ends.
func (a *Aggregator) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		if err := a.Tick(); err != nil {
			a.health.Fail(health.Name("records", "aggregator"), "records", "", err, "")
		} else {
			a.health.OK(health.Name("records", "aggregator"), "records", "", 1)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick commits every window that has closed since the last tick.
func (a *Aggregator) Tick() error {
	now := a.opts.Now().UTC()
	// Seal: from here on every record is stamped after now.
	a.records.AdvanceClock(now)
	size := a.opts.Window

	// Windows whose end + Settle has passed.
	closable := now.Add(-a.opts.Settle).Truncate(size)
	baseWM, ok := a.ledger.Watermark(SourceBase)
	if !ok {
		baseWM = a.startFrom(closable)
	}
	if closable.After(baseWM) {
		if err := a.commitBase(baseWM, capRange(baseWM, closable, size)); err != nil {
			return err
		}
	}

	// Arrival windows that have fully passed.
	arrived := now.Truncate(size)
	lateWM, ok := a.ledger.Watermark(SourceLate)
	if !ok {
		lateWM = a.startFrom(arrived)
	}
	if arrived.After(lateWM) {
		return a.commitLate(lateWM, capRange(lateWM, arrived, size))
	}
	return nil
}

// maxWindowsPerTick bounds one tick's work (and the request IDs it holds in
// memory) while catching up after downtime.
const maxWindowsPerTick = 60

func capRange(from, to time.Time, size time.Duration) time.Time {
	if limit := from.Add(maxWindowsPerTick * size); to.After(limit) {
		return limit
	}
	return to
}

// startFrom picks where a fresh aggregator begins: the window of the oldest
// stored record, so nothing already accepted is skipped.
func (a *Aggregator) startFrom(fallback time.Time) time.Time {
	st := a.records.Stats()
	if st.OldestAt.IsZero() {
		return fallback
	}
	start := st.OldestAt.Add(-a.opts.MaxFuture).Truncate(a.opts.Window)
	if start.After(fallback) {
		return fallback
	}
	return start
}

type group struct {
	ev    usage.Event
	count int
}

// key groups records by everything that identifies a usage event.
func key(ev *usage.Event) string {
	e := *ev
	e.ID, e.Quantity, e.Properties = "", 0, nil
	return usage.DeriveID(&e)
}

func addTo(groups map[string]*group, order *[]string, ev *usage.Event) {
	k := key(ev)
	g, ok := groups[k]
	if !ok {
		e := *ev
		e.ID, e.Quantity, e.Properties, e.Source = "", 0, nil, SourceBase
		g = &group{ev: e}
		groups[k] = g
		*order = append(*order, k)
	}
	g.ev.Quantity += ev.Quantity
	g.count++
}

// commitBase commits the windows ending in (from, to].
func (a *Aggregator) commitBase(from, to time.Time) error {
	type win struct {
		groups map[string]*group
		order  []string
		seen   map[string]bool
		dups   int
	}
	wins := map[time.Time]*win{}
	err := a.records.ScanRecords(from, to, func(ev *usage.Event, at time.Time) error {
		if at.After(ev.WindowEnd.Add(a.opts.Settle)) {
			return nil // late: billed by commitLate
		}
		w := wins[ev.WindowEnd]
		if w == nil {
			w = &win{groups: map[string]*group{}, seen: map[string]bool{}}
			wins[ev.WindowEnd] = w
		}
		if w.seen[ev.ID] {
			w.dups++
			return nil // a retried request
		}
		w.seen[ev.ID] = true
		addTo(w.groups, &w.order, ev)
		return nil
	})
	if err != nil {
		return fmt.Errorf("read records: %w", err)
	}
	now := a.opts.Now().UTC()
	var emptyThrough time.Time
	flushEmpty := func() error {
		if emptyThrough.IsZero() {
			return nil
		}
		err := a.ledger.AppendWindow(SourceBase, emptyThrough, nil, "")
		emptyThrough = time.Time{}
		return err
	}
	for end := from.Add(a.opts.Window); !end.After(to); end = end.Add(a.opts.Window) {
		w := wins[end]
		if w == nil || len(w.order) == 0 {
			emptyThrough = end // consecutive empty windows share one commit
			continue
		}
		if err := flushEmpty(); err != nil {
			return err
		}
		evs := a.finish(w.groups, w.order, now, nil)
		note := ""
		if w.dups > 0 {
			note = fmt.Sprintf("%d duplicate record(s) ignored", w.dups)
		}
		if err := a.ledger.AppendWindow(SourceBase, end, evs, note); err != nil {
			return fmt.Errorf("commit records window %s: %w", end.Format(time.RFC3339), err)
		}
		a.tel.Add("vbilling_records_events_total", "Usage events built from per-request records.", map[string]string{"kind": "on_time"}, float64(len(evs)))
	}
	return flushEmpty()
}

// commitLate commits the arrival windows ending in (from, to].
func (a *Aggregator) commitLate(from, to time.Time) error {
	size := a.opts.Window
	type late struct {
		byWindow map[time.Time][]usage.Event
	}
	arrivals := map[time.Time]*late{}
	err := a.records.ScanAppended(from, to, func(ev *usage.Event, at time.Time) error {
		if !at.After(ev.WindowEnd.Add(a.opts.Settle)) {
			return nil // on time: billed by commitBase
		}
		end := at.Truncate(size).Add(size)
		l := arrivals[end]
		if l == nil {
			l = &late{byWindow: map[time.Time][]usage.Event{}}
			arrivals[end] = l
		}
		l.byWindow[ev.WindowEnd] = append(l.byWindow[ev.WindowEnd], *ev)
		return nil
	})
	if err != nil {
		return fmt.Errorf("read late records: %w", err)
	}
	now := a.opts.Now().UTC()
	var emptyThrough time.Time
	for end := from.Add(size); !end.After(to); end = end.Add(size) {
		l := arrivals[end]
		if l == nil {
			emptyThrough = end
			continue
		}
		if !emptyThrough.IsZero() {
			if err := a.ledger.AppendWindow(SourceLate, emptyThrough, nil, ""); err != nil {
				return err
			}
			emptyThrough = time.Time{}
		}
		arrivalStart := end.Add(-size)
		var evs []usage.Event
		dups := 0
		windows := make([]time.Time, 0, len(l.byWindow))
		for we := range l.byWindow {
			windows = append(windows, we)
		}
		sort.Slice(windows, func(i, j int) bool { return windows[i].Before(windows[j]) })
		for _, we := range windows {
			// Everything stored for this window before this arrival window has
			// been billed already, on time or as an earlier adjustment.
			billed := map[string]bool{}
			onTimeCut := we.Add(a.opts.Settle)
			err := a.records.ScanRecords(we.Add(-size), we, func(ev *usage.Event, at time.Time) error {
				if ev.WindowEnd.Equal(we) && (!at.After(onTimeCut) || at.Before(arrivalStart)) {
					billed[ev.ID] = true
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("read records of %s: %w", we.Format(time.RFC3339), err)
			}
			groups := map[string]*group{}
			var order []string
			for i := range l.byWindow[we] {
				ev := &l.byWindow[we][i]
				if billed[ev.ID] {
					dups++
					continue
				}
				billed[ev.ID] = true
				addTo(groups, &order, ev)
			}
			evs = append(evs, a.finish(groups, order, now, &end)...)
		}
		note := fmt.Sprintf("late records arrived by %s", end.Format(time.RFC3339))
		if dups > 0 {
			note += fmt.Sprintf("; %d duplicate record(s) ignored", dups)
		}
		if err := a.ledger.AppendWindow(SourceLate, end, evs, note); err != nil {
			return fmt.Errorf("commit late records %s: %w", end.Format(time.RFC3339), err)
		}
		if len(evs) > 0 {
			log.Printf("[records] %d late usage event(s) for windows before %s", len(evs), arrivalStart.Format(time.RFC3339))
		}
		a.tel.Add("vbilling_records_events_total", "Usage events built from per-request records.", map[string]string{"kind": "late"}, float64(len(evs)))
	}
	if !emptyThrough.IsZero() {
		return a.ledger.AppendWindow(SourceLate, emptyThrough, nil, "")
	}
	return nil
}

// finish turns groups into finalized events. lateBy marks late adjustments,
// whose IDs are salted with the arrival window so they never collide with
// the window's on-time event.
func (a *Aggregator) finish(groups map[string]*group, order []string, now time.Time, lateBy *time.Time) []usage.Event {
	out := make([]usage.Event, 0, len(order))
	for _, k := range order {
		g := groups[k]
		ev := g.ev
		ev.RecordedAt = now
		ev.Properties = map[string]any{"records": g.count}
		if lateBy != nil {
			ev.Source = SourceLate
			ev.Properties["late"] = true
			ev.Properties["arrived_by"] = lateBy.Format(time.RFC3339)
			ev.Finalize()
			ev.ID = usage.DeriveIDWith(&ev, "late@"+lateBy.Format(time.RFC3339))
		} else {
			ev.Finalize()
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
