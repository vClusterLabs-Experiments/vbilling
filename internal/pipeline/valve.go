package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// The operator's controls over a destination's delivery.
//
// A valve for a deliberate pause. Pause closes it: the loop reads nothing
// from the ledger, releases nothing from the held store and asks nothing of
// the destination. The cursor stays where it is, so Resume continues exactly
// where delivery stopped. The pause is a mark in the ledger's directory and
// survives restarts.
//
// A fence for removal. The ledger keeps the cursor of a destination that is
// taken out of ADAPTERS, and the held store keeps its held events, so adding
// it back would deliver everything recorded meanwhile without anyone asking
// for it. At every start the destinations that have a cursor but are not
// configured are marked detached. A detached destination that is configured
// again and has a backlog (ledger records after its cursor, or held events)
// delivers nothing: it awaits its start until an operator decides with Start
// (deliver the backlog) or Position (start at the head or at a time, and
// move the held events recorded before that to its dead letters, where they
// stay replayable). Nothing moves either way without that decision.

// ErrUnknownDestination is returned for a name that is neither configured
// nor known to the ledger.
var ErrUnknownDestination = errors.New("unknown destination")

// StateError says that an operator's action does not apply to a destination
// in the state it is in.
type StateError struct {
	Destination string
	State       string
	Msg         string
}

func (e *StateError) Error() string {
	return fmt.Sprintf("destination %s is %s: %s", e.Destination, e.State, e.Msg)
}

// ReplayError is a destination's refusal of a rewind it could not
// deduplicate (destinations.ReplayValidator).
type ReplayError struct{ Err error }

func (e *ReplayError) Error() string { return e.Err.Error() }
func (e *ReplayError) Unwrap() error { return e.Err }

// Backlog is what a destination that was removed has not received: what its
// start decision is about.
type Backlog struct {
	// Records is how many ledger records follow its cursor (usage events of
	// every tenant and the commit records between them).
	Records uint64 `json:"records"`
	// Compacted is how many of those are no longer in the retained ledger:
	// retention removed them while the destination was not configured. They
	// cannot be delivered from the ledger (ARCHIVE_DIR keeps a copy).
	Compacted uint64 `json:"compacted_records,omitempty"`
	// HeldEvents is how many events wait for it in the held store (held at
	// the pricing gate or parked for a tenant).
	HeldEvents int `json:"held_events"`
	// FirstWindow and LastWindow bound the windows of the usage in the
	// backlog, read from its oldest and newest records.
	FirstWindow time.Time `json:"first_window,omitzero"`
	LastWindow  time.Time `json:"last_window,omitzero"`
	// Since is when vBilling first started without the destination.
	Since time.Time `json:"since,omitzero"`
}

// waitReport is what a destination's loop last reported while not delivering.
type waitReport struct {
	state    string
	at       time.Time // of the last health report
	pausedAt time.Time // the pause the loop has seen (zero: none)
}

