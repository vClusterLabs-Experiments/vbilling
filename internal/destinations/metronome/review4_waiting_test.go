package metronome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Fourth review: the waiting store for usage of metrics no billable metric
// counts yet (waiting.go, commit 1286df1) stranded, lost and dropped usage.
// It is gone: price coverage reports such usage as uncounted, and the
// pricing gate holds it in vBilling's held store (DATA_DIR/held), in every
// PRICE_CHECK mode but off and in both Metronome modes, releasing it in
// order once a billable metric counts it, or unpriced after
// pipeline.MaxUnpricedHold with failing health. These tests assert the
// properties the review asked of the waiting store against that mechanism.

// review4Proxy stands in front of the fake Metronome. Ingest requests go
// through answer first (n counts them from 1): a non-zero status is returned
// to vBilling instead of forwarding, as Metronome refusing or failing a
// request would. Everything else reaches the fake unchanged.
func review4Proxy(t *testing.T, f *metronometest.Server, answer func(n int, body []byte) int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/ingest" && answer != nil {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if status := answer(n, body); status != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"message":"refused in front of the fake (HTTP %d)"}`, status)
				return
			}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, f.URL+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(p.Close)
	return p
}

// Finding 7. Usage was held in default mode; the operator then turns on
// METRONOME_PROVISION and vBilling creates the billable metrics. The waiting
// store was read in default mode only, so what waited was never sent. Held
// usage now sits in the held store, which every mode releases: once the
// billable metric vBilling created counts it, it ships, and Metronome counts
// it, since the metric existed before it arrived.
func TestReview4WaitingUsageIsStrandedWhenProvisioningIsTurnedOn(t *testing.T) {
	f := newRigFake(t)
	dir := t.TempDir()
	at := rigNow()

	waited := gpu("tenant-acme", "c1", at.Add(-3*time.Minute), 1)
	a := newHoldRig(t, dir, rigOptions{base: f.URL})
	a.commit(t, waited)
	waitFor(t, "the GPU usage held while no billable metric counts it", func() bool { return a.held.Count(rigDest) == 1 && a.delivered() })
	if n := len(f.Events()); n != 0 {
		t.Fatalf("precondition: %d event(s) sent before any billable metric counted GPU hours", n)
	}
	a.stop()

	// Restart with METRONOME_PROVISION=true: vBilling creates the billable
	// metrics and keeps delivering. A price check that ran before they
	// existed still says nothing counts GPU hours; the next one, within a
	// minute while usage is held, finds them.
	b := newHoldRig(t, dir, rigOptions{base: f.URL, provision: true})
	waitFor(t, "the billable metrics created", func() bool {
		st := b.disp.Statuses()
		return len(st) == 1 && st[0].Ready
	})
	b.check()
	later := gpu("tenant-acme", "c1", at.Add(-time.Minute), 1)
	b.commit(t, later)
	waitFor(t, "the usage held in default mode sent after the switch", func() bool {
		sent := sentIDs(f)
		return sent[waited.ID] && sent[later.ID] && b.held.Count(rigDest) == 0
	})
	if got := b.counted(t, "tenant-acme", usage.MetricGPUHours, at); got != 2 {
		t.Errorf("Metronome bills %v GPU-hours, want both: the held one and the newer one", got)
	}
}

