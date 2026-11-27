package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// site is a data directory over several lives of vBilling, each with its own
// ADAPTERS.
type site struct {
	t      *testing.T
	dir    string
	s      *spool.Spool
	held   *HeldStore
	gate   *skuGate
	tel    *telemetry.Registry
	health *health.Registry
	boots  atomic.Int32
	r      *running
	open   func(dir string) *spool.Spool
}

func newSite(t *testing.T) *site {
	t.Helper()
	return &site{t: t, dir: t.TempDir(), gate: newSKUGate("H100")}
}

// run starts vBilling with these destinations (stopping the previous life).
func (x *site) run(dests ...destinations.Destination) *Dispatcher {
	x.t.Helper()
	x.stop()
	if x.open != nil {
		x.s = x.open(filepath.Join(x.dir, "ledger"))
	} else {
		x.s = openSpool(x.t, filepath.Join(x.dir, "ledger"))
	}
	x.held = openHeld(x.t, filepath.Join(x.dir, "held"))
	x.tel = telemetry.New()
	x.health = health.New(x.tel)
	x.health.SetLogger(func(string, ...any) {})
	// A destination's loop asks for its cursor when it starts; a test that
	// commits usage right after run must not race it to the ledger head.
	for _, dest := range dests {
		x.s.Cursor(dest.Name())
	}
	d := New(x.s, dests, Options{BatchSize: 7, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, IdlePoll: 10 * time.Millisecond,
		Held: x.held, Gate: x.gate, Health: x.health,
		Bootstrap: func(context.Context, destinations.Destination) error { x.boots.Add(1); return nil }}, x.tel)
	ctx, cancel := context.WithCancel(context.Background())
	x.r = &running{d: d, cancel: cancel, done: make(chan struct{})}
	go func() { d.Run(ctx); close(x.r.done) }()
	return d
}

func (x *site) stop() {
	if x.r != nil {
		x.r.stop()
		x.r = nil
		x.s.Close()
	}
}

func (x *site) status(name string) Status {
	x.t.Helper()
	st, ok := x.r.d.Status(name)
	if !ok {
		x.t.Fatalf("nothing is known about destination %s", name)
	}
	return st
}

func (x *site) source(name string) health.Source {
	for _, s := range x.health.List() {
		if s.Name == name {
			return s
		}
	}
	return health.Source{}
}

func (x *site) state(name, state string) float64 {
	return x.tel.Get("vbilling_destination_state", map[string]string{"destination": name, "state": state})
}

func received(d *orderedDest, events []usage.Event) []int {
	d.fakeDest.mu.Lock()
	defer d.fakeDest.mu.Unlock()
	out := make([]int, len(events))
	for i, e := range events {
		out[i] = d.got[e.ID]
	}
	return out
}