// attach compares the destinations the ledger knows with the configured
// ones, once, when the dispatcher is built: it marks those that are not
// configured as detached, and decides for a detached one that is configured
// again whether it may start (nothing was recorded meanwhile, or an operator
// positioned it) or must await its start.
func (d *Dispatcher) attach() {
	configured := map[string]bool{}
	for _, dest := range d.dests {
		configured[dest.Name()] = true
	}
	committed := d.spool.Committed()
	for _, name := range d.spool.KnownDestinations() {
		cursor, hasCursor := d.spool.CursorOf(name)
		marks := d.spool.DestinationState(name)
		waiting := 0
		if d.opts.Held != nil {
			waiting = d.opts.Held.Total(name)
		}
		var lag uint64
		if committed > cursor {
			lag = committed - cursor
		}
		var err error
		switch {
		case !configured[name]:
			if !hasCursor {
				continue
			}
			var fresh bool
			if fresh, err = d.spool.MarkDetached(name); fresh {
				log.Printf("[pipeline] WARNING: destination %s has a cursor (seq %d of %d) and %d held event(s) but is not in ADAPTERS: it is detached and receives nothing. "+
					"If it is configured again, it delivers nothing recorded meanwhile until an operator decides where it starts (POST /api/v1/destinations/%s/start)",
					name, cursor, committed, waiting, name)
			} else if marks.Detached != nil {
				log.Printf("[pipeline] destination %s is detached since %s: %d ledger record(s) and %d held event(s) recorded for it are not delivered",
					name, marks.Detached.At.Format(time.RFC3339), lag, waiting)
			}
			d.exportState(name, StateDetached)
		case marks.Detached == nil:
			// Configured all along.
		case marks.Detached.Decided:
			log.Printf("[pipeline] destination %s is configured again (detached since %s); an operator positioned it at seq %d meanwhile: it starts there",
				name, marks.Detached.At.Format(time.RFC3339), cursor)
			err = d.spool.ClearDetached(name)
		case lag == 0 && waiting == 0:
			log.Printf("[pipeline] destination %s is configured again (detached since %s); nothing was recorded for it meanwhile: it starts at the ledger head",
				name, marks.Detached.At.Format(time.RFC3339))
			err = d.spool.ClearDetached(name)
		default:
			_, err = d.spool.AwaitStart(name) // the loop reports it, with the numbers
		}
		if err != nil {
			log.Printf("[pipeline] destination %s: saving its marks: %v", name, err)
		}
	}
	for name := range configured {
		d.exportState(name, stateOf(d.spool.DestinationState(name)))
	}
}

// exportState publishes a destination's delivery state as a gauge: 1 for the
// state it is in, 0 for the others.
func (d *Dispatcher) exportState(name, state string) {
	for _, s := range deliveryStates {
		v := 0.0
		if s == state {
			v = 1
		}
		d.tel.Set("vbilling_destination_state", "Delivery state of a destination: 1 for the state it is in (delivering, paused, awaiting_start, detached).",
			map[string]string{"destination": name, "state": s}, v)
	}
}

// delivering notes that a destination's loop delivers (again).
func (d *Dispatcher) delivering(dest destinations.Destination) {
	name := dest.Name()
	d.mu.Lock()
	w, was := d.waiting[name]
	delete(d.waiting, name)
	d.mu.Unlock()
	if !was {
		return
	}
	d.exportState(name, StateDelivering)
	log.Printf("[pipeline] %s: delivering again (it was %s)", name, w.state)
	if d.opts.Health != nil {
		d.opts.Health.OK(health.Name("delivery", name), "delivery", name, 1)
	}
}

// wait is one pass of a destination that is paused or awaits its start: it
// reports the state and how far behind the destination is, and returns when
// an operator changes something or the idle poll passes.
func (d *Dispatcher) wait(ctx context.Context, dest destinations.Destination, state string, marks spool.DestinationState, cursor uint64) {
	name := dest.Name()
	var oldest time.Time
	if recs, err := d.spool.Read(cursor, 1); err == nil && len(recs) > 0 {
		oldest = recs[0].At
	}
	lag := 0.0
	if !oldest.IsZero() {
		lag = time.Since(oldest).Seconds()
	}
	d.update(name, func(s *Status) { s.OldestPending = oldest })
	d.tel.Set("vbilling_destination_lag_seconds", "Age of the oldest event not yet delivered.", map[string]string{"destination": name}, lag)
	if d.opts.Held != nil {
		d.publishHeld(name)
	}
	d.reportWaiting(name, state, marks, cursor)

	idle := time.NewTimer(d.opts.IdlePoll)
	defer idle.Stop()
	select {
	case <-ctx.Done():
	case <-d.wake[name]:
	case <-idle.C:
	}
}

