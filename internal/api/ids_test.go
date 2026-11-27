package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// An id sent with the shared ingest token is kept, so it must not take a
// prefix vBilling derives its own IDs with.
func TestIngestRefusesReservedIDs(t *testing.T) {
	e := setup(t)
	ws, we := now.Add(-5*time.Minute).Format(time.RFC3339), now.Add(-4*time.Minute).Format(time.RFC3339)
	for _, id := range []string{"vb1_72ee4bf14f34feed3f56dd67b3481196", "te1_0", "ie1_0"} {
		body := fmt.Sprintf(`[{"id":%q,"tenant":"acme","metric":%q,"quantity":1,"window_start":%q,"window_end":%q}]`, id, pushedMetric, ws, we)
		if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", body); code != http.StatusBadRequest {
			t.Errorf("id %s: %d %v, want 400", id, code, out)
		}
	}
	body := fmt.Sprintf(`[{"id":"gw-123","tenant":"acme","metric":%q,"quantity":1,"window_start":%q,"window_end":%q}]`, pushedMetric, ws, we)
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", body); code != 200 || out["ids"].([]any)[0] != "gw-123" {
		t.Fatalf("an ordinary id is kept as is: %d %v", code, out)
	}
}

// Events without an id get an ID in the ingest namespace: never the
// collector's, different per token and client, the same on a retry. A
// retry of an event v0.3.0 stored under its derived ID counts once.
func TestIngestIDsWithoutAnIDAreNamespaced(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	ws := now.Add(-5 * time.Minute)
	ev := usage.Event{Tenant: "acme", Metric: pushedMetric, SKU: "NVIDIA-H100", Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	ev.Finalize()
	collectorID := ev.ID
	body := fmt.Sprintf(`[{"tenant":"acme","metric":%q,"quantity":1,"sku":"NVIDIA-H100","window_start":%q,"window_end":%q}]`,
		pushedMetric, ws.Format(time.RFC3339), ws.Add(time.Minute).Format(time.RFC3339))

	ids := map[string]string{}
	for _, tok := range []string{"ingest-secret", "tok-acme"} {
		code, out := postJSON(t, e.srv.URL+"/api/v1/events", tok, body)
		if code != 200 || out["accepted"] != float64(1) {
			t.Fatalf("%s: %d %v", tok, code, out)
		}
		id := out["ids"].([]any)[0].(string)
		if !strings.HasPrefix(id, "ie1_") || id == collectorID {
			t.Fatalf("%s: ID %s is not in the ingest namespace", tok, id)
		}
		ids[tok] = id
		if code, out := postJSON(t, e.srv.URL+"/api/v1/events", tok, body); code != 200 || out["duplicates"] != float64(1) {
			t.Fatalf("%s: a retry must deduplicate: %d %v", tok, code, out)
		}
	}
	if ids["ingest-secret"] == ids["tok-acme"] {
		t.Fatal("the shared and the bound token produced the same ID")
	}
	if want := IngestEventID("ingest:api", "acme", limitKey("tok-acme"), "api", collectorID); ids["tok-acme"] != want {
		t.Fatalf("bound ID %s, want %s", ids["tok-acme"], want)
	}

	// v0.3.0 stored an id-less event under its derived ID; its retry after
	// the upgrade is a duplicate, not new usage.
	old := usage.Event{Tenant: "acme", Metric: pushedMetric, Quantity: 1, SKU: "NVIDIA-L40S", Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute), Source: "ingest:api", RecordedAt: now}
	old.Finalize()
	if _, _, err := e.sp.AppendIngest(old.Source, []usage.Event{old}); err != nil {
		t.Fatal(err)
	}
	retry := strings.Replace(body, "NVIDIA-H100", "NVIDIA-L40S", 1)
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", retry); code != 200 || out["accepted"] != float64(0) || out["duplicates"] != float64(1) {
		t.Fatalf("a retry across the upgrade must count once: %d %v", code, out)
	}
}
