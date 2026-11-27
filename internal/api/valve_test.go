package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// sink is a named destination that records what it receives.
type sink struct {
	name string
	mu   sync.Mutex
	got  map[string]int // event ID -> times received
	// refuse makes it refuse a whole batch for good when the batch holds one
	// of these event IDs, as a receiver that validates a batch does.
	refuse map[string]bool
	down   error // every send fails with it
	ensure map[string]error
}

func newSink(name string) *sink {
	return &sink{name: name, got: map[string]int{}, refuse: map[string]bool{}, ensure: map[string]error{}}
}

func (d *sink) Name() string                                       { return d.name }
func (d *sink) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (d *sink) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (d *sink) EnsureTenant(_ context.Context, t usage.Tenant) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ensure[t.ID]
}
func (d *sink) SendEvents(_ context.Context, evs []usage.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down != nil {
		return d.down
	}
	for _, e := range evs {
		if d.refuse[e.ID] {
			return destinations.Permanent(errors.New("HTTP 400: invalid event " + e.ID))
		}
	}
	for _, e := range evs {
		d.got[e.ID]++
	}
	return nil
}

func (d *sink) times(evs []usage.Event) []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]int, len(evs))
	for i, e := range evs {
		out[i] = d.got[e.ID]
	}
	return out
}

func each(n []int, want int) bool {
	for _, v := range n {
		if v != want {
			return false
		}
	}
	return true
}

// holdSKU is a pricing gate that holds the events of one SKU at one
// destination (whose billing platform has no price for it).
type holdSKU struct {
	mu        sync.Mutex
	dest, sku string
}

// hold makes the gate hold sku at dest; hold("", "") holds nothing.
func (g *holdSKU) hold(dest, sku string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dest, g.sku = dest, sku
}

func (g *holdSKU) held(dest, sku string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sku != "" && sku == g.sku && dest == g.dest
}
func (g *holdSKU) Decide(_ context.Context, dest string, events []usage.Event) []pipeline.Decision {
	out := make([]pipeline.Decision, len(events))
	for i, e := range events {
		out[i] = pipeline.Decision{Series: e.Metric + "/" + e.SKU, Hold: g.held(dest, e.SKU), Reason: "unpriced: no rate for " + e.SKU}
	}
	return out
}
func (g *holdSKU) Releasable(_ context.Context, dest, _ string, sample *usage.Event) bool {
	return !g.held(dest, sample.SKU)
}
func (g *holdSKU) Changed() <-chan struct{}                           { return nil }
func (g *holdSKU) ReleasedUnpriced(string, string, *usage.Event, int) {}

// station is a data directory and an API over several lives of vBilling,
// each with its own ADAPTERS, as an operator sees them.
type station struct {
	t      *testing.T
	mu     sync.Mutex
	clock  time.Time
	sp     *spool.Spool
	held   *pipeline.HeldStore
	gate   *holdSKU
	reg    *pipeline.TenantRegistry
	health *health.Registry
	server *Server
	srv    *httptest.Server
	stop   func()
}

func newStation(t *testing.T) *station {
	t.Helper()
	v := &station{t: t, clock: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC), gate: &holdSKU{}, reg: pipeline.NewTenantRegistry()}
	dir := t.TempDir()
	var err error
	if v.sp, err = spool.Open(filepath.Join(dir, "ledger"), spool.Options{NoSync: true, Now: v.now}); err != nil {
		t.Fatal(err)
	}
	if v.held, err = pipeline.OpenHeld(filepath.Join(dir, "held"), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if v.stop != nil {
			v.stop()
		}
		if v.srv != nil {
			v.srv.Close()
		}
		v.sp.Close()
	})
	return v
}

func (v *station) now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.clock
}

func (v *station) advance(d time.Duration) {
	v.mu.Lock()
	v.clock = v.clock.Add(d)
	v.mu.Unlock()
}