// The valve: a paused destination delivers nothing and releases no held
// event, its cursor stays, its lag grows and is reported. Resume continues
// at the cursor: everything recorded meanwhile arrives exactly once.
func TestPausedDestinationDeliversNothingAndResumesAtItsCursor(t *testing.T) {
	x := newSite(t)
	dest, other := newOrdered("metronome"), newOrdered("webhook")
	d := x.run(dest, other)
	defer x.stop()
	first := fillSKUs(t, x.s, 3, "H100", "B200") // B200 has no price: held
	waitFor(t, "delivered before the pause", func() bool {
		return all(received(dest, first["H100"]), 1) && x.held.Count("metronome") == 3 && x.s.Cursor("metronome") == x.s.Committed()
	})
	cursor := x.s.Cursor("metronome")

	// When Pause returns, delivery has stopped: nothing recorded from here on
	// reaches the destination.
	st, stopped, err := d.Pause("metronome", "sandbox cleanup")
	if err != nil || !stopped || st.State != StatePaused || st.Paused == nil || st.Paused.Reason != "sandbox cleanup" || st.Cursor != cursor {
		t.Fatalf("pause: %+v stopped %v %v", st, stopped, err)
	}
	during := fillSKUs(t, x.s, 4, "H100", "B200")
	x.gate.price("B200") // held events may ship now: not while paused
	waitFor(t, "the other destination is not affected", func() bool { return all(received(other, during["H100"]), 1) })
	waitFor(t, "the pause is reported", func() bool {
		return x.state("metronome", StatePaused) == 1 && x.state("metronome", StateDelivering) == 0 &&
			x.tel.Get("vbilling_destination_lag_seconds", map[string]string{"destination": "metronome"}) > 0 &&
			x.source("delivery/metronome").Status == health.Paused
	})
	time.Sleep(40 * time.Millisecond)
	if !all(received(dest, during["H100"]), 0) || !all(received(dest, first["B200"]), 0) {
		t.Fatalf("a paused destination received events: new %v, held %v", received(dest, during["H100"]), received(dest, first["B200"]))
	}
	st = x.status("metronome")
	if st.Cursor != cursor || st.LagRecords != x.s.Committed()-cursor || st.LagRecords == 0 || st.Held != 3 || st.State != StatePaused {
		t.Fatalf("status while paused: %+v", st)
	}
	if src := x.source("delivery/metronome"); !strings.Contains(src.Message, "sandbox cleanup") || !strings.Contains(src.Hint, "/resume") {
		t.Fatalf("health source while paused: %+v", src)
	}
	if st, _, err := d.Pause("metronome", ""); err != nil || st.Paused.Reason != "sandbox cleanup" {
		t.Fatalf("pausing again must keep the first pause: %+v %v", st, err)
	}

	st, was, err := d.Resume("metronome")
	if err != nil || was == nil || was.Reason != "sandbox cleanup" || st.State != StateDelivering {
		t.Fatalf("resume: %+v %+v %v", st, was, err)
	}
	everything := append(append(append(first["H100"], first["B200"]...), during["H100"]...), during["B200"]...)
	waitFor(t, "caught up after the resume", func() bool {
		return all(received(dest, everything), 1) && x.s.Cursor("metronome") == x.s.Committed() && x.held.Count("metronome") == 0
	})
	waitFor(t, "delivering again is reported", func() bool {
		return x.state("metronome", StateDelivering) == 1 && x.state("metronome", StatePaused) == 0 && x.source("delivery/metronome").Status == health.OK
	})
	if _, was, err := d.Resume("metronome"); err != nil || was != nil {
		t.Fatalf("resuming a destination that is not paused: %+v %v", was, err)
	}
}

// The pause is on disk: after a restart the destination is still paused, and
// nothing is asked of it, not even its bootstrap.
func TestPauseSurvivesARestart(t *testing.T) {
	x := newSite(t)
	dest := newOrdered("metronome")
	d := x.run(dest)
	defer x.stop()
	if _, stopped, err := d.Pause("metronome", "migration"); err != nil || !stopped {
		t.Fatalf("pause: stopped %v, %v", stopped, err)
	}
	evs := fillSKUs(t, x.s, 3, "H100")

	boots := x.boots.Load()
	d = x.run(dest)
	more := fillSKUs(t, x.s, 2, "H100")
	waitFor(t, "still paused after the restart", func() bool { return x.state("metronome", StatePaused) == 1 })
	time.Sleep(40 * time.Millisecond)
	if st := x.status("metronome"); st.State != StatePaused || st.Paused.Reason != "migration" || st.Ready || st.LagRecords == 0 {
		t.Fatalf("status after the restart: %+v", st)
	}
	if dest.received() != 0 || x.boots.Load() != boots {
		t.Fatalf("a paused destination was used after a restart: %d events, %d bootstraps", dest.received(), x.boots.Load()-boots)
	}
	if _, _, err := d.Resume("metronome"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delivered after the resume", func() bool {
		return all(received(dest, append(evs["H100"], more["H100"]...)), 1) && x.s.Cursor("metronome") == x.s.Committed()
	})
	if x.boots.Load() != boots+1 {
		t.Fatalf("bootstrapped %d times after the resume, want once", x.boots.Load()-boots)
	}
}