// reportWaiting logs a destination's waiting state when it begins and keeps
// its health source current (at most once a minute: the registry forgets
// sources nobody reports).
func (d *Dispatcher) reportWaiting(name, state string, marks spool.DestinationState, cursor uint64) {
	now := time.Now()
	var pausedAt time.Time
	if marks.Paused != nil {
		pausedAt = marks.Paused.At
	}
	d.mu.Lock()
	w := d.waiting[name]
	entered := w.state != state
	if !entered && now.Sub(w.at) < time.Minute {
		w.pausedAt = pausedAt
		d.waiting[name] = w
		d.mu.Unlock()
		return
	}
	d.waiting[name] = waitReport{state: state, at: now, pausedAt: pausedAt}
	d.mu.Unlock()

	committed := d.spool.Committed()
	b := d.backlog(name, cursor, committed, marks)
	switch state {
	case StateAwaitingStart:
		msg := fmt.Sprintf("%s was removed from ADAPTERS (detached since %s) and is configured again: it has not delivered %d ledger record(s) recorded since%s and %d held event(s)",
			name, b.Since.UTC().Format(time.RFC3339), b.Records, b.windows(), b.HeldEvents)
		if b.Compacted > 0 {
			msg += fmt.Sprintf("; %d of those records were compacted away and cannot be delivered from the ledger", b.Compacted)
		}
		if marks.Paused != nil {
			msg += "; it is also paused"
		}
		msg += ". Nothing is delivered until an operator decides where it starts"
		hint := fmt.Sprintf(`POST /api/v1/destinations/%s/start with {"from":"cursor"} to deliver all of it, {"from":"head"} to skip it (its held events go to its dead letters), `+
			`or {"from":"<RFC 3339 time>"} to start at a time`, name)
		if entered {
			d.exportState(name, state)
			log.Printf("[pipeline] WARNING: %s (hint: %s)", msg, hint)
		}
		if d.opts.Health != nil {
			d.opts.Health.Set(health.Name("delivery", name), "delivery", name, health.Failing, msg, hint)
		}
	case StatePaused:
		p := marks.Paused
		why := ""
		if p.Reason != "" {
			why = " (" + p.Reason + ")"
		}
		msg := fmt.Sprintf("%s is paused by an operator since %s%s: nothing is delivered, its cursor stays at seq %d; %d ledger record(s) and %d held event(s) wait",
			name, p.At.UTC().Format(time.RFC3339), why, cursor, b.Records, b.HeldEvents)
		hint := "POST /api/v1/destinations/" + name + "/resume continues where delivery stopped"
		if entered {
			d.exportState(name, state)
			log.Printf("[pipeline] %s (hint: %s)", msg, hint)
		}
		if d.opts.Health != nil {
			d.opts.Health.Set(health.Name("delivery", name), "delivery", name, health.Paused, msg, hint)
		}
	}
}

// windows renders the backlog's window range for a message.
func (b Backlog) windows() string {
	if b.FirstWindow.IsZero() || b.LastWindow.IsZero() {
		return ""
	}
	return fmt.Sprintf(" (windows %s to %s)", b.FirstWindow.UTC().Format(time.RFC3339), b.LastWindow.UTC().Format(time.RFC3339))
}

// backlog describes what a destination has not received. The window range
// is read from the oldest and the newest batch of the backlog, not from all
// of it, so it costs the same however long the destination was away.
func (d *Dispatcher) backlog(name string, cursor, committed uint64, marks spool.DestinationState) Backlog {
	var b Backlog
	if committed > cursor {
		b.Records = committed - cursor
	}
	if first := d.spool.FirstSeq(); first > cursor+1 {
		b.Compacted = min(first-1-cursor, b.Records)
	}
	if marks.Detached != nil {
		b.Since = marks.Detached.At
	}
	window := func(start, end time.Time) {
		if !start.IsZero() && (b.FirstWindow.IsZero() || start.Before(b.FirstWindow)) {
			b.FirstWindow = start
		}
		if end.After(b.LastWindow) {
			b.LastWindow = end
		}
	}
	if d.opts.Held != nil {
		b.HeldEvents = d.opts.Held.Total(name)
		for _, h := range d.opts.Held.AllSeries(name) {
			window(h.Oldest, h.Sample.WindowEnd)
		}
	}
	read := func(after uint64) {
		recs, err := d.spool.Read(after, d.opts.BatchSize)
		if err != nil {
			return
		}
		for _, r := range recs {
			if r.Event != nil {
				window(r.Event.WindowStart, r.Event.WindowEnd)
			}
		}
	}
	if b.Records > 0 {
		read(cursor)
		if n := uint64(d.opts.BatchSize); b.Records > n {
			read(committed - n)
		}
	}
	return b
}