// Finding 10. A held event Metronome refuses for good is isolated and
// dead-lettered on its own when it is released (the dispatcher bisects a
// refused batch); the other tenants' batches before and after it are
// delivered. docs/metronome.html: only the rejected events are
// dead-lettered, the rest delivered.
func TestReview4AWaitingEventMetronomeRefusesDeadLettersOtherTenantsUsage(t *testing.T) {
	f := newRigFake(t)
	financeMetric(t, f, usage.MetricCPUCoreHours, "CPU hours")
	at := rigNow()
	refused := gpu("tenant-acme", "c1", at.Add(-10*time.Minute), 1)
	proxy := review4Proxy(t, f, func(_ int, body []byte) int {
		if bytes.Contains(body, []byte(refused.ID)) {
			return http.StatusBadRequest
		}
		return 0
	})
	r := newHoldRig(t, t.TempDir(), rigOptions{base: proxy.URL, batch: 5})
	// No billable metric counts GPU hours yet: the event is held.
	r.commit(t, refused)
	waitFor(t, "the GPU event held", func() bool { return r.held.Count(rigDest) == 1 && r.delivered() })

	// Two later windows of CPU usage of two other tenants.
	var cpu []usage.Event
	for w, tenant := range []string{"tenant-beta", "tenant-gamma"} {
		ws := at.Add(time.Duration(w-3) * time.Minute)
		var window []usage.Event
		for i := 0; i < 4; i++ {
			e := usage.Event{Tenant: tenant, Metric: usage.MetricCPUCoreHours, Quantity: 2, Region: "ap-southeast-2",
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), Source: "collector", RecordedAt: at,
				Dimensions: map[string]string{usage.DimTenantCluster: fmt.Sprintf("%s-%d", tenant, i)}}
			e.Finalize()
			window = append(window, e)
		}
		r.commit(t, window...)
		cpu = append(cpu, window...)
	}
	// Finance adds the GPU billable metric: the held event is released,
	// and Metronome refuses it.
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	r.check()
	waitFor(t, "the ledger delivered and the refused event dead-lettered", func() bool {
		dls, _ := r.sp.DeadLetters(rigDest)
		return r.delivered() && r.held.Count(rigDest) == 0 && len(dls) > 0
	})

	sent := sentIDs(f)
	for _, e := range cpu {
		if !sent[e.ID] {
			t.Errorf("CPU event of %s (%s) never reached Metronome", e.Tenant, e.Dimensions[usage.DimTenantCluster])
		}
	}
	dls, err := r.sp.DeadLetters(rigDest)
	if err != nil {
		t.Fatal(err)
	}
	if len(dls) != 1 || dls[0].Event.ID != refused.ID || !strings.Contains(dls[0].Reason, "400") {
		t.Errorf("dead letters %+v, want only the GPU event Metronome refused, with its reason", dls)
	}
}

