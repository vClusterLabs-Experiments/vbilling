package metronome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Fourth review: the waiting store for usage of metrics no billable metric
// counts yet (waiting.go, commit 1286df1).

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

// review4Open builds a default-mode adapter on base and dir at a fixed time
// (a new instance, as after a restart, reads the billable metrics afresh).
func review4Open(base, dir string, provision bool, at time.Time) *Adapter {
	a := New(NewClient(base, metronometest.Token), &config.Config{DataDir: dir, MetronomeProvision: provision}, kv.Memory(), nil)
	a.now = func() time.Time { return at }
	return a
}

func review4Sent(f *metronometest.Server) map[string]bool {
	sent := map[string]bool{}
	for _, e := range f.Events() {
		sent[e.TransactionID] = true
	}
	return sent
}

// Usage waits in default mode. The operator then lets vBilling create the
// billable metrics: METRONOME_PROVISION=true, which docs/metronome.html
// describes (metronome.stripeLink=true turns it on too). In provisioning
// mode neither Bootstrap nor SendEvents calls releaseWaiting, so what waited
// in DATA_DIR/metronome-waiting is never sent, and the dispatcher's cursor
// is long past it.
func TestReview4WaitingUsageIsStrandedWhenProvisioningIsTurnedOn(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	ctx := context.Background()

	waited := gpu("tenant-acme", "c1", now.Add(-3*time.Minute), 1)
	a := review4Open(f.URL, dir, false, now)
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if err := a.SendEvents(ctx, []usage.Event{waited}); err != nil {
		t.Fatal(err)
	}
	if len(f.Events()) != 0 {
		t.Fatalf("precondition: %d event(s) sent before any billable metric counted GPU hours", len(f.Events()))
	}

	// Restart with METRONOME_PROVISION=true: vBilling creates the billable
	// metrics and keeps delivering.
	b := review4Open(f.URL, dir, true, now.Add(2*time.Minute))
	if err := b.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	later := gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)
	if err := b.SendEvents(ctx, []usage.Event{later}); err != nil {
		t.Fatal(err)
	}

	if sent := review4Sent(f); !sent[waited.ID] {
		_, statErr := os.Stat(filepath.Join(dir, "metronome-waiting", usage.MetricGPUHours+".jsonl"))
		t.Errorf("GPU usage that waited in default mode was never sent after METRONOME_PROVISION was turned on and vBilling created "+
			"the GPU billable metric: Metronome received %d event(s), the waiting one (%s) not among them; its file is still in "+
			"DATA_DIR/metronome-waiting (stat error: %v) and nothing in provisioning mode ever reads it",
			len(f.Events()), waited.ID, statErr)
	}
}

