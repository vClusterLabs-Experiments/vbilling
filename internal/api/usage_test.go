package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// usageServer is an API over a ledger with an archive and a held store.
func usageServer(t *testing.T, sp *spool.Spool, held *pipeline.HeldStore) *env {
	t.Helper()
	cfg := &config.Config{Region: "ap-southeast-2", Adapters: []string{"fakebill"}, CollectionInterval: time.Minute, EnforcementMode: "observe", APIToken: adminToken}
	dest := &recDest{recorded: map[string]float64{}, reject: map[string]bool{}}
	disp := pipeline.New(sp, []destinations.Destination{dest}, pipeline.Options{Held: held}, nil)
	s := New(cfg, sp, disp, pipeline.NewTenantRegistry(), nil, nil, telemetry.New(), "test")
	s.now = func() time.Time { return now }
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, server: s, sp: sp, dest: dest, disp: disp, token: adminToken}
}

func gpuHour(tenant, sku string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: q, SKU: sku, WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
	e.Finalize()
	return e
}

type usageRow struct {
	Tenant       string             `json:"tenant"`
	SKU          string             `json:"sku"`
	Metric       string             `json:"metric"`
	Quantity     float64            `json:"quantity"`
	Events       int                `json:"events"`
	DeadLettered map[string]float64 `json:"dead_lettered"`
	Held         map[string]float64 `json:"held"`
}

func usageRows(t *testing.T, e *env, query string) ([]usageRow, int) {
	t.Helper()
	code, body := e.get(t, "/api/v1/usage?"+query)
	if code != 200 {
		t.Fatalf("usage: %d %s", code, body)
	}
	var resp struct {
		Rows     []usageRow `json:"rows"`
		Repeated int        `json:"repeated_ids"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Rows, resp.Repeated
}

// Usage counts each event ID once (billing platforms keep one copy), and
// shows apart the parts a destination refused for good (dead letters, each
// parked once even when a retried batch rejected it again) or holds until
// priced.
func TestUsageCountsEachEventOnceAndShowsUndelivered(t *testing.T) {
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	held, err := pipeline.OpenHeld(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	e := usageServer(t, sp, held)
	ws := now.Add(-2 * time.Hour)
	h100, l4 := gpuHour("acme", "H100", ws, 1), gpuHour("acme", "L4", ws, 0.5)
	if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), []usage.Event{h100, l4}, ""); err != nil {
		t.Fatal(err)
	}
	// The same event again under another source (as the v0.2 to v0.3 egress
	// move could): one copy is usage.
	if err := sp.AppendWindow("prometheus", ws.Add(time.Minute), []usage.Event{h100}, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // a bisected batch retried: rejected twice
		if err := sp.AddDeadLetter("fakebill", l4, "customer not found"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := held.Add("fakebill", []pipeline.HeldEntry{{Seq: 1, At: now, Series: "h100", Reason: "no price", Event: h100}}); err != nil {
		t.Fatal(err)
	}

	rows, repeated := usageRows(t, e, "group_by=tenant,sku")
	if repeated != 1 || len(rows) != 2 {
		t.Fatalf("rows %+v, repeated %d: want 2 rows and 1 repeated ID", rows, repeated)
	}
	for _, r := range rows {
		switch r.SKU {
		case "H100":
			if r.Quantity != 1 || r.Events != 1 || r.Held["fakebill"] != 1 || r.DeadLettered != nil {
				t.Errorf("H100 row %+v: want 1 GPU-hour once, all of it held", r)
			}
		case "L4":
			if r.Quantity != 0.5 || r.DeadLettered["fakebill"] != 0.5 || r.Held != nil {
				t.Errorf("L4 row %+v: want 0.5 GPU-hours, dead-lettered once", r)
			}
		}
	}
}

// A batch bisected after a rejection and retried after a transient error
// rejects the same event again. Reconciliation subtracts it once.
func TestReconcileSubtractsARetriedDeadLetterOnce(t *testing.T) {
	e := setup(t)
	e.sp.Cursor("fakebill")
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.commit(t, 10, day.Add(time.Hour))
	var victim usage.Event
	e.sp.Scan(day, day.Add(24*time.Hour), func(ev *usage.Event) error {
		if victim.ID == "" && ev.Metric == usage.MetricGPUHours {
			victim = *ev
		}
		return nil
	})
	e.dest.reject[victim.ID] = true
	e.deliver(t)
	if err := e.sp.AddDeadLetter("fakebill", victim, "unknown customer"); err != nil { // the retried half
		t.Fatal(err)
	}
	code, body := e.get(t, "/api/v1/reconcile")
	var resp struct {
		Results []reconcileResult `json:"results"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &resp) != nil || len(resp.Results) != 1 || len(resp.Results[0].Rows) != 1 {
		t.Fatalf("reconcile: %d %s", code, body)
	}
	if row := resp.Results[0].Rows[0]; row.DeadLettered != usage.Round(victim.Quantity) || !row.Match {
		t.Fatalf("row %+v: want the dead letter (%v) subtracted once and the totals to match", row, victim.Quantity)
	}
}

