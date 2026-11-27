package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// tenantDest is a destination that cannot set up chosen tenants, like a
// billing platform whose account has no room for another customer.
type tenantDest struct {
	*orderedDest
	tmu      sync.Mutex
	failing  map[string]error
	attempts map[string]int
	starts   map[string]time.Time // usage start of the attempt that set the tenant up
}

func newTenantDest(name string) *tenantDest {
	return &tenantDest{orderedDest: newOrdered(name), failing: map[string]error{}, attempts: map[string]int{}, starts: map[string]time.Time{}}
}

func (d *tenantDest) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	d.attempts[t.ID]++
	if err := d.failing[t.ID]; err != nil {
		return err
	}
	d.starts[t.ID], _ = destinations.UsageStart(ctx)
	return nil
}

func (d *tenantDest) fail(tenant string, err error) {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	if err == nil {
		delete(d.failing, tenant)
		return
	}
	d.failing[tenant] = err
}

func (d *tenantDest) tried(tenant string) int {
	d.tmu.Lock()
	defer d.tmu.Unlock()
	return d.attempts[tenant]
}

// times returns how often each of events was received.
func (d *tenantDest) times(events []usage.Event) []int {
	d.fakeDest.mu.Lock()
	defer d.fakeDest.mu.Unlock()
	out := make([]int, len(events))
	for i, e := range events {
		out[i] = d.got[e.ID]
	}
	return out
}

// sequence returns, in the order received, the IDs of events that are among want.
func (d *tenantDest) sequence(want []usage.Event) []string {
	in := map[string]bool{}
	for _, e := range want {
		in[e.ID] = true
	}
	d.omu.Lock()
	defer d.omu.Unlock()
	var out []string
	for _, id := range d.order {
		if in[id] {
			out = append(out, id)
		}
	}
	return out
}

func all(n []int, want int) bool {
	for _, v := range n {
		if v != want {
			return false
		}
	}
	return true
}

// errQuota is what a billing platform answers when it has no room for
// another customer: not a permanent error, so it used to be retried forever.
var errQuota = errors.New("HTTP 404: accounts on this plan are limited to 5 active customers")

// fillTenants commits windows with one event per tenant and SKU and returns
// them by tenant, in ledger order.
func fillTenants(t *testing.T, s *spool.Spool, windows int, skus []string, tenants ...string) map[string][]usage.Event {
	t.Helper()
	start := t0
	if wm, ok := s.Watermark(spool.SourceCollector); ok {
		start = wm
	}
	if len(skus) == 0 {
		skus = []string{""}
	}
	out := map[string][]usage.Event{}
	for w := 0; w < windows; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		var evs []usage.Event
		for _, sku := range skus {
			for _, tenant := range tenants {
				e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, SKU: sku, Quantity: float64(w + 1),
					WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: time.Now()}
				e.Finalize()
				evs = append(evs, e)
				out[tenant] = append(out[tenant], e)
			}
		}
		if err := s.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func registry(tenants ...string) *TenantRegistry {
	reg := NewTenantRegistry()
	for _, id := range tenants {
		reg.Upsert(usage.Tenant{ID: id, DisplayName: strings.ToUpper(id)})
	}
	return reg
}

// rig is a ledger, a held store and one destination under test.
type rig struct {
	t      *testing.T
	dir    string
	s      *spool.Spool
	held   *HeldStore
	dest   *tenantDest
	reg    *TenantRegistry
	health *health.Registry
	tel    *telemetry.Registry
	r      *running
	opts   Options
}

func newRig(t *testing.T, opts Options, tenants ...string) *rig {
	t.Helper()
	g := &rig{t: t, dir: t.TempDir(), dest: newTenantDest("metronome"), reg: registry(tenants...), opts: opts}
	g.open()
	g.s.Cursor(g.dest.Name()) // the destination exists before the usage does
	return g
}

// open opens the ledger and the held store (again, after stop).
func (g *rig) open() {
	g.t.Helper()
	g.s = openSpool(g.t, filepath.Join(g.dir, "ledger"))
	g.held = openHeld(g.t, filepath.Join(g.dir, "held"))
	g.tel = telemetry.New()
	g.health = health.New(g.tel)
	g.health.SetLogger(func(string, ...any) {})
}

