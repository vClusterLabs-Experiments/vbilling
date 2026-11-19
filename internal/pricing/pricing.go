// Package pricing checks that every billing platform vBilling ships usage to
// has a price for that usage.
//
// A billing platform that receives usage it has no price for bills it at $0
// without an error: a new SKU, region or metric that reaches Metronome before
// Finance adds its rate (a "rate miss") silently undercounts invoices, and
// prepaid balances and spend alerts never see it. vBilling keeps no price list
// of its own. It asks each billing adapter, read-only, whether the platform
// prices every billable metric in the catalog and every series (metric, SKU
// and other priced dimensions) seen in the ledger recently, reports gaps in
// GET /api/v1/pricing, the source health registry and /metrics, and with
// PRICE_CHECK=hold keeps the events of an unpriced series out of that
// destination until the price exists (see pipeline.HeldStore).
package pricing

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Modes.
const (
	ModeOff  = "off"
	ModeWarn = "warn"
	ModeHold = "hold"
)

// Options tune the checks.
type Options struct {
	Mode     string        // warn (default) or hold
	Interval time.Duration // periodic check of every destination (default 10m)
	Lookback time.Duration // series seen in the ledger this recently are checked (default 24h)
	Timeout  time.Duration // one check of one destination (default 1m)
	// DecideWait bounds how long delivery waits for the first check of a new
	// series in hold mode before holding its events (default 20s).
	DecideWait time.Duration
	// Debounce coalesces the checks new series trigger (default 2s).
	Debounce time.Duration
	// MaxHold is how long usage is held at most, for messages (the dispatcher enforces it).
	MaxHold time.Duration
	// Ready reports when tenants (and so their plans) are known; the first
	// full check waits for it, up to ReadyWait (default 2m).
	Ready     func() bool
	ReadyWait time.Duration
}

func (o *Options) defaults() {
	if o.Mode == "" {
		o.Mode = ModeWarn
	}
	if o.Interval <= 0 {
		o.Interval = 10 * time.Minute
	}
	if o.Lookback <= 0 {
		o.Lookback = 24 * time.Hour
	}
	if o.Timeout <= 0 {
		o.Timeout = time.Minute
	}
	if o.DecideWait <= 0 {
		o.DecideWait = 20 * time.Second
	}
	if o.Debounce <= 0 {
		o.Debounce = 2 * time.Second
	}
	if o.ReadyWait <= 0 {
		o.ReadyWait = 2 * time.Minute
	}
}

// Service runs price coverage checks and, in hold mode, is the dispatcher's
// pipeline.Gate.
type Service struct {
	spool   *spool.Spool
	tenants *pipeline.TenantRegistry
	held    *pipeline.HeldStore // nil without hold mode and leftovers
	health  *health.Registry
	tel     *telemetry.Registry
	opts    Options
	now     func() time.Time

	order []string
	dests map[string]*destState

	mu      sync.Mutex // guards series maps, changed, scanned
	changed chan struct{}
	scanned uint64
	kick    chan string
	ctx     context.Context // set by Run, for checks started outside it
}

type destState struct {
	name string
	pc   destinations.PriceChecker
	dest destinations.Destination

	checkMu sync.Mutex // one check of a destination at a time

	// guarded by Service.mu
	series      map[string]*seriesState
	prepared    map[string]bool
	lastCheck   time.Time
	lastSuccess time.Time
	lastErr     string
	retryAt     time.Time
	gapSources  map[string]bool // health sources reported unpriced
}

type seriesState struct {
	s         destinations.PriceSeries
	lastSeen  time.Time
	status    string // "" until a check answered for it
	detail    string
	hint      string
	checkedAt time.Time
	attempted bool // a check ran (or failed) since the series appeared
	// releasedUnpriced counts held events released without a price after
	// MaxHold (since the start), at releasedAt.
	releasedUnpriced int
	releasedAt       time.Time
	// tenants that produced the series: a tenant whose plan changed (or
	// became known after discovery) moves to the series of its new plan.
	tenants map[string]bool
}

// staleHeld is when held usage gets a warning about backdating limits.
const staleHeld = 7 * 24 * time.Hour

// heldRecheck is how often a destination with held events is checked.
const heldRecheck = time.Minute

// maxTenantsPerSeries bounds the tenants a series remembers. A series with
// more stays under the plan it was first seen with.
const maxTenantsPerSeries = 2000

