package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Third review: what a tenant sends with an ingest token bound to it.

// gpuEvents returns n valid GPU-hour events with IDs, in the window that
// ended five minutes before the test clock.
func gpuEvents(n int, sku func(i int) string) []map[string]any {
	ws := now.Add(-5 * time.Minute)
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"id": fmt.Sprintf("ev-%d", i), "metric": usage.MetricGPUHours, "quantity": 0.000001,
			"window_start": ws, "window_end": ws.Add(time.Minute)}
		if sku != nil {
			out[i]["sku"] = sku(i)
		}
	}
	return out
}

func postWithClient(t *testing.T, url, token, client string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if client != "" {
		req.Header.Set("X-VBilling-Client", client)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// X-VBilling-Client is chosen by whoever holds the token and is copied,
// unbounded, into the Source of every event of the request. One request with
// a large header and a small body writes the header once per event: with the
// default 1 MiB header limit and 5,000 events, one request asks for 5 GB in
// one buffer (the chart's memory limit is 256Mi) or fills the 5Gi ledger
// volume, and metering stops for every tenant.
//
// The fix bounds the name's length and character set and refuses anything
// else with 400 (and the server's headers to 64 KiB), so this request is
// refused and writes nothing. A valid name costs at most its own length
// per event.
func TestReview3ClientHeaderIsCopiedIntoEveryLedgerRecord(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	body, _ := json.Marshal(gpuEvents(200, nil))
	client := strings.Repeat("c", 64<<10) // 64 KiB, far below the 1 MiB header limit
	before := e.sp.Stats().Bytes
	if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", client, body); code != http.StatusBadRequest {
		t.Fatalf("ingest answered %d, want 400 for a 64 KiB client name", code)
	}
	grew := e.sp.Stats().Bytes - before
	sent := int64(len(body) + len(client))
	if grew > 4*sent || grew != 0 {
		t.Fatalf("a %d-byte request (%d of it the client header) grew the ledger by %d bytes (%.0fx): "+
			"the header is stored with every event; bound its length and character set (or take the client from the token)",
			sent, len(client), grew, float64(grew)/float64(sent))
	}
	// The longest valid name: each event stores it once, nothing more.
	growth := func(client string) int64 {
		e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
		before := e.sp.Stats().Bytes
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", client, body); code != http.StatusOK {
			t.Fatalf("client %q: ingest answered %d", client, code)
		}
		return e.sp.Stats().Bytes - before
	}
	long, short := growth(strings.Repeat("c", 64)), growth("")
	if extra := long - short; extra > 200*2*64 {
		t.Fatalf("a 64-character client name added %d bytes to 200 events, more than twice its length per event", extra)
	}
}

// Every distinct X-VBilling-Client value creates telemetry series that are
// never removed (and, for records, a health source). Requests that are all
// rejected cost the tenant nothing and still add a series each: memory and
// /metrics output grow without bound at the token's 500 requests per second.
func TestReview3ClientNamesGrowTelemetryWithoutBound(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	bad, _ := json.Marshal([]map[string]any{{"id": "x", "metric": "no_such_metric", "quantity": 1,
		"window_start": now.Add(-5 * time.Minute), "window_end": now.Add(-4 * time.Minute)}})
	const n = 300
	for i := 0; i < n; i++ {
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", fmt.Sprintf("gw-%d", i), bad); code != http.StatusBadRequest {
			t.Fatalf("request %d: %d, want 400 (nothing valid)", i, code)
		}
	}
	var out bytes.Buffer
	e.server.tel.WriteText(&out)
	series := strings.Count(out.String(), "\nvbilling_ingested_events_total{")
	if series > 10 {
		t.Fatalf("%d rejected requests from one tenant-bound token created %d vbilling_ingested_events_total series "+
			"(one per client name it chose); cardinality must not follow a header the tenant writes", n, series)
	}
}

// The SKU cap counts every SKU a tenant's usage brings in, whoever reports
// it. A tenant fills its own day's cap through its bound token with 100
// throwaway SKUs (one microunit each), and the usage the operator's gateway
// reports for it afterwards, through the shared token, is stored under
// sku-limit-exceeded: a SKU no rate card or price card prices unless the
// operator thought of it, so the tenant's real model usage is billed at no
// price for the rest of the UTC day.
func TestReview3BoundTokenFillsTheSKUCapOfTheOperatorsGateway(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	junk, _ := json.Marshal(gpuEvents(maxSKUsPerTenantPerDay, func(i int) string { return fmt.Sprintf("junk-%d", i) }))
	if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", "", junk); code != http.StatusOK {
		t.Fatalf("junk SKUs: %d", code)
	}
	// The operator's gateway (shared ingest token) reports acme's real usage.
	ws := now.Add(-3 * time.Minute)
	real, _ := json.Marshal([]map[string]any{{"id": "gw-req-1", "tenant": "acme", "metric": usage.MetricGPUHours, "quantity": 2,
		"sku": "NVIDIA-H100-80GB-HBM3", "window_start": ws, "window_end": ws.Add(time.Minute)}})
	if code := postWithClient(t, e.srv.URL+"/api/v1/events", "ingest-secret", "operator-gw", real); code != http.StatusOK {
		t.Fatalf("gateway usage: %d", code)
	}
	var stored string
	err := e.sp.ScanAll(now.Add(-time.Hour), now, func(ev *usage.Event) error {
		if ev.Source == "ingest:operator-gw" {
			stored = ev.SKU
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored != "NVIDIA-H100-80GB-HBM3" {
		t.Fatalf("the operator's gateway reported SKU NVIDIA-H100-80GB-HBM3 for acme and it was stored as %q: "+
			"acme's own bound token used up the cap", stored)
	}
}
