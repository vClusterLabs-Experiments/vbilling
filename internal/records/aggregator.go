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

// copies indexes the stored copies of requests by record ID. A request is
// stored again when its emitter retries or a puller reads it back.
type copies map[string][]recordCopy

// recordCopy is the window and ledger position of one stored copy.
type recordCopy struct {
	start, end time.Time
	seq        uint64
}

func (c copies) add(ev *usage.Event, seq uint64) {
	for _, x := range c[ev.ID] {
		if x.seq == seq {
			return // the same record, read again
		}
	}
	c[ev.ID] = append(c[ev.ID], recordCopy{start: ev.WindowStart, end: ev.WindowEnd, seq: seq})
}

// earlier reports whether a copy of ev was stored before it (seq) in a
// window that overlaps its own. The first copy stored is the one billed:
// in its window if it was on time, else as a late adjustment. With one
// window size only copies in the same window overlap; after a change of
// window size, a request retried across it sits in two overlapping windows
// of different sizes.
func (c copies) earlier(ev *usage.Event, seq uint64) bool {
	for _, x := range c[ev.ID] {
		if x.seq < seq && x.end.After(ev.WindowStart) && x.start.Before(ev.WindowEnd) {
			return true
		}
	}
	return false
}

// baseWindow is the on-time records of one window end.
type baseWindow struct {
	groups map[string]*group
	order  []string
	dups   int
}

// commitBase commits the windows ending in (from, to]. It commits the
// window ends its records carry rather than stepping a grid from the
// watermark: records are bucketed when stored, so the records of an
// install that used another window size keep their windows, and its
// watermark may be off the current grid.
func (a *Aggregator) commitBase(from, to time.Time) error {
	wins, minStart, err := a.baseWindows(from, to, copies{})
	if err != nil {
		return err
	}
	if minStart.Before(from) {
		// A window that started before from (a longer one, stored before
		// the window size changed) overlaps windows committed already: a
		// copy of a request stored there was billed there.
		prior := copies{}
		err := a.records.ScanRecordsSeq(minStart, from, func(ev *usage.Event, _ time.Time, seq uint64) error {
			prior.add(ev, seq)
			return nil
		})
		if err != nil {
			return fmt.Errorf("read records: %w", err)
		}
		if wins, _, err = a.baseWindows(from, to, prior); err != nil {
			return err
		}
	}
	ends := make([]time.Time, 0, len(wins))
	for end, w := range wins {
		if len(w.order) > 0 {
			ends = append(ends, end)
		}
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i].Before(ends[j]) })
	now := a.opts.Now().UTC()
	for _, end := range ends {
		w := wins[end]
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
	if len(ends) == 0 || ends[len(ends)-1].Before(to) {
		return a.ledger.AppendWindow(SourceBase, to, nil, "") // the empty windows up to to share one commit
	}
	return nil
}

// baseWindows groups the on-time records of the windows ending in
// (from, to], in ledger order, dropping every request that has an earlier
// copy (in prior or in the range). It also returns the earliest start of
// those windows.
func (a *Aggregator) baseWindows(from, to time.Time, seen copies) (map[time.Time]*baseWindow, time.Time, error) {
	wins := map[time.Time]*baseWindow{}
	minStart := from
	err := a.records.ScanRecordsSeq(from, to, func(ev *usage.Event, at time.Time, seq uint64) error {
		dup := seen.earlier(ev, seq)
		seen.add(ev, seq)
		if !ev.WindowEnd.After(from) || ev.WindowEnd.After(to) || at.After(ev.WindowEnd.Add(a.opts.Settle)) {
			return nil // committed, not closed yet, or late (billed by commitLate)
		}
		if ev.WindowStart.Before(minStart) {
			minStart = ev.WindowStart
		}
		w := wins[ev.WindowEnd]
		if w == nil {
			w = &baseWindow{groups: map[string]*group{}}
			wins[ev.WindowEnd] = w
		}
		if dup {
			w.dups++
			return nil // a retried request
		}
		addTo(w.groups, &w.order, ev)
		return nil
	})
	if err != nil {
		return nil, from, fmt.Errorf("read records: %w", err)
	}
	return wins, minStart, nil
}

// lateRecord is a record that arrived after its window was committed.
type lateRecord struct {
	ev  usage.Event
	seq uint64
}

// recordWindow is a record's window, as bucketed when it was stored.
type recordWindow struct{ start, end time.Time }

// commitLate commits the arrival windows ending in (from, to].
func (a *Aggregator) commitLate(from, to time.Time) error {
	size := a.opts.Window
	arrivals := map[time.Time]map[recordWindow][]lateRecord{}
	err := a.records.ScanAppendedSeq(from, to, func(ev *usage.Event, at time.Time, seq uint64) error {
		if !at.After(ev.WindowEnd.Add(a.opts.Settle)) {
			return nil // on time: billed by commitBase
		}
		end := at.Truncate(size).Add(size)
		if arrivals[end] == nil {
			arrivals[end] = map[recordWindow][]lateRecord{}
		}
		w := recordWindow{ev.WindowStart, ev.WindowEnd}
		arrivals[end][w] = append(arrivals[end][w], lateRecord{*ev, seq})
		return nil
	})
	if err != nil {
		return fmt.Errorf("read late records: %w", err)
	}
	ends := make([]time.Time, 0, len(arrivals))
	for end := range arrivals {
		ends = append(ends, end)
	}
	sort.Slice(ends, func(i, j int) bool { return ends[i].Before(ends[j]) })
	now := a.opts.Now().UTC()
	for _, end := range ends {
		byWindow := arrivals[end]
		windows := make([]recordWindow, 0, len(byWindow))
		for w := range byWindow {
			windows = append(windows, w)
		}
		sort.Slice(windows, func(i, j int) bool {
			if !windows[i].end.Equal(windows[j].end) {
				return windows[i].end.Before(windows[j].end)
			}
			return windows[i].start.Before(windows[j].start)
		})
		var evs []usage.Event
		dups := 0
		for _, w := range windows {
			// A copy stored before this one, in this window or one that
			// overlaps it, was billed already, on time or as an earlier
			// adjustment, or will be when its window closes.
			seen := copies{}
			err := a.records.ScanRecordsSeq(w.start, w.end, func(ev *usage.Event, _ time.Time, seq uint64) error {
				seen.add(ev, seq)
				return nil
			})
			if err != nil {
				return fmt.Errorf("read records of %s: %w", w.end.Format(time.RFC3339), err)
			}
			groups := map[string]*group{}
			var order []string
			for i := range byWindow[w] {
				r := &byWindow[w][i]
				if seen.earlier(&r.ev, r.seq) {
					dups++
					continue
				}
				addTo(groups, &order, &r.ev)
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
			log.Printf("[records] %d late usage event(s) for windows before %s", len(evs), end.Add(-size).Format(time.RFC3339))
		}
		a.tel.Add("vbilling_records_events_total", "Usage events built from per-request records.", map[string]string{"kind": "late"}, float64(len(evs)))
	}
	if len(ends) == 0 || ends[len(ends)-1].Before(to) {
		return a.ledger.AppendWindow(SourceLate, to, nil, "")
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
