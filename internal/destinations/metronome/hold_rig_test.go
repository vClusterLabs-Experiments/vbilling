package metronome

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// holdRig runs the adapter as vBilling does: the ledger, the held store and
// the pricing service's gate in front of the dispatcher, everything under
// DATA_DIR dir, so a second rig on the same dir is a restart. The
// dispatcher releases held usage by the real clock (at most
// pipeline.MaxUnpricedHold), so usage in these tests is dated by it
// (rigNow), and the adapter keeps the real clock.
type holdRig struct {
	dir  string
	sp   *spool.Spool
	held *pipeline.HeldStore
	a    *Adapter
	svc  *pricing.Service
	disp *pipeline.Dispatcher
	h    *health.Registry

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type rigOptions struct {
	base      string        // Metronome's URL
	provision bool          // METRONOME_PROVISION
	rateCard  string        // METRONOME_RATE_CARD
	mode      string        // PRICE_CHECK (default warn)
	maxHold   time.Duration // default pipeline.MaxUnpricedHold
	batch     int           // ledger records per delivery (default 500)
}

const rigDest = "metronome"

// rigNow is the real clock, to the minute.
func rigNow() time.Time { return time.Now().UTC().Truncate(time.Minute) }

// newRigFake is a fake Metronome on the real clock.
func newRigFake(t *testing.T) *metronometest.Server {
	f := metronometest.New(t)
	f.SetClock(time.Now)
	return f
}

// openHoldRig opens DATA_DIR and builds the pipeline without starting it.
func openHoldRig(t *testing.T, dir string, o rigOptions) *holdRig {
	t.Helper()
	if o.mode == "" {
		o.mode = pricing.ModeWarn
	}
	if o.maxHold == 0 {
		o.maxHold = pipeline.MaxUnpricedHold
	}
	sp, err := spool.Open(filepath.Join(dir, "ledger"), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	sp.Cursor(rigDest) // from the start of the ledger, not its end
	held, err := pipeline.OpenHeld(filepath.Join(dir, "held"), true)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: dir, MetronomeProvision: o.provision, MetronomeRateCard: o.rateCard, PriceCheck: o.mode, PriceCheckMaxHold: o.maxHold,
		MetronomeStripeCollect: "charge_automatically"}
	a := New(NewClient(o.base, metronometest.Token), cfg, kv.Memory(), nil)
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	tel := telemetry.New()
	tenants := pipeline.NewTenantRegistry()
	svc := pricing.New(sp, []destinations.Destination{a}, tenants, held, h, tel, pricing.Options{Mode: o.mode, MaxHold: o.maxHold,
		Debounce: 5 * time.Millisecond, DecideWait: 10 * time.Second})
	disp := pipeline.New(sp, []destinations.Destination{a}, pipeline.Options{Tenants: tenants, Held: held, Gate: svc.Gate(), MaxHold: o.maxHold,
		BatchSize: o.batch, IdlePoll: 10 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		Bootstrap: func(ctx context.Context, d destinations.Destination) error { return d.Bootstrap(ctx, usage.Catalog()) }}, tel)
	r := &holdRig{dir: dir, sp: sp, held: held, a: a, svc: svc, disp: disp, h: h}
	t.Cleanup(r.stop)
	return r
}

// newHoldRig opens DATA_DIR and starts delivering.
func newHoldRig(t *testing.T, dir string, o rigOptions) *holdRig {
	t.Helper()
	r := openHoldRig(t, dir, o)
	r.start()
	return r
}

func (r *holdRig) start() {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.done = make(chan struct{})
	go r.svc.Run(r.ctx)
	go func() { defer close(r.done); r.disp.Run(r.ctx) }()
}

// stop stops delivery and closes the ledger, as vBilling does when it exits.
func (r *holdRig) stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
		r.cancel = nil
	}
	if r.sp != nil {
		r.sp.Close()
		r.sp = nil
	}
}

// commit appends events to the ledger, as one batch.
func (r *holdRig) commit(t *testing.T, events ...usage.Event) {
	t.Helper()
	if err := r.sp.AppendWindow(spool.SourceCollector, time.Time{}, events, ""); err != nil {
		t.Fatal(err)
	}
}

// delivered reports whether the dispatcher acknowledged the whole ledger.
func (r *holdRig) delivered() bool { return r.sp.Cursor(rigDest) == r.sp.Committed() }

// check runs a price check now, as the next periodic one would.
func (r *holdRig) check() { r.svc.CheckNow(r.ctx) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sentIDs lists the transaction IDs Metronome received.
func sentIDs(f *metronometest.Server) map[string]bool {
	sent := map[string]bool{}
	for _, e := range f.Events() {
		sent[e.TransactionID] = true
	}
	return sent
}

// counted reads what Metronome bills tenant for metric over the days of at
// (and the next, for a test that runs across midnight).
func (r *holdRig) counted(t *testing.T, tenant, metric string, at time.Time) float64 {
	t.Helper()
	day := at.UTC().Truncate(24 * time.Hour)
	totals, err := r.a.RecordedTotals(context.Background(), destinations.TotalsQuery{From: day, To: day.Add(48 * time.Hour),
		Tenants: []usage.Tenant{{ID: tenant}}, Metrics: []string{metric}})
	if err != nil {
		t.Fatal(err)
	}
	var q float64
	for _, tot := range totals {
		q += tot.Quantity
	}
	return q
}
