package pipeline

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Gate decides whether events may ship to a destination now. PRICE_CHECK=hold
// installs one that holds the events of series the billing platform has no
// price for.
type Gate interface {
	// Decide returns one decision per event. It may block (bounded by ctx)
	// to check series it has not seen before.
	Decide(ctx context.Context, dest string, events []usage.Event) []Decision
	// Releasable reports whether a held series may ship now; sample is one
	// of its held events (for a series the gate does not know yet).
	Releasable(ctx context.Context, dest, series string, sample *usage.Event) bool
	// Changed is closed when a held series may have become releasable.
	Changed() <-chan struct{}
	// ReleasedUnpriced reports n held events of a series released without a
	// price because they reached Options.MaxHold.
	ReleasedUnpriced(dest, series string, sample *usage.Event, n int)
}

// Decision is a gate's verdict for one event.
type Decision struct {
	Series string // the event's series at the destination; "" when the gate does not apply
	Hold   bool
	Reason string
}

// shippedLog remembers, per destination and series, the newest ledger
// sequence a release delivered, so the same events read again from the ledger
// (a batch retried after its held part was released) are not sent twice.
type shippedLog struct {
	mu sync.Mutex
	m  map[string]map[string]uint64
}

func (s *shippedLog) get(dest, series string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[dest][series]
}

func (s *shippedLog) set(dest, series string, seq uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]map[string]uint64{}
	}
	if s.m[dest] == nil {
		s.m[dest] = map[string]uint64{}
	}
	if seq > s.m[dest][series] {
		s.m[dest][series] = seq
	}
}

// reset forgets a destination's releases: an operator rewound its cursor
// to replay, and a replay must send everything again.
func (s *shippedLog) reset(dest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, dest)
}

// park splits a batch into the events to deliver now and the events to hold,
// and writes the held ones durably before the cursor can move past them. An
// event is held when the gate says so, or when its series already has held
// events, so a series always ships in ledger order.
func (d *Dispatcher) park(ctx context.Context, dest destinations.Destination, events []usage.Event, seqs []uint64) ([]usage.Event, error) {
	name := dest.Name()
	var decisions []Decision
	if d.opts.Gate != nil {
		decisions = d.opts.Gate.Decide(ctx, name, events)
	}
	now := time.Now().UTC()
	ship := events[:0:0]
	var hold []HeldEntry
	var already uint64
	for i := range events {
		var dec Decision
		if i < len(decisions) {
			dec = decisions[i]
		}
		if dec.Series == "" {
			ship = append(ship, events[i])
			continue
		}
		if seqs[i] <= d.shipped.get(name, dec.Series) {
			already++ // delivered by a release
			continue
		}
		if dec.Hold || d.opts.Held.Has(name, dec.Series) {
			reason := dec.Reason
			if !dec.Hold {
				reason = "queued behind held events of the same series"
			}
			hold = append(hold, HeldEntry{Seq: seqs[i], At: now, Series: dec.Series, Reason: reason, Event: events[i]})
			continue
		}
		ship = append(ship, events[i])
	}
	if len(hold) > 0 {
		// The tenant exists whatever its prices: create it now (a Lago
		// subscription only counts usage from its start, a Metronome contract
		// from its start date), so released usage lands in its period.
		evs := make([]usage.Event, len(hold))
		for i := range hold {
			evs[i] = hold[i].Event
		}
		if err := d.ensureTenants(ctx, dest, evs); err != nil {
			return nil, err
		}
		added, err := d.opts.Held.Add(name, hold)
		if err != nil {
			return nil, fmt.Errorf("hold events: %w", err)
		}
		if added > 0 {
			log.Printf("[pipeline] %s: holding %d event(s) (%s): %s", name, added, destinations.DescribeSeries(seriesOfEntry(hold[0])), hold[0].Reason)
			d.tel.Add("vbilling_events_held_total", "Events held back from a destination until their series is priced (PRICE_CHECK=hold).", map[string]string{"destination": name}, float64(added))
		}
	}
	if already > 0 {
		d.update(name, func(s *Status) { s.Skipped += already })
	}
	d.publishHeld(name)
	return ship, nil
}

func seriesOfEntry(e HeldEntry) destinations.PriceSeries {
	return destinations.PriceSeries{Metric: e.Event.Metric, SKU: e.Event.SKU}
}

// MaxUnpricedHold bounds how long usage is held for a price. A billing
// platform finalizes an invoice period once it ends plus a grace period
// (Metronome: 24 hours by default), and usage that arrives after that is
// never invoiced for that period, priced or not. Usage from the last moment
// of a period must therefore ship within the grace of that moment, so held
// usage is released, unpriced, once its window started 23 hours ago: the
// platform can still rate it once Finance adds the price, before the period
// is final. Options.MaxHold can only make the hold shorter.
const MaxUnpricedHold = 23 * time.Hour

// maxHold is how long a held event may wait for its price: Options.MaxHold,
// at most MaxUnpricedHold. Zero holds without a limit (tests only: vBilling
// always sets PRICE_CHECK_MAX_HOLD).
func (d *Dispatcher) maxHold() time.Duration {
	return min(d.opts.MaxHold, MaxUnpricedHold)
}

