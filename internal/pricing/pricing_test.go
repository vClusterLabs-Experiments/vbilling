package pricing

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// platform is a billing adapter whose prices the test controls.
type platform struct {
	name string

	mu       sync.Mutex
	status   map[string]string // sku -> coverage status; missing = unpriced
	fail     bool
	checks   int
	prepared []string
	got      map[string]int
	sum      map[string]float64 // sku -> quantity received
	refresh  bool
	slow     time.Duration
}

func newPlatform(name string, priced ...string) *platform {
	p := &platform{name: name, status: map[string]string{}, got: map[string]int{}, sum: map[string]float64{}}
	for _, sku := range priced {
		p.status[sku] = destinations.PriceCovered
	}
	return p
}

func (p *platform) Name() string                                       { return p.name }
func (p *platform) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (p *platform) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (p *platform) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (p *platform) Accepts(metric string) bool                         { return destinations.BillableOnly(metric) }
func (p *platform) PriceDims(string) []string                          { return nil }
func (p *platform) RefreshTenantsOnPriceChange() bool                  { return p.refresh }
func (p *platform) PricePlan(t usage.Tenant) string {
	if t.Plan != "" {
		return t.Plan
	}
	return "standard"
}

func (p *platform) SendEvents(_ context.Context, evs []usage.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range evs {
		p.got[e.ID]++
		p.sum[e.SKU] += e.Quantity
	}
	return nil
}

func (p *platform) PrepareSeries(_ context.Context, series []destinations.PriceSeries) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range series {
		p.prepared = append(p.prepared, s.SKU)
	}
	return nil
}

func (p *platform) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	if p.slow > 0 {
		time.Sleep(p.slow)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	if p.fail {
		return nil, errors.New("billing platform unavailable")
	}
	var out []destinations.PriceCoverage
	for _, s := range series {
		c := destinations.PriceCoverage{Series: s, Status: destinations.PriceCovered, Detail: "rate found"}
		if !s.Catalog && s.Metric == usage.MetricGPUHours {
			st, ok := p.status[s.SKU]
			if !ok {
				st = destinations.PriceMissing
			}
			c.Status = st
			if st == destinations.PriceMissing {
				c.Detail, c.Hint = "no rate for "+s.SKU, "add a rate for product \"GPU Hours\" sku="+s.SKU
			}
		}
		if s.Catalog && s.Metric == usage.MetricLBHours {
			c.Status, c.Detail, c.Hint = destinations.PriceMissing, "no product", "create a product for LB hours"
		}
		out = append(out, c)
	}
	return out, nil
}

func (p *platform) price(sku, status string) {
	p.mu.Lock()
	p.status[sku] = status
	p.mu.Unlock()
}

func (p *platform) received(sku string) (n int, sum float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got), p.sum[sku]
}

var t0 = time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

func commit(t *testing.T, s *spool.Spool, windows int, skus ...string) map[string]float64 {
	t.Helper()
	start := t0
	if wm, ok := s.Watermark(spool.SourceCollector); ok {
		start = wm
	}
	sums := map[string]float64{}
	for w := 0; w < windows; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		var evs []usage.Event
		for i, sku := range skus {
			e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: sku, Quantity: float64(w+1) + float64(i)/10,
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: time.Now()}
			e.Finalize()
			evs = append(evs, e)
			sums[sku] += e.Quantity
		}
		if err := s.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
	}
	return sums
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func row(rep Report, dest, sku string) *Row {
	for _, d := range rep.Destinations {
		if d.Destination != dest {
			continue
		}
		for i := range d.Series {
			if d.Series[i].Metric == usage.MetricGPUHours && d.Series[i].SKU == sku {
				return &d.Series[i]
			}
		}
	}
	return nil
}

