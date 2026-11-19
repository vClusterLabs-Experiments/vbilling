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

// X-VBilling-Client is a short plain name; anything else is refused before
// the body is read.
func TestIngestClientNamesAreValidated(t *testing.T) {
	e := setup(t)
	evs := gpuEvents(1, nil)
	evs[0]["tenant"] = "acme"
	good, _ := json.Marshal(evs)
	for _, bad := range []string{"-gw", ".gw", "gw/eu", "gw eu", "gwé", strings.Repeat("g", 65), "gw\"x"} {
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "ingest-secret", bad, good); code != http.StatusBadRequest {
			t.Errorf("client %q: %d, want 400", bad, code)
		}
	}
	for i, ok := range []string{"gw", "edge-gateway", "litellm.eu_1", strings.Repeat("g", 64), "0x"} {
		evs[0]["id"] = fmt.Sprintf("ev-%d", i)
		good, _ := json.Marshal(evs)
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "ingest-secret", ok, good); code != http.StatusOK {
			t.Errorf("client %q: %d, want 200", ok, code)
		}
	}
	if st := e.sp.Stats(); st.Committed == 0 {
		t.Fatal("nothing stored")
	}
}

// Each token may name a bounded number of clients while vBilling runs. A
// name counts once a request under it stored usage: requests that are
// refused cost nothing, and names already counted keep working.
func TestIngestClientNamesPerTokenAreCapped(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	bad, _ := json.Marshal([]map[string]any{{"id": "x", "metric": "no_such_metric", "quantity": 1,
		"window_start": now.Add(-5 * time.Minute), "window_end": now.Add(-4 * time.Minute)}})
	for i := 0; i < 100; i++ {
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", fmt.Sprintf("junk-%d", i), bad); code != http.StatusBadRequest {
			t.Fatalf("refused request %d: %d", i, code)
		}
	}
	post := func(token, client string, i int) int {
		body, _ := json.Marshal([]map[string]any{{"id": fmt.Sprintf("ev-%s-%d", client, i), "tenant": "acme", "metric": usage.MetricGPUHours, "quantity": 0.1,
			"window_start": now.Add(-5 * time.Minute), "window_end": now.Add(-4 * time.Minute)}})
		return postWithClient(t, e.srv.URL+"/api/v1/events", token, client, body)
	}
	for i := 0; i < maxClientsPerBoundToken; i++ {
		if code := post("tok-acme", fmt.Sprintf("gw-%d", i), 0); code != http.StatusOK {
			t.Fatalf("client %d of %d: %d", i+1, maxClientsPerBoundToken, code)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/events", bytes.NewReader([]byte(`[]`)))
	req.Header.Set("Authorization", "Bearer tok-acme")
	req.Header.Set("X-VBilling-Client", "one-more")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out["error"], "client names") {
		t.Fatalf("a client name past the limit: %d %v", resp.StatusCode, out)
	}
	if code := post("tok-acme", "gw-3", 1); code != http.StatusOK {
		t.Fatalf("a name the token used before: %d", code)
	}
	if code := post("ingest-secret", "operator-gw", 0); code != http.StatusOK {
		t.Fatalf("the shared token has its own names: %d", code)
	}
	var tel bytes.Buffer
	e.server.tel.WriteText(&tel)
	if n := strings.Count(tel.String(), "\nvbilling_ingested_events_total{"); n != maxClientsPerBoundToken+1 {
		t.Fatalf("%d series, want one per accepted client name", n)
	}
}

// Records sent under a client name that never had a request accepted add
// no health source and no metric series.
func TestRecordsClientsAppearOnlyOnceAccepted(t *testing.T) {
	e := setupRecords(t)
	at := now.Add(-2 * time.Minute).Format(time.RFC3339)
	bad := fmt.Sprintf(`{"id":"r-1","tenant":"acme","time":%q,"usage":{"no_such_metric":1}}`, at)
	for i := 0; i < 50; i++ {
		if code, _ := postJSON(t, e.srv.URL+"/api/v1/records", "gw-globex", bad, "X-VBilling-Client", fmt.Sprintf("junk-%d", i)); code != http.StatusBadRequest {
			t.Fatalf("refused records %d: %d", i, code)
		}
	}
	if n := len(e.health.List()); n != 0 {
		t.Fatalf("refused records from made-up clients created %d health sources: %+v", n, e.health.List())
	}
	good := fmt.Sprintf(`{"id":"r-2","tenant":"acme","time":%q,"usage":{"inference_output_tokens":9}}`, at)
	if code, out := postJSON(t, e.srv.URL+"/api/v1/records", "gw-globex", good, "X-VBilling-Client", "edge-gw"); code != http.StatusOK {
		t.Fatalf("accepted records: %d %v", code, out)
	}
	if l := e.health.List(); len(l) != 1 || l[0].Name != "records/edge-gw" {
		t.Fatalf("health after an accepted push: %+v", l)
	}
	// From then on, refused records of that client are counted.
	if code, _ := postJSON(t, e.srv.URL+"/api/v1/records", "gw-globex", bad, "X-VBilling-Client", "edge-gw"); code != http.StatusBadRequest {
		t.Fatalf("refused records of a known client: %d", code)
	}
	var tel bytes.Buffer
	e.server.tel.WriteText(&tel)
	if n := strings.Count(tel.String(), "\nvbilling_records_received_total{"); n != 1 || !strings.Contains(tel.String(), `vbilling_records_rejected_total{client="edge-gw"} 1`) {
		t.Fatalf("%d received series; want one, for edge-gw, which counts its refused record:\n%s", n, tel.String())
	}
}