// start runs a dispatcher over the rig's ledger until stop.
func (g *rig) start(dests ...destinations.Destination) {
	g.t.Helper()
	if len(dests) == 0 {
		dests = []destinations.Destination{g.dest}
	}
	opts := g.opts
	opts.Held, opts.Tenants, opts.Health = g.held, g.reg, g.health
	if opts.BatchSize == 0 {
		opts.BatchSize = 7
	}
	opts.MinBackoff, opts.MaxBackoff, opts.IdlePoll = time.Millisecond, 5*time.Millisecond, 10*time.Millisecond
	d := New(g.s, dests, opts, g.tel)
	ctx, cancel := context.WithCancel(context.Background())
	g.r = &running{d: d, cancel: cancel, done: make(chan struct{})}
	go func() { d.Run(ctx); close(g.r.done) }()
}

func (g *rig) stop() {
	if g.r != nil {
		g.r.stop()
		g.r = nil
	}
}

func (g *rig) close() {
	g.stop()
	g.s.Close()
}

// restart stops delivery, closes the ledger and opens everything again, as
// a new process would.
func (g *rig) restart(dests ...destinations.Destination) {
	g.t.Helper()
	g.close()
	g.open()
	g.start(dests...)
}

func (g *rig) status() Status {
	g.t.Helper()
	st, ok := g.r.d.Status(g.dest.Name())
	if !ok {
		g.t.Fatalf("no status for %s", g.dest.Name())
	}
	return st
}

func (g *rig) source(name string) (health.Source, bool) {
	for _, s := range g.health.List() {
		if s.Name == name {
			return s, true
		}
	}
	return health.Source{}, false
}

// A billing platform that cannot create one tenant's customer used
// to stop delivery for every tenant: the whole batch was retried forever and
// the cursor never moved. Now only that tenant waits.
func TestTenantThatCannotBeSetUpIsParkedWhileTheOthersAreDelivered(t *testing.T) {
	g := newRig(t, Options{}, "a", "b", "c")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 6, nil, "a", "b", "c")
	g.start()

	waitFor(t, "the other tenants delivered and the cursor at the head", func() bool {
		return all(g.dest.times(evs["b"]), 1) && all(g.dest.times(evs["c"]), 1) && g.s.Cursor("metronome") == g.s.Committed()
	})
	if !all(g.dest.times(evs["a"]), 0) {
		t.Fatalf("usage of a tenant that is not set up was sent: %v", g.dest.times(evs["a"]))
	}
	if n := g.held.ParkedCount("metronome"); n != len(evs["a"]) {
		t.Fatalf("parked %d events, want the %d of tenant a", n, len(evs["a"]))
	}
	if n := g.held.Count("metronome"); n != 0 {
		t.Fatalf("parked events are counted as held at the pricing gate: %d", n)
	}
	// More usage: the other tenants keep flowing, the failing one keeps waiting.
	more := fillTenants(t, g.s, 3, nil, "a", "b", "c")
	waitFor(t, "later windows delivered too", func() bool {
		return all(g.dest.times(more["b"]), 1) && all(g.dest.times(more["c"]), 1) && g.s.Cursor("metronome") == g.s.Committed()
	})
	if n := g.held.ParkedCount("metronome"); n != 9 {
		t.Fatalf("parked %d events, want 9", n)
	}
	// Offboarding and the usage API read the store with Each: parked events
	// are a tenant's undelivered usage like held ones, so a tenant is not
	// offboarded while its usage is parked.
	waiting := map[string]int{}
	g.held.Each("metronome", func(e HeldEntry) bool { waiting[e.Event.Tenant]++; return true })
	if len(waiting) != 1 || waiting["a"] != 9 {
		t.Fatalf("Each reports %v, want the 9 parked events of tenant a", waiting)
	}

	// It is reported: status, a failing source with the platform's error,
	// a gauge and a counter. Each provisioning attempt is retried on its own.
	st := g.status()
	if st.Parked != 9 || st.BlockedTenants != 1 || st.Held != 0 || st.LagRecords != 0 || st.Delivered != 18 || st.State != StateDelivering {
		t.Fatalf("status %+v", st)
	}
	waitFor(t, "provisioning retried on its own", func() bool { return g.dest.tried("a") >= 3 })
	src, ok := g.source("provisioning/metronome/a")
	if !ok || src.Status != health.Failing || !strings.Contains(src.Message, errQuota.Error()) || !strings.Contains(src.Message, "parked") || src.Hint == "" {
		t.Fatalf("health source: %+v (found %v)", src, ok)
	}
	labels := map[string]string{"destination": "metronome"}
	if v := g.tel.Get("vbilling_tenants_blocked", labels); v != 1 {
		t.Fatalf("vbilling_tenants_blocked = %v, want 1", v)
	}
	if v := g.tel.Get("vbilling_tenant_provisioning_failures_total", labels); v < 3 {
		t.Fatalf("vbilling_tenant_provisioning_failures_total = %v, want every failed attempt", v)
	}
	if v := g.tel.Get("vbilling_parked_events", labels); v != 9 {
		t.Fatalf("vbilling_parked_events = %v, want 9", v)
	}
	rows := g.r.d.Blocked("metronome")
	if len(rows) != 1 || rows[0].Tenant != "a" || rows[0].Parked != 9 || rows[0].Permanent || !strings.Contains(rows[0].Error, "limited to 5") {
		t.Fatalf("blocked tenants: %+v", rows)
	}
}

