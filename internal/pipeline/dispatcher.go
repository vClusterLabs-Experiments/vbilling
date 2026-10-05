// Package pipeline delivers ledger records to every configured destination.
//
// Each destination gets its own goroutine and its own cursor into the
// spool, so a backend that is down or slow only delays itself. Delivery is
// at-least-once; event IDs are deterministic, so backends deduplicate.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Options tune delivery.
type Options struct {
	BatchSize  int           // records read per delivery attempt
	MinBackoff time.Duration // first retry delay
	MaxBackoff time.Duration // retry delay cap
	IdlePoll   time.Duration // fallback poll when no append wakes us
	// Bootstrap, when set, must succeed for a destination before any event
	// is delivered to it (meters and billable metrics must exist first, or
	// some backends store events without attributing them).
	Bootstrap func(context.Context, destinations.Destination) error
	// Tenants, when set, is used to create each tenant in a destination
	// before its first event reaches it.
	Tenants *TenantRegistry
}

func (o *Options) defaults() {
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 5 * time.Minute
	}
	if o.IdlePoll <= 0 {
		o.IdlePoll = 5 * time.Second
	}
}

// Status is a destination's delivery state.
type Status struct {
	Name                string    `json:"name"`
	Ready               bool      `json:"ready"`
	Cursor              uint64    `json:"cursor"`
	Committed           uint64    `json:"committed"`
	LagRecords          uint64    `json:"lag_records"`
	Delivered           uint64    `json:"delivered_events"`
	Skipped             uint64    `json:"skipped_events"`
	DeadLettered        uint64    `json:"dead_lettered_events"`
	LastSuccess         time.Time `json:"last_success,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastErrorAt         time.Time `json:"last_error_at,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	OldestPending       time.Time `json:"oldest_pending,omitempty"`
}

// Dispatcher fans the ledger out to destinations.
type Dispatcher struct {
	spool *spool.Spool
	dests []destinations.Destination
	opts  Options
	tel   *telemetry.Registry

	mu     sync.Mutex
	status map[string]*Status
}

// New builds a dispatcher. tel may be nil.
func New(s *spool.Spool, dests []destinations.Destination, opts Options, tel *telemetry.Registry) *Dispatcher {
	opts.defaults()
	if tel == nil {
		tel = telemetry.New()
	}
	d := &Dispatcher{spool: s, dests: dests, opts: opts, tel: tel, status: map[string]*Status{}}
	for _, dest := range dests {
		d.status[dest.Name()] = &Status{Name: dest.Name()}
	}
	return d
}

// Run delivers until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range d.dests {
		wg.Add(1)
		go func(dest destinations.Destination) {
			defer wg.Done()
			d.loop(ctx, dest)
		}(dest)
	}
	wg.Wait()
}

// Statuses returns a snapshot sorted by name.
func (d *Dispatcher) Statuses() []Status {
	committed := d.spool.Committed()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Status, 0, len(d.status))
	for _, st := range d.status {
		s := *st
		s.Committed = committed
		if committed > s.Cursor {
			s.LagRecords = committed - s.Cursor
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Destinations returns the configured destinations.
func (d *Dispatcher) Destinations() []destinations.Destination { return d.dests }

func (d *Dispatcher) update(name string, fn func(*Status)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.status[name])
}

func (d *Dispatcher) loop(ctx context.Context, dest destinations.Destination) {
	name := dest.Name()
	labels := map[string]string{"destination": name}
	cursor := d.spool.Cursor(name)
	d.update(name, func(s *Status) { s.Cursor = cursor })
	var backoff time.Duration

	if d.opts.Bootstrap != nil {
		for ctx.Err() == nil {
			err := d.opts.Bootstrap(ctx, dest)
			if err == nil {
				break
			}
			log.Printf("[pipeline] %s: bootstrap failed (delivery paused, will retry): %v", name, err)
			d.update(name, func(s *Status) { s.LastError, s.LastErrorAt = "bootstrap: "+err.Error(), time.Now().UTC() })
			backoff = d.sleepBackoff(ctx, backoff)
		}
		backoff = 0
	}
	d.update(name, func(s *Status) { s.Ready = true })

	for ctx.Err() == nil {
		cursor = d.spool.Cursor(name) // re-read so an operator rewind takes effect
		changed := d.spool.Changed()  // taken before Read so no append is missed
		recs, err := d.spool.Read(cursor, d.opts.BatchSize)
		if err != nil {
			log.Printf("[pipeline] %s: read spool: %v", name, err)
			backoff = d.sleepBackoff(ctx, backoff)
			continue
		}
		if len(recs) == 0 {
			d.tel.Set("vbilling_destination_lag_seconds", "Age of the oldest event not yet delivered.", labels, 0)
			select {
			case <-ctx.Done():
				return
			case <-changed:
			case <-time.After(d.opts.IdlePoll):
			}
			continue
		}

		var events []usage.Event
		var skipped uint64
		for _, r := range recs {
			if r.Type != spool.TypeEvent || r.Event == nil {
				continue
			}
			if !destinations.Accepts(dest, r.Event.Metric) {
				skipped++
				continue
			}
			events = append(events, *r.Event)
		}
		if len(events) > 0 {
			oldest := events[0].RecordedAt
			d.update(name, func(s *Status) { s.OldestPending = oldest })
			d.tel.Set("vbilling_destination_lag_seconds", "Age of the oldest event not yet delivered.", labels, time.Since(oldest).Seconds())

			dead, err := d.deliver(ctx, dest, events)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[pipeline] %s: delivery failed (will retry): %v", name, err)
				d.tel.Add("vbilling_delivery_failures_total", "Failed delivery attempts.", labels, 1)
				d.update(name, func(s *Status) {
					s.LastError = err.Error()
					s.LastErrorAt = time.Now().UTC()
					s.ConsecutiveFailures++
				})
				backoff = d.sleepBackoff(ctx, backoff)
				continue
			}
			delivered := uint64(len(events) - dead)
			d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(delivered))
			if dead > 0 {
				d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(dead))
			}
			d.update(name, func(s *Status) {
				s.Delivered += delivered
				s.DeadLettered += uint64(dead)
			})
		}

		last := recs[len(recs)-1].Seq
		if err := d.spool.Ack(name, last); err != nil {
			log.Printf("[pipeline] %s: ack %d: %v", name, last, err)
			backoff = d.sleepBackoff(ctx, backoff)
			continue
		}
		cursor = last
		backoff = 0
		d.update(name, func(s *Status) {
			s.Cursor = cursor
			s.Skipped += skipped
			s.LastSuccess = time.Now().UTC()
			s.ConsecutiveFailures = 0
			s.OldestPending = time.Time{}
		})
		d.tel.Set("vbilling_destination_cursor", "Last ledger sequence acknowledged by a destination.", labels, float64(cursor))
	}
}

