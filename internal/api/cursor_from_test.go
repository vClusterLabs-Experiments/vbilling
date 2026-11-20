package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A destination added to a running install starts at the ledger's end. An
// operator who wants it to have the last hours too rewinds its cursor to a
// time: everything the ledger committed since then is sent again, and
// nothing committed before.
func TestCursorRewindsToATime(t *testing.T) {
	clock := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	cfg := &config.Config{Region: "us-central1", Adapters: []string{"fakebill"}, CollectionInterval: time.Minute,
		APIToken: adminToken, IngestToken: "ingest-secret", EnforcementMode: "observe"}
	dest := &recDest{recorded: map[string]float64{}, reject: map[string]bool{}}
	disp := pipeline.New(sp, []destinations.Destination{dest}, pipeline.Options{}, nil)
	tel := telemetry.New()
	s := New(cfg, sp, disp, pipeline.NewTenantRegistry(), nil, enforcement.New(nil, enforcement.Options{Mode: "observe", Telemetry: tel}), tel, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Three windows committed a minute apart, two events each.
	commitAt := map[uint64]time.Time{}
	for i := 0; i < 3; i++ {
		ws := clock
		clock = clock.Add(time.Minute + 5*time.Second)
		var evs []usage.Event
		for _, tenant := range []string{"acme", "globex"} {
			ev := usage.Event{Tenant: tenant, Metric: usage.MetricInstanceHours, Quantity: 1.0 / 60, Region: "us-central1",
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: clock}
			ev.Finalize()
			evs = append(evs, ev)
		}
		if err := sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
		commitAt[sp.Committed()] = clock
	}
	if sp.Cursor("fakebill") != sp.Committed() {
		t.Fatalf("a new destination starts at %d, want the ledger's end %d", sp.Cursor("fakebill"), sp.Committed())
	}

	post := func(body string) (int, string) {
		t.Helper()
		resp := do(t, http.MethodPost, srv.URL+"/api/v1/destinations/fakebill/cursor", adminToken, strings.NewReader(body))
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// From the second window's commit: the second and third windows again.
	second := time.Date(2026, 11, 20, 0, 2, 10, 0, time.UTC)
	if code, body := post(`{"from":"` + second.Format(time.RFC3339) + `"}`); code != 200 {
		t.Fatalf("rewind to a time: %d %s", code, body)
	}
	recs, err := sp.Read(sp.Cursor("fakebill"), 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []*usage.Event
	for i := range recs {
		if recs[i].Type == spool.TypeEvent {
			if recs[i].At.Before(second) {
				t.Fatalf("record %d committed at %s, before the rewind time %s", recs[i].Seq, recs[i].At, second)
			}
			events = append(events, recs[i].Event)
		}
	}
	if len(events) != 4 {
		t.Fatalf("rewound to %d: %d events to send again, want the 4 committed since %s", sp.Cursor("fakebill"), len(events), second)
	}

	// From before the ledger: everything. After its end: nothing.
	if code, body := post(`{"from":"2026-11-19T00:00:00Z"}`); code != 200 || sp.Cursor("fakebill") != 0 {
		t.Fatalf("rewind to before the ledger: %d %s, cursor %d", code, body, sp.Cursor("fakebill"))
	}
	if code, body := post(`{"from":"2026-11-21T00:00:00Z"}`); code != 200 || sp.Cursor("fakebill") != sp.Committed() {
		t.Fatalf("rewind to after the ledger: %d %s, cursor %d of %d", code, body, sp.Cursor("fakebill"), sp.Committed())
	}
	var out struct {
		Cursor uint64 `json:"cursor"`
	}
	_, body := post(`{"seq":0}`)
	if json.Unmarshal([]byte(body), &out); out.Cursor != 0 {
		t.Fatalf("rewind by sequence number: %s", body)
	}
	for _, bad := range []string{`{}`, `{"seq":0,"from":"2026-11-19T00:00:00Z"}`, `{"from":"yesterday"}`} {
		if code, body := post(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", bad, code, body)
		}
	}
}