// Finding 8. Releasing a backlog reads it from the held store a batch at a
// time: what is live when the first slice reaches Metronome does not grow
// with the backlog. (The waiting store read a type's whole backlog into
// memory and kept it until every slice was accepted.)
func TestReview4ReleaseHoldsTheWholeBacklogInMemory(t *testing.T) {
	const n = 100_000
	f := newRigFake(t)
	dir := t.TempDir()
	at := rigNow()
	var live atomic.Uint64
	proxy := review4Proxy(t, f, func(call int, _ []byte) int {
		if call == 1 {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			live.Store(ms.HeapAlloc)
		}
		return http.StatusServiceUnavailable // stop after the first slice; nothing is lost
	})

	// An earlier run held n GPU events (100 clusters, 1,000 one-minute
	// windows each) while no billable metric counted GPU hours.
	func() {
		held, err := pipeline.OpenHeld(filepath.Join(dir, "held"), true)
		if err != nil {
			t.Fatal(err)
		}
		a := New(NewClient(f.URL, metronometest.Token), &config.Config{}, nil, nil)
		var batch []pipeline.HeldEntry
		for i := 0; i < n; i++ {
			e := gpu("tenant-acme", fmt.Sprintf("c%d", i%100), at.Add(-time.Duration(1000-i/100)*time.Minute), 1)
			key := destinations.SeriesOf(a, aliasPlan+"tenant-acme", &e).Key()
			batch = append(batch, pipeline.HeldEntry{Seq: uint64(i + 1), At: at, Series: key, Reason: "not counted yet", Event: e})
			if len(batch) == 5000 || i == n-1 {
				if _, err := held.Add(rigDest, batch); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
	}()
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	r := openHoldRig(t, dir, rigOptions{base: proxy.URL})
	if c := r.held.Count(rigDest); c != n {
		t.Fatalf("precondition: %d events held, want %d", c, n)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc

	r.start()
	waitFor(t, "the first slice of the release sent", func() bool { return live.Load() != 0 })
	r.stop()
	held := int64(live.Load()) - int64(base)
	if held > 16<<20 {
		t.Errorf("releasing %d held events kept %d MiB live when the first slice of 100 went out (%d bytes per event): the release must read the backlog a batch at a time",
			n, held>>20, held/n)
	}
	if c := r.held.Count(rigDest); c != n {
		t.Errorf("%d events held after Metronome failed the release, want all %d still held", c, n)
	}
}

// Finding 22. A write cut short (vBilling killed mid-append, or ENOSPC)
// leaves a line without its newline, and the dispatcher, which got no ack,
// delivers the same batch again. The held store cuts such a tail off when it
// opens, so the batch held again is whole: every event of both batches
// reaches Metronome once a billable metric counts them. (The waiting store
// appended onto the cut line and skipped it as torn, losing an event.)
func TestReview4AnAppendAfterATornWriteLosesAnEvent(t *testing.T) {
	f := newRigFake(t)
	dir := t.TempDir()
	at := rigNow()

	first := []usage.Event{gpu("tenant-acme", "c1", at.Add(-5*time.Minute), 1), gpu("tenant-acme", "c2", at.Add(-5*time.Minute), 1)}
	a := newHoldRig(t, dir, rigOptions{base: f.URL})
	a.commit(t, first...)
	waitFor(t, "the first batch held", func() bool { return a.held.Count(rigDest) == 2 && a.delivered() })
	// The next batch is committed; vBilling is killed while holding it, its
	// append cut short inside the first line, and the dispatcher never
	// acknowledged the batch.
	next := []usage.Event{gpu("tenant-acme", "c1", at.Add(-4*time.Minute), 1), gpu("tenant-acme", "c2", at.Add(-4*time.Minute), 1)}
	a.cancel()
	<-a.done
	a.cancel = nil
	a.commit(t, next...)
	a.stop()
	files, _ := filepath.Glob(filepath.Join(dir, "held", rigDest, "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("precondition: held files %v, want one series", files)
	}
	line, err := json.Marshal(pipeline.HeldEntry{Seq: 99, At: at, Series: "torn", Reason: "not counted yet", Event: next[0]})
	if err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	// After the restart the dispatcher delivers the same batch again: it is
	// held. Finance adds the billable metric: what was held is sent.
	b := newHoldRig(t, dir, rigOptions{base: f.URL})
	waitFor(t, "the second batch held again", func() bool { return b.held.Count(rigDest) == 4 && b.delivered() })
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	b.check()
	waitFor(t, "the held usage released", func() bool { return b.held.Count(rigDest) == 0 })
	sent := sentIDs(f)
	for _, e := range append(first, next...) {
		if !sent[e.ID] {
			t.Errorf("GPU event %s (cluster %s, window %s) was held and never reached Metronome", e.ID, e.Dimensions[usage.DimTenantCluster], e.WindowStart.Format(time.RFC3339))
		}
	}
}

// Finding 16. A billable metric's property filters decide what it bills,
// for example the SKUs Finance sells: usage they leave out is a price gap,
// reported unpriced and held only with PRICE_CHECK=hold. Only usage of an
// event type no billable metric sums is held whatever PRICE_CHECK says
// (but off), because Metronome would never count it.
func TestReview4UsageAPropertyFilterLeavesOutDoesNotWait(t *testing.T) {
	f := newRigFake(t)
	cus, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	if err != nil {
		t.Fatal(err)
	}
	h100 := financeMetric(t, f, usage.MetricGPUHours, "GPU hours H100", func(m *BillableMetric) {
		for i := range m.PropertyFilters {
			if m.PropertyFilters[i].Name == "sku" {
				m.PropertyFilters[i].InValues = []string{"NVIDIA-H100-80GB-HBM3"}
			}
		}
	})
	prod := f.AddProduct("GPU Hours", h100, "sku")
	card := f.AddRateCard("List")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: prod, Price: 300, PricingGroupValues: map[string]string{"sku": "NVIDIA-H100-80GB-HBM3"}})
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: time.Now().AddDate(0, -1, 0)})

	at := rigNow()
	// A new GPU model goes live before Finance has a billable metric for it,
	// and CPU usage no billable metric counts at all.
	b200 := gpu("tenant-acme", "c1", at.Add(-3*time.Minute), 1)
	b200.SKU, b200.ID = "NVIDIA-B200", ""
	b200.Finalize()
	cpu := cpuEvent("tenant-acme", at.Add(-3*time.Minute), 2)

	for _, mode := range []string{pricing.ModeWarn, pricing.ModeHold} {
		a := newReadOnly(t, f)
		a.now = time.Now
		sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sp.Close() })
		svc := pricing.New(sp, []destinations.Destination{a}, nil, nil, nil, nil, pricing.Options{Mode: mode, DecideWait: 10 * time.Second})
		dec := svc.Gate().Decide(context.Background(), rigDest, []usage.Event{b200, cpu})
		if got := dec[0].Hold; got != (mode == pricing.ModeHold) {
			t.Errorf("PRICE_CHECK=%s: B200 usage the property filter leaves out held %v, want %v: %+v", mode, got, mode == pricing.ModeHold, dec[0])
		}
		if !dec[1].Hold || !strings.Contains(dec[1].Reason, "not counted yet") {
			t.Errorf("PRICE_CHECK=%s: CPU usage no billable metric counts must be held: %+v", mode, dec[1])
		}
		for _, d := range svc.Report().Destinations {
			for _, row := range d.Series {
				switch {
				case row.Metric == usage.MetricGPUHours && row.SKU == "NVIDIA-B200":
					if row.Status != destinations.PriceMissing || row.Uncounted || !strings.Contains(row.Detail, "property filters") {
						t.Errorf("PRICE_CHECK=%s: B200 row %+v, want unpriced (a price gap, the property filters say why), not uncounted", mode, row)
					}
				case row.Metric == usage.MetricCPUCoreHours && !row.NoUsageYet:
					if row.Status != destinations.PriceMissing || !row.Uncounted || row.Hint != noMetricHint {
						t.Errorf("PRICE_CHECK=%s: CPU row %+v, want uncounted with the billable-metrics hint", mode, row)
					}
				}
			}
		}
	}
}

