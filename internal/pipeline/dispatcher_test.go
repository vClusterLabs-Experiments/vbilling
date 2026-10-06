package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type fakeDest struct {
	ensured   []string
	ensureErr int    // fail EnsureTenant this many times
	reject    string // tenant EnsureTenant rejects permanently
	partial   bool   // report poison events with PartialError instead of failing the batch
	name      string
	mu        sync.Mutex
	got       map[string]int // event ID -> times received
	failN     int            // fail the first N calls with a retryable error
	calls     int
	poison    map[string]bool // event IDs rejected permanently
	billable  bool
}

func newFake(name string) *fakeDest {
	return &fakeDest{name: name, got: map[string]int{}, poison: map[string]bool{}}
}

func (f *fakeDest) Name() string                                       { return f.name }
func (f *fakeDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (f *fakeDest) EnsureTenant(_ context.Context, t usage.Tenant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ensureErr > 0 {
		f.ensureErr--
		return errors.New("billing API timeout")
	}
	if t.ID == f.reject {
		return destinations.Permanent(fmt.Errorf("plan for %s is misconfigured", t.ID))
	}
	f.ensured = append(f.ensured, t.ID)
	return nil
}
func (f *fakeDest) RemoveTenant(context.Context, usage.Tenant) error { return nil }
func (f *fakeDest) Accepts(metric string) bool {
	return !f.billable || destinations.BillableOnly(metric)
}
func (f *fakeDest) SendEvents(_ context.Context, evs []usage.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failN {
		return errors.New("503 service unavailable")
	}
	if f.partial {
		rejected := map[string]string{}
		for _, e := range evs {
			if f.poison[e.ID] {
				rejected[e.ID] = "invalid customer"
				continue
			}
			f.got[e.ID]++
		}
		if len(rejected) > 0 {
			return &destinations.PartialError{Rejected: rejected}
		}
		return nil
	}
	for _, e := range evs {
		if f.poison[e.ID] {
			return destinations.Permanent(fmt.Errorf("customer for %s not found", e.Tenant))
		}
	}
	for _, e := range evs {
		f.got[e.ID]++
	}
	return nil
}

func (f *fakeDest) received() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

func fill(t *testing.T, s *spool.Spool, windows, perWindow int, metric string) []usage.Event {
	t.Helper()
	start := t0
	if wm, ok := s.Watermark(spool.SourceCollector); ok {
		start = wm // continue after whatever is already committed
	}
	var all []usage.Event
	for w := 0; w < windows; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		var evs []usage.Event
		for i := 0; i < perWindow; i++ {
			e := usage.Event{Tenant: fmt.Sprintf("t%d", i), Metric: metric, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: time.Now()}
			e.Finalize()
			evs = append(evs, e)
		}
		if err := s.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	return all
}

func run(t *testing.T, s *spool.Spool, until func() bool, dests ...destinations.Destination) *Dispatcher {
	t.Helper()
	d := New(s, dests, Options{BatchSize: 7, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, IdlePoll: 10 * time.Millisecond}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	for !until() {
		if ctx.Err() != nil {
			t.Fatal("timed out waiting for delivery")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	return d
}

func openSpool(t *testing.T, dir string) *spool.Spool {
	t.Helper()
	s, err := spool.Open(dir, spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeliversEverythingAndAcks(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("stripe")
	s.Cursor("stripe") // register before data so it starts at seq 0
	all := fill(t, s, 5, 4, usage.MetricGPUHours)

	run(t, s, func() bool { return dest.received() == len(all) }, dest)
	for _, e := range all {
		if dest.got[e.ID] != 1 {
			t.Fatalf("event %s received %d times", e.ID, dest.got[e.ID])
		}
	}
	if s.Cursor("stripe") != s.Committed() {
		t.Fatalf("cursor %d != committed %d", s.Cursor("stripe"), s.Committed())
	}
}

func TestRetriesTransientFailuresWithoutLoss(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("metronome")
	dest.failN = 4
	s.Cursor("metronome")
	all := fill(t, s, 3, 3, usage.MetricGPUHours)

	d := run(t, s, func() bool { return dest.received() == len(all) }, dest)
	st := d.Statuses()[0]
	if st.DeadLettered != 0 || st.Delivered != uint64(len(all)) {
		t.Fatalf("status after retries: %+v", st)
	}
}

func TestPoisonEventIsIsolatedAndDeadLettered(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("stripe")
	s.Cursor("stripe")
	all := fill(t, s, 2, 5, usage.MetricGPUHours)
	bad := all[3]
	dest.poison[bad.ID] = true

	d := run(t, s, func() bool { return dest.received() == len(all)-1 && s.Cursor("stripe") == s.Committed() }, dest)
	if dest.got[bad.ID] != 0 {
		t.Fatal("poison event delivered")
	}
	dls, _ := s.DeadLetters("stripe")
	if len(dls) != 1 || dls[0].Event.ID != bad.ID {
		t.Fatalf("dead letters: %+v", dls)
	}
	if st := d.Statuses()[0]; st.DeadLettered != 1 {
		t.Fatalf("status: %+v", st)
	}
}

func TestOneDestinationDownDoesNotBlockAnother(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	up, down := newFake("webhook"), newFake("metronome")
	down.failN = 1 << 30
	s.Cursor("webhook")
	s.Cursor("metronome")
	all := fill(t, s, 4, 2, usage.MetricCPUCoreHours)

	run(t, s, func() bool { return up.received() == len(all) }, up, down)
	if down.received() != 0 {
		t.Fatal("failing destination should have received nothing")
	}
	if s.Cursor("metronome") != 0 {
		t.Fatalf("failing destination cursor advanced to %d", s.Cursor("metronome"))
	}
}

func TestFilterSkipsInformationalMetrics(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("stripe")
	dest.billable = true
	s.Cursor("stripe")
	fill(t, s, 2, 2, usage.MetricGPUUtilization) // informational
	billable := fill(t, s, 1, 2, usage.MetricGPUHours)

	run(t, s, func() bool { return s.Cursor("stripe") == s.Committed() }, dest)
	if dest.received() != len(billable) {
		t.Fatalf("received %d events, want only the %d billable ones", dest.received(), len(billable))
	}
}

func TestResumesFromCursorAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, dir)
	first := newFake("lago")
	s.Cursor("lago")
	batch1 := fill(t, s, 2, 2, usage.MetricGPUHours)
	run(t, s, func() bool { return first.received() == len(batch1) }, first)
	s.Close()

	s = openSpool(t, dir)
	defer s.Close()
	second := newFake("lago")
	ws := t0.Add(10 * time.Minute)
	e := usage.Event{Tenant: "t0", Metric: usage.MetricGPUHours, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: time.Now()}
	e.Finalize()
	if err := s.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), []usage.Event{e}, ""); err != nil {
		t.Fatal(err)
	}
	run(t, s, func() bool { return second.received() == 1 }, second)
	if second.got[e.ID] != 1 {
		t.Fatal("after restart only the new event should be delivered")
	}
}

func TestPartialErrorDeadLettersOnlyRejectedEvents(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("metronome")
	dest.partial = true
	s.Cursor("metronome")
	all := fill(t, s, 1, 6, usage.MetricGPUHours)
	dest.poison[all[1].ID] = true
	dest.poison[all[4].ID] = true

	d := run(t, s, func() bool { return s.Cursor("metronome") == s.Committed() }, dest)
	if dest.received() != 4 {
		t.Fatalf("received %d, want 4", dest.received())
	}
	if dest.calls != 1 {
		t.Fatalf("partial rejection should not trigger resends, got %d calls", dest.calls)
	}
	if st := d.Statuses()[0]; st.DeadLettered != 2 || st.Delivered != 4 {
		t.Fatalf("status: %+v", st)
	}
}

func TestBootstrapGateAndTenantEnsuredBeforeEvents(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("metronome")
	dest.ensureErr = 2
	s.Cursor("metronome")
	all := fill(t, s, 2, 3, usage.MetricGPUHours)

	reg := NewTenantRegistry()
	reg.Upsert(usage.Tenant{ID: "t0", DisplayName: "Tenant Zero"})
	bootstrapCalls := 0
	d := New(s, []destinations.Destination{dest}, Options{
		BatchSize: 100, MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, IdlePoll: 5 * time.Millisecond,
		Tenants: reg,
		Bootstrap: func(context.Context, destinations.Destination) error {
			bootstrapCalls++
			if bootstrapCalls < 3 {
				return errors.New("metronome unavailable")
			}
			return nil
		},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go d.Run(ctx)
	for dest.received() < len(all) {
		if ctx.Err() != nil {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if bootstrapCalls != 3 {
		t.Fatalf("bootstrap calls = %d", bootstrapCalls)
	}
	dest.mu.Lock()
	defer dest.mu.Unlock()
	if len(dest.ensured) != 3 { // t0, t1, t2 once each, after two transient failures
		t.Fatalf("ensured = %v", dest.ensured)
	}
	if reg.NeedsEnsure("metronome", usage.Tenant{ID: "t0", DisplayName: "Tenant Zero"}) {
		t.Fatal("tenant not marked ensured")
	}
}

func TestPermanentlyRejectedTenantDoesNotBlockDelivery(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newFake("stripe")
	dest.reject = "t1"
	s.Cursor("stripe")
	all := fill(t, s, 3, 3, usage.MetricGPUHours)

	reg := NewTenantRegistry()
	for _, id := range []string{"t0", "t1", "t2"} {
		reg.Upsert(usage.Tenant{ID: id})
	}
	d := New(s, []destinations.Destination{dest}, Options{
		BatchSize: 4, MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, IdlePoll: 5 * time.Millisecond,
		Tenants: reg,
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go d.Run(ctx)
	for dest.received() < len(all) {
		if ctx.Err() != nil {
			t.Fatalf("delivery stalled at %d of %d events", dest.received(), len(all))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if !reg.NeedsEnsure("stripe", usage.Tenant{ID: "t1"}) {
		t.Fatal("rejected tenant marked ensured")
	}
	if reg.NeedsEnsure("stripe", usage.Tenant{ID: "t0"}) || reg.NeedsEnsure("stripe", usage.Tenant{ID: "t2"}) {
		t.Fatal("other tenants not ensured")
	}
}