func openSpool(t *testing.T, dir string) *spool.Spool {
	t.Helper()
	s, err := spool.Open(dir, spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWarnModeReportsGapsAndClears(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	commit(t, sp, 3, "H100", "B200")
	p := newPlatform("metronome", "H100")
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{p}, nil, nil, h, tel, Options{Debounce: 10 * time.Millisecond})
	if svc.Gate() != nil {
		t.Fatal("warn mode installs no gate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)

	eventually(t, "startup check", func() bool { r := row(svc.Report(), "metronome", "B200"); return r != nil && r.Status != "unknown" })
	rep := svc.Report()
	b := row(rep, "metronome", "B200")
	if b.Status != destinations.PriceMissing || !strings.Contains(b.Hint, `product "GPU Hours"`) || b.Plan != "standard" {
		t.Fatalf("B200 row: %+v", b)
	}
	if r := row(rep, "metronome", "H100"); r.Status != destinations.PriceCovered {
		t.Fatalf("H100 row: %+v", r)
	}
	var lb *Row
	for i, r := range rep.Destinations[0].Series {
		if r.Metric == usage.MetricLBHours {
			lb = &rep.Destinations[0].Series[i]
		}
	}
	if lb == nil || !lb.NoUsageYet || lb.Status != destinations.PriceMissing {
		t.Fatalf("catalog metric without usage: %+v", lb)
	}
	if rep.OK || rep.Unpriced != 2 {
		t.Fatalf("report %+v", rep)
	}
	if v := tel.Get("vbilling_unpriced_series", map[string]string{"destination": "metronome"}); v != 2 {
		t.Fatalf("gauge %v", v)
	}
	var gap *health.Source
	for _, src := range h.Problems() {
		if src.Name == "pricing/metronome/standard/vcluster_gpu_hours/B200" {
			src := src
			gap = &src
		}
	}
	if gap == nil || gap.Status != health.Unpriced || !strings.Contains(gap.Hint, "sku=B200") {
		t.Fatalf("health: %+v", h.Problems())
	}
	p.mu.Lock()
	prepared := strings.Join(p.prepared, ",")
	p.mu.Unlock()
	if !strings.Contains(prepared, "B200") {
		t.Errorf("new series were not prepared: %s", prepared)
	}

	// A new SKU appears in the ledger: checked right away, not in 10 minutes.
	before := p.checks
	commit(t, sp, 1, "L40S")
	eventually(t, "check of the new SKU", func() bool {
		r := row(svc.Report(), "metronome", "L40S")
		return r != nil && r.Status == destinations.PriceMissing
	})
	if p.checks <= before {
		t.Fatal("no check after the new SKU")
	}

	// Finance adds the rates.
	p.price("B200", destinations.PriceCovered)
	p.price("L40S", destinations.PriceZero)
	rep = svc.CheckNow(ctx)
	if r := row(rep, "metronome", "B200"); r.Status != destinations.PriceCovered {
		t.Fatalf("after pricing: %+v", r)
	}
	if r := row(rep, "metronome", "L40S"); r.Status != destinations.PriceZero {
		t.Fatalf("zero is information: %+v", r)
	}
	for _, src := range h.List() {
		if src.Name == "pricing/metronome/standard/vcluster_gpu_hours/B200" && src.Status != health.OK {
			t.Fatalf("gap source did not recover: %+v", src)
		}
	}
	if v := tel.Get("vbilling_unpriced_series", map[string]string{"destination": "metronome"}); v != 1 { // LB hours
		t.Fatalf("gauge after pricing %v", v)
	}
}

func TestHoldModeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	sp := openSpool(t, filepath.Join(dir, "ledger"))
	held, err := pipeline.OpenHeld(filepath.Join(dir, "held"), true)
	if err != nil {
		t.Fatal(err)
	}
	p := newPlatform("lago", "H100")
	p.refresh = true
	tenants := pipeline.NewTenantRegistry()
	tenants.Upsert(usage.Tenant{ID: "acme"})
	tenants.MarkEnsured("lago", usage.Tenant{ID: "acme"})
	tel := telemetry.New()
	svc := New(sp, []destinations.Destination{p}, tenants, held, nil, tel, Options{Mode: ModeHold, Debounce: 10 * time.Millisecond})
	disp := pipeline.New(sp, []destinations.Destination{p}, pipeline.Options{
		Held: held, Gate: svc.Gate(), BatchSize: 5,
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, IdlePoll: 10 * time.Millisecond,
	}, tel)
	sp.Cursor("lago")
	ledger := commit(t, sp, 8, "H100", "B200")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	done := make(chan struct{})
	go func() { disp.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	eventually(t, "priced usage delivered and unpriced usage held", func() bool {
		n, _ := p.received("H100")
		return n == 8 && held.Count("lago") == 8 && sp.Cursor("lago") == sp.Committed()
	})
	if _, b := p.received("B200"); b != 0 {
		t.Fatal("unpriced usage reached the billing platform")
	}
	rep := svc.Report()
	if r := row(rep, "lago", "B200"); r.HeldEvents != 8 || r.Status != destinations.PriceMissing {
		t.Fatalf("report row: %+v", r)
	}
	if rep.HeldEvents != 8 || rep.OK {
		t.Fatalf("report: %+v", rep)
	}

	// More usage while the price is missing, then the price appears.
	more := commit(t, sp, 4, "H100", "B200")
	eventually(t, "new usage held", func() bool { return held.Count("lago") == 12 })
	p.price("B200", destinations.PriceCovered)
	svc.CheckNow(ctx)
	eventually(t, "held usage released", func() bool { n, _ := p.received("B200"); return n == 24 && held.Count("lago") == 0 })
	_, b := p.received("B200")
	if usage.Round(b) != usage.Round(ledger["B200"]+more["B200"]) {
		t.Fatalf("B200 at the platform %v != ledger %v", b, ledger["B200"]+more["B200"])
	}
	p.mu.Lock()
	for id, n := range p.got {
		if n != 1 {
			t.Fatalf("%s delivered %d times", id, n)
		}
	}
	p.mu.Unlock()
	if !tenants.NeedsEnsure("lago", usage.Tenant{ID: "acme"}) {
		t.Error("tenants were not re-ensured after a price appeared")
	}
}

func TestHoldModeNewPricedSeriesIsNotHeld(t *testing.T) {
	dir := t.TempDir()
	sp := openSpool(t, filepath.Join(dir, "ledger"))
	held, _ := pipeline.OpenHeld(filepath.Join(dir, "held"), true)
	p := newPlatform("metronome", "H100", "L40S")
	svc := New(sp, []destinations.Destination{p}, nil, held, nil, nil, Options{Mode: ModeHold})
	g := svc.Gate()
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "L40S", Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
	ev.Finalize()
	info := usage.Event{Tenant: "acme", Metric: usage.MetricGPUUtilization, Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
	ds := g.Decide(context.Background(), "metronome", []usage.Event{ev, info})
	if ds[0].Hold || ds[0].Series == "" {
		t.Fatalf("a new SKU that is priced must not wait: %+v", ds[0])
	}
	if ds[1].Series != "" || ds[1].Hold {
		t.Fatalf("informational metrics are not gated: %+v", ds[1])
	}
	if g.Decide(context.Background(), "webhook", []usage.Event{ev}) != nil {
		t.Fatal("a destination without price checks is not gated")
	}
}

func TestHoldModeCheckFailureHoldsUnconfirmedOnly(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	p := newPlatform("stripe", "H100")
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{p}, nil, nil, h, nil, Options{Mode: ModeHold, DecideWait: time.Second})
	g := svc.Gate()
	mk := func(sku string) usage.Event {
		e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: sku, Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
		e.Finalize()
		return e
	}
	if d := g.Decide(context.Background(), "stripe", []usage.Event{mk("H100")}); d[0].Hold {
		t.Fatal("priced series held")
	}
	p.mu.Lock()
	p.fail = true
	p.mu.Unlock()
	d := g.Decide(context.Background(), "stripe", []usage.Event{mk("H100"), mk("NEW")})
	if d[0].Hold {
		t.Error("a series confirmed priced is not held because a later check failed")
	}
	if !d[1].Hold || !strings.Contains(d[1].Reason, "last check failed") {
		t.Errorf("a series never confirmed is held while checks fail: %+v", d[1])
	}
	checks := p.checks
	g.Decide(context.Background(), "stripe", []usage.Event{mk("NEW")})
	if p.checks != checks {
		t.Error("delivery waited for another check of a series already attempted")
	}
	var failing bool
	for _, s := range h.Problems() {
		failing = failing || (s.Name == "pricing/stripe" && s.Status == health.Failing)
	}
	if !failing {
		t.Errorf("health: %+v", h.Problems())
	}
	p.mu.Lock()
	p.fail = false
	p.status["NEW"] = destinations.PriceUnknown
	p.mu.Unlock()
	svc.CheckAll(context.Background())
	if d := g.Decide(context.Background(), "stripe", []usage.Event{mk("NEW")}); d[0].Hold {
		t.Error("a series the check cannot decide (unknown) is not held")
	}
	if !g.Releasable(context.Background(), "stripe", "no-such-key", ptr(mk("H100"))) {
		t.Error("an unknown held key resolves through its sample event")
	}
}

func ptr(e usage.Event) *usage.Event { return &e }

func TestDecideWaitIsBounded(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	p := newPlatform("metronome", "H100")
	p.slow = 300 * time.Millisecond
	svc := New(sp, []destinations.Destination{p}, nil, nil, nil, nil, Options{Mode: ModeHold, DecideWait: 20 * time.Millisecond})
	e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "H100", Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
	e.Finalize()
	start := time.Now()
	d := svc.Gate().Decide(context.Background(), "metronome", []usage.Event{e})
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("Decide waited %s", time.Since(start))
	}
	if !d[0].Hold {
		t.Fatal("unconfirmed series must be held while its check runs")
	}
	ch := svc.Gate().Changed()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the gate did not signal when the slow check confirmed the price")
	}
}

func TestSeriesFollowTenantPlans(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	p := newPlatform("metronome", "H100")
	reg := pipeline.NewTenantRegistry()
	svc := New(sp, []destinations.Destination{p}, reg, nil, nil, nil, Options{})
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "H100", Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
	ev.Finalize()
	other := ev
	other.Tenant = "globex"
	svc.Observe([]usage.Event{ev, other}, time.Now()) // before discovery: both on the default plan
	if r := row(svc.CheckNow(context.Background()), "metronome", "H100"); r == nil || r.Plan != "standard" {
		t.Fatalf("before discovery: %+v", r)
	}
	reg.Upsert(usage.Tenant{ID: "acme", Plan: "enterprise"})
	rep := svc.CheckNow(context.Background())
	plans := map[string]bool{}
	for _, r := range rep.Destinations[0].Series {
		if r.Metric == usage.MetricGPUHours && !r.NoUsageYet {
			plans[r.Plan] = true
		}
	}
	if !plans["enterprise"] || !plans["standard"] || len(plans) != 2 {
		t.Fatalf("acme should move to enterprise, globex stay on standard: %v", plans)
	}
	reg.Upsert(usage.Tenant{ID: "globex", Plan: "enterprise"})
	rep = svc.CheckNow(context.Background())
	for _, r := range rep.Destinations[0].Series {
		if r.Metric == usage.MetricGPUHours && r.Plan == "standard" && !r.NoUsageYet {
			t.Fatalf("a series with no tenant left on its plan lingers: %+v", r)
		}
	}
}

func TestReleasedUnpricedTurnsHealthFailing(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	p := newPlatform("metronome", "H100")
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{p}, nil, nil, h, nil, Options{Mode: ModeHold, MaxHold: 28 * 24 * time.Hour})
	ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "B200", Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute)}
	ev.Finalize()
	g := svc.Gate()
	d := g.Decide(context.Background(), "metronome", []usage.Event{ev})
	if !d[0].Hold {
		t.Fatal("unpriced series not held")
	}
	g.ReleasedUnpriced("metronome", d[0].Series, &ev, 4)
	var src *health.Source
	for _, x := range h.List() {
		if x.Name == "pricing/metronome/standard/vcluster_gpu_hours/B200" {
			x := x
			src = &x
		}
	}
	if src == nil || src.Status != health.Failing || !strings.Contains(src.Message, "WITHOUT A PRICE") || !strings.Contains(src.Hint, "Finance must add the price") {
		t.Fatalf("health after a release without a price: %+v", src)
	}
	if r := row(svc.Report(), "metronome", "B200"); r.ReleasedUnpriced != 4 || r.ReleasedUnpricedAt.IsZero() {
		t.Fatalf("row %+v", r)
	}
	p.price("B200", destinations.PriceCovered)
	svc.CheckNow(context.Background())
	for _, x := range h.List() {
		if x.Name == "pricing/metronome/standard/vcluster_gpu_hours/B200" && x.Status != health.OK {
			t.Fatalf("still %s after the price was added", x.Status)
		}
	}
}