// boot starts a life of vBilling with these destinations (ending the one
// before), and serves its API.
func (v *station) boot(dests ...destinations.Destination) *pipeline.Dispatcher {
	v.t.Helper()
	if v.stop != nil {
		v.stop()
	}
	tel := telemetry.New()
	v.health = health.New(tel)
	v.health.SetLogger(func(string, ...any) {})
	var names []string
	for _, d := range dests {
		names = append(names, d.Name())
		v.sp.Cursor(d.Name()) // its loop would ask the same, a moment later
	}
	disp := pipeline.New(v.sp, dests, pipeline.Options{BatchSize: 50, IdlePoll: 5 * time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		Held: v.held, Gate: v.gate, Tenants: v.reg, Health: v.health}, tel)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { disp.Run(ctx); close(done) }()
	v.stop = func() { cancel(); <-done }
	cfg := &config.Config{Region: "us-central1", Adapters: names, CollectionInterval: time.Minute, APIToken: adminToken, EnforcementMode: "observe"}
	v.server = New(cfg, v.sp, disp, v.reg, nil, nil, tel, "test").WithHealth(v.health)
	v.server.now = v.now
	if v.srv != nil {
		v.srv.Close()
	}
	v.srv = httptest.NewServer(v.server.Handler())
	return disp
}

// commit appends windows a minute apart, one event per tenant and SKU, the
// clock moving with them, and returns the events by SKU.
func (v *station) commit(windows int, tenants []string, skus ...string) map[string][]usage.Event {
	v.t.Helper()
	out := map[string][]usage.Event{}
	for w := 0; w < windows; w++ {
		ws := v.now().Truncate(time.Minute)
		v.advance(time.Minute)
		var evs []usage.Event
		for _, tenant := range tenants {
			for _, sku := range skus {
				e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, SKU: sku, Quantity: 0.5, Region: "us-central1",
					WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: v.now()}
				e.Finalize()
				evs = append(evs, e)
				out[sku] = append(out[sku], e)
			}
		}
		if err := v.sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			v.t.Fatal(err)
		}
	}
	return out
}

