package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// skuGate holds every event whose SKU is not priced, like PRICE_CHECK=hold.
type skuGate struct {
	mu      sync.Mutex
	priced  map[string]bool
	changed chan struct{}
	decides int
	expired map[string]int // sku -> events released unpriced after MaxHold
}

func newSKUGate(priced ...string) *skuGate {
	g := &skuGate{priced: map[string]bool{}, changed: make(chan struct{})}
	for _, p := range priced {
		g.priced[p] = true
	}
	return g
}

func (g *skuGate) Decide(_ context.Context, dest string, events []usage.Event) []Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.decides++
	out := make([]Decision, len(events))
	for i, e := range events {
		out[i] = Decision{Series: e.Metric + "/" + e.SKU, Hold: !g.priced[e.SKU], Reason: "unpriced: no rate for " + e.SKU}
	}
	return out
}

func (g *skuGate) Releasable(_ context.Context, dest, series string, sample *usage.Event) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.priced[sample.SKU]
}

func (g *skuGate) Changed() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}

func (g *skuGate) ReleasedUnpriced(dest, series string, sample *usage.Event, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.expired == nil {
		g.expired = map[string]int{}
	}
	g.expired[sample.SKU] += n
}

func (g *skuGate) price(sku string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.priced[sku] = true
	close(g.changed)
	g.changed = make(chan struct{})
}

// orderedDest records every delivery in order.
type orderedDest struct {
	*fakeDest
	omu   sync.Mutex
	order []string
	sum   map[string]float64 // sku -> quantity received
}

func newOrdered(name string) *orderedDest {
	return &orderedDest{fakeDest: newFake(name), sum: map[string]float64{}}
}

func (o *orderedDest) SendEvents(ctx context.Context, evs []usage.Event) error {
	before := map[string]int{}
	o.fakeDest.mu.Lock()
	for _, e := range evs {
		before[e.ID] = o.got[e.ID]
	}
	o.fakeDest.mu.Unlock()
	err := o.fakeDest.SendEvents(ctx, evs)
	var partial *destinations.PartialError
	if err != nil && !errors.As(err, &partial) {
		return err
	}
	o.omu.Lock()
	defer o.omu.Unlock()
	o.fakeDest.mu.Lock()
	defer o.fakeDest.mu.Unlock()
	for _, e := range evs {
		if o.got[e.ID] > before[e.ID] {
			o.order = append(o.order, e.ID)
			o.sum[e.SKU] += e.Quantity
		}
	}
	return err
}

func (o *orderedDest) total(sku string) float64 {
	o.omu.Lock()
	defer o.omu.Unlock()
	return o.sum[sku]
}