// Finding 15. A SQL billable metric may count any event type, and vBilling
// cannot tell which: while one is active, usage no other billable metric
// counts is reported unknown and never held, in any mode, so it is never
// stalled into invoice periods Metronome finalized.
func TestReview4UsageASQLBillableMetricBillsWaits(t *testing.T) {
	f := newRigFake(t)
	f.AddBillableMetric([]byte(`{"name":"GPU hours (SQL)","sql":"SELECT customer_id, SUM(CAST(properties.value AS DECIMAL)) AS value FROM events WHERE event_type = 'vcluster_gpu_hours' GROUP BY customer_id"}`))
	at := rigNow()
	ev := gpu("tenant-acme", "c1", at.Add(-2*time.Minute), 1)

	for _, mode := range []string{pricing.ModeWarn, pricing.ModeHold} {
		r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, mode: mode})
		e := ev
		e.Tenant, e.ID = "tenant-"+mode, ""
		e.Finalize()
		r.commit(t, e)
		waitFor(t, "the GPU usage delivered", func() bool { return r.delivered() })
		if !sentIDs(f)[e.ID] || r.held.Count(rigDest) != 0 {
			t.Errorf("PRICE_CHECK=%s: GPU usage held (%d) although an active SQL billable metric may bill it", mode, r.held.Count(rigDest))
		}
		for _, d := range r.svc.Report().Destinations {
			for _, row := range d.Series {
				if row.Metric == usage.MetricGPUHours && !row.NoUsageYet && (row.Status != destinations.PriceUnknown || !strings.Contains(row.Detail, "SQL billable metric")) {
					t.Errorf("PRICE_CHECK=%s: GPU row %+v, want unknown, naming the SQL billable metric", mode, row)
				}
			}
		}
		r.stop()
	}
}

// Finding 31. Held usage never disappears. While Metronome cannot be
// reached, it stays held; usage held for the longest hold is released
// without a billable metric (with failing health), and an event Metronome
// would refuse as older than 34 days is dead-lettered, so it shows in the
// dead letters and in reconciliation.
func TestReview4WaitingUsagePastMetronomesWindowVanishes(t *testing.T) {
	f := newRigFake(t)
	at := rigNow()
	tooOld := gpu("tenant-acme", "c1", at.Add(-35*24*time.Hour), 1)
	held := gpu("tenant-acme", "c1", at.Add(-2*24*time.Hour), 1)
	var refusals atomic.Int32
	proxy := review4Proxy(t, f, func(n int, _ []byte) int {
		if n <= 3 { // Metronome cannot be reached at first
			refusals.Add(1)
			return http.StatusServiceUnavailable
		}
		return 0
	})
	r := newHoldRig(t, t.TempDir(), rigOptions{base: proxy.URL})
	r.commit(t, tooOld, held)
	waitFor(t, "the held usage released and delivered", func() bool { return r.delivered() && r.held.Count(rigDest) == 0 && sentIDs(f)[held.ID] })

	if refusals.Load() != 3 {
		t.Errorf("Metronome failed %d release(s), want 3 before it answered", refusals.Load())
	}
	dls, err := r.sp.DeadLetters(rigDest)
	if err != nil {
		t.Fatal(err)
	}
	if len(dls) != 1 || dls[0].Event.ID != tooOld.ID || !strings.Contains(dls[0].Reason, "34-day") {
		t.Errorf("dead letters %+v, want the event older than Metronome's 34-day window", dls)
	}
	// The dispatcher counts the release, and reports it to the gate, once
	// the held store is drained.
	var st pipeline.Status
	waitFor(t, "the release counted and reported", func() bool {
		st = r.disp.Statuses()[0]
		failing := false
		for _, s := range r.h.Problems() {
			failing = failing || (s.Kind == "pricing" && s.Status == health.Failing && strings.Contains(s.Message, "released"))
		}
		return st.ReleasedUnpriced == 2 && failing
	})
	if st.DeadLettered != 1 || st.Delivered != 1 {
		t.Errorf("status %+v: want both held events released without a billable metric, one delivered, one dead-lettered", st)
	}
}