// call sends a request as the operator and decodes the JSON answer.
func (v *station) call(method, path, body string) (int, map[string]any) {
	v.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	resp := do(v.t, method, v.srv.URL+path, adminToken, rd)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		v.t.Fatalf("%s %s: %d, not JSON: %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

// row returns one destination of GET /api/v1/destinations.
func (v *station) row(name string) map[string]any {
	v.t.Helper()
	code, out := v.call("GET", "/api/v1/destinations", "")
	if code != 200 {
		v.t.Fatalf("GET destinations: %d %v", code, out)
	}
	for _, r := range out["destinations"].([]any) {
		if m := r.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func num(v any) int64 {
	n, _ := v.(json.Number)
	i, _ := n.Int64()
	return i
}

func (v *station) until(what string, cond func() bool) {
	v.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			v.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The delivery valve over the API: pause with a reason, see it, resume.
func TestPauseAndResumeADestination(t *testing.T) {
	v := newStation(t)
	dest := newSink("metronome")
	v.boot(dest)
	first := v.commit(2, []string{"acme"}, "H100")
	v.until("delivered", func() bool { return each(dest.times(first["H100"]), 1) && v.sp.Cursor("metronome") == v.sp.Committed() })

	code, out := v.call("POST", "/api/v1/destinations/metronome/pause", `{"reason":"sandbox cleanup"}`)
	if code != 200 || out["destination"] != "metronome" || out["state"] != "paused" || out["paused"].(map[string]any)["reason"] != "sandbox cleanup" || out["in_flight"] != nil {
		t.Fatalf("pause: %d %v", code, out)
	}
	during := v.commit(3, []string{"acme"}, "H100")
	v.until("the pause is a paused source, not a problem", func() bool {
		_, src := v.call("GET", "/api/v1/sources?kind=delivery", "")
		list := src["sources"].([]any)
		return len(list) == 1 && list[0].(map[string]any)["status"] == "paused"
	})
	time.Sleep(30 * time.Millisecond)
	if !each(dest.times(during["H100"]), 0) {
		t.Fatalf("a paused destination received %v", dest.times(during["H100"]))
	}
	row := v.row("metronome")
	if row["state"] != "paused" || num(row["lag_records"]) != 6 || num(row["cursor"]) != 4 {
		t.Fatalf("listed while paused: %v", row)
	}
	_, status := v.call("GET", "/api/v1/status", "")
	if st := status["destinations"].([]any)[0].(map[string]any); st["state"] != "paused" || status["healthy"] != true {
		t.Fatalf("status while paused: %v, healthy %v", st, status["healthy"])
	}
	_, metrics := 0, ""
	if resp := do(t, "GET", v.srv.URL+"/metrics", adminToken, nil); resp != nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		metrics = string(b)
	}
	for _, want := range []string{`vbilling_destination_state{destination="metronome",state="paused"} 1`, `vbilling_destination_state{destination="metronome",state="delivering"} 0`,
		`vbilling_destination_lag_records{destination="metronome"} 6`} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("/metrics lacks %q", want)
		}
	}

	code, out = v.call("POST", "/api/v1/destinations/metronome/resume", "")
	if code != 200 || out["state"] != "delivering" || out["resumed"] != true || out["was_paused"].(map[string]any)["reason"] != "sandbox cleanup" {
		t.Fatalf("resume: %d %v", code, out)
	}
	v.until("caught up exactly once", func() bool {
		return each(dest.times(append(first["H100"], during["H100"]...)), 1) && v.sp.Cursor("metronome") == v.sp.Committed()
	})
	if code, out = v.call("POST", "/api/v1/destinations/metronome/resume", ""); code != 200 || out["resumed"] != false {
		t.Fatalf("resume of a destination that is not paused: %d %v", code, out)
	}

	// What the calls refuse.
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/v1/destinations/nothing/pause", "", 404},
		{"/api/v1/destinations/nothing/resume", "", 404},
		{"/api/v1/destinations/nothing/start", `{"from":"head"}`, 404},
		{"/api/v1/destinations/metronome/pause", `{"reason":`, 400},
		{"/api/v1/destinations/metronome/pause", `{"reason":"` + strings.Repeat("x", maxPauseReason+1) + `"}`, 400},
		{"/api/v1/destinations/metronome/start", ``, 400},
		{"/api/v1/destinations/metronome/start", `{"from":"yesterday"}`, 400},
		{"/api/v1/destinations/metronome/start", `{"from":"head"}`, 409}, // it delivers: nothing to decide
		{"/api/v1/destinations/metronome/start", `{"from":"cursor"}`, 409},
	} {
		if code, out := v.call("POST", c.path, c.body); code != c.want {
			t.Errorf("POST %s %s: %d %v, want %d", c.path, c.body, code, out, c.want)
		}
	}
	if st := v.sp.DestinationState("metronome"); st.Paused != nil || v.sp.Cursor("metronome") != v.sp.Committed() {
		t.Fatalf("a refused call changed the destination: %+v", st)
	}
}

// removed brings a station to the state the validation found: a destination delivered, was
// taken out of ADAPTERS while the ledger went on (leaving held events
// behind), and the operator looks at it.
func removed(t *testing.T) (v *station, dest, other *sink, before, away map[string][]usage.Event) {
	t.Helper()
	v = newStation(t)
	dest, other = newSink("metronome"), newSink("noop")
	v.gate.hold("metronome", "B200") // its billing platform has no price for B200 yet
	v.boot(dest, other)
	before = v.commit(3, []string{"acme"}, "H100", "B200")
	v.until("first life", func() bool {
		return each(dest.times(before["H100"]), 1) && v.held.Count("metronome") == 3 && v.sp.Cursor("metronome") == v.sp.Committed()
	})
	v.boot(other) // metronome is no longer in ADAPTERS
	away = v.commit(4, []string{"acme"}, "H100")
	v.until("the ledger goes on", func() bool { return v.sp.Cursor("noop") == v.sp.Committed() })
	return v, dest, other, before, away
}

// A destination that is not in ADAPTERS is listed, with its state, cursor,
// backlog and held events, and can be positioned before it is configured
// again. The price check's report says its held events are still there.
func TestDetachedDestinationIsListedAndCanBePositioned(t *testing.T) {
	v, dest, other, before, away := removed(t)
	row := v.row("metronome")
	backlog, _ := row["backlog"].(map[string]any)
	if row == nil || row["state"] != "detached" || num(row["cursor"]) != 9 || num(row["lag_records"]) != 8 || num(row["held_events"]) != 3 ||
		row["detached"] == nil || backlog == nil || num(backlog["records"]) != 8 || num(backlog["held_events"]) != 3 || row["ready"] != false {
		t.Fatalf("a detached destination as listed: %v", row)
	}
	if r := v.row("noop"); r["state"] != "delivering" || r["backlog"] != nil {
		t.Fatalf("the configured destination as listed: %v", r)
	}
	svc := pricing.New(v.sp, []destinations.Destination{other}, v.reg, v.held, nil, nil, pricing.Options{})
	if rep := svc.Report(); rep.HeldEvents != 3 || len(rep.Destinations) != 1 || rep.Destinations[0].Destination != "metronome" {
		t.Fatalf("price check report for a removed destination with held events: %+v", rep)
	}

	// What needs its adapter is refused, with the reason.
	for path, body := range map[string]string{
		"/api/v1/destinations/metronome/pause":               ``,
		"/api/v1/destinations/metronome/start":               `{"from":"head"}`,
		"/api/v1/destinations/metronome/dead-letters/replay": ``,
		"/api/v1/destinations/metronome/cursor":              `{"seq":0}`, // a rewind nothing can check
	} {
		if code, out := v.call("POST", path, body); code != 409 || !strings.Contains(out["error"].(string), "ADAPTERS") {
			t.Errorf("POST %s on a detached destination: %d %v, want 409 naming ADAPTERS", path, code, out)
		}
	}
	if code, out := v.call("POST", "/api/v1/destinations/nothing/cursor", `{"seq":0}`); code != 404 {
		t.Fatalf("cursor of an unknown destination: %d %v", code, out)
	}
	if _, known := v.sp.CursorOf("nothing"); known {
		t.Fatal("a call for an unknown destination created a cursor")
	}

	// Positioned at the head: the held events recorded before go to its dead
	// letters, which can be read, and nothing is held for it any more.
	head := v.sp.Committed()
	code, out := v.call("POST", "/api/v1/destinations/metronome/cursor", `{"from":"`+v.now().Add(time.Second).Format(time.RFC3339)+`"}`)
	if code != 200 || out["destination"] != "metronome" || num(out["cursor"]) != int64(head) || num(out["position"]) != int64(head) || num(out["previous_cursor"]) != 9 ||
		num(out["skipped_held_events"]) != 3 || out["state"] != "detached" || num(out["dead_letters"]) != 3 {
		t.Fatalf("position a detached destination: %d %v", code, out)
	}
	code, out = v.call("GET", "/api/v1/destinations/metronome/dead-letters", "")
	if dls, _ := out["dead_letters"].([]any); code != 200 || len(dls) != 3 || !strings.Contains(dls[0].(map[string]any)["reason"].(string), "skipped at start by the operator") {
		t.Fatalf("dead letters of a detached destination: %d %v", code, out)
	}
	if rep := svc.Report(); rep.HeldEvents != 0 || len(rep.Destinations) != 0 {
		t.Fatalf("price check report after the held events were skipped: %+v", rep)
	}

	// Configured again: it starts where the operator put it, without waiting.
	v.gate.hold("", "")
	v.boot(dest, other)
	after := v.commit(2, []string{"acme"}, "H100")
	v.until("delivers what is new", func() bool { return each(dest.times(after["H100"]), 1) && v.sp.Cursor("metronome") == v.sp.Committed() })
	if r := v.row("metronome"); r["state"] != "delivering" || r["detached"] != nil {
		t.Fatalf("a positioned destination after it came back: %v", r)
	}
	if !each(dest.times(away["H100"]), 0) || !each(dest.times(before["B200"]), 0) {
		t.Fatal("it delivered what was recorded before its position")
	}
	// The skipped held events stay replayable.
	if code, out := v.call("POST", "/api/v1/destinations/metronome/dead-letters/replay", ""); code != 200 || num(out["replayed"]) != 3 || !each(dest.times(before["B200"]), 1) {
		t.Fatalf("replay of the skipped held events: %d %v", code, out)
	}
}

// Configured again with a backlog, a destination awaits its start: listed
// with the numbers, a failing source and a status warning. The decision
// "from head" skips the backlog and moves its held events to the dead
// letters.
func TestReturningDestinationAwaitsItsStartDecision(t *testing.T) {
	v, dest, other, before, away := removed(t)
	v.gate.hold("", "")
	v.boot(dest, other)
	v.until("a failing delivery source", func() bool {
		_, src := v.call("GET", "/api/v1/sources?kind=delivery", "")
		list, _ := src["sources"].([]any)
		return len(list) == 1 && list[0].(map[string]any)["status"] == "failing" && list[0].(map[string]any)["name"] == "delivery/metronome"
	})
	_, status := v.call("GET", "/api/v1/status", "")
	warnings, _ := json.Marshal(status["warnings"])
	if status["healthy"] != false || !strings.Contains(string(warnings), "8 ledger record(s)") || !strings.Contains(string(warnings), "/start") {
		t.Fatalf("status of an install with a destination that awaits its start: healthy %v, warnings %s", status["healthy"], warnings)
	}
	row := v.row("metronome")
	backlog, _ := row["backlog"].(map[string]any)
	if row["state"] != "awaiting_start" || backlog == nil || num(backlog["records"]) != 8 || num(backlog["held_events"]) != 3 ||
		backlog["first_window"] != before["B200"][0].WindowStart.Format(time.RFC3339) || backlog["last_window"] != away["H100"][3].WindowEnd.Format(time.RFC3339) {
		t.Fatalf("a destination that awaits its start as listed: %v", row)
	}
	time.Sleep(30 * time.Millisecond)
	if !each(dest.times(away["H100"]), 0) || !each(dest.times(before["B200"]), 0) {
		t.Fatal("it delivered its backlog without a decision")
	}
	// Dead letters cannot be replayed into a destination that delivers nothing.
	v.sp.AddDeadLetter("metronome", before["H100"][0], "test")
	if code, out := v.call("POST", "/api/v1/destinations/metronome/dead-letters/replay", ""); code != 409 {
		t.Fatalf("replay into a destination that awaits its start: %d %v", code, out)
	}
	v.sp.RemoveDeadLetters("metronome", []string{before["H100"][0].ID})

	head := v.sp.Committed()
	code, out := v.call("POST", "/api/v1/destinations/metronome/start", `{"from":"head"}`)
	decided, _ := out["backlog"].(map[string]any)
	if code != 200 || out["from"] != "head" || out["state"] != "delivering" || num(out["position"]) != int64(head) || num(out["previous_cursor"]) != 9 ||
		num(out["skipped_held_events"]) != 3 || num(out["dead_letters"]) != 3 || decided == nil || num(decided["records"]) != 8 {
		t.Fatalf("start from the head: %d %v", code, out)
	}
	after := v.commit(2, []string{"acme"}, "H100")
	v.until("delivers what is new", func() bool { return each(dest.times(after["H100"]), 1) && v.sp.Cursor("metronome") == v.sp.Committed() })
	if !each(dest.times(away["H100"]), 0) || !each(dest.times(before["B200"]), 0) {
		t.Fatal("the skipped backlog was delivered")
	}
	v.until("no problem left", func() bool {
		_, status := v.call("GET", "/api/v1/status", "")
		return status["healthy"] == true
	})
	if code, out := v.call("POST", "/api/v1/destinations/metronome/start", `{"from":"head"}`); code != 409 {
		t.Fatalf("a second start decision: %d %v", code, out)
	}
}

// The decision "from cursor" delivers everything recorded while the
// destination was removed, held events included.
func TestStartFromTheCursorDeliversTheBacklog(t *testing.T) {
	v, dest, other, before, away := removed(t)
	v.gate.hold("", "")
	v.boot(dest, other)
	code, out := v.call("POST", "/api/v1/destinations/metronome/start", `{"from":"cursor"}`)
	if code != 200 || out["from"] != "cursor" || out["state"] != "delivering" || num(out["skipped_held_events"]) != 0 || num(out["position"]) != 9 || num(out["previous_cursor"]) != 9 {
		t.Fatalf("start from the cursor: %d %v", code, out)
	}
	v.until("the backlog and the held events delivered", func() bool {
		return each(dest.times(away["H100"]), 1) && each(dest.times(before["B200"]), 1) && v.sp.Cursor("metronome") == v.sp.Committed() && v.held.Total("metronome") == 0
	})
	if !each(dest.times(before["H100"]), 1) {
		t.Fatalf("what it had received before was sent again: %v", dest.times(before["H100"]))
	}
}

// The decision from a time starts with what the ledger committed since then;
// the held events recorded before it go to the dead letters.
func TestStartFromATime(t *testing.T) {
	v, dest, other, before, away := removed(t)
	// The third of the four windows recorded while it was away.
	at := away["H100"][2].WindowEnd
	v.gate.hold("", "")
	v.boot(dest, other)
	code, out := v.call("POST", "/api/v1/destinations/metronome/start", `{"from":"`+at.Format(time.RFC3339)+`"}`)
	if code != 200 || out["state"] != "delivering" || num(out["skipped_held_events"]) != 3 || num(out["position"]) != 13 || num(out["previous_cursor"]) != 9 {
		t.Fatalf("start from a time: %d %v", code, out)
	}
	v.until("delivered from that time on", func() bool {
		return each(dest.times(away["H100"][2:]), 1) && v.sp.Cursor("metronome") == v.sp.Committed()
	})
	if !each(dest.times(away["H100"][:2]), 0) || !each(dest.times(before["B200"]), 0) {
		t.Fatalf("it received what was recorded before the time: %v, held %v", dest.times(away["H100"][:2]), dest.times(before["B200"]))
	}
	if dls, _ := v.sp.DeadLetters("metronome"); len(dls) != 3 {
		t.Fatalf("%d dead letters, want the 3 held events recorded before the time", len(dls))
	}
	if st := v.sp.DestinationState("metronome").Start; st == nil || st.Seq != 13 || st.How != spool.StartPositioned {
		t.Fatalf("recorded start: %+v", st)
	}
}

// A replay answers 200 with what the destination took and
// what it still refuses, whichever way the destination refuses: one event it
// still refuses used to fail the whole replay (502) at a destination that
// refuses a batch as a whole, so the events it would take were never sent.
// 502 means the destination could not be asked: nothing changed.
func TestReplayDeliversWhatTheDestinationTakesAndKeepsWhatItRefuses(t *testing.T) {
	v := newStation(t)
	dest := newSink("webhook")
	v.boot(dest)
	evs := v.commit(1, []string{"acme", "globex", "initech"}, "H100")["H100"]
	v.until("delivered", func() bool { return v.sp.Cursor("webhook") == v.sp.Committed() })
	dest.mu.Lock()
	dest.got = map[string]int{}
	dest.refuse[evs[1].ID] = true // still refused
	dest.mu.Unlock()
	for _, e := range evs {
		if err := v.sp.AddDeadLetter("webhook", e, "HTTP 400"); err != nil {
			t.Fatal(err)
		}
	}

	code, out := v.call("POST", "/api/v1/destinations/webhook/dead-letters/replay", "")
	refused, _ := out["refused"].(map[string]any)
	if code != 200 || num(out["replayed"]) != 2 || num(out["still_rejected"]) != 1 || len(refused) != 1 || !strings.Contains(refused[evs[1].ID].(string), "HTTP 400") {
		t.Fatalf("replay with one event still refused: %d %v", code, out)
	}
	if got := dest.times(evs); got[0] != 1 || got[1] != 0 || got[2] != 1 {
		t.Fatalf("delivered %v, want the two the destination takes", got)
	}
	if dls, _ := v.sp.DeadLetters("webhook"); len(dls) != 1 || dls[0].Event.ID != evs[1].ID {
		t.Fatalf("dead letters after the replay: %+v", dls)
	}
	// The same answer when everything is still refused.
	if code, out := v.call("POST", "/api/v1/destinations/webhook/dead-letters/replay", ""); code != 200 || num(out["replayed"]) != 0 || num(out["still_rejected"]) != 1 {
		t.Fatalf("replay of one event that is still refused: %d %v", code, out)
	}

	// A destination that cannot be reached: 502, everything kept.
	dest.mu.Lock()
	dest.down = errors.New("HTTP 503: service unavailable")
	dest.mu.Unlock()
	code, out = v.call("POST", "/api/v1/destinations/webhook/dead-letters/replay", "")
	if code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "replay failed, dead letters kept") {
		t.Fatalf("replay into a destination that is down: %d %v", code, out)
	}
	if dls, _ := v.sp.DeadLetters("webhook"); len(dls) != 1 {
		t.Fatalf("a failed replay removed dead letters: %+v", dls)
	}

	// Fixed: the last one is delivered, and an empty list answers 0.
	dest.mu.Lock()
	dest.down, dest.refuse = nil, map[string]bool{}
	dest.mu.Unlock()
	if code, out := v.call("POST", "/api/v1/destinations/webhook/dead-letters/replay", ""); code != 200 || num(out["replayed"]) != 1 || num(out["still_rejected"]) != 0 {
		t.Fatalf("replay after the fix: %d %v", code, out)
	}
	if code, out := v.call("POST", "/api/v1/destinations/webhook/dead-letters/replay", ""); code != 200 || num(out["replayed"]) != 0 {
		t.Fatalf("replay of nothing: %d %v", code, out)
	}
}