// When the tenant can be set up at last, its parked events arrive once, in
// ledger order across its series, before anything newer of the tenant, and
// its billing period starts early enough to count the oldest of them.
func TestParkedEventsShipOnceInLedgerOrderWhenTheTenantIsSetUp(t *testing.T) {
	// A gate that prices everything gives each SKU its own series, so the
	// tenant's events are parked in two files.
	g := newRig(t, Options{Gate: newSKUGate("H100", "B200")}, "a", "b")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 12, []string{"H100", "B200"}, "a", "b")
	g.start()
	waitFor(t, "tenant b delivered, tenant a parked", func() bool {
		return all(g.dest.times(evs["b"]), 1) && g.held.ParkedCount("metronome") == len(evs["a"]) && g.s.Cursor("metronome") == g.s.Committed()
	})
	if n := len(g.held.ParkedSeries("metronome")); n != 2 {
		t.Fatalf("tenant a's events are parked in %d files, want one per series", n)
	}

	g.dest.fail("a", nil) // the operator made room
	late := fillTenants(t, g.s, 2, []string{"H100", "B200"}, "a", "b")
	want := append(append([]usage.Event{}, evs["a"]...), late["a"]...)
	waitFor(t, "everything of tenant a delivered", func() bool {
		return all(g.dest.times(want), 1) && g.held.ParkedCount("metronome") == 0
	})
	if got := g.dest.sequence(want); strings.Join(got, ",") != strings.Join(ids(want), ",") {
		t.Fatalf("tenant a's events arrived out of ledger order:\n got %v\nwant %v", got, ids(want))
	}
	if !all(g.dest.times(append(evs["b"], late["b"]...)), 1) {
		t.Fatal("tenant b's events were not delivered exactly once")
	}
	g.dest.tmu.Lock()
	start := g.dest.starts["a"]
	g.dest.tmu.Unlock()
	if !start.Equal(evs["a"][0].WindowStart) {
		t.Fatalf("tenant a was set up for usage from %v, want its oldest parked window %v", start, evs["a"][0].WindowStart)
	}
	waitFor(t, "the tenant is no longer reported", func() bool {
		src, ok := g.source("provisioning/metronome/a")
		return ok && src.Status == health.OK && g.tel.Get("vbilling_tenants_blocked", map[string]string{"destination": "metronome"}) == 0
	})
	if st := g.status(); st.Parked != 0 || st.BlockedTenants != 0 || st.DeadLettered != 0 {
		t.Fatalf("status %+v", st)
	}
}

