package metronome

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func cpuEvent(tenant string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricCPUCoreHours, Quantity: q, Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: "c1"}}
	e.Finalize()
	return e
}

// Usage of an event type no billable metric counts is held; usage of the
// others goes out. Price coverage reports it uncounted for every tenant,
// customer or not. Once Finance adds the billable metric, the next check
// releases what was held, before newer usage of the series, and Metronome
// counts all of it.
func TestUncountedUsageIsHeldUntilABillableMetricCountsIt(t *testing.T) {
	f := newRigFake(t)
	financeMetric(t, f, usage.MetricCPUCoreHours, "CPU hours")
	if _, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme"); err != nil {
		t.Fatal(err)
	}
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL})
	at := rigNow()
	held, cpu := gpu("tenant-acme", "c1", at.Add(-3*time.Minute), 1), cpuEvent("tenant-acme", at.Add(-3*time.Minute), 2)
	r.commit(t, held, cpu)
	waitFor(t, "the CPU usage sent and the GPU usage held", func() bool { return r.delivered() && sentIDs(f)[cpu.ID] && r.held.Count(rigDest) == 1 })
	if sentIDs(f)[held.ID] {
		t.Fatal("GPU usage reached Metronome before a billable metric counted it")
	}
	for _, c := range coverageOf(t, r.a, gpuSeriesOf(r.a, "tenant-acme", "NVIDIA-H100-80GB-HBM3"), gpuSeriesOf(r.a, "tenant-nobody", "NVIDIA-H100-80GB-HBM3")) {
		if c.Status != destinations.PriceMissing || c.Hint != noMetricHint || !r.a.Uncounted(c) {
			t.Errorf("coverage of usage no billable metric counts (customer or not): %+v", c)
		}
	}

	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	newer := gpu("tenant-acme", "c1", at.Add(-2*time.Minute), 1)
	r.check()
	r.commit(t, newer)
	waitFor(t, "the held and the newer GPU usage sent", func() bool { s := sentIDs(f); return s[held.ID] && s[newer.ID] && r.held.Count(rigDest) == 0 })
	var order []string
	for _, e := range f.Events() {
		if e.EventType == usage.MetricGPUHours {
			order = append(order, e.TransactionID)
		}
	}
	if len(order) != 2 || order[0] != held.ID {
		t.Errorf("GPU events sent %v, want the held one first", order)
	}
	if got := r.counted(t, "tenant-acme", usage.MetricGPUHours, at); got != 2 {
		t.Fatalf("Metronome counts %v GPU-hours, want both, the held one included", got)
	}
}

// Held usage is in DATA_DIR before the dispatcher's cursor moves past it, so
// a restart keeps it, and the next instance sends it once a billable metric
// counts it.
func TestUncountedUsageSurvivesARestart(t *testing.T) {
	f := newRigFake(t)
	dir := t.TempDir()
	at := rigNow()
	evs := []usage.Event{gpu("tenant-acme", "c1", at.Add(-5*time.Minute), 1), gpu("tenant-acme", "c1", at.Add(-4*time.Minute), 1)}
	a := newHoldRig(t, dir, rigOptions{base: f.URL})
	a.commit(t, evs...)
	waitFor(t, "the GPU usage held", func() bool { return a.held.Count(rigDest) == 2 && a.delivered() })
	a.stop()
	if len(f.Events()) != 0 {
		t.Fatalf("%d event(s) sent without a billable metric", len(f.Events()))
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "held", rigDest, "*.jsonl")); len(files) != 1 {
		t.Fatalf("held usage not on disk: %v", files)
	}

	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	b := newHoldRig(t, dir, rigOptions{base: f.URL})
	waitFor(t, "the held usage sent after the restart", func() bool { return len(f.Events()) == 2 && b.held.Count(rigDest) == 0 })
	if _, err := os.Stat(filepath.Join(dir, "metronome-waiting")); !os.IsNotExist(err) {
		t.Errorf("DATA_DIR/metronome-waiting exists: %v", err)
	}
}

// Usage held for the longest hold (PRICE_CHECK_MAX_HOLD, at most 23 hours)
// is released without a billable metric, before its invoice period can be
// final; younger usage stays held.
func TestUncountedUsageHeldTooLongIsReleased(t *testing.T) {
	f := newRigFake(t)
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, maxHold: time.Hour})
	at := rigNow()
	old, young := gpu("tenant-acme", "c1", at.Add(-3*time.Hour), 1), gpu("tenant-acme", "c1", at.Add(-10*time.Minute), 1)
	r.commit(t, old, young)
	waitFor(t, "the old usage released, and counted as released without a billable metric", func() bool {
		return sentIDs(f)[old.ID] && r.held.Count(rigDest) == 1 && r.disp.Statuses()[0].ReleasedUnpriced == 1
	})
	if sentIDs(f)[young.ID] {
		t.Fatal("usage younger than the longest hold was released")
	}
}

// Held usage needs an answer to ship: after a restart, while Metronome's
// billable metrics cannot be read, it stays held, and it ships once a check
// reads that a billable metric counts it.
func TestUncountedUsageStaysHeldWhileBillableMetricsCannotBeRead(t *testing.T) {
	f := newRigFake(t)
	dir := t.TempDir()
	at := rigNow()
	ev := gpu("tenant-acme", "c1", at.Add(-2*time.Minute), 1)
	a := newHoldRig(t, dir, rigOptions{base: f.URL})
	a.commit(t, ev)
	waitFor(t, "the GPU usage held", func() bool { return a.held.Count(rigDest) == 1 && a.delivered() })
	a.stop()

	financeMetric(t, f, usage.MetricGPUHours, "GPU hours")
	f.FailNext("GET /v1/billable-metrics", 1_000_000)
	b := newHoldRig(t, dir, rigOptions{base: f.URL})
	time.Sleep(300 * time.Millisecond)
	b.check()
	if len(f.Events()) != 0 || b.held.Count(rigDest) != 1 {
		t.Fatalf("held usage shipped (%d sent, %d held) although no check could read whether a billable metric counts it", len(f.Events()), b.held.Count(rigDest))
	}
	f.FailNext("GET /v1/billable-metrics", 0)
	b.check()
	waitFor(t, "the held usage sent", func() bool { return sentIDs(f)[ev.ID] && b.held.Count(rigDest) == 0 })
}