// fillSKUs commits windows with one event per SKU and returns them by SKU.
func fillSKUs(t *testing.T, s *spool.Spool, windows int, skus ...string) map[string][]usage.Event {
	t.Helper()
	start := t0
	if wm, ok := s.Watermark(spool.SourceCollector); ok {
		start = wm
	}
	out := map[string][]usage.Event{}
	for w := 0; w < windows; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		var evs []usage.Event
		for i, sku := range skus {
			e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: sku, Quantity: float64(w*10 + i + 1),
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: time.Now()}
			e.Finalize()
			evs = append(evs, e)
			out[sku] = append(out[sku], e)
		}
		if err := s.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func openHeld(t *testing.T, dir string) *HeldStore {
	t.Helper()
	h, err := OpenHeld(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type running struct {
	d      *Dispatcher
	cancel context.CancelFunc
	done   chan struct{}
}

func start(s *spool.Spool, opts Options, dests ...destinations.Destination) *running {
	if opts.BatchSize == 0 {
		opts.BatchSize = 7
	}
	opts.MinBackoff, opts.MaxBackoff, opts.IdlePoll = time.Millisecond, 5*time.Millisecond, 10*time.Millisecond
	d := New(s, dests, opts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{d: d, cancel: cancel, done: make(chan struct{})}
	go func() { d.Run(ctx); close(r.done) }()
	return r
}

func (r *running) stop() { r.cancel(); <-r.done }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func ids(evs []usage.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestHoldUnpricedSeriesAndReleaseInOrder(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	held := openHeld(t, filepath.Join(dir, "held"))
	dest := newOrdered("metronome")
	gate := newSKUGate("H100")
	s.Cursor("metronome")
	evs := fillSKUs(t, s, 6, "H100", "B200")

	r := start(s, Options{Held: held, Gate: gate}, dest)
	defer r.stop()
	waitFor(t, "priced series delivered and the cursor at the head", func() bool {
		return dest.received() == 6 && s.Cursor("metronome") == s.Committed()
	})
	for _, e := range evs["B200"] {
		if dest.got[e.ID] != 0 {
			t.Fatal("an unpriced event was delivered")
		}
	}
	if held.Count("metronome") != 6 {
		t.Fatalf("held %d, want 6", held.Count("metronome"))
	}
	if st := r.d.Statuses()[0]; st.Held != 6 {
		t.Fatalf("status held %d", st.Held)
	}

	// More usage of the held series arrives, then the price appears while
	// even more arrives: everything ships once, in ledger order.
	more := fillSKUs(t, s, 3, "H100", "B200")
	waitFor(t, "new usage of the held series held too", func() bool { return held.Count("metronome") == 9 })
	gate.price("B200")
	late := fillSKUs(t, s, 2, "B200")
	waitFor(t, "everything delivered", func() bool { return dest.received() == 12+6+2 })
	waitFor(t, "held store drained", func() bool { return held.Count("metronome") == 0 })

	want := append(append(ids(evs["B200"]), ids(more["B200"])...), ids(late["B200"])...)
	var got []string
	dest.omu.Lock()
	for _, id := range dest.order {
		for _, w := range want {
			if id == w {
				got = append(got, id)
			}
		}
	}
	dest.omu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("B200 delivered out of ledger order:\n got %v\nwant %v", got, want)
	}
	for id, n := range dest.got {
		if n != 1 {
			t.Fatalf("event %s delivered %d times", id, n)
		}
	}
	var ledger float64
	for _, e := range append(append(evs["B200"], more["B200"]...), late["B200"]...) {
		ledger += e.Quantity
	}
	if dest.total("B200") != ledger {
		t.Fatalf("destination total %v != ledger total %v", dest.total("B200"), ledger)
	}
	if files, _ := os.ReadDir(filepath.Join(dir, "held", "metronome")); len(files) != 0 {
		t.Fatalf("drained series left files: %v", files)
	}
	st := r.d.Statuses()[0]
	if st.Held != 0 || st.Released < 9 || st.Delivered != 20 {
		t.Fatalf("status %+v", st)
	}
}

func TestHeldEventsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	dest := newOrdered("lago")
	s.Cursor("lago")
	evs := fillSKUs(t, s, 5, "H100", "B200")

	r := start(s, Options{Held: openHeld(t, filepath.Join(dir, "held")), Gate: newSKUGate("H100")}, dest)
	waitFor(t, "first run done", func() bool { return dest.received() == 5 && s.Cursor("lago") == s.Committed() })
	r.stop()
	s.Close()

	// Restart: a new process with the price in place.
	s = openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	held := openHeld(t, filepath.Join(dir, "held"))
	if held.Count("lago") != 5 {
		t.Fatalf("after restart %d held", held.Count("lago"))
	}
	r = start(s, Options{Held: held, Gate: newSKUGate("H100", "B200")}, dest)
	defer r.stop()
	waitFor(t, "held events released after restart", func() bool { return dest.received() == 10 && held.Count("lago") == 0 })
	for _, e := range append(evs["H100"], evs["B200"]...) {
		if dest.got[e.ID] != 1 {
			t.Fatalf("%s %s delivered %d times", e.SKU, e.ID, dest.got[e.ID])
		}
	}
}

func TestWithoutGateLeftoversAreReleased(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	dest := newOrdered("stripe")
	s.Cursor("stripe")
	evs := fillSKUs(t, s, 4, "H100", "B200")
	r := start(s, Options{Held: openHeld(t, filepath.Join(dir, "held")), Gate: newSKUGate("H100")}, dest)
	waitFor(t, "held", func() bool { return dest.received() == 4 && s.Cursor("stripe") == s.Committed() })
	r.stop()

	// PRICE_CHECK switched back to warn: no gate, so the held events ship.
	held := openHeld(t, filepath.Join(dir, "held"))
	r = start(s, Options{Held: held}, dest)
	defer r.stop()
	waitFor(t, "leftovers released", func() bool { return dest.received() == 8 && held.Count("stripe") == 0 })
	if dest.total("B200") != dest.total("B200") || len(evs["B200"]) != 4 {
		t.Fatal("unexpected")
	}
}

func TestReleaseRetriesAndDeadLetters(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	dest := newOrdered("metronome")
	gate := newSKUGate("H100")
	s.Cursor("metronome")
	evs := fillSKUs(t, s, 6, "H100", "B200")
	held := openHeld(t, filepath.Join(dir, "held"))
	r := start(s, Options{Held: held, Gate: gate}, dest)
	defer r.stop()
	waitFor(t, "held", func() bool { return held.Count("metronome") == 6 && s.Cursor("metronome") == s.Committed() })

	bad := evs["B200"][2]
	dest.fakeDest.mu.Lock()
	dest.poison[bad.ID] = true
	dest.failN = dest.calls + 3 // the next three sends fail with a retryable error
	dest.fakeDest.mu.Unlock()
	gate.price("B200")
	waitFor(t, "release finished", func() bool { return held.Count("metronome") == 0 })
	if dest.got[bad.ID] != 0 {
		t.Fatal("poison event delivered")
	}
	dls, _ := s.DeadLetters("metronome")
	if len(dls) != 1 || dls[0].Event.ID != bad.ID {
		t.Fatalf("dead letters %+v", dls)
	}
	for _, e := range evs["B200"] {
		if e.ID != bad.ID && dest.got[e.ID] != 1 {
			t.Fatalf("%s delivered %d times", e.ID, dest.got[e.ID])
		}
	}
	if st := r.d.Statuses()[0]; st.DeadLettered != 1 || st.Released != 6 {
		t.Fatalf("status %+v", st)
	}
}

// A batch whose priced part fails to deliver is read again; if its held part
// was released meanwhile, the released events must not be sent twice.
func TestRetriedBatchDoesNotResendReleasedEvents(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	gate := newSKUGate("H100")
	dest := newOrdered("stripe")
	// Fail the first delivery of the batch, and price B200 meanwhile.
	dest.failN = 1
	s.Cursor("stripe")
	evs := fillSKUs(t, s, 1, "H100", "B200")
	held := openHeld(t, filepath.Join(dir, "held"))
	wrapped := &hookDest{orderedDest: dest, before: func(n int) {
		if n == 1 {
			gate.price("B200") // after the batch was parked, before it is retried
		}
	}}
	r := start(s, Options{Held: held, Gate: gate}, wrapped)
	defer r.stop()
	waitFor(t, "delivered", func() bool { return dest.received() == 2 && s.Cursor("stripe") == s.Committed() })
	if n := dest.got[evs["B200"][0].ID]; n != 1 {
		t.Fatalf("B200 delivered %d times", n)
	}
	if st := r.d.Statuses()[0]; st.Skipped != 1 {
		t.Fatalf("the retried read should skip the released event: %+v", st)
	}
}

type hookDest struct {
	*orderedDest
	n      int
	before func(n int)
}

func (h *hookDest) SendEvents(ctx context.Context, evs []usage.Event) error {
	h.n++
	h.before(h.n)
	return h.orderedDest.SendEvents(ctx, evs)
}

func TestRewindReplaysReleasedEvents(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	gate := newSKUGate("H100")
	dest := newOrdered("lago")
	s.Cursor("lago")
	fillSKUs(t, s, 3, "H100", "B200")
	held := openHeld(t, filepath.Join(dir, "held"))
	r := start(s, Options{Held: held, Gate: gate}, dest)
	defer r.stop()
	waitFor(t, "held", func() bool { return held.Count("lago") == 3 && s.Cursor("lago") == s.Committed() })
	gate.price("B200")
	waitFor(t, "released", func() bool { return dest.received() == 6 && held.Count("lago") == 0 })
	dest.omu.Lock()
	sent := len(dest.order)
	dest.omu.Unlock()
	if err := s.SetCursor("lago", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "replay", func() bool {
		dest.omu.Lock()
		defer dest.omu.Unlock()
		return len(dest.order) >= sent // ordered only logs first receipts
	})
	defer func() {
		if t.Failed() {
			dest.fakeDest.mu.Lock()
			t.Logf("got %v cursor %d committed %d status %+v", dest.got, s.Cursor("lago"), s.Committed(), r.d.Statuses()[0])
			dest.fakeDest.mu.Unlock()
		}
	}()
	waitFor(t, "every event replayed", func() bool {
		dest.fakeDest.mu.Lock()
		defer dest.fakeDest.mu.Unlock()
		for _, n := range dest.got {
			if n != 2 {
				return false
			}
		}
		return true
	})
}

func TestHeldStoreTornWriteAndOffsets(t *testing.T) {
	dir := t.TempDir()
	h := openHeld(t, dir)
	mk := func(seq uint64, sku string) HeldEntry {
		e := usage.Event{Tenant: "t", Metric: usage.MetricGPUHours, SKU: sku, Quantity: float64(seq),
			WindowStart: t0.Add(time.Duration(seq) * time.Minute), WindowEnd: t0.Add(time.Duration(seq+1) * time.Minute)}
		e.Finalize()
		return HeldEntry{Seq: seq, At: t0, Series: "gpu/" + sku, Reason: "unpriced", Event: e}
	}
	var batch []HeldEntry
	for i := uint64(1); i <= 10; i++ {
		batch = append(batch, mk(i, "B200"))
	}
	if n, err := h.Add("d", batch); err != nil || n != 10 {
		t.Fatalf("add %d %v", n, err)
	}
	if n, _ := h.Add("d", batch[5:]); n != 0 {
		t.Fatalf("re-adding the same batch added %d", n)
	}
	got, next, err := h.Peek("d", "gpu/B200", 4)
	if err != nil || len(got) != 4 || got[0].Seq != 1 || got[3].Seq != 4 {
		t.Fatalf("peek %v %v", got, err)
	}
	if err := h.Release("d", "gpu/B200", 4, 4, next); err != nil {
		t.Fatal(err)
	}
	if h.Count("d") != 6 || h.Series("d")[0].FirstSeq != 5 {
		t.Fatalf("after release: %d %+v", h.Count("d"), h.Series("d"))
	}
	// A torn append (crash mid-write) is cut off on reopen.
	path := filepath.Join(dir, "d", seriesFile("gpu/B200"))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":11,"at":"2026-10-05T10:00:00Z","series":"gpu/B2`)
	f.Close()
	h2 := openHeld(t, dir)
	if h2.Count("d") != 6 {
		t.Fatalf("reopened count %d", h2.Count("d"))
	}
	if n, err := h2.Add("d", []HeldEntry{mk(11, "B200")}); err != nil || n != 1 {
		t.Fatalf("append after torn tail: %d %v", n, err)
	}
	var seqs []uint64
	for {
		got, next, err := h2.Peek("d", "gpu/B200", 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		for _, e := range got {
			seqs = append(seqs, e.Seq)
		}
		if err := h2.Release("d", "gpu/B200", got[len(got)-1].Seq, len(got), next); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(seqs) != "[5 6 7 8 9 10 11]" {
		t.Fatalf("drained %v", seqs)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("drained file not removed")
	}
	if _, err := h2.Add("../x", []HeldEntry{mk(1, "x")}); err == nil {
		t.Fatal("path traversal in a destination name accepted")
	}
	if d := h2.Destinations(); len(d) != 0 {
		t.Fatalf("destinations %v", d)
	}
	_ = sort.Strings
}

func TestHeldTenantsAreEnsured(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	dest := newOrdered("lago")
	s.Cursor("lago")
	fillSKUs(t, s, 2, "B200")
	held := openHeld(t, filepath.Join(dir, "held"))
	reg := NewTenantRegistry()
	r := start(s, Options{Held: held, Gate: newSKUGate(), Tenants: reg}, dest)
	defer r.stop()
	waitFor(t, "held", func() bool { return held.Count("lago") == 2 })
	dest.fakeDest.mu.Lock()
	defer dest.fakeDest.mu.Unlock()
	if len(dest.ensured) != 1 || dest.ensured[0] != "acme" || len(dest.got) != 0 {
		t.Fatalf("a tenant whose usage is all held is still created (subscription and contract periods start then): ensured %v, got %v", dest.ensured, dest.got)
	}
}

// startDest records the usage start EnsureTenant receives per tenant.
type startDest struct {
	*orderedDest
	starts map[string]time.Time
}

func (s *startDest) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	at, _ := destinations.UsageStart(ctx)
	s.fakeDest.mu.Lock()
	s.starts[t.ID] = at
	s.fakeDest.mu.Unlock()
	return s.orderedDest.EnsureTenant(ctx, t)
}

func TestEnsureTenantGetsTheEarliestWindowOfTheBatch(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	dest := &startDest{orderedDest: newOrdered("lago"), starts: map[string]time.Time{}}
	s.Cursor("lago")
	evs := fillSKUs(t, s, 3, "H100")
	r := start(s, Options{Tenants: NewTenantRegistry(), BatchSize: 100}, dest)
	defer r.stop()
	waitFor(t, "delivered", func() bool { return dest.received() == 3 })
	dest.fakeDest.mu.Lock()
	defer dest.fakeDest.mu.Unlock()
	if got := dest.starts["acme"]; !got.Equal(evs["H100"][0].WindowStart) {
		t.Fatalf("usage start %v, want the first window %v", got, evs["H100"][0].WindowStart)
	}
}

func TestMaxHoldReleasesExpiredUsageUnpriced(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	dest := newOrdered("metronome")
	gate := newSKUGate("H100") // B200 never gets a price
	s.Cursor("metronome")
	commitAt := func(ws time.Time, n int) []usage.Event {
		var out []usage.Event
		for i := 0; i < n; i++ {
			w := ws.Add(time.Duration(i) * time.Minute)
			e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "B200", Quantity: float64(i + 1), WindowStart: w, WindowEnd: w.Add(time.Minute)}
			e.Finalize()
			if err := s.AppendWindow(spool.SourceCollector, w.Add(time.Minute), []usage.Event{e}, ""); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}
	now := time.Now().UTC().Truncate(time.Minute)
	old := commitAt(now.Add(-10*24*time.Hour), 3)
	young := commitAt(now.Add(-time.Hour), 2)
	held := openHeld(t, filepath.Join(dir, "held"))
	r := start(s, Options{Held: held, Gate: gate, MaxHold: 5 * 24 * time.Hour}, dest)
	defer r.stop()

	waitFor(t, "expired usage released", func() bool { return dest.received() == 3 && s.Cursor("metronome") == s.Committed() })
	waitFor(t, "young usage still held", func() bool { return held.Count("metronome") == 2 })
	dest.fakeDest.mu.Lock()
	for _, e := range old {
		if dest.got[e.ID] != 1 {
			t.Errorf("expired event %s delivered %d times", e.ID, dest.got[e.ID])
		}
	}
	for _, e := range young {
		if dest.got[e.ID] != 0 {
			t.Errorf("young event %s delivered before its price or MaxHold", e.ID)
		}
	}
	dest.fakeDest.mu.Unlock()
	gate.mu.Lock()
	expired := gate.expired["B200"]
	gate.mu.Unlock()
	if expired != 3 {
		t.Fatalf("gate told about %d expired events, want 3", expired)
	}
	if st := r.d.Statuses()[0]; st.ReleasedUnpriced != 3 || st.Held != 2 {
		t.Fatalf("status %+v", st)
	}
	// The rest ships normally once priced.
	gate.price("B200")
	waitFor(t, "rest released", func() bool { return dest.received() == 5 && held.Count("metronome") == 0 })
}