// Parked events are on disk: a restart neither loses nor sends them.
func TestParkedEventsSurviveARestart(t *testing.T) {
	g := newRig(t, Options{}, "a", "b")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 5, nil, "a", "b")
	g.start()
	waitFor(t, "first run", func() bool {
		return all(g.dest.times(evs["b"]), 1) && g.held.ParkedCount("metronome") == 5 && g.s.Cursor("metronome") == g.s.Committed()
	})

	g.restart() // tenant a still cannot be set up
	more := fillTenants(t, g.s, 2, nil, "a", "b")
	waitFor(t, "second run", func() bool {
		return all(g.dest.times(more["b"]), 1) && g.held.ParkedCount("metronome") == 7 && g.s.Cursor("metronome") == g.s.Committed()
	})
	if !all(g.dest.times(append(evs["a"], more["a"]...)), 0) {
		t.Fatal("a restart sent the events of a tenant that is not set up")
	}
	if st := g.status(); st.Parked != 7 || st.BlockedTenants != 1 {
		t.Fatalf("status after the restart: %+v", st)
	}

	g.restart()
	g.dest.fail("a", nil)
	want := append(evs["a"], more["a"]...)
	waitFor(t, "released after the second restart", func() bool { return all(g.dest.times(want), 1) && g.held.ParkedCount("metronome") == 0 })
	if got := g.dest.sequence(want); strings.Join(got, ",") != strings.Join(ids(want), ",") {
		t.Fatalf("out of ledger order after restarts:\n got %v\nwant %v", got, ids(want))
	}
	if !all(g.dest.times(append(evs["b"], more["b"]...)), 1) {
		t.Fatal("tenant b's events were not delivered exactly once across restarts")
	}
}

// An operator's rewind replays the ledger. The events of a tenant that is
// parked stay parked once: the replay neither sends them nor parks a second
// copy, and they still arrive once when the tenant is set up.
func TestRewindWhileATenantIsParked(t *testing.T) {
	g := newRig(t, Options{}, "a", "b")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 6, nil, "a", "b")
	g.start()
	waitFor(t, "delivered and parked", func() bool {
		return all(g.dest.times(evs["b"]), 1) && g.held.ParkedCount("metronome") == 6 && g.s.Cursor("metronome") == g.s.Committed()
	})

	if err := g.s.SetCursor("metronome", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tenant b replayed", func() bool { return all(g.dest.times(evs["b"]), 2) && g.s.Cursor("metronome") == g.s.Committed() })
	if n := g.held.ParkedCount("metronome"); n != 6 {
		t.Fatalf("after the rewind %d events are parked, want the same 6", n)
	}
	if !all(g.dest.times(evs["a"]), 0) {
		t.Fatal("the rewind sent events of a tenant that is not set up")
	}

	g.dest.fail("a", nil)
	waitFor(t, "tenant a released", func() bool { return all(g.dest.times(evs["a"]), 1) && g.held.ParkedCount("metronome") == 0 })
	time.Sleep(30 * time.Millisecond) // nothing more follows
	if !all(g.dest.times(evs["a"]), 1) || !all(g.dest.times(evs["b"]), 2) {
		t.Fatalf("after the release: a %v, b %v", g.dest.times(evs["a"]), g.dest.times(evs["b"]))
	}
}

