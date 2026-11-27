package api

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// gpuWindow is what one window of env.commit adds to acme's GPU hours.
var gpuWindow = 2 * usage.Round(8.0/60)

func near(got, want float64) bool { return math.Abs(got-want) < 1e-6 }

// reconciled answers GET /api/v1/reconcile for the day before now.
func reconciled(t *testing.T, e *env) reconcileResult {
	t.Helper()
	code, body := e.get(t, "/api/v1/reconcile")
	var resp struct {
		Results []reconcileResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || code != 200 || len(resp.Results) != 1 {
		t.Fatalf("reconcile: %d %s", code, body)
	}
	return resp.Results[0]
}

func gpuRow(t *testing.T, res reconcileResult) reconcileRow {
	t.Helper()
	if len(res.Rows) != 1 || res.Rows[0].Tenant != "acme" || res.Rows[0].Metric != usage.MetricGPUHours {
		t.Fatalf("rows: %+v", res.Rows)
	}
	return res.Rows[0]
}

// A destination added in the middle of a day starts at the
// ledger head. Reconciliation used to report every earlier quantity of the
// day as missing; now it reports it as before_start and compares what the
// destination was meant to receive. A gap in that is still a mismatch.
func TestReconcileReportsUsageRecordedBeforeADestinationStarted(t *testing.T) {
	e := setup(t)
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.commit(t, 10, day.Add(time.Hour)) // the day's usage before the destination exists
	head := e.sp.Committed()
	e.sp.Cursor("fakebill") // added now: it starts at the head
	e.commit(t, 5, day.Add(7*time.Hour))
	e.deliver(t)

	res := reconciled(t, e)
	row := gpuRow(t, res)
	if !res.OK || res.Error != "" || res.Start == nil || res.Start.Seq != head || res.Start.How != spool.StartCreated {
		t.Fatalf("result: %+v, start %+v", res, res.Start)
	}
	if !near(row.Ledger, 15*gpuWindow) || !near(row.BeforeStart, 10*gpuWindow) || !near(row.Expected, 5*gpuWindow) || !near(row.Recorded, 5*gpuWindow) ||
		row.DeadLettered != 0 || !near(row.Difference, 0) || !row.Match {
		t.Fatalf("row: %+v", row)
	}
	// The row adds up, and the ledger column is still the usage API's.
	if !near(row.Ledger, row.BeforeStart+row.DeadLettered+row.Expected) {
		t.Fatalf("ledger %v != before_start %v + dead_lettered %v + expected %v", row.Ledger, row.BeforeStart, row.DeadLettered, row.Expected)
	}

	// A real gap in what it was meant to receive is a mismatch.
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] -= gpuWindow
	e.dest.mu.Unlock()
	if res := reconciled(t, e); res.OK || gpuRow(t, res).Match || !near(gpuRow(t, res).Difference, -gpuWindow) {
		t.Fatalf("a delivery gap after the start was not reported: %+v", res.Rows)
	}
	// So is more than it was sent: a destination created at the head never
	// received anything recorded before.
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] += 3 * gpuWindow
	e.dest.mu.Unlock()
	if res := reconciled(t, e); res.OK || gpuRow(t, res).Match {
		t.Fatalf("more than expected at a destination created at the head was accepted: %+v", res.Rows)
	}
}