// An operator can position a paused destination: nothing moves until the
// resume, which then delivers from the new position.
func TestRewindWhilePaused(t *testing.T) {
	x := newSite(t)
	dest := newOrdered("lago")
	d := x.run(dest)
	defer x.stop()
	evs := fillSKUs(t, x.s, 4, "H100")
	waitFor(t, "delivered", func() bool { return all(received(dest, evs["H100"]), 1) && x.s.Cursor("lago") == x.s.Committed() })
	if _, stopped, err := d.Pause("lago", ""); err != nil || !stopped {
		t.Fatalf("pause: stopped %v, %v", stopped, err)
	}
	res, err := d.Position(context.Background(), "lago", 0, false)
	if err != nil || res.Status.Cursor != 0 || res.Status.State != StatePaused || res.SkippedHeld != 0 {
		t.Fatalf("position while paused: %+v %v", res, err)
	}
	time.Sleep(40 * time.Millisecond)
	if !all(received(dest, evs["H100"]), 1) || x.s.Cursor("lago") != 0 {
		t.Fatalf("a rewind while paused delivered: %v, cursor %d", received(dest, evs["H100"]), x.s.Cursor("lago"))
	}
	if _, _, err := d.Resume("lago"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "replayed from the new position", func() bool { return all(received(dest, evs["H100"]), 2) && x.s.Cursor("lago") == x.s.Committed() })
}

// awaiting brings a site to the state the validation found: a destination that
// delivered, was removed from ADAPTERS while the ledger went on (with held
// events of its own left behind), and is configured again.
func awaiting(t *testing.T) (x *site, dest *orderedDest, before, held, backlog map[string][]usage.Event) {
	t.Helper()
	x = newSite(t)
	dest = newOrdered("metronome")
	x.run(dest, newOrdered("noop"))
	before = fillSKUs(t, x.s, 3, "H100", "B200") // B200 unpriced: held for metronome
	waitFor(t, "first life", func() bool {
		return all(received(dest, before["H100"]), 1) && x.held.Count("metronome") == 3 && x.s.Cursor("metronome") == x.s.Committed()
	})
	held = map[string][]usage.Event{"B200": before["B200"]}

	x.run(newOrdered("noop")) // metronome removed from ADAPTERS
	backlog = fillSKUs(t, x.s, 5, "H100", "B200")
	waitFor(t, "the ledger goes on without it", func() bool { return x.s.Cursor("noop") == x.s.Committed() })
	if st := x.status("metronome"); st.State != StateDetached || st.Detached == nil || st.LagRecords == 0 || st.Held != 3 || st.Backlog == nil || st.Backlog.HeldEvents != 3 {
		t.Fatalf("a removed destination must be listed as detached, with its backlog: %+v", st)
	}
	if x.state("metronome", StateDetached) != 1 {
		t.Fatal("no state gauge for the detached destination")
	}

	x.gate = newSKUGate("H100", "B200") // everything is priced when it comes back
	x.run(dest, newOrdered("noop"))
	return x, dest, before, held, backlog
}

// The fence. A destination that was removed and is configured again
// does not deliver what was recorded meanwhile, nor its held events, until
// an operator decides. It says so loudly, and a restart changes nothing.
func TestReturningDestinationAwaitsItsStart(t *testing.T) {
	x, dest, before, held, backlog := awaiting(t)
	defer x.stop()
	waitFor(t, "awaiting its start", func() bool {
		return x.state("metronome", StateAwaitingStart) == 1 && x.source("delivery/metronome").Status == health.Failing
	})
	time.Sleep(40 * time.Millisecond)
	if !all(received(dest, backlog["H100"]), 0) || !all(received(dest, held["B200"]), 0) || !all(received(dest, before["H100"]), 1) {
		t.Fatalf("a returning destination delivered without a decision: backlog %v, held %v", received(dest, backlog["H100"]), received(dest, held["B200"]))
	}
	st := x.status("metronome")
	b := st.Backlog
	if st.State != StateAwaitingStart || b == nil || b.Records != st.LagRecords || b.Records != 15 || b.HeldEvents != 3 || b.Since.IsZero() ||
		!b.FirstWindow.Equal(held["B200"][0].WindowStart) || !b.LastWindow.Equal(backlog["H100"][4].WindowEnd) {
		t.Fatalf("status while awaiting: %+v backlog %+v", st, b)
	}
	src := x.source("delivery/metronome")
	for _, want := range []string{"15 ledger record(s)", "3 held event(s)", "detached since", "windows " + b.FirstWindow.Format(time.RFC3339), `{"from":"head"}`} {
		if !strings.Contains(src.Message+" "+src.Hint, want) {
			t.Fatalf("the health source does not say %q: %+v", want, src)
		}
	}

	// A restart while awaiting: still awaiting, still nothing delivered.
	x.run(dest, newOrdered("noop"))
	waitFor(t, "awaiting after a restart", func() bool { return x.state("metronome", StateAwaitingStart) == 1 })
	time.Sleep(40 * time.Millisecond)
	if !all(received(dest, backlog["H100"]), 0) || !all(received(dest, held["B200"]), 0) {
		t.Fatal("a restart made a returning destination deliver its backlog")
	}
	// A pause and a resume are no decision.
	if _, _, err := x.r.d.Pause("metronome", ""); err != nil {
		t.Fatal(err)
	}
	if st, _, err := x.r.d.Resume("metronome"); err != nil || st.State != StateAwaitingStart {
		t.Fatalf("resume of an awaiting destination: %+v %v", st, err)
	}

	// The decision "from cursor": everything recorded while it was removed
	// is delivered, held events included, once.
	st, err := x.r.d.Start("metronome")
	if err != nil || st.State != StateDelivering || st.Detached != nil {
		t.Fatalf("start: %+v %v", st, err)
	}
	everything := append(append(append(before["H100"], held["B200"]...), backlog["H100"]...), backlog["B200"]...)
	waitFor(t, "backlog delivered", func() bool {
		return all(received(dest, everything), 1) && x.s.Cursor("metronome") == x.s.Committed() && x.held.Total("metronome") == 0
	})
	waitFor(t, "delivering", func() bool {
		return x.state("metronome", StateDelivering) == 1 && x.state("metronome", StateAwaitingStart) == 0 && x.source("delivery/metronome").Status == health.OK
	})
	if _, err := x.r.d.Start("metronome"); err == nil {
		t.Fatal("a destination that delivers has no start decision to take")
	}
	// And it stays decided over a restart.
	x.run(dest, newOrdered("noop"))
	if st := x.status("metronome"); st.State != StateDelivering {
		t.Fatalf("after the decision and a restart: %+v", st)
	}
}