// Parked events do not wait forever. The health entry says when they will be
// dead-lettered before it happens; at the limit they are dead-lettered with
// the provisioning error, and they stay replayable.
func TestParkedEventsAreDeadLetteredAtTheLimit(t *testing.T) {
	g := newRig(t, Options{MaxHold: time.Hour}, "a", "b")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 4, nil, "a", "b")
	g.start()
	waitFor(t, "delivered and parked", func() bool {
		return all(g.dest.times(evs["b"]), 1) && g.held.ParkedCount("metronome") == 4 && g.s.Cursor("metronome") == g.s.Committed()
	})
	waitFor(t, "the health entry says how long is left", func() bool {
		src, _ := g.source("provisioning/metronome/a")
		return strings.Contains(src.Message, "the oldest is dead-lettered in 1h0m0s") && strings.Contains(src.Hint, "dead-letters/replay")
	})
	if rows := g.r.d.Blocked("metronome"); len(rows) != 1 || time.Until(rows[0].DeadLetterAt) < 59*time.Minute {
		t.Fatalf("blocked tenants: %+v", rows)
	}
	if dls, _ := g.s.DeadLetters("metronome"); len(dls) != 0 {
		t.Fatalf("dead-lettered before the limit: %+v", dls)
	}

	// The same ledger and held store with a limit that has passed.
	g.stop()
	g.opts.MaxHold = 20 * time.Millisecond
	time.Sleep(25 * time.Millisecond)
	g.start()
	more := fillTenants(t, g.s, 1, nil, "b") // the destination works for others
	waitFor(t, "dead-lettered at the limit", func() bool {
		dls, _ := g.s.DeadLetters("metronome")
		return len(dls) == 4 && g.held.ParkedCount("metronome") == 0 && all(g.dest.times(more["b"]), 1)
	})
	dls, _ := g.s.DeadLetters("metronome")
	for i, dl := range dls {
		if dl.Event.ID != evs["a"][i].ID || !strings.Contains(dl.Reason, errQuota.Error()) || !strings.Contains(dl.Reason, "tenant a could not be set up at metronome") {
			t.Fatalf("dead letter %d: %+v", i, dl)
		}
	}
	if !all(g.dest.times(evs["a"]), 0) {
		t.Fatal("dead-lettered events were sent")
	}
	// The dispatcher counts them just after they left the held store.
	waitFor(t, "the dead letters counted in the status and the metric", func() bool {
		st := g.status()
		return st.DeadLettered == 4 && st.Parked == 0 &&
			g.tel.Get("vbilling_events_dead_lettered_total", map[string]string{"destination": "metronome"}) == 4
	})
	// Nothing of the tenant waits any more: offboarding it can go ahead.
	left := 0
	g.held.Each("metronome", func(HeldEntry) bool { left++; return true })
	if left != 0 {
		t.Fatalf("%d events still wait in the held store", left)
	}

	// Replayable: while the tenant still cannot be set up, a replay keeps its
	// dead letters back; once it can, they are delivered.
	events := make([]usage.Event, len(dls))
	for i := range dls {
		events[i] = dls[i].Event
	}
	res, err := g.r.d.Replay(context.Background(), "metronome", events)
	if err != nil || len(res.NotSetUp) != 1 || !strings.Contains(res.NotSetUp["a"], "limited to 5") || !all(g.dest.times(evs["a"]), 0) {
		t.Fatalf("replay while the tenant is not set up: %+v %v, sent %v", res, err, g.dest.times(evs["a"]))
	}
	g.dest.fail("a", nil)
	res, err = g.r.d.Replay(context.Background(), "metronome", events)
	if err != nil || len(res.NotSetUp) != 0 || len(res.Refused) != 0 || !all(g.dest.times(evs["a"]), 1) {
		t.Fatalf("replay after the fix: %+v %v, sent %v", res, err, g.dest.times(evs["a"]))
	}
}

// A destination that accepts nothing from anyone is down. Its tenants'
// parked events are not dead-lettered for being late then, as nothing else
// is; they ship when it works again.
func TestNothingIsDeadLetteredWhileTheDestinationAcceptsNothing(t *testing.T) {
	g := newRig(t, Options{MaxHold: 20 * time.Millisecond}, "a", "b")
	defer g.close()
	down := errors.New("HTTP 503: service unavailable")
	g.dest.fail("a", down)
	g.dest.fail("b", down)
	evs := fillTenants(t, g.s, 3, nil, "a", "b")
	g.start()
	waitFor(t, "everything parked", func() bool { return g.held.ParkedCount("metronome") == 6 && g.s.Cursor("metronome") == g.s.Committed() })
	waitFor(t, "provisioning retried well past the limit", func() bool { return g.dest.tried("a") >= 6 && g.dest.tried("b") >= 6 })
	time.Sleep(60 * time.Millisecond)
	if dls, _ := g.s.DeadLetters("metronome"); len(dls) != 0 || g.held.ParkedCount("metronome") != 6 {
		t.Fatalf("an outage dead-lettered %d parked event(s); %d still parked", len(dls), g.held.ParkedCount("metronome"))
	}
	src, _ := g.source("provisioning/metronome/a")
	if !strings.Contains(src.Message, "nothing is dead-lettered until it works for another tenant") {
		t.Fatalf("health entry does not say why nothing is dead-lettered: %q", src.Message)
	}

	g.dest.fail("a", nil)
	g.dest.fail("b", nil)
	waitFor(t, "delivered once the destination works again", func() bool {
		return all(g.dest.times(evs["a"]), 1) && all(g.dest.times(evs["b"]), 1) && g.held.ParkedCount("metronome") == 0
	})
	if dls, _ := g.s.DeadLetters("metronome"); len(dls) != 0 {
		t.Fatalf("dead letters after the recovery: %+v", dls)
	}
}

