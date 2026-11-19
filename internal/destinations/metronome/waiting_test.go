package metronome

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func cpuEvent(tenant string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricCPUCoreHours, Quantity: q, Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: "c1"}}
	e.Finalize()
	return e
}

// Usage of an event type no billable metric counts waits; usage of the
// others goes out. Once Finance adds the billable metric, the next delivery
// sends what waited first, and Metronome counts all of it.
func TestWaitingUsageIsSentOnceItsBillableMetricExists(t *testing.T) {
	f := newFake(t)
	financeMetric(t, f, usage.MetricCPUCoreHours, "CPU hours")
	if _, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme"); err != nil {
		t.Fatal(err)
	}
	a := newReadOnly(t, f)
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	first := []usage.Event{gpu("tenant-acme", "c1", now.Add(-3*time.Minute), 1), cpuEvent("tenant-acme", now.Add(-3*time.Minute), 2)}
	if err := a.SendEvents(ctx, first); err != nil {
		t.Fatal(err)
	}
	count := func(code string) int {
		n := 0
		for _, e := range f.Events() {
			if e.EventType == code {
				n++
			}
		}
		return n
	}
	if count(usage.MetricGPUHours) != 0 || count(usage.MetricCPUCoreHours) != 1 {
		t.Fatalf("sent %d GPU and %d CPU events; want the GPU usage held and the CPU usage sent", count(usage.MetricGPUHours), count(usage.MetricCPUCoreHours))
	}
	cov := coverageOf(t, a, gpuSeriesOf(a, "tenant-acme", "NVIDIA-H100-80GB-HBM3"), gpuSeriesOf(a, "tenant-nobody", "NVIDIA-H100-80GB-HBM3"))
	for _, c := range cov {
		if c.Status != destinations.PriceMissing || c.Hint != noMetricHint {
			t.Errorf("coverage of usage no billable metric counts (customer or not): %+v", c)
		}
	}

	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	a.now = func() time.Time { return now.Add(2 * time.Minute) } // the cached metrics are read again
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}
	evs := f.Events()
	if count(usage.MetricGPUHours) != 2 || evs[len(evs)-2].Timestamp != rfc3339(now.Add(-3*time.Minute)) {
		t.Fatalf("GPU events sent: %d (the waiting one first?): %+v", count(usage.MetricGPUHours), evs)
	}
	day := now.Truncate(24 * time.Hour)
	totals, err := a.RecordedTotals(ctx, destinations.TotalsQuery{From: day, To: day.Add(24 * time.Hour),
		Tenants: []usage.Tenant{{ID: "tenant-acme"}}, Metrics: []string{usage.MetricGPUHours}})
	if err != nil || len(totals) != 1 || totals[0].Quantity != 2 {
		t.Fatalf("Metronome counts %+v, %v; want the 2 GPU-hours, the waiting one included", totals, err)
	}
	if left, _ := a.wait.pending(); len(left) != 0 {
		t.Fatalf("still waiting: %v", left)
	}
}

// Waiting usage is kept on disk before SendEvents accepts it, so a restart
// keeps it; the next instance sends it once the billable metric exists.
func TestWaitingUsageSurvivesARestart(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	open := func() *Adapter {
		a := New(NewClient(f.URL, "mtk_test"), &config.Config{DataDir: dir}, kv.Memory(), nil)
		a.now = func() time.Time { return now }
		return a
	}
	ctx := context.Background()
	if err := open().SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-5*time.Minute), 1), gpu("tenant-acme", "c1", now.Add(-4*time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}
	if len(f.Events()) != 0 {
		t.Fatalf("%d event(s) sent without a billable metric", len(f.Events()))
	}
	if _, err := os.Stat(filepath.Join(dir, "metronome-waiting", usage.MetricGPUHours+".jsonl")); err != nil {
		t.Fatalf("waiting usage not on disk: %v", err)
	}
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	b := open()
	if err := b.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.Events()); n != 2 {
		t.Fatalf("after the restart, %d event(s) sent, want the 2 that waited", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "metronome-waiting", usage.MetricGPUHours+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("waiting file left after the release: %v", err)
	}
}

// Usage that waited longer than PRICE_CHECK_MAX_HOLD is sent anyway,
// before Metronome's 34-day limit, so Metronome support can still reflow
// it; younger usage keeps waiting.
func TestUsageThatWaitedTooLongIsSentAnyway(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f, func(c *config.Config) { c.PriceCheckMaxHold = time.Hour })
	ctx := context.Background()
	old, young := gpu("tenant-acme", "c1", now.Add(-3*time.Hour), 1), gpu("tenant-acme", "c1", now.Add(-10*time.Minute), 1)
	if err := a.SendEvents(ctx, []usage.Event{old, young}); err != nil {
		t.Fatal(err)
	}
	if len(f.Events()) != 0 {
		t.Fatal("sent at once")
	}
	if err := a.SendEvents(ctx, nil); err != nil {
		t.Fatal(err)
	}
	evs := f.Events()
	if len(evs) != 1 || evs[0].TransactionID != old.ID {
		t.Fatalf("sent %+v, want only the event that waited longer than the limit", evs)
	}
	if left, _ := a.wait.pending(); len(left) != 1 {
		t.Fatalf("waiting %v, want the younger event still held", left)
	}
}

// Without the billable metrics read once, nothing can be checked: delivery
// fails and is retried. With an earlier read, that one is used.
func TestBillableMetricsThatCannotBeReadHoldDelivery(t *testing.T) {
	f := newFake(t)
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	a := newReadOnly(t, f)
	ctx := context.Background()
	f.FailNext("GET /v1/billable-metrics", 1)
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)}); err == nil {
		t.Fatal("delivered without knowing which billable metrics exist")
	}
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return now.Add(2 * time.Minute) }
	f.FailNext("GET /v1/billable-metrics", 1)
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)}); err != nil {
		t.Fatalf("with an earlier read: %v", err)
	}
	if n := len(f.Events()); n != 2 {
		t.Fatalf("%d event(s) sent, want 2", n)
	}
}