// New builds the service for the destinations that can check prices.
func New(sp *spool.Spool, dests []destinations.Destination, tenants *pipeline.TenantRegistry, held *pipeline.HeldStore,
	h *health.Registry, tel *telemetry.Registry, opts Options) *Service {
	opts.defaults()
	if tel == nil {
		tel = telemetry.New()
	}
	if tenants == nil {
		tenants = pipeline.NewTenantRegistry()
	}
	s := &Service{spool: sp, tenants: tenants, held: held, health: h, tel: tel, opts: opts, now: time.Now,
		dests: map[string]*destState{}, changed: make(chan struct{}), kick: make(chan string, 64), ctx: context.Background()}
	for _, d := range dests {
		pc, ok := d.(destinations.PriceChecker)
		if !ok {
			continue
		}
		s.order = append(s.order, d.Name())
		s.dests[d.Name()] = &destState{name: d.Name(), pc: pc, dest: d, series: map[string]*seriesState{}, prepared: map[string]bool{}, gapSources: map[string]bool{}}
	}
	return s
}

// SetReady sets Options.Ready (the controller exists after the service).
func (s *Service) SetReady(ready func() bool) { s.opts.Ready = ready }

// Mode returns warn or hold.
func (s *Service) Mode() string { return s.opts.Mode }

// Enabled reports whether any destination can check prices.
func (s *Service) Enabled() bool { return len(s.order) > 0 }

// priced reports whether a metric is one a billing platform prices.
func priced(ds *destState, metric string) bool {
	return destinations.BillableOnly(metric) && destinations.Accepts(ds.dest, metric)
}

// observeLocked records that an event's series exists at ds and returns its
// key; isNew is true the first time the series is seen.
func (s *Service) observeLocked(ds *destState, ev *usage.Event, seen time.Time) (key string, isNew bool) {
	t, _ := s.tenants.Get(ev.Tenant)
	ser := destinations.SeriesOf(ds.pc, ds.pc.PricePlan(t), ev)
	key = ser.Key()
	st := ds.series[key]
	if st == nil {
		st = &seriesState{s: ser, tenants: map[string]bool{}}
		ds.series[key] = st
		isNew = true
	}
	if seen.After(st.lastSeen) {
		st.lastSeen = seen
	}
	if st.tenants != nil && !st.tenants[ev.Tenant] {
		if len(st.tenants) < maxTenantsPerSeries {
			st.tenants[ev.Tenant] = true
		} else {
			st.tenants = nil // too many to follow plan changes
		}
	}
	return key, isNew
}

// rekeyLocked moves tenants whose plan is not the one their series was seen
// under (plans become known after discovery, and can change) to the series
// of their current plan, and drops series no tenant is left in.
func (s *Service) rekeyLocked(ds *destState) {
	for key, st := range ds.series {
		if st.s.Catalog || st.tenants == nil {
			continue
		}
		for id := range st.tenants {
			t, _ := s.tenants.Get(id)
			plan := ds.pc.PricePlan(t)
			if plan == st.s.Plan {
				continue
			}
			moved := st.s
			moved.Plan = plan
			mk := moved.Key()
			dst := ds.series[mk]
			if dst == nil {
				dst = &seriesState{s: moved, tenants: map[string]bool{}}
				ds.series[mk] = dst
			}
			dst.releasedUnpriced += st.releasedUnpriced
			if st.releasedAt.After(dst.releasedAt) {
				dst.releasedAt = st.releasedAt
			}
			if st.lastSeen.After(dst.lastSeen) {
				dst.lastSeen = st.lastSeen
			}
			if dst.tenants != nil && len(dst.tenants) < maxTenantsPerSeries {
				dst.tenants[id] = true
			}
			delete(st.tenants, id)
		}
		if len(st.tenants) == 0 {
			delete(ds.series, key)
			delete(ds.prepared, key)
		}
	}
}

// Observe registers the series of events appended to the ledger and starts
// a check for destinations that see a series for the first time.
func (s *Service) Observe(events []usage.Event, at time.Time) {
	kicks := map[string]bool{}
	s.mu.Lock()
	for i := range events {
		ev := &events[i]
		for _, name := range s.order {
			ds := s.dests[name]
			if !priced(ds, ev.Metric) {
				continue
			}
			if _, isNew := s.observeLocked(ds, ev, at); isNew {
				kicks[name] = true
			}
		}
	}
	s.mu.Unlock()
	for name := range kicks {
		s.Kick(name)
	}
}

// Kick asks for a check of one destination soon.
func (s *Service) Kick(name string) {
	select {
	case s.kick <- name:
	default: // a check is queued already
	}
}