// ensureTenants creates any tenant in the batch that this destination has
// not seen yet. Permanent failures are logged and delivery continues (the
// events may then be dead-lettered); anything else is retried.
func (d *Dispatcher) ensureTenants(ctx context.Context, dest destinations.Destination, events []usage.Event) error {
	if d.opts.Tenants == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range events {
		if seen[e.Tenant] {
			continue
		}
		seen[e.Tenant] = true
		t, _ := d.opts.Tenants.Get(e.Tenant)
		if !d.opts.Tenants.NeedsEnsure(dest.Name(), t) {
			continue
		}
		if err := dest.EnsureTenant(ctx, t); err != nil {
			if destinations.IsPermanent(err) {
				log.Printf("[pipeline] %s: tenant %s rejected: %v", dest.Name(), t.ID, err)
				continue
			}
			return fmt.Errorf("ensure tenant %s: %w", t.ID, err)
		}
		d.opts.Tenants.MarkEnsured(dest.Name(), t)
	}
	return nil
}

// deliver sends events, bisecting on permanent errors to isolate the events
// the backend rejects. Isolated events are dead-lettered; it returns how many.
func (d *Dispatcher) deliver(ctx context.Context, dest destinations.Destination, events []usage.Event) (int, error) {
	if err := d.ensureTenants(ctx, dest, events); err != nil {
		return 0, err
	}
	err := dest.SendEvents(ctx, events)
	if err == nil {
		return 0, nil
	}
	var partial *destinations.PartialError
	if errors.As(err, &partial) {
		dead := 0
		for _, ev := range events {
			reason, rejected := partial.Rejected[ev.ID]
			if !rejected {
				continue
			}
			log.Printf("[pipeline] %s: dead-lettering event %s (%s/%s): %s", dest.Name(), ev.ID, ev.Tenant, ev.Metric, reason)
			if derr := d.spool.AddDeadLetter(dest.Name(), ev, reason); derr != nil {
				return 0, derr
			}
			dead++
		}
		return dead, nil
	}
	if !destinations.IsPermanent(err) {
		return 0, err
	}
	if len(events) == 1 {
		log.Printf("[pipeline] %s: dead-lettering event %s (%s/%s): %v",
			dest.Name(), events[0].ID, events[0].Tenant, events[0].Metric, err)
		if derr := d.spool.AddDeadLetter(dest.Name(), events[0], err.Error()); derr != nil {
			return 0, derr
		}
		return 1, nil
	}
	mid := len(events) / 2
	a, err := d.deliver(ctx, dest, events[:mid])
	if err != nil {
		return 0, err
	}
	b, err := d.deliver(ctx, dest, events[mid:])
	if err != nil {
		return 0, err
	}
	return a + b, nil
}

func (d *Dispatcher) sleepBackoff(ctx context.Context, prev time.Duration) time.Duration {
	next := prev * 2
	if next < d.opts.MinBackoff {
		next = d.opts.MinBackoff
	}
	if next > d.opts.MaxBackoff {
		next = d.opts.MaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(next)/5 + 1))
	select {
	case <-ctx.Done():
	case <-time.After(next + jitter):
	}
	return next
}