// Month to date reads through the archive: windows that compaction moved
// out of the live ledger after retention still count.
func TestUsageMonthToDateSurvivesCompaction(t *testing.T) {
	data := t.TempDir()
	clock := now.Add(-4 * 24 * time.Hour)
	sp, err := spool.Open(filepath.Join(data, "ledger"), spool.Options{NoSync: true, SegmentMaxRecords: 3, Retention: time.Hour,
		ArchiveDir: filepath.Join(data, "archive"), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	e := usageServer(t, sp, nil)
	sp.Cursor("fakebill")
	for d := 4; d >= 1; d-- { // one window a day, the last four days
		clock = now.Add(-time.Duration(d) * 24 * time.Hour)
		ws := clock.Add(-time.Minute)
		if err := sp.AppendWindow(spool.SourceCollector, clock, []usage.Event{gpuHour("acme", "H100", ws, 2)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	if err := sp.Ack("fakebill", sp.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := sp.Compact([]string{"fakebill"}); err != nil || n == 0 {
		t.Fatalf("compacted %d segments (%v)", n, err)
	}
	rows, _ := usageRows(t, e, "") // month to date
	if len(rows) != 1 || rows[0].Quantity != 8 || rows[0].Events != 4 {
		t.Fatalf("month to date after compaction: %+v, want 8 GPU-hours over 4 events", rows)
	}
}

// Counting each event ID once must not keep every ID of the range: the
// dashboard reads a month to date, which for a large installation is
// millions of collector events. Only IDs that can repeat are kept for the
// whole range (ingested events, fills); measured windows keep theirs for
// the last few hours of the scan.
func TestUsageKeepsFewIDsForALargeRange(t *testing.T) {
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	e := usageServer(t, sp, nil)
	const days, perWindow, ingested = 3, 10, 50
	start := now.Add(-days * 24 * time.Hour).Truncate(time.Minute)
	windows := days * 24 * 60
	for w := 0; w < windows; w++ {
		ws := start.Add(time.Duration(w) * time.Minute)
		evs := make([]usage.Event, perWindow)
		for i := range evs {
			evs[i] = gpuHour(fmt.Sprintf("tenant-%d", i), "H100", ws, 1)
		}
		if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
		if w == windows/2 { // the same window measured again under another source: one copy is usage
			if err := sp.AppendWindow("prometheus", ws.Add(time.Minute), evs[:1], ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	var in []usage.Event
	for i := 0; i < ingested; i++ {
		ev := gpuHour("acme", "H100", start.Add(time.Duration(i)*time.Hour), 1)
		ev.ID, ev.Source = fmt.Sprintf("gw-%d", i), "ingest:gw"
		in = append(in, ev)
	}
	if _, _, err := sp.AppendIngest("ingest:gw", in); err != nil {
		t.Fatal(err)
	}
	// The same ID ingested again after the ingest index forgot it (48h).
	if _, err := sp.AppendBatch("ingest:gw", in[:1]); err != nil {
		t.Fatal(err)
	}
	events := 0
	dups, kept, err := e.server.scanUsageStats(start, now, func(*usage.Event) error { events++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if events != windows*perWindow+ingested || dups != 2 {
		t.Fatalf("%d events counted and %d repeats skipped, want %d and 2", events, dups, windows*perWindow+ingested)
	}
	limit := ingested + perWindow*int(sameWindowSpan/time.Minute+2)
	if kept > limit {
		t.Fatalf("kept %d IDs at once for %d events (about %d KiB), want at most %d", kept, events, kept*16>>10, limit)
	}
	t.Logf("%d events, at most %d IDs kept at once (%d%%)", events, kept, kept*100/events)
}
