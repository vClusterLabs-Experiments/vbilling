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

// Gate decides whether events may ship to a destination now. The pricing
// service's gate holds the events of series the billing platform would not
// count yet (in every PRICE_CHECK mode but off), and with PRICE_CHECK=hold
// those it has no price for.
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

// item is one event on its way to a destination: where it sits in the ledger
// and the pricing series the gate gave it ("" when no gate decides for it).
type item struct {
	seq    uint64
	series string
	ev     usage.Event
}

func eventsOf(items []item) []usage.Event {
	out := make([]usage.Event, len(items))
	for i := range items {
		out[i] = items[i].ev
	}
	return out
}

func itemsOf(entries []HeldEntry) []item {
	out := make([]item, len(entries))
	for i := range entries {
		out[i] = item{seq: entries[i].Seq, series: entries[i].Series, ev: entries[i].Event}
	}
	return out
}

// shippedLog remembers, per destination and key (a pricing series or a
// ParkKey), the newest ledger sequence a release delivered, so the same
// events read again (a batch retried after part of it was released) are not
// sent twice.
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

// park splits a batch into the events that may go on to the destination and
// the events the pricing gate holds, and writes the held ones durably before
// the cursor can move past them. An event is held when the gate says so, or
// when its series already has held events, so a series always ships in
// ledger order. Whether the events that go on are sent or parked for their
// tenant is decided next (dispatch).
func (d *Dispatcher) park(ctx context.Context, dest destinations.Destination, items []item) ([]item, error) {
	name := dest.Name()
	var decisions []Decision
	if d.opts.Gate != nil {
		decisions = d.opts.Gate.Decide(ctx, name, eventsOf(items))
	}
	now := time.Now().UTC()
	ship := items[:0:0]
	var hold []HeldEntry
	var already uint64
	for i := range items {
		var dec Decision
		if i < len(decisions) {
			dec = decisions[i]
		}
		it := items[i]
		it.series = dec.Series
		if dec.Series == "" {
			ship = append(ship, it)
			continue
		}
		if it.seq <= d.shipped.get(name, dec.Series) {
			already++ // delivered by a release
			continue
		}
		if dec.Hold || d.opts.Held.Has(name, dec.Series) {
			reason := dec.Reason
			if !dec.Hold {
				reason = "queued behind held events of the same series"
			}
			hold = append(hold, HeldEntry{Seq: it.seq, At: now, Series: dec.Series, Reason: reason, Event: it.ev})
			continue
		}
		ship = append(ship, it)
	}
	if len(hold) > 0 {
		// The tenant exists whatever its prices: create it now (a Lago
		// subscription only counts usage from its start, a Metronome contract
		// from its start date), so released usage lands in its period. A
		// tenant that cannot be set up does not keep anything from being
		// held: it is retried on its own, and its events are parked when the
		// gate releases them (dispatch).
		if err := d.setUpHeldTenants(ctx, dest, hold); err != nil {
			return nil, err
		}
		added, err := d.opts.Held.Add(name, hold)
		if err != nil {
			return nil, fmt.Errorf("hold events: %w", err)
		}
		if added > 0 {
			log.Printf("[pipeline] %s: holding %d event(s) (%s): %s", name, added, destinations.DescribeSeries(seriesOfEntry(hold[0])), hold[0].Reason)
			d.tel.Add("vbilling_events_held_total", "Events held back from a destination until their series is priced (PRICE_CHECK=hold) or counted by the billing platform.", map[string]string{"destination": name}, float64(added))
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
// always sets one).
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
			// Sent, or parked for a tenant that is not set up: either way the
			// events are safe before they leave the series' file.
			out, err := d.dispatch(ctx, dest, itemsOf(entries))
			if err != nil {
				return fmt.Errorf("release held events: %w", err)
			}
			last := entries[len(entries)-1].Seq
			if err := d.opts.Held.Release(name, h.Series, last, len(entries), next); err != nil {
				return fmt.Errorf("release held events: %w", err)
			}
			d.shipped.set(name, h.Series, last)
			d.checkpointDelivery(dest, out.sent)
			released := len(entries) - out.parked - out.already
			total += released
			labels := map[string]string{"destination": name}
			d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(out.delivered))
			d.tel.Add("vbilling_events_released_total", "Held events delivered once their series was priced.", labels, float64(released))
			if out.dead > 0 {
				d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(out.dead))
			}
			d.update(name, func(s *Status) {
				s.Delivered += uint64(out.delivered)
				s.DeadLettered += uint64(out.dead)
				s.Released += uint64(released)
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
		out, err := d.dispatch(ctx, dest, itemsOf(take))
		if err != nil {
			return fmt.Errorf("release expired held events: %w", err)
		}
		last := take[n-1]
		if err := d.opts.Held.Release(name, h.Series, last.Seq, n, last.end); err != nil {
			return fmt.Errorf("release expired held events: %w", err)
		}
		d.shipped.set(name, h.Series, last.Seq)
		d.checkpointDelivery(dest, out.sent)
		released := n - out.parked - out.already
		total += released
		labels := map[string]string{"destination": name}
		d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(out.delivered))
		d.tel.Add("vbilling_events_released_unpriced_total", "Held events released without a price, before their invoice period is final.", labels, float64(released))
		if out.dead > 0 {
			d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(out.dead))
		}
		d.update(name, func(s *Status) {
			s.Delivered += uint64(out.delivered)
			s.DeadLettered += uint64(out.dead)
			s.ReleasedUnpriced += uint64(released)
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

// publishHeld exports how many events wait for a destination: held at the
// pricing gate, and parked for tenants that are not set up.
func (d *Dispatcher) publishHeld(name string) {
	held, parked := d.opts.Held.Count(name), d.opts.Held.ParkedCount(name)
	d.update(name, func(s *Status) { s.Held, s.Parked = uint64(held), uint64(parked) })
	labels := map[string]string{"destination": name}
	d.tel.Set("vbilling_held_events", "Events held back from a destination until their series is priced (PRICE_CHECK=hold) or counted by the billing platform.", labels, float64(held))
	d.tel.Set("vbilling_parked_events", "Events parked because their tenant is not set up at the destination; they ship, in order, once it is.", labels, float64(parked))
}

// gateChanged returns the gate's change channel while a destination has
// held events (nil otherwise, which never fires).
func (d *Dispatcher) gateChanged(name string) <-chan struct{} {
	if d.opts.Gate == nil || d.opts.Held == nil || d.opts.Held.Count(name) == 0 {
		return nil
	}
	return d.opts.Gate.Changed()
}
