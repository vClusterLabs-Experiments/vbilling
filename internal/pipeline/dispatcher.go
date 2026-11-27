// Package pipeline delivers ledger records to every configured destination.
//
// Each destination gets its own goroutine and its own cursor into the
// spool, so a backend that is down or slow only delays itself. Delivery is
// at-least-once; event IDs are deterministic, so backends deduplicate.
//
// Within a destination, each tenant is on its own too: a tenant the
// destination cannot set up (EnsureTenant fails) has its events parked and
// retried on its own, while every other tenant's events are delivered and
// the cursor moves on (provision.go).
//
// An operator controls a destination's delivery with two explicit
// mechanisms (valve.go): a pause, and a start decision for a destination
// that was removed from ADAPTERS and is configured again with a backlog.
package pipeline

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
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
	// Held, when set, parks events that must wait: those the Gate holds
	// (unpriced or uncounted), released in order once the Gate allows, and
	// those of a tenant the destination could not set up, released in order
	// once it is. Without a Gate, anything left held (from an earlier run in
	// hold mode) is released at once.
	Held *HeldStore
	Gate Gate
	// MaxHold bounds how long usage is held: once a held event's window
	// started longer ago than this, it is released unpriced (with every
	// older held event of its series), before its invoice period is final.
	// An event parked for its tenant this long is dead-lettered. It is at
	// most MaxUnpricedHold. Zero holds without a limit.
	MaxHold time.Duration
	// Health, when set, reports the destinations that wait for an operator
	// (paused, or awaiting a start decision) and the tenants a destination
	// could not set up.
	Health *health.Registry
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

// Delivery states of a destination.
const (
	// StateDelivering: the destination receives ledger records as they are
	// committed (or retries, when it is down).
	StateDelivering = "delivering"
	// StatePaused: an operator paused it. Nothing is delivered and no held
	// event is released; its cursor stays where it is.
	StatePaused = "paused"
	// StateAwaitingStart: it was removed from ADAPTERS, is configured again
	// and has a backlog. Nothing is delivered until an operator decides
	// where it starts.
	StateAwaitingStart = "awaiting_start"
	// StateDetached: it has a cursor but is not configured.
	StateDetached = "detached"
)

var deliveryStates = []string{StateDelivering, StatePaused, StateAwaitingStart, StateDetached}

// stateOf is the delivery state of a configured destination with these marks.
func stateOf(marks spool.DestinationState) string {
	switch {
	case marks.Detached != nil && marks.Detached.AwaitingStart:
		return StateAwaitingStart
	case marks.Paused != nil:
		return StatePaused
	}
	return StateDelivering
}

// Status is a destination's delivery state.
type Status struct {
	Name string `json:"name"`
	// State is delivering, paused, awaiting_start or, for a destination that
	// has a cursor but is not configured, detached.
	State string `json:"state"`
	// Ready: bootstrapped and delivering (never while paused, awaiting its
	// start or detached).
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
	// Held events wait for a price or for the billing platform to count
	// them; Released counts the held events delivered since the start.
	Held     uint64 `json:"held_events"`
	Released uint64 `json:"released_events,omitempty"`
	// ReleasedUnpriced counts held events released after MaxHold without a price.
	ReleasedUnpriced uint64 `json:"released_unpriced_events,omitempty"`
	// Parked events wait for their tenant to be set up at the destination;
	// they are not counted in Held. BlockedTenants is how many tenants the
	// destination could not set up.
	Parked         uint64 `json:"parked_events"`
	BlockedTenants int    `json:"blocked_tenants"`
	// Paused says since when and why an operator paused the destination.
	Paused *spool.DestinationPause `json:"paused,omitempty"`
	// Start is where the destination started: the ledger sequence its cursor
	// was created at, or was last positioned at by an operator. Nil for a
	// destination an earlier version created (it started "at the beginning").
	Start *spool.DestinationStart `json:"start,omitempty"`
	// Detached says since when the destination is (or, awaiting its start,
	// was) not configured.
	Detached *spool.DestinationDetach `json:"detached,omitempty"`
	// Backlog is what a detached destination, or one awaiting its start, has
	// not received: what a start decision is about.
	Backlog *Backlog `json:"backlog,omitempty"`
}