// The decision "from head" skips the backlog. The held events are not
// dropped: they move to the dead letters, with the reason, and can be
// replayed. The pricing report no longer finds held events for it.
func TestStartFromTheHeadSkipsTheBacklogAndDeadLettersItsHeldEvents(t *testing.T) {
	x, dest, _, held, backlog := awaiting(t)
	defer x.stop()
	waitFor(t, "awaiting its start", func() bool { return x.state("metronome", StateAwaitingStart) == 1 })
	head := x.s.Committed()
	res, err := x.r.d.Position(context.Background(), "metronome", head, true)
	if err != nil || res.SkippedHeld != 3 || res.To != head || res.Status.State != StateDelivering || res.Status.Cursor != head {
		t.Fatalf("start from the head: %+v %v", res, err)
	}
	after := fillSKUs(t, x.s, 2, "H100", "B200")
	waitFor(t, "new usage delivered", func() bool {
		return all(received(dest, after["H100"]), 1) && all(received(dest, after["B200"]), 1) && x.s.Cursor("metronome") == x.s.Committed()
	})
	if !all(received(dest, backlog["H100"]), 0) || !all(received(dest, backlog["B200"]), 0) || !all(received(dest, held["B200"]), 0) {
		t.Fatalf("the skipped backlog was delivered: %v %v, held %v", received(dest, backlog["H100"]), received(dest, backlog["B200"]), received(dest, held["B200"]))
	}
	dls, _ := x.s.DeadLetters("metronome")
	if len(dls) != 3 {
		t.Fatalf("%d dead letters, want the 3 held events", len(dls))
	}
	for i, dl := range dls {
		if dl.Event.ID != held["B200"][i].ID || !strings.Contains(dl.Reason, "skipped at start by the operator") || !strings.Contains(dl.Reason, "unpriced") {
			t.Fatalf("dead letter %d: %+v", i, dl)
		}
	}
	// The pricing report lists the destinations that have held events (its
	// row "held events for a destination that no longer checks prices"): not
	// this one any more.
	if x.held.Total("metronome") != 0 || strings.Contains(strings.Join(x.held.Destinations(), ","), "metronome") {
		t.Fatalf("held events left behind: %d, destinations %v", x.held.Total("metronome"), x.held.Destinations())
	}
	if st := x.status("metronome"); st.Start == nil || st.Start.Seq != head || st.Start.How != spool.StartPositioned {
		t.Fatalf("the decision is where the destination starts: %+v", st.Start)
	}
	// Replayable.
	events := make([]usage.Event, len(dls))
	for i := range dls {
		events[i] = dls[i].Event
	}
	if r, err := x.r.d.Replay(context.Background(), "metronome", events); err != nil || len(r.Refused) != 0 || !all(received(dest, held["B200"]), 1) {
		t.Fatalf("replay of the skipped held events: %+v %v", r, err)
	}
}