// A permanent rejection means the destination will not take the tenant as
// it is. Its usage is still sent, as it always was (the customer may exist
// while its plan cannot be subscribed). What the destination then refuses
// of that tenant is parked with the tenant, not dead-lettered at once, and
// ships when the operator has fixed the cause.
func TestEventsRefusedForARejectedTenantAreParkedNotDeadLettered(t *testing.T) {
	g := newRig(t, Options{}, "a", "b")
	defer g.close()
	g.dest.partial = true
	g.dest.fail("a", destinations.Permanent(errors.New("plan big has 21 prices; the platform allows 20 items per subscription")))
	evs := fillTenants(t, g.s, 4, nil, "a", "b")
	// The platform takes the first two events of the rejected tenant and
	// refuses the rest.
	g.dest.fakeDest.mu.Lock()
	for _, e := range evs["a"][2:] {
		g.dest.poison[e.ID] = true
	}
	g.dest.fakeDest.mu.Unlock()
	g.start()
	waitFor(t, "delivered what the platform takes", func() bool {
		return all(g.dest.times(evs["b"]), 1) && all(g.dest.times(evs["a"][:2]), 1) && g.s.Cursor("metronome") == g.s.Committed()
	})
	if dls, _ := g.s.DeadLetters("metronome"); len(dls) != 0 {
		t.Fatalf("refused events of a rejected tenant were dead-lettered at once: %+v", dls)
	}
	if n := g.held.ParkedCount("metronome"); n != 2 {
		t.Fatalf("parked %d refused events, want 2", n)
	}
	rows := g.r.d.Blocked("metronome")
	if len(rows) != 1 || !rows[0].Permanent || rows[0].Parked != 2 {
		t.Fatalf("blocked tenants: %+v", rows)
	}
	src, _ := g.source("provisioning/metronome/a")
	if src.Status != health.Failing || !strings.Contains(src.Message, "21 prices") || !strings.Contains(src.Message, "still sent") {
		t.Fatalf("health source: %+v", src)
	}
	// Later usage of the rejected tenant is still sent: nothing queues
	// behind the refused events.
	more := fillTenants(t, g.s, 2, nil, "a", "b")
	waitFor(t, "later usage of the rejected tenant sent", func() bool { return all(g.dest.times(more["a"]), 1) && all(g.dest.times(more["b"]), 1) })

	// The operator fixes the plan: the tenant is set up and the parked
	// events are sent again.
	g.dest.fakeDest.mu.Lock()
	g.dest.poison = map[string]bool{}
	g.dest.fakeDest.mu.Unlock()
	g.dest.fail("a", nil)
	waitFor(t, "parked events released", func() bool { return all(g.dest.times(evs["a"]), 1) && g.held.ParkedCount("metronome") == 0 })
	if dls, _ := g.s.DeadLetters("metronome"); len(dls) != 0 {
		t.Fatalf("dead letters: %+v", dls)
	}
}

// The pricing gate and the tenants are independent. Held events of a tenant
// that is not set up move from their series to the tenant's parked events
// when the gate releases the series, so the series' other tenants are not
// kept waiting, and the gate never releases parked events.
func TestHeldSeriesReleasesAroundATenantThatIsNotSetUp(t *testing.T) {
	gate := newSKUGate("H100") // B200 has no price yet
	g := newRig(t, Options{Gate: gate}, "a", "b")
	defer g.close()
	g.dest.fail("a", errQuota)
	evs := fillTenants(t, g.s, 5, []string{"B200"}, "a", "b")
	g.start()
	waitFor(t, "held for the price", func() bool { return g.held.Count("metronome") == 10 && g.s.Cursor("metronome") == g.s.Committed() })
	if g.dest.received() != 0 || g.held.ParkedCount("metronome") != 0 {
		t.Fatalf("unpriced usage: received %d, parked %d", g.dest.received(), g.held.ParkedCount("metronome"))
	}
	if g.dest.tried("a") == 0 {
		t.Fatal("the tenant of held usage is set up while its usage waits for a price")
	}

	gate.price("B200")
	waitFor(t, "the series is released around tenant a", func() bool {
		return all(g.dest.times(evs["b"]), 1) && g.held.Count("metronome") == 0 && g.held.ParkedCount("metronome") == 5
	})
	if !all(g.dest.times(evs["a"]), 0) {
		t.Fatal("the pricing gate released events of a tenant that is not set up")
	}
	time.Sleep(30 * time.Millisecond) // the gate allows everything: parked events still wait for the tenant
	if g.held.ParkedCount("metronome") != 5 || !all(g.dest.times(evs["a"]), 0) {
		t.Fatal("parked events left without their tenant being set up")
	}

	g.dest.fail("a", nil)
	waitFor(t, "tenant a's events delivered", func() bool { return all(g.dest.times(evs["a"]), 1) && g.held.Total("metronome") == 0 })
	if got := g.dest.sequence(evs["a"]); strings.Join(got, ",") != strings.Join(ids(evs["a"]), ",") {
		t.Fatalf("out of ledger order:\n got %v\nwant %v", got, ids(evs["a"]))
	}
}