// Run scans recent ledger usage, checks every destination at startup and then
// every Interval, follows the ledger for new series, and checks a destination
// soon after one appears.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	// Series seen recently: everything appended within the lookback.
	start := s.spool.Committed()
	now := s.now()
	err := s.spool.ScanAppended(now.Add(-s.opts.Lookback), now.Add(time.Hour), func(ev *usage.Event, at time.Time) error {
		s.observeQuiet(ev, at)
		return nil
	})
	if err != nil {
		log.Printf("[pricing] scan recent usage: %v", err)
	}
	s.mu.Lock()
	s.scanned = start
	s.mu.Unlock()
	changed := s.spool.Changed() // taken before reading, so no append is missed
	s.follow()
	// Plans are known once tenants are discovered: the first full check
	// waits for that (series seen meanwhile move to their plan's series).
	s.waitReady(ctx)
	log.Printf("[pricing] PRICE_CHECK=%s: checking %s every %s (usage of the last %s)",
		s.opts.Mode, strings.Join(s.order, ", "), s.opts.Interval, s.opts.Lookback)
	s.CheckAll(ctx)

	tick := time.NewTicker(s.opts.Interval)
	defer tick.Stop()
	retry := time.NewTicker(15 * time.Second)
	defer retry.Stop()
	pending := map[string]bool{}
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.CheckAll(ctx)
		case <-changed:
			changed = s.spool.Changed()
			s.follow()
		case name := <-s.kick:
			pending[name] = true
			if debounce == nil {
				debounce = time.After(s.opts.Debounce)
			}
		case <-debounce:
			debounce = nil
			for _, name := range s.order {
				if pending[name] {
					s.check(ctx, s.dests[name])
				}
			}
			pending = map[string]bool{}
		case <-retry.C:
			// A destination whose last check failed is retried after a
			// minute, and one with held events is checked every minute, so
			// held usage ships soon after its price appears.
			for _, name := range s.order {
				ds := s.dests[name]
				s.mu.Lock()
				due := !ds.retryAt.IsZero() && !s.now().Before(ds.retryAt)
				due = due || (s.held != nil && s.held.Count(name) > 0 && s.now().Sub(ds.lastCheck) >= heldRecheck)
				s.mu.Unlock()
				if due {
					s.check(ctx, ds)
				}
			}
		}
	}
}

func (s *Service) waitReady(ctx context.Context) {
	if s.opts.Ready == nil {
		return
	}
	deadline := time.Now().Add(s.opts.ReadyWait)
	for !s.opts.Ready() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
			s.follow()
		}
	}
}

// observeQuiet records a series without kicking a check (startup scan).
func (s *Service) observeQuiet(ev *usage.Event, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.order {
		ds := s.dests[name]
		if priced(ds, ev.Metric) {
			s.observeLocked(ds, ev, at)
		}
	}
}

// follow reads ledger records appended since the last call.
func (s *Service) follow() {
	for {
		s.mu.Lock()
		after := s.scanned
		s.mu.Unlock()
		recs, err := s.spool.Read(after, 1000)
		if err != nil {
			log.Printf("[pricing] read ledger: %v", err)
			return
		}
		if len(recs) == 0 {
			return
		}
		var evs []usage.Event
		for _, r := range recs {
			if r.Type == spool.TypeEvent && r.Event != nil {
				evs = append(evs, *r.Event)
			}
		}
		at := recs[len(recs)-1].At
		if at.IsZero() {
			at = s.now()
		}
		s.Observe(evs, at)
		s.mu.Lock()
		s.scanned = recs[len(recs)-1].Seq
		s.mu.Unlock()
	}
}

// CheckAll checks every destination now.
func (s *Service) CheckAll(ctx context.Context) {
	for _, name := range s.order {
		s.check(ctx, s.dests[name])
	}
}