// row returns one destination's status: a configured one's, or a detached
// one's. ok is false for a name nothing is known about.
func (d *Dispatcher) row(name string) (Status, bool) {
	committed := d.spool.Committed()
	d.mu.Lock()
	st, configured := d.status[name]
	var s Status
	if configured {
		s = *st
	}
	d.mu.Unlock()
	if !configured {
		if _, known := d.spool.CursorOf(name); !known {
			return Status{}, false
		}
		s = Status{Name: name}
	}
	d.fill(&s, committed, configured)
	return s, true
}

// Status returns one destination's status, configured or detached.
func (d *Dispatcher) Status(name string) (Status, bool) { return d.row(name) }

// Detached returns the status of every destination the ledger has a cursor
// for that is not configured, sorted by name.
func (d *Dispatcher) Detached() []Status {
	var out []Status
	for _, name := range d.spool.KnownDestinations() {
		if d.destination(name) != nil {
			continue
		}
		if s, ok := d.row(name); ok {
			out = append(out, s)
		}
	}
	return out
}

// notConfigured is the error for an action that needs the destination's
// adapter.
func (d *Dispatcher) notConfigured(name string) error {
	if _, known := d.spool.CursorOf(name); !known {
		return ErrUnknownDestination
	}
	return &StateError{Destination: name, State: StateDetached, Msg: "it is not in ADAPTERS"}
}

// pauseSettle is how long Pause waits for a batch that is being sent to
// complete before it answers.
const pauseSettle = 10 * time.Second

// Pause stops a destination's delivery until Resume: nothing is read from
// the ledger for it, no held event is released and its cursor stays. A batch
// that is being sent when Pause is called completes, and Pause waits for
// that (up to pauseSettle): stopped reports whether delivery has stopped, so
// that nothing more reaches the destination once Pause has returned. Pausing
// a paused destination keeps the first pause's time.
func (d *Dispatcher) Pause(name, reason string) (st Status, stopped bool, err error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.destination(name) == nil {
		return Status{}, false, d.notConfigured(name)
	}
	pause, err := d.spool.PauseDestination(name, reason)
	if err != nil {
		return Status{}, false, err
	}
	d.kick(name)
	stopped = d.settled(name, pause.At, pauseSettle)
	st, _ = d.row(name)
	return st, stopped, nil
}