// Dispatcher fans the ledger out to destinations.
type Dispatcher struct {
	spool *spool.Spool
	dests []destinations.Destination
	opts  Options
	tel   *telemetry.Registry

	mu       sync.Mutex
	status   map[string]*Status
	shipped  shippedLog
	blocks   map[string]map[string]*tenantBlock // destination -> tenant it could not set up
	accepted map[string]time.Time               // destination -> when it last accepted an event or a tenant
	pace     map[string]time.Time               // destination -> no provisioning retry before
	wake     map[string]chan struct{}           // destination -> an operator changed its state
	waiting  map[string]waitReport              // destination -> what the loop last reported while not delivering
	looping  map[string]bool                    // destination -> its loop runs

	provMu map[string]*sync.Mutex // destination -> one EnsureTenant at a time
	opMu   sync.Mutex             // one operator action at a time (valve.go)
}

// New builds a dispatcher. tel may be nil. It marks the destinations that
// have a cursor but are not in dests as detached, and makes a detached
// destination that is in dests again wait for a start decision if it has a
// backlog (see attach).
func New(s *spool.Spool, dests []destinations.Destination, opts Options, tel *telemetry.Registry) *Dispatcher {
	opts.defaults()
	if tel == nil {
		tel = telemetry.New()
	}
	d := &Dispatcher{spool: s, dests: dests, opts: opts, tel: tel, status: map[string]*Status{},
		blocks: map[string]map[string]*tenantBlock{}, accepted: map[string]time.Time{}, pace: map[string]time.Time{},
		wake: map[string]chan struct{}{}, waiting: map[string]waitReport{}, looping: map[string]bool{}, provMu: map[string]*sync.Mutex{}}
	for _, dest := range dests {
		d.status[dest.Name()] = &Status{Name: dest.Name()}
		d.wake[dest.Name()] = make(chan struct{}, 1)
		d.provMu[dest.Name()] = &sync.Mutex{}
	}
	d.attach()
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

// Statuses returns a snapshot of the configured destinations sorted by name
// (see Detached for the others).
func (d *Dispatcher) Statuses() []Status {
	d.mu.Lock()
	out := make([]Status, 0, len(d.status))
	for _, st := range d.status {
		out = append(out, *st)
	}
	d.mu.Unlock()
	committed := d.spool.Committed()
	for i := range out {
		d.fill(&out[i], committed, true)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fill completes a status with what the ledger and the held store say now.
func (d *Dispatcher) fill(s *Status, committed uint64, configured bool) {
	marks := d.spool.DestinationState(s.Name)
	if cur, ok := d.spool.CursorOf(s.Name); ok {
		s.Cursor = cur
	}
	s.Committed = committed
	if committed > s.Cursor {
		s.LagRecords = committed - s.Cursor
	}
	s.State = StateDetached
	if configured {
		s.State = stateOf(marks)
	}
	if s.State != StateDelivering {
		s.Ready = false // bootstrapped or not, it receives nothing
	}
	s.Paused, s.Start, s.Detached = marks.Paused, marks.Start, marks.Detached
	if d.opts.Held != nil {
		s.Held = uint64(d.opts.Held.Count(s.Name))
		s.Parked = uint64(d.opts.Held.ParkedCount(s.Name))
	}
	s.BlockedTenants = d.blockedTenants(s.Name)
	if s.State == StateAwaitingStart || s.State == StateDetached {
		b := d.backlog(s.Name, s.Cursor, committed, marks)
		s.Backlog = &b
	}
}

// Destinations returns the configured destinations.
func (d *Dispatcher) Destinations() []destinations.Destination { return d.dests }

// Held returns the store of events held from destinations, or nil.
func (d *Dispatcher) Held() *HeldStore { return d.opts.Held }

func (d *Dispatcher) update(name string, fn func(*Status)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(d.status[name])
}

func (d *Dispatcher) destination(name string) destinations.Destination {
	for _, dest := range d.dests {
		if dest.Name() == name {
			return dest
		}
	}
	return nil
}

// kick wakes a destination's loop: an operator changed its state or cursor.
func (d *Dispatcher) kick(name string) {
	if ch := d.wake[name]; ch != nil {
		select {
		case ch <- struct{}{}:
		default: // a wake-up is pending already
		}
	}
}

func (d *Dispatcher) loop(ctx context.Context, dest destinations.Destination) {
	name := dest.Name()
	labels := map[string]string{"destination": name}
	d.mu.Lock()
	d.looping[name] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.looping[name] = false
		d.mu.Unlock()
	}()
	var (
		backoff time.Duration
		ready   bool
		cursor  uint64
		started bool
	)
	// retry records a failed attempt and waits before the next one.
	retry := func(err error, counted bool) {
		log.Printf("[pipeline] %s: %v (will retry)", name, err)
		if counted {
			d.tel.Add("vbilling_delivery_failures_total", "Failed delivery attempts.", labels, 1)
		}
		d.update(name, func(s *Status) {
			s.LastError, s.LastErrorAt = err.Error(), time.Now().UTC()
			if counted {
				s.ConsecutiveFailures++
			}
		})
		backoff = d.sleepBackoff(ctx, name, backoff)
	}

	for ctx.Err() == nil {
		// The cursor, its generation and the marks as of one moment: a pause
		// or a positioning that completed before this is never missed, and
		// one that happens after it cannot be undone by this pass (AckAt).
		cur, gen, marks := d.spool.Delivery(name)
		if started && cur < cursor {
			d.shipped.reset(name) // an operator's rewind: a replay sends released events again too
		}
		cursor, started = cur, true
		d.update(name, func(s *Status) { s.Cursor = cursor })

		if state := stateOf(marks); state != StateDelivering {
			// Paused, or awaiting a start decision: nothing is read from the
			// ledger, released from the held store or asked of the
			// destination until an operator says so.
			d.wait(ctx, dest, state, marks, cursor)
			continue
		}
		d.delivering(dest)

		if !ready {
			if d.opts.Bootstrap != nil {
				if err := d.opts.Bootstrap(ctx, dest); err != nil {
					if ctx.Err() != nil {
						return
					}
					log.Printf("[pipeline] %s: bootstrap failed (delivery paused, will retry): %v", name, err)
					d.update(name, func(s *Status) { s.LastError, s.LastErrorAt = "bootstrap: "+err.Error(), time.Now().UTC() })
					backoff = d.sleepBackoff(ctx, name, backoff)
					continue
				}
				backoff = 0
			}
			ready = true
			d.update(name, func(s *Status) { s.Ready = true })
			if d.opts.Held != nil {
				d.publishHeld(name)
			}
		}

		// Tenants first: parked events of tenants that are set up now ship,
		// in ledger order, and one tenant that is not gets its next
		// provisioning attempt. Then held events whose series may ship now.
		err := d.tendTenants(ctx, dest)
		if err == nil && d.opts.Held != nil {
			err = d.release(ctx, dest)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			retry(err, true)
			continue
		}

		changed := d.spool.Changed() // taken before Read so no append is missed
		recs, err := d.spool.Read(cursor, d.opts.BatchSize)
		if err != nil {
			log.Printf("[pipeline] %s: read spool: %v", name, err)
			backoff = d.sleepBackoff(ctx, name, backoff)
			continue
		}
		if len(recs) == 0 {
			d.tel.Set("vbilling_destination_lag_seconds", "Age of the oldest event not yet delivered.", labels, 0)
			idle := time.NewTimer(d.idleFor(name))
			select {
			case <-ctx.Done():
			case <-changed:
			case <-d.gateChanged(name):
			case <-d.wake[name]:
			case <-idle.C:
			}
			idle.Stop()
			continue
		}
		if first := recs[0].Seq; first > cursor+1 {
			// Only a destination that was not a consumer of the ledger (it
			// was detached) can find records gone: say so, never skip silently.
			log.Printf("[pipeline] WARNING: %s: ledger records %d to %d were compacted away before it read them (it was not configured when "+
				"retention removed them); they are not delivered. With ARCHIVE_DIR set they are in the archive", name, cursor+1, first-1)
		}

		var items []item
		var skipped uint64
		for _, r := range recs {
			if r.Type != spool.TypeEvent || r.Event == nil {
				continue
			}
			if !destinations.AcceptsEvent(dest, r.Event) {
				skipped++
				continue
			}
			items = append(items, item{seq: r.Seq, ev: *r.Event})
		}
		if len(items) > 0 && d.opts.Held != nil {
			if items, err = d.park(ctx, dest, items); err != nil {
				if ctx.Err() != nil {
					return
				}
				retry(err, false)
				continue
			}
		}
		var sent []usage.Event
		if len(items) > 0 {
			oldest := items[0].ev.RecordedAt
			d.update(name, func(s *Status) { s.OldestPending = oldest })
			d.tel.Set("vbilling_destination_lag_seconds", "Age of the oldest event not yet delivered.", labels, time.Since(oldest).Seconds())

			out, err := d.dispatch(ctx, dest, items)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				var waits *tenantWaitError
				if errors.As(err, &waits) {
					retry(err, false) // nowhere to park: the batch waits for the tenant, as it always did
				} else {
					log.Printf("[pipeline] %s: delivery failed (will retry): %v", name, err)
					d.tel.Add("vbilling_delivery_failures_total", "Failed delivery attempts.", labels, 1)
					d.update(name, func(s *Status) {
						s.LastError = err.Error()
						s.LastErrorAt = time.Now().UTC()
						s.ConsecutiveFailures++
					})
					backoff = d.sleepBackoff(ctx, name, backoff)
				}
				continue
			}
			sent = out.sent
			d.tel.Add("vbilling_events_delivered_total", "Events accepted by a destination.", labels, float64(out.delivered))
			if out.dead > 0 {
				d.tel.Add("vbilling_events_dead_lettered_total", "Events a destination permanently rejected.", labels, float64(out.dead))
			}
			d.update(name, func(s *Status) {
				s.Delivered += uint64(out.delivered)
				s.DeadLettered += uint64(out.dead)
				s.Skipped += uint64(out.already)
			})
		}

		last := recs[len(recs)-1].Seq
		mine, err := d.spool.AckAt(name, last, gen)
		if err != nil {
			log.Printf("[pipeline] %s: ack %d: %v", name, last, err)
			backoff = d.sleepBackoff(ctx, name, backoff)
			continue
		}
		if !mine {
			// An operator positioned the cursor while this batch was on its
			// way: the next pass reads from where they put it.
			log.Printf("[pipeline] %s: its cursor was positioned while records %d to %d were being delivered; continuing from the new position",
				name, recs[0].Seq, last)
			continue
		}
		d.checkpointDelivery(dest, sent)
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

// Forget confirmations only after the corresponding outbox cursor or held
// release is durable. A cleanup failure can safely retain extra receipts.
func (d *Dispatcher) checkpointDelivery(dest destinations.Destination, events []usage.Event) {
	if len(events) == 0 {
		return
	}
	if checkpoint, ok := dest.(destinations.DeliveryCheckpointer); ok {
		if err := checkpoint.CheckpointDelivery(events); err != nil {
			log.Printf("[pipeline] %s: outbox result is durable but delivery receipt cleanup failed: %v", dest.Name(), err)
		}
	}
}

// send delivers events and returns the ones the destination refuses for
// good, each with its reason. A batch it refuses as a whole is bisected to
// isolate them. Any other error means nothing is known to be refused: the
// caller retries the whole batch (destinations deduplicate on event IDs).
func (d *Dispatcher) send(ctx context.Context, dest destinations.Destination, events []usage.Event) (map[string]string, error) {
	if len(events) == 0 {
		return nil, nil
	}
	err := dest.SendEvents(ctx, events)
	if err == nil {
		return nil, nil
	}
	var partial *destinations.PartialError
	if errors.As(err, &partial) {
		refused := map[string]string{}
		for _, ev := range events {
			if reason, ok := partial.Rejected[ev.ID]; ok {
				refused[ev.ID] = reason
			}
		}
		return refused, nil
	}
	if !destinations.IsPermanent(err) {
		return nil, err
	}
	if len(events) == 1 {
		return map[string]string{events[0].ID: err.Error()}, nil
	}
	mid := len(events) / 2
	a, err := d.send(ctx, dest, events[:mid])
	if err != nil {
		return nil, err
	}
	b, err := d.send(ctx, dest, events[mid:])
	if err != nil {
		return nil, err
	}
	if a == nil {
		return b, nil
	}
	for id, reason := range b {
		a[id] = reason
	}
	return a, nil
}

// deadLetter parks an event a destination refused for operator review.
func (d *Dispatcher) deadLetter(dest string, ev usage.Event, reason string) error {
	log.Printf("[pipeline] %s: dead-lettering event %s (%s/%s): %s", dest, ev.ID, ev.Tenant, ev.Metric, reason)
	return d.spool.AddDeadLetter(dest, ev, reason)
}

// sleepBackoff waits before a retry, or until an operator changes the
// destination's state (a pause must not wait out a five minute backoff).
func (d *Dispatcher) sleepBackoff(ctx context.Context, name string, prev time.Duration) time.Duration {
	next := prev * 2
	if next < d.opts.MinBackoff {
		next = d.opts.MinBackoff
	}
	if next > d.opts.MaxBackoff {
		next = d.opts.MaxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(next)/5 + 1))
	t := time.NewTimer(next + jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-d.wake[name]:
	case <-t.C:
	}
	return next
}