// A tenant that could not be set up and has no usage on its way is not
// retried forever: nothing waits for it.
func TestTenantWithoutUsageIsNotRetriedForever(t *testing.T) {
	g := newRig(t, Options{}, "a")
	defer g.close()
	g.dest.fail("a", destinations.Permanent(errors.New("unknown plan")))
	evs := fillTenants(t, g.s, 1, nil, "a")
	g.start()
	waitFor(t, "sent", func() bool { return all(g.dest.times(evs["a"]), 1) })
	waitFor(t, "retried while it has usage", func() bool { return g.dest.tried("a") >= 2 })

	// An hour without usage of the tenant.
	g.r.d.mu.Lock()
	g.r.d.blocks["metronome"]["a"].seen = time.Now().Add(-2 * blockIdle)
	g.r.d.mu.Unlock()
	waitFor(t, "reported as idle", func() bool {
		src, _ := g.source("provisioning/metronome/a")
		return src.Status == health.Paused
	})
	tried := g.dest.tried("a")
	time.Sleep(40 * time.Millisecond)
	if n := g.dest.tried("a"); n != tried {
		t.Fatalf("a tenant without usage was tried %d more time(s)", n-tried)
	}
	if v := g.tel.Get("vbilling_tenants_blocked", map[string]string{"destination": "metronome"}); v != 0 {
		t.Fatalf("vbilling_tenants_blocked = %v for a tenant nothing waits for", v)
	}
	// Its next usage brings it back.
	fillTenants(t, g.s, 1, nil, "a")
	waitFor(t, "retried when it has usage again", func() bool { return g.dest.tried("a") > tried })
}

// Without a held store there is nowhere to park: the batch waits for the
// tenant, as every batch did before (tools and tests build such dispatchers;
// vBilling always has a held store).
func TestWithoutAHeldStoreTheBatchWaitsForTheTenant(t *testing.T) {
	s := openSpool(t, t.TempDir())
	defer s.Close()
	dest := newTenantDest("lago")
	dest.fail("a", errQuota)
	s.Cursor("lago")
	evs := fillTenants(t, s, 2, nil, "a", "b")
	r := start(s, Options{Tenants: registry("a", "b")}, dest)
	defer r.stop()
	waitFor(t, "retried", func() bool { return dest.tried("a") >= 3 })
	if s.Cursor("lago") != 0 || dest.received() != 0 {
		t.Fatalf("cursor %d, received %d: without a held store nothing may pass a tenant that is not set up", s.Cursor("lago"), dest.received())
	}
	dest.fail("a", nil)
	waitFor(t, "delivered", func() bool {
		return all(dest.times(evs["a"]), 1) && all(dest.times(evs["b"]), 1) && s.Cursor("lago") == s.Committed()
	})
}

// The health entry of a parked tenant is written when its events are parked,
// so it never says less than what waits.
func TestHealthEntryCountsWhatIsParked(t *testing.T) {
	g := newRig(t, Options{MaxHold: time.Hour}, "a")
	defer g.close()
	g.dest.fail("a", errQuota)
	fillTenants(t, g.s, 3, nil, "a")
	g.start()
	waitFor(t, "parked", func() bool { return g.held.ParkedCount("metronome") == 3 })
	waitFor(t, "reported with the parked events", func() bool {
		src, _ := g.source("provisioning/metronome/a")
		return strings.Contains(src.Message, fmt.Sprintf("%d event(s) parked", 3))
	})
}