// docs/metronome.html: on HTTP 400, 413 or 422 "the batch is split until
// the rejected events are isolated. Those are dead-lettered; the rest are
// delivered". SendEvents releases waiting usage first and returns the
// release's error as its own, so when Metronome refuses a waiting event for
// good, the dispatcher bisects and dead-letters the batch it was delivering
// (other tenants' usage of a metric a billable metric counts), never the
// refused event, which keeps waiting and does the same to every later batch.
func TestReview4AWaitingEventMetronomeRefusesDeadLettersOtherTenantsUsage(t *testing.T) {
	f := newFake(t)
	financeMetric(t, f, usage.MetricCPUCoreHours, "CPU hours")
	dir := t.TempDir()

	refused := gpu("tenant-acme", "c1", now.Add(-10*time.Minute), 1)
	proxy := review4Proxy(t, f, func(_ int, body []byte) int {
		if bytes.Contains(body, []byte(refused.ID)) {
			return http.StatusBadRequest
		}
		return 0
	})
	// No billable metric counts GPU hours yet: the event waits.
	if err := review4Open(proxy.URL, dir, false, now).SendEvents(context.Background(), []usage.Event{refused}); err != nil {
		t.Fatal(err)
	}
	// Finance adds it; vBilling (restarted, or a minute later) releases.
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	a := review4Open(proxy.URL, dir, false, now.Add(2*time.Minute))

	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	// Two later windows of CPU usage of two other tenants.
	var cpu []usage.Event
	for w, tenant := range []string{"tenant-beta", "tenant-gamma"} {
		ws := now.Add(time.Duration(w-3) * time.Minute)
		var window []usage.Event
		for i := 0; i < 4; i++ {
			e := usage.Event{Tenant: tenant, Metric: usage.MetricCPUCoreHours, Quantity: 2, Region: "ap-southeast-2",
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), Source: "collector", RecordedAt: now,
				Dimensions: map[string]string{usage.DimTenantCluster: fmt.Sprintf("%s-%d", tenant, i)}}
			e.Finalize()
			window = append(window, e)
		}
		if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), window, ""); err != nil {
			t.Fatal(err)
		}
		cpu = append(cpu, window...)
	}
	if err := sp.SetCursor(a.Name(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	disp := pipeline.New(sp, []destinations.Destination{a}, pipeline.Options{BatchSize: 5, IdlePoll: 10 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}, nil)
	done := make(chan struct{})
	go func() { defer close(done); disp.Run(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); sp.Cursor(a.Name()) < sp.Committed() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	sent := review4Sent(f)
	dls, err := sp.DeadLetters(a.Name())
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for _, e := range cpu {
		if !sent[e.ID] {
			missing++
		}
	}
	refusedDead := false
	for _, dl := range dls {
		refusedDead = refusedDead || dl.Event.ID == refused.ID
	}
	if missing > 0 || !refusedDead {
		reason := ""
		if len(dls) > 0 {
			reason = dls[0].Reason
		}
		left, _ := a.wait.pending()
		t.Errorf("Metronome refused (HTTP 400) one GPU event that had waited for its billable metric. %d of the %d CPU events of "+
			"tenant-beta and tenant-gamma (two later batches, of a metric a billable metric counts) never reached Metronome: %d dead "+
			"letter(s), the first because %q. The refused GPU event is dead-lettered: %v; it still waits (%v), so every later batch "+
			"is dead-lettered the same way. docs/metronome.html: only the rejected events are dead-lettered, the rest delivered",
			missing, len(cpu), len(dls), reason, refusedDead, left)
	}
}

// take reads a type's whole backlog into memory before it sends the first
// slice of 100, and keeps it there until every slice is accepted (nothing is
// recorded as sent before that). The chart limits vBilling to 256Mi, so a
// backlog of one metric (a custom metric that waits a week for Finance, or
// any metric no billable metric ever counts once it is 28 days deep, when
// every delivery takes and rewrites it again) OOM-kills vBilling on release,
// and the restart's Bootstrap releases, and is killed, again.
func TestReview4ReleaseHoldsTheWholeBacklogInMemory(t *testing.T) {
	const n = 100_000
	f := newFake(t)
	dir := t.TempDir()
	ctx := context.Background()
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

	// n GPU events wait (100 series, 1,000 one-minute windows each).
	func() {
		evs := make([]usage.Event, 0, n)
		for i := 0; i < n; i++ {
			evs = append(evs, gpu("tenant-acme", fmt.Sprintf("c%d", i%100), now.Add(-time.Duration(i/100+1)*time.Minute), 1))
		}
		if err := review4Open(proxy.URL, dir, false, now).SendEvents(ctx, evs); err != nil {
			t.Fatal(err)
		}
	}()
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	a := review4Open(proxy.URL, dir, false, now.Add(2*time.Minute))
	if _, err := a.wait.pending(); err != nil { // the index, before the baseline
		t.Fatal(err)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc

	err := a.SendEvents(ctx, nil)
	if live.Load() == 0 {
		t.Fatalf("the release sent no ingest request: %v", err)
	}
	held := int64(live.Load()) - int64(base)
	if held > 16<<20 {
		per := held / n
		upper := (256 << 20) / per // live heap alone at the chart's limit
		t.Errorf("releasing %d waiting events kept %d MiB live when the first slice of 100 went out (%d bytes per event): take "+
			"loads a type's whole backlog, and SendEvents holds it until every slice is accepted. Live heap alone reaches the "+
			"chart's 256Mi limit at %d events; with Go's default GC (heap up to twice the live size) and the rest of vBilling, "+
			"about half that: %d events, %.0f series-days of one-minute windows (20 GPU series waiting %.1f days)",
			n, held>>20, per, upper, upper/2, float64(upper/2)/1440, float64(upper/2)/1440/20)
	}
}

// add appends to the type's file with O_APPEND, and scanEvents skips a line
// that does not parse ("torn by a crash: it was never acknowledged"). A
// write cut short (vBilling killed mid-append, or ENOSPC) leaves a line
// without its newline, and the dispatcher, which got no ack, delivers the
// same batch again: its first event is appended onto the cut line, which is
// skipped, so that event is acknowledged but never sent. (pipeline.HeldStore
// cuts such a tail off when it opens; this store never does.)
func TestReview4AnAppendAfterATornWriteLosesAnEvent(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	ctx := context.Background()

	batch := []usage.Event{gpu("tenant-acme", "c1", now.Add(-5*time.Minute), 1), gpu("tenant-acme", "c2", now.Add(-5*time.Minute), 1)}
	// The append of this batch was cut short inside its first line; the
	// dispatcher never acknowledged it.
	line, err := json.Marshal(batch[0])
	if err != nil {
		t.Fatal(err)
	}
	waitDir := filepath.Join(dir, "metronome-waiting")
	if err := os.MkdirAll(waitDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(waitDir, usage.MetricGPUHours+".jsonl"), line[:len(line)/2], 0o640); err != nil {
		t.Fatal(err)
	}
	// After the restart the dispatcher delivers the same batch again: it waits.
	if err := review4Open(f.URL, dir, false, now).SendEvents(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// Finance adds the billable metric: what waited is sent.
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	b := review4Open(f.URL, dir, false, now.Add(2*time.Minute))
	if err := b.SendEvents(ctx, nil); err != nil {
		t.Fatal(err)
	}
	sent := review4Sent(f)
	left, _ := b.wait.pending()
	for _, e := range batch {
		if !sent[e.ID] {
			t.Errorf("GPU event %s (cluster %s) was accepted into the waiting store and acknowledged, then never reached Metronome "+
				"and no longer waits (waiting: %v): appended after a line a crash cut short, it shares that line, which scanEvents "+
				"skips as torn", e.ID, e.Dimensions[usage.DimTenantCluster], left)
		}
	}
}

// docs/metronome.html: "Property filters may narrow one, for example to the
// SKUs you sell". The waiting store asks only whether some billable metric
// sums the event type, not whether one counts the event: usage of a new SKU
// that every GPU billable metric's filter leaves out is sent at once, and the
// billable metric Finance adds for it afterwards never counts it.
func TestReview4UsageAPropertyFilterLeavesOutDoesNotWait(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	cus, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	if err != nil {
		t.Fatal(err)
	}
	narrow := func(sku string) func(*BillableMetric) {
		return func(m *BillableMetric) {
			for i := range m.PropertyFilters {
				if m.PropertyFilters[i].Name == "sku" {
					m.PropertyFilters[i].InValues = []string{sku}
				}
			}
		}
	}
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours H100", narrow("NVIDIA-H100-80GB-HBM3"))
	a := newReadOnly(t, f)

	// A new GPU model goes live before Finance has a billable metric for it.
	b200 := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricGPUHours, Quantity: 1, SKU: "NVIDIA-B200", Region: "ap-southeast-2",
		WindowStart: now.Add(-3 * time.Minute), WindowEnd: now.Add(-2 * time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: "c1", usage.DimCapacityType: usage.CapacityOnDemand}}
	b200.Finalize()
	if err := a.SendEvents(ctx, []usage.Event{b200}); err != nil {
		t.Fatal(err)
	}
	// Finance adds the billable metric for B200; the next delivery releases.
	bm := financeMetric(t, f, usage.MetricGPUHours, "GPU hours B200", narrow("NVIDIA-B200"))
	a.now = func() time.Time { return now.Add(2 * time.Minute) }
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}

	day := now.Truncate(24 * time.Hour)
	rows, err := a.c.Usage(ctx, UsageRequest{StartingOn: rfc3339(day), EndingBefore: rfc3339(day.Add(24 * time.Hour)), WindowSize: "day",
		CustomerIDs: []string{cus}, BillableMetrics: []map[string]any{{"id": bm}}})
	if err != nil {
		t.Fatal(err)
	}
	var counted float64
	for _, r := range rows {
		v, _ := r.Value.Float64()
		counted += v
	}
	if counted != 1 {
		t.Errorf("usage of GPU SKU NVIDIA-B200, which the only GPU billable metric's property filter (sku in [NVIDIA-H100-80GB-HBM3]) "+
			"leaves out, did not wait: it was sent at once, so the billable metric Finance added for B200 afterwards counts %v of its "+
			"1 GPU-hour (Metronome counts only usage sent after a billable metric exists)", counted)
	}
}

// summing ignores SQL billable metrics (price coverage documents them as not
// covered). Before 1286df1 that only made the price check report a gap; now,
// whatever PRICE_CHECK says, usage of a type only a SQL billable metric bills
// waits 28 days and is sent long after Metronome finalized the invoices of
// its periods (24 hours after a period ends, docs/metronome.html), so it is
// never invoiced. Before, it was sent at once.
func TestReview4UsageASQLBillableMetricBillsWaits(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.AddBillableMetric([]byte(`{"name":"GPU hours (SQL)","sql":"SELECT customer_id, SUM(CAST(properties.value AS DECIMAL)) AS value FROM events WHERE event_type = 'vcluster_gpu_hours' GROUP BY customer_id"}`))
	a := newReadOnly(t, f) // PRICE_CHECK=warn, the default
	ev := gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)
	if err := a.SendEvents(ctx, []usage.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if !review4Sent(f)[ev.ID] {
		left, _ := a.wait.pending()
		t.Errorf("GPU usage was held (waiting: %v) although Finance bills GPU hours with an active SQL billable metric: it goes "+
			"out after 28 days, into invoice periods Metronome finalized long before, so it is never invoiced (before 1286df1 it "+
			"was sent at once)", left)
	}
}

// docs/metronome.html: "vBilling dead-letters older ones [than 34 days]
// before sending them ... so a rewound cursor or a longer outage shows in
// the dead-letter list and in reconciliation instead of disappearing".
// Waiting usage that is past Metronome's window when it is finally released
// (Metronome unreachable for the last days of its 28-day wait) is dropped
// with a log line (after a "sent ... WITHOUT a billable metric" warning that
// counts it as sent), and is not reported to the dispatcher.
func TestReview4WaitingUsagePastMetronomesWindowVanishes(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	a := newReadOnly(t, f, func(c *config.Config) { c.DataDir = t.TempDir() })
	ev := gpu("tenant-acme", "c1", now.Add(-27*24*time.Hour), 1)
	if err := a.SendEvents(ctx, []usage.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if len(f.Events()) != 0 {
		t.Fatal("precondition: sent before a billable metric counted it")
	}
	// Metronome cannot be reached for a week (an outage, a revoked token):
	// nothing is released meanwhile.
	a.now = func() time.Time { return now.Add(7 * 24 * time.Hour) }
	err := a.SendEvents(ctx, nil)

	var partial *destinations.PartialError
	reported := errors.As(err, &partial) && partial.Rejected[ev.ID] != ""
	left, _ := a.wait.pending()
	if !review4Sent(f)[ev.ID] && !reported && len(left) == 0 {
		t.Errorf("a GPU event that waited until it was 34 days old disappeared: not sent, no longer waiting, and SendEvents "+
			"returned %v, so it is not dead-lettered either (the dead-letter list and reconciliation's dead_lettered never show it)", err)
	}
}