// release delivers the held events of every series that may ship now, in
// ledger order, before any newer event of the destination. An error leaves
// the rest held; the caller retries after a backoff.
func (d *Dispatcher) release(ctx context.Context, dest destinations.Destination) error {
	name := dest.Name()
	for _, h := range d.opts.Held.Series(name) {
		if d.opts.Gate != nil && !d.opts.Gate.Releasable(ctx, name, h.Series, &h.Sample) {
			if mh := d.maxHold(); mh > 0 && !h.Oldest.IsZero() && time.Since(h.Oldest) > mh {
				if err := d.releaseExpired(ctx, dest, h); err != nil {
					return err
				}
			}
			continue
		}
		total := 0
		for ctx.Err() == nil {
			entries, next, err := d.opts.Held.Peek(name, h.Series, d.opts.BatchSize)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				break
			}
			events := make([]usage.Event, len(entries))
			for i := range entries {
				events[i] = entries[i].Event
			}
			dead, err := d.deliver(ctx, dest, events)
			if err != nil {
				return fmt.Errorf("release held events: %w", err)
			}
			last := entries[len(entries)-1].Seq
			if err := d.opts.Held.Release(name, h.Series, last, len(entries), next); err != nil {
				return fmt.Errorf("release held events: %w", err)
			}
			d.shipped.set(name, h.Series, last)
			delivered := uint64(len(entries) - dead)
			total += len(entries)
			labels := map[string]string{"destination": name}
			d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(delivered))
			d.tel.Add("vbilling_events_released_total", "Held events delivered once their series was priced.", labels, float64(len(entries)))
			if dead > 0 {
				d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(dead))
			}
			d.update(name, func(s *Status) {
				s.Delivered += delivered
				s.DeadLettered += uint64(dead)
				s.Released += uint64(len(entries))
				s.LastSuccess = time.Now().UTC()
				s.ConsecutiveFailures = 0
			})
			d.publishHeld(name)
		}
		if total > 0 {
			log.Printf("[pipeline] %s: released %d held event(s) of %s", name, total, destinations.DescribeSeries(seriesOfEntry(HeldEntry{Event: h.Sample})))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// releaseExpired delivers, unpriced, the held events of a series up to and
// including the last one whose window started more than maxHold ago. Usage
// held longer would reach the billing platform after the invoice period it
// belongs to is final (MaxUnpricedHold); shipped now, the platform can
// re-rate it once Finance adds the price. Held events are in ledger order,
// not window order, so a late adjustment for an old window can sit behind
// younger usage: those younger events ship with it, also unpriced, rather
// than strand it past the deadline. Events after it stay held.
func (d *Dispatcher) releaseExpired(ctx context.Context, dest destinations.Destination, h HeldSummary) error {
	name := dest.Name()
	total := 0
	through, err := d.opts.Held.ExpiredThrough(name, h.Series, time.Now().Add(-d.maxHold()))
	if err != nil {
		return err
	}
	for ctx.Err() == nil && through > 0 {
		entries, _, err := d.opts.Held.Peek(name, h.Series, d.opts.BatchSize)
		if err != nil {
			return err
		}
		n := 0
		for n < len(entries) && entries[n].Seq <= through {
			n++
		}
		if n == 0 {
			break
		}
		take := entries[:n]
		events := make([]usage.Event, n)
		for i := range take {
			events[i] = take[i].Event
		}
		dead, err := d.deliver(ctx, dest, events)
		if err != nil {
			return fmt.Errorf("release expired held events: %w", err)
		}
		last := take[n-1]
		if err := d.opts.Held.Release(name, h.Series, last.Seq, n, last.end); err != nil {
			return fmt.Errorf("release expired held events: %w", err)
		}
		d.shipped.set(name, h.Series, last.Seq)
		total += n
		labels := map[string]string{"destination": name}
		d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(n-dead))
		d.tel.Add("vbilling_events_released_unpriced_total", "Held events released without a price, before their invoice period is final.", labels, float64(n))
		if dead > 0 {
			d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(dead))
		}
		d.update(name, func(s *Status) {
			s.Delivered += uint64(n - dead)
			s.DeadLettered += uint64(dead)
			s.ReleasedUnpriced += uint64(n)
			s.LastSuccess = time.Now().UTC()
		})
		d.publishHeld(name)
		if n < len(entries) {
			break
		}
	}
	if total > 0 {
		log.Printf("[pipeline] WARNING: %s: released %d held event(s) of %s WITHOUT A PRICE: they were held %s, and usage held longer may reach %s "+
			"after its invoice period is final. %s bills them at 0 until Finance adds the price, so it can re-rate them", name, total,
			destinations.DescribeSeries(seriesOfEntry(HeldEntry{Event: h.Sample})), d.maxHold(), name, name)
		if d.opts.Gate != nil {
			d.opts.Gate.ReleasedUnpriced(name, h.Series, &h.Sample, total)
		}
	}
	return ctx.Err()
}

func (d *Dispatcher) publishHeld(name string) {
	n := d.opts.Held.Count(name)
	d.update(name, func(s *Status) { s.Held = uint64(n) })
	d.tel.Set("vbilling_held_events", "Events held back from a destination until their series is priced (PRICE_CHECK=hold).", map[string]string{"destination": name}, float64(n))
}

// gateChanged returns the gate's change channel while a destination has
// held events (nil otherwise, which never fires).
func (d *Dispatcher) gateChanged(name string) <-chan struct{} {
	if d.opts.Gate == nil || d.opts.Held == nil || d.opts.Held.Count(name) == 0 {
		return nil
	}
	return d.opts.Gate.Changed()
}