// snapshot returns the series to check: recent ones, plus a catalog entry for
// each billable metric with no recent usage under a plan in use.
func (s *Service) snapshotLocked(ds *destState) []destinations.PriceSeries {
	s.rekeyLocked(ds)
	cutoff := s.now().Add(-s.opts.Lookback)
	plans := map[string]bool{ds.pc.PricePlan(usage.Tenant{}): true}
	seen := map[[2]string]bool{}
	var out []destinations.PriceSeries
	for key, st := range ds.series {
		if st.s.Catalog {
			continue
		}
		if st.lastSeen.Before(cutoff) {
			delete(ds.series, key)
			delete(ds.prepared, key)
			continue
		}
		plans[st.s.Plan] = true
		seen[[2]string{st.s.Plan, st.s.Metric}] = true
		out = append(out, st.s)
	}
	for _, t := range s.tenants.All() {
		plans[ds.pc.PricePlan(t)] = true
	}
	want := map[string]bool{}
	for plan := range plans {
		for _, def := range usage.Billable() {
			if !priced(ds, def.Code) || seen[[2]string{plan, def.Code}] {
				continue
			}
			c := destinations.PriceSeries{Plan: plan, Metric: def.Code, Catalog: true}
			want[c.Key()] = true
			if ds.series[c.Key()] == nil {
				ds.series[c.Key()] = &seriesState{s: c}
			}
			out = append(out, c)
		}
	}
	for key, st := range ds.series {
		if st.s.Catalog && !want[key] {
			delete(ds.series, key) // the metric has usage now, or the plan is gone
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// check runs one check of ds and records the result.
func (s *Service) check(ctx context.Context, ds *destState) {
	ds.checkMu.Lock()
	defer ds.checkMu.Unlock()

	s.mu.Lock()
	list := s.snapshotLocked(ds)
	var fresh []destinations.PriceSeries
	for _, ser := range list {
		if !ser.Catalog && !ds.prepared[ser.Key()] {
			fresh = append(fresh, ser)
		}
	}
	s.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	if prep, ok := ds.dest.(destinations.SeriesPreparer); ok && len(fresh) > 0 {
		if err := prep.PrepareSeries(cctx, fresh); err != nil {
			log.Printf("[pricing] %s: prepare new series: %v", ds.name, err)
		} else {
			s.mu.Lock()
			for _, ser := range fresh {
				ds.prepared[ser.Key()] = true
			}
			s.mu.Unlock()
		}
	}
	covs, err := ds.pc.CheckPrices(cctx, list)
	now := s.now().UTC()
	labels := map[string]string{"destination": ds.name}
	s.tel.Add("vbilling_price_checks_total", "Price coverage checks of a billing platform.", labels, 1)

	s.mu.Lock()
	ds.lastCheck = now
	for _, ser := range list {
		if st := ds.series[ser.Key()]; st != nil {
			st.attempted = true
		}
	}
	if err != nil {
		ds.lastErr = err.Error()
		ds.retryAt = now.Add(time.Minute)
		s.mu.Unlock()
		s.tel.Add("vbilling_price_check_errors_total", "Price coverage checks that could not read a billing platform.", labels, 1)
		if s.health != nil {
			s.health.Fail(health.Name("pricing", ds.name), "pricing", ds.name, err,
				"vBilling could not read prices from "+ds.name+"; nothing was concluded (series never checked stay held in hold mode)")
		}
		log.Printf("[pricing] %s: check failed: %v", ds.name, err)
		return
	}
	ds.lastErr, ds.lastSuccess, ds.retryAt = "", now, time.Time{}
	var released, newlyPriced bool
	type transition struct {
		ser          destinations.PriceSeries
		from, to     string
		detail, hint string
	}
	var moves []transition
	for _, c := range covs {
		st := ds.series[c.Series.Key()]
		if st == nil {
			continue
		}
		prev := st.status
		st.status, st.detail, st.hint, st.checkedAt = c.Status, c.Detail, c.Hint, now
		if prev != c.Status {
			moves = append(moves, transition{c.Series, prev, c.Status, c.Detail, c.Hint})
		}
		if !c.Series.Catalog && (prev == "" || prev == destinations.PriceMissing) && c.Status != destinations.PriceMissing {
			released = true
			if c.Status == destinations.PriceCovered || c.Status == destinations.PriceZero {
				newlyPriced = true
			}
		}
	}
	if released {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	s.mu.Unlock()

	for _, m := range moves {
		if m.ser.Catalog && m.from == "" && m.to != destinations.PriceMissing {
			continue
		}
		switch {
		case m.to == destinations.PriceMissing:
			log.Printf("[pricing] %s: %s%s is unpriced: %s (hint: %s)", ds.name, destinations.DescribeSeries(m.ser), planSuffix(m.ser.Plan), m.detail, m.hint)
		case m.from == destinations.PriceMissing:
			log.Printf("[pricing] %s: %s%s is %s now: %s", ds.name, destinations.DescribeSeries(m.ser), planSuffix(m.ser.Plan), m.to, m.detail)
		case m.to == destinations.PriceUnknown && m.from == "":
			log.Printf("[pricing] %s: %s%s cannot be checked: %s", ds.name, destinations.DescribeSeries(m.ser), planSuffix(m.ser.Plan), m.detail)
		}
	}
	if newlyPriced {
		if r, ok := ds.dest.(destinations.TenantRefresher); ok && r.RefreshTenantsOnPriceChange() {
			s.tenants.Reensure(ds.name)
		}
	}
	s.report(ds)
}

func planSuffix(plan string) string {
	if plan == "" {
		return ""
	}
	return " (plan " + plan + ")"
}

// report publishes a destination's coverage to health and telemetry.
func (s *Service) report(ds *destState) {
	rep := s.destReport(ds)
	labels := map[string]string{"destination": ds.name}
	s.tel.Set("vbilling_unpriced_series", "Series (metric, SKU, plan) a billing platform has no price for: it would bill their usage at $0.", labels, float64(rep.Unpriced))
	for st, n := range map[string]int{destinations.PriceCovered: rep.Priced, destinations.PriceMissing: rep.Unpriced,
		destinations.PriceZero: rep.Zero, destinations.PriceUnknown: rep.Unknown} {
		s.tel.Set("vbilling_price_series", "Series by price coverage status (see GET /api/v1/pricing).",
			map[string]string{"destination": ds.name, "status": st}, float64(n))
	}
	if !rep.LastSuccess.IsZero() {
		s.tel.Set("vbilling_price_check_last_success_timestamp_seconds", "When prices were last read from a billing platform.", labels, float64(rep.LastSuccess.Unix()))
	}
	if s.health == nil {
		return
	}
	var gaps []string
	expired := 0
	now := map[string]bool{}
	for _, r := range rep.Series {
		if r.Status != destinations.PriceMissing {
			continue
		}
		name := health.Name("pricing", ds.name, r.Plan, r.Metric, r.SKU)
		msg := r.Detail
		if r.HeldEvents > 0 {
			msg += fmt.Sprintf(" (%d event(s) held since %s)", r.HeldEvents, r.HeldSince.Format(time.RFC3339))
			if s.now().Sub(r.OldestHeld) > staleHeld {
				msg += fmt.Sprintf("; the oldest is from %s, and Metronome and Stripe reject usage older than 34 and 35 days", r.OldestHeld.Format("2006-01-02"))
			}
		}
		now[name] = true
		gaps = append(gaps, r.Metric+skuSuffix(r.SKU))
		if r.ReleasedUnpriced > 0 {
			expired++
			s.health.Fail(name, "pricing", ds.name,
				fmt.Errorf("%d held event(s) were released WITHOUT A PRICE after being held %s (before their invoice period can be final), last at %s, and %s bills them at 0: %s",
					r.ReleasedUnpriced, s.opts.MaxHold, r.ReleasedUnpricedAt.Format(time.RFC3339), ds.name, msg),
				"Finance must add the price in "+ds.name+" so it re-rates the released usage: "+r.Hint)
			continue
		}
		s.health.Set(name, "pricing", ds.name, health.Unpriced, msg, r.Hint)
	}
	// Gaps that closed report OK once (the log says "recovered"), then age out.
	s.mu.Lock()
	var closed []string
	for name := range ds.gapSources {
		if !now[name] {
			closed = append(closed, name)
		}
	}
	ds.gapSources = now
	s.mu.Unlock()
	for _, name := range closed {
		s.health.OK(name, "pricing", ds.name, 1)
	}
	summary := health.Name("pricing", ds.name)
	if expired > 0 {
		s.health.Fail(summary, "pricing", ds.name,
			fmt.Errorf("%d unpriced series, %d with usage released without a price after the longest hold: %s", len(gaps), expired, strings.Join(first(gaps, 5), ", ")),
			"Finance must add the missing prices in "+ds.name+" so it re-rates the released usage; see GET /api/v1/pricing")
		return
	}
	if len(gaps) > 0 {
		msg := fmt.Sprintf("%d unpriced series: %s", len(gaps), strings.Join(first(gaps, 5), ", "))
		if rep.HeldEvents > 0 {
			msg += fmt.Sprintf("; %d event(s) held", rep.HeldEvents)
		}
		s.health.Set(summary, "pricing", ds.name, health.Unpriced, msg,
			"Finance adds the missing product, rate, price or charge in "+ds.name+"; see GET /api/v1/pricing")
		return
	}
	s.health.OK(summary, "pricing", ds.name, len(rep.Series))
}

func skuSuffix(sku string) string {
	if sku == "" {
		return ""
	}
	return " sku=" + sku
}

func first(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string(nil), l[:n]...), fmt.Sprintf("and %d more", len(l)-n))
}