// A detached destination can be positioned before it is configured again:
// that is the decision. It can only move forward (nothing can check a
// replay without its adapter), and the held events it skips go to its dead
// letters.
func TestDetachedDestinationCanBePositionedBeforeItComesBack(t *testing.T) {
	x := newSite(t)
	dest := newOrdered("metronome")
	x.run(dest, newOrdered("noop"))
	defer x.stop()
	before := fillSKUs(t, x.s, 3, "H100", "B200")
	waitFor(t, "first life", func() bool { return x.held.Count("metronome") == 3 && x.s.Cursor("metronome") == x.s.Committed() })
	old := x.s.Cursor("metronome")

	d := x.run(newOrdered("noop"))
	skipped := fillSKUs(t, x.s, 4, "H100")
	if _, _, err := d.Pause("metronome", ""); err == nil {
		t.Fatal("a destination that is not configured cannot be paused")
	}
	if _, err := d.Start("metronome"); err == nil {
		t.Fatal("a destination that is not configured cannot be started")
	}
	var state *StateError
	if _, err := d.Position(context.Background(), "metronome", old-1, false); !errors.As(err, &state) {
		t.Fatalf("a rewind of a detached destination must be refused, got %v", err)
	}
	if _, err := d.Position(context.Background(), "nothing", 0, false); !errors.Is(err, ErrUnknownDestination) {
		t.Fatalf("positioning an unknown name: %v", err)
	}
	head := x.s.Committed()
	res, err := d.Position(context.Background(), "metronome", head, false)
	if err != nil || res.From != old || res.To != head || res.SkippedHeld != 3 || res.Status.State != StateDetached || res.Status.Cursor != head {
		t.Fatalf("position a detached destination: %+v %v", res, err)
	}
	if dls, _ := x.s.DeadLetters("metronome"); len(dls) != 3 || x.held.Total("metronome") != 0 {
		t.Fatalf("held events of the skipped range: %d dead letters, %d still held", len(dls), x.held.Total("metronome"))
	}

	x.gate = newSKUGate("H100", "B200")
	x.run(dest, newOrdered("noop"))
	after := fillSKUs(t, x.s, 2, "H100")
	waitFor(t, "delivers from the position", func() bool {
		return all(received(dest, after["H100"]), 1) && x.s.Cursor("metronome") == x.s.Committed()
	})
	if st := x.status("metronome"); st.State != StateDelivering || st.Detached != nil {
		t.Fatalf("a positioned destination must not await its start: %+v", st)
	}
	if !all(received(dest, skipped["H100"]), 0) || !all(received(dest, before["B200"]), 0) {
		t.Fatal("a destination positioned at the head delivered what was recorded before")
	}
}

// With nothing recorded while it was away, a returning destination simply
// starts. And a destination never seen before starts at the ledger head, as
// it always did, now with its start recorded.
func TestDestinationsWithNothingToDecideJustStart(t *testing.T) {
	x := newSite(t)
	dest := newOrdered("metronome")
	x.run(newOrdered("noop"))
	defer x.stop()
	history := fillSKUs(t, x.s, 3, "H100")
	waitFor(t, "history", func() bool { return x.s.Cursor("noop") == x.s.Committed() })
	head := x.s.Committed()

	x.run(dest, newOrdered("noop")) // first time
	waitFor(t, "its cursor exists", func() bool { _, ok := x.s.CursorOf("metronome"); return ok })
	evs := fillSKUs(t, x.s, 2, "H100")
	waitFor(t, "a new destination delivers what is new", func() bool { return all(received(dest, evs["H100"]), 1) })
	if !all(received(dest, history["H100"]), 0) {
		t.Fatal("a destination never seen before received history")
	}
	if st := x.status("metronome"); st.State != StateDelivering || st.Start == nil || st.Start.Seq != head || st.Start.How != spool.StartCreated {
		t.Fatalf("a new destination starts at the head, recorded: %+v", st)
	}
	waitFor(t, "caught up", func() bool { return x.s.Cursor("metronome") == x.s.Committed() })

	x.run(newOrdered("noop")) // removed, and nothing recorded meanwhile
	x.run(dest, newOrdered("noop"))
	if st := x.status("metronome"); st.State != StateDelivering || st.Detached != nil {
		t.Fatalf("with no backlog it just starts and the mark is cleared: %+v", st)
	}
	more := fillSKUs(t, x.s, 1, "H100")
	waitFor(t, "delivering", func() bool { return all(received(dest, more["H100"]), 1) })
}