// A replay sets tenants up first, as delivery does: the dead letters of a
// tenant the destination still cannot set up are kept, not sent to where
// the tenant does not exist. A paused destination takes no replay either.
func TestReplayKeepsDeadLettersOfATenantThatIsNotSetUp(t *testing.T) {
	v := newStation(t)
	dest := newSink("lago")
	dest.ensure["globex"] = errors.New("HTTP 404: no room for another customer")
	v.reg.Upsert(usage.Tenant{ID: "acme"})
	v.reg.Upsert(usage.Tenant{ID: "globex"})
	v.boot(dest)
	evs := v.commit(1, []string{"acme", "globex"}, "H100")["H100"]
	v.until("acme delivered, globex parked", func() bool { return dest.times(evs)[0] == 1 && v.held.ParkedCount("lago") == 1 })
	row := v.row("lago")
	tenants, _ := row["tenants_not_set_up"].([]any)
	if num(row["parked_events"]) != 1 || num(row["blocked_tenants"]) != 1 || len(tenants) != 1 || tenants[0].(map[string]any)["tenant"] != "globex" ||
		!strings.Contains(tenants[0].(map[string]any)["error"].(string), "no room") {
		t.Fatalf("a destination with a tenant it cannot set up as listed: %v", row)
	}
	_, src := v.call("GET", "/api/v1/sources?kind=provisioning", "")
	if list, _ := src["sources"].([]any); len(list) != 1 || list[0].(map[string]any)["name"] != "provisioning/lago/globex" || list[0].(map[string]any)["status"] != "failing" {
		t.Fatalf("provisioning sources: %v", src)
	}

	for _, e := range evs {
		v.sp.AddDeadLetter("lago", e, "test")
	}
	dest.mu.Lock()
	dest.got = map[string]int{}
	dest.mu.Unlock()
	code, out := v.call("POST", "/api/v1/destinations/lago/dead-letters/replay", "")
	waits, _ := out["tenants_not_set_up"].(map[string]any)
	if code != 200 || num(out["replayed"]) != 1 || num(out["still_rejected"]) != 1 || len(waits) != 1 || !strings.Contains(waits["globex"].(string), "no room") {
		t.Fatalf("replay with a tenant that is not set up: %d %v", code, out)
	}
	if got := dest.times(evs); got[0] != 1 || got[1] != 0 {
		t.Fatalf("replayed %v, want only the tenant that is set up", got)
	}

	if code, out := v.call("POST", "/api/v1/destinations/lago/pause", ""); code != 200 {
		t.Fatalf("pause: %d %v", code, out)
	}
	if code, out := v.call("POST", "/api/v1/destinations/lago/dead-letters/replay", ""); code != 409 || !strings.Contains(out["error"].(string), "paused") {
		t.Fatalf("replay into a paused destination: %d %v", code, out)
	}
}