// A destination an operator positioned had an earlier life: what was
// recorded before its start is not expected of it, and what it holds of it
// is not a mismatch. Less than what was recorded after its start still is.
func TestReconcileOfADestinationAnOperatorPositioned(t *testing.T) {
	e := setup(t)
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.sp.Cursor("fakebill") // there from the beginning
	e.commit(t, 10, day.Add(time.Hour))
	e.deliver(t)
	if res := reconciled(t, e); !res.OK || gpuRow(t, res).BeforeStart != 0 || !near(gpuRow(t, res).Expected, 10*gpuWindow) || res.Start == nil || res.Start.Seq != 0 {
		t.Fatalf("a destination that was there from the beginning: %+v", res)
	}

	resp := do(t, http.MethodPost, e.srv.URL+"/api/v1/destinations/fakebill/cursor", e.token, strings.NewReader(`{"from":"2026-12-01T00:00:00Z"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 || e.sp.Cursor("fakebill") != e.sp.Committed() {
		t.Fatalf("position at the head: %d", resp.StatusCode)
	}
	e.commit(t, 5, day.Add(7*time.Hour))
	e.deliver(t)

	res := reconciled(t, e)
	row := gpuRow(t, res)
	if !res.OK || res.Start == nil || res.Start.How != spool.StartPositioned || !row.Match || !near(row.BeforeStart, 10*gpuWindow) ||
		!near(row.Expected, 5*gpuWindow) || !near(row.Recorded, 15*gpuWindow) || !strings.Contains(row.Note, "before the destination's start") {
		t.Fatalf("a positioned destination that holds earlier usage: ok %v, row %+v", res.OK, row)
	}
	// It holds less than what was recorded after its start: missing.
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] = 4 * gpuWindow
	e.dest.mu.Unlock()
	if res := reconciled(t, e); res.OK || gpuRow(t, res).Match {
		t.Fatalf("a gap after the start of a positioned destination was not reported: %+v", res.Rows)
	}
	// It holds more than everything the ledger has: wrong.
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] = 16 * gpuWindow
	e.dest.mu.Unlock()
	if res := reconciled(t, e); res.OK || gpuRow(t, res).Match {
		t.Fatalf("more than the ledger holds was accepted: %+v", res.Rows)
	}
}

// A destination an earlier version created has no recorded start: it is
// compared from the beginning, as before.
func TestReconcileWithoutARecordedStartComparesFromTheBeginning(t *testing.T) {
	dir := t.TempDir()
	clock := func() time.Time { return now }
	sp, err := spool.Open(dir, spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	e := usageServer(t, sp, nil)
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.commit(t, 10, day.Add(time.Hour))
	head := sp.Committed()
	sp.Close()
	// What v0.3.2 left behind for a destination added at this point: a
	// cursor at the head, and nothing else.
	if err := os.WriteFile(filepath.Join(dir, "cursors.json"), []byte(`{"fakebill": 40}`), 0o640); err != nil || head != 40 {
		t.Fatalf("write cursors.json: %v (head %d)", err, head)
	}
	if sp, err = spool.Open(dir, spool.Options{NoSync: true, Now: clock}); err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	e = usageServer(t, sp, nil)
	e.commit(t, 5, day.Add(7*time.Hour))
	e.deliver(t)

	code, body := e.get(t, "/api/v1/reconcile")
	if code != 200 || strings.Contains(body, `"start"`) {
		t.Fatalf("a destination without a recorded start must not report one: %d %s", code, body)
	}
	res := reconciled(t, e)
	row := gpuRow(t, res)
	if res.OK || row.Match || row.BeforeStart != 0 || !near(row.Expected, 15*gpuWindow) || !near(row.Recorded, 5*gpuWindow) {
		t.Fatalf("without a recorded start the whole day is expected, as before: ok %v, row %+v", res.OK, row)
	}
}

// A dead letter of an event recorded before the start (a held event the
// operator skipped at the start) is part of before_start: only what the
// destination refused of the usage it was meant to receive is subtracted.
// And an event recorded both before and after the start was meant for it:
// it received the later copy.
func TestReconcileCountsDeadLettersAndRepeatedEventsOnTheRightSide(t *testing.T) {
	e := setup(t)
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.commit(t, 4, day.Add(time.Hour))
	var early, repeated usage.Event
	e.sp.Scan(day, day.Add(24*time.Hour), func(ev *usage.Event) error {
		if ev.Metric == usage.MetricGPUHours && early.ID == "" {
			early = *ev
		} else if ev.Metric == usage.MetricGPUHours && repeated.ID == "" {
			repeated = *ev
		}
		return nil
	})
	e.sp.Cursor("fakebill") // starts here
	if err := e.sp.AddDeadLetter("fakebill", early, "skipped at start by the operator"); err != nil {
		t.Fatal(err)
	}
	// The same event again from another source, after the start.
	if err := e.sp.AppendWindow("prometheus", repeated.WindowEnd, []usage.Event{repeated}, ""); err != nil {
		t.Fatal(err)
	}
	e.commit(t, 3, day.Add(7*time.Hour))
	var late usage.Event
	e.sp.Scan(day.Add(7*time.Hour), day.Add(24*time.Hour), func(ev *usage.Event) error {
		if ev.Metric == usage.MetricGPUHours && late.ID == "" {
			late = *ev
		}
		return nil
	})
	e.dest.reject[late.ID] = true // refused of what it was meant to receive
	e.deliver(t)

	res := reconciled(t, e)
	row := gpuRow(t, res)
	q := usage.Round(8.0 / 60)
	if !res.OK || !row.Match || !near(row.Ledger, 7*gpuWindow) || !near(row.BeforeStart, 4*gpuWindow-q) || !near(row.DeadLettered, q) ||
		!near(row.Expected, 3*gpuWindow) || !near(row.Recorded, 3*gpuWindow) {
		t.Fatalf("ok %v, row %+v", res.OK, row)
	}
}