// settled waits until a destination's loop has seen the pause of pausedAt
// (from then on it delivers nothing), for at most limit. A destination whose
// loop does not run delivers nothing either.
func (d *Dispatcher) settled(name string, pausedAt time.Time, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		d.mu.Lock()
		w, waiting := d.waiting[name]
		looping := d.looping[name]
		d.mu.Unlock()
		if !looping || (waiting && w.pausedAt.Equal(pausedAt)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Resume lifts a pause: delivery continues at the cursor, so everything
// recorded meanwhile is delivered once. It returns the pause it lifted, nil
// when the destination was not paused.
func (d *Dispatcher) Resume(name string) (Status, *spool.DestinationPause, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.destination(name) == nil {
		return Status{}, nil, d.notConfigured(name)
	}
	was, err := d.spool.ResumeDestination(name)
	if err != nil {
		return Status{}, nil, err
	}
	d.kick(name)
	s, _ := d.row(name)
	return s, was, nil
}

// Start is the decision to deliver a returning destination's backlog: it
// continues at its cursor, and its held events are released as their gates
// allow. It applies only to a destination that awaits its start.
func (d *Dispatcher) Start(name string) (Status, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	if d.destination(name) == nil {
		return Status{}, d.notConfigured(name)
	}
	if state := stateOf(d.spool.DestinationState(name)); state != StateAwaitingStart {
		return Status{}, &StateError{Destination: name, State: state, Msg: "it does not await a start decision"}
	}
	if err := d.spool.ClearDetached(name); err != nil {
		return Status{}, err
	}
	d.kick(name)
	s, _ := d.row(name)
	return s, nil
}

// Positioned is the result of Position.
type Positioned struct {
	Status   Status
	From, To uint64 // the cursor before and after
	// SkippedHeld is how many waiting events (held or parked) were recorded
	// at or before the new position and moved to the dead letters.
	SkippedHeld int
}

// Position moves a destination's cursor to seq.
//
// For a destination that delivers or is paused it is a rewind (a replay,
// which the destination may refuse: ReplayError) or a skip, as before.
//
// For a destination behind the fence it is also the start decision. One that
// awaits its start begins delivering from seq. One that is detached keeps
// the position for when it is configured again; it can only be moved
// forward, because nothing can check a replay for a destination without its
// adapter. In both cases the events waiting for it in the held store that
// were recorded at or before seq are part of what the operator skips: they
// move to its dead letters, where they stay replayable.
//
// start says the caller is the start decision, which applies only to a
// destination that awaits its start.
func (d *Dispatcher) Position(ctx context.Context, name string, seq uint64, start bool) (Positioned, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	dest := d.destination(name)
	cursor, known := d.spool.CursorOf(name)
	if dest == nil && !known {
		return Positioned{}, ErrUnknownDestination
	}
	if seq > d.spool.Committed() {
		seq = d.spool.Committed()
	}
	marks := d.spool.DestinationState(name)
	res := Positioned{To: seq}
	if dest == nil {
		if start {
			return res, d.notConfigured(name)
		}
		if seq < cursor {
			return res, &StateError{Destination: name, State: StateDetached, Msg: fmt.Sprintf("a rewind below its cursor (seq %d) sends it usage again, "+
				"which only its adapter can check: add it to ADAPTERS first (it then awaits its start), and rewind it with the start or cursor call", cursor)}
		}
	} else {
		if !known {
			cursor = d.spool.Cursor(name) // never delivered: it starts at the head
		}
		if state := stateOf(marks); start && state != StateAwaitingStart {
			return res, &StateError{Destination: name, State: state, Msg: "it does not await a start decision"}
		}
		if v, ok := dest.(destinations.ReplayValidator); ok {
			if err := v.ValidateReplay(ctx, cursor, seq); err != nil {
				return res, &ReplayError{Err: err}
			}
		}
	}
	res.From = cursor
	fenced := dest == nil || marks.Detached != nil
	if fenced {
		n, err := d.skipWaiting(name, seq)
		res.SkippedHeld = n
		if err != nil {
			return res, fmt.Errorf("move skipped held events to the dead letters: %w", err)
		}
	}
	if err := d.spool.SetCursor(name, seq); err != nil {
		return res, err
	}
	var err error
	switch {
	case dest == nil:
		err = d.spool.DecideDetached(name)
	case fenced:
		err = d.spool.ClearDetached(name)
	}
	if err != nil {
		return res, err
	}
	d.kick(name)
	res.Status, _ = d.row(name)
	return res, nil
}

// skipWaiting moves the events that wait for a destination in the held
// store and were recorded at or before through to its dead letters: an
// operator starts the destination after them. Nothing is dropped: a dead
// letter can be replayed.
func (d *Dispatcher) skipWaiting(name string, through uint64) (int, error) {
	if d.opts.Held == nil {
		return 0, nil
	}
	total := 0
	for _, h := range d.opts.Held.AllSeries(name) {
		for {
			entries, _, err := d.opts.Held.Peek(name, h.Series, d.opts.BatchSize)
			if err != nil {
				return total, err
			}
			n := 0
			for n < len(entries) && entries[n].Seq <= through {
				n++
			}
			if n == 0 {
				break
			}
			letters := make([]spool.DeadLetter, n)
			for i, e := range entries[:n] {
				letters[i] = spool.DeadLetter{Event: e.Event, Reason: fmt.Sprintf("skipped at start by the operator: recorded before the position %s was started from (ledger seq %d). "+
					"It was waiting because: %s", name, through, e.Reason)}
			}
			if _, err := d.spool.AddDeadLetters(name, letters); err != nil {
				return total, err
			}
			last := entries[n-1]
			if err := d.opts.Held.Release(name, h.Series, last.Seq, n, last.end); err != nil {
				return total, err
			}
			total += n
			if n < len(entries) {
				break
			}
		}
	}
	if total > 0 {
		log.Printf("[pipeline] %s: moved %d held event(s) recorded at or before seq %d to its dead letters: an operator starts it after them",
			name, total, through)
		if d.destination(name) != nil {
			d.publishHeld(name)
		}
	}
	return total, nil
}