// While a destination is detached, retention removes ledger segments it
// never read. When it comes back, the decision knows: the backlog says how
// many records are gone, and "from cursor" delivers what is left.
func TestReturningDestinationWhoseBacklogWasCompactedAway(t *testing.T) {
	x := newSite(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	x.open = func(dir string) *spool.Spool {
		s, err := spool.Open(dir, spool.Options{NoSync: true, SegmentMaxRecords: 4, Retention: time.Hour, Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	dest := newOrdered("metronome")
	x.run(dest, newOrdered("noop"))
	defer x.stop()
	first := fillSKUs(t, x.s, 2, "H100")
	waitFor(t, "first life", func() bool {
		return all(received(dest, first["H100"]), 1) && x.s.Cursor("metronome") == x.s.Committed()
	})
	cursor := x.s.Cursor("metronome")

	x.run(newOrdered("noop"))
	gone := fillSKUs(t, x.s, 6, "H100")
	waitFor(t, "the ledger goes on", func() bool { return x.s.Cursor("noop") == x.s.Committed() })
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	left := fillSKUs(t, x.s, 2, "H100")
	waitFor(t, "the ledger goes on", func() bool { return x.s.Cursor("noop") == x.s.Committed() })
	if n, err := x.s.Compact([]string{"noop"}); err != nil || n == 0 {
		t.Fatalf("compaction removed %d segments (%v): the detached destination's cursor must not hold it back", n, err)
	}
	firstLeft := x.s.FirstSeq()
	if firstLeft <= cursor+1 {
		t.Fatalf("nothing of the backlog was compacted (first retained %d, cursor %d)", firstLeft, cursor)
	}

	x.run(dest, newOrdered("noop"))
	waitFor(t, "awaiting", func() bool {
		return x.state("metronome", StateAwaitingStart) == 1 && x.source("delivery/metronome").Status == health.Failing
	})
	st := x.status("metronome")
	if st.Backlog == nil || st.Backlog.Compacted != firstLeft-1-cursor || st.Backlog.Records != x.s.Committed()-cursor {
		t.Fatalf("backlog %+v, want %d compacted of %d", st.Backlog, firstLeft-1-cursor, x.s.Committed()-cursor)
	}
	if src := x.source("delivery/metronome"); !strings.Contains(src.Message, "compacted away") {
		t.Fatalf("the health source does not say that records are gone: %q", src.Message)
	}
	if _, err := x.r.d.Start("metronome"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "what is left is delivered", func() bool { return all(received(dest, left["H100"]), 1) && x.s.Cursor("metronome") == x.s.Committed() })
	delivered := 0
	for _, n := range received(dest, gone["H100"]) {
		delivered += n
	}
	if want := int(x.s.Committed()-firstLeft+1)/2 - len(left["H100"]); delivered != want {
		t.Fatalf("%d of the older events delivered, want the %d still in the ledger", delivered, want)
	}
}

// rewindingDest rewinds its own cursor while its first batch is being sent,
// as an operator's cursor call can.
type rewindingDest struct {
	*orderedDest
	s    *spool.Spool
	once sync.Once
}

func (d *rewindingDest) SendEvents(ctx context.Context, evs []usage.Event) error {
	err := d.orderedDest.SendEvents(ctx, evs)
	d.once.Do(func() { d.s.SetCursor("lago", 0) })
	return err
}

// An operator's positioning while a batch is on its way must stand: the
// batch's acknowledgement used to move the cursor past the batch again, and
// the rewind was lost.
func TestCursorPositionedDuringADeliveryIsNotUndone(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := &rewindingDest{orderedDest: newOrdered("lago"), s: s}
	s.Cursor("lago")
	evs := fill(t, s, 2, 2, usage.MetricGPUHours)
	r := start(s, Options{BatchSize: 100}, dest)
	defer r.stop()
	waitFor(t, "the rewind replayed everything", func() bool {
		return all(received(dest.orderedDest, evs), 2) && s.Cursor("lago") == s.Committed()
	})
}
