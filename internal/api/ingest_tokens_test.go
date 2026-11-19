package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Records sent with two tokens bound to the same tenant, under the same
// client and request ID, get IDs of each token's own (fourth review,
// finding 3): neither copy can take the other's place, so both are stored.
func TestRecordsOfEachBoundTokenGetIDsOfTheirOwn(t *testing.T) {
	e := setupRecords(t)
	e.server.cfg.IngestTokens["tok-globex-own"] = "globex" // gw-globex is the operator's LiteLLM that serves globex
	at := now.Add(-2 * time.Minute).Unix()
	litellm := func(prompt int) string {
		return fmt.Sprintf(`[{"id":"chatcmpl-1","litellm_call_id":"call-1","status":"success","model_group":"llama-70b","prompt_tokens":%d,`+
			`"completion_tokens":1,"endTime":%d,"metadata":{"user_api_key_team_id":"globex"}}]`, prompt, at)
	}
	for _, tok := range []string{"tok-globex-own", "gw-globex"} {
		if code, out := postJSON(t, e.srv.URL+"/api/v1/records?format=litellm", tok, litellm(map[string]int{"tok-globex-own": 1, "gw-globex": 512}[tok])); code != http.StatusOK || out["accepted"].(float64) != 1 {
			t.Fatalf("%s: %d %v", tok, code, out)
		}
	}
	var input float64
	ids := map[string]bool{}
	err := e.records.ScanRecords(now.Add(-time.Hour), now.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		if ev.Metric == "inference_input_tokens" {
			input += ev.Quantity
			ids[ev.ID] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if input != 513 || len(ids) != 2 {
		t.Fatalf("stored %v input tokens under %d record ID(s), want both copies (513) under two", input, len(ids))
	}
}

// Properties stay what they are for: numbers, booleans and plain strings
// under names like dimension keys. Anything else is refused with the
// event, and the answer lists at most the first 100 refusals, with a count.
func TestIngestedEventPropertiesAndRejectionCount(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	ws := now.Add(-5 * time.Minute)
	ev := func(id string, props map[string]any) map[string]any {
		return map[string]any{"id": id, "metric": usage.MetricGPUHours, "quantity": 1, "unit": "GPU-hours", "properties": props,
			"window_start": ws, "window_end": ws.Add(time.Minute)}
	}
	good, _ := json.Marshal([]any{ev("ok", map[string]any{"gpu_devices": 6, "pods": 3, "backfilled": true, "note": "nightly run"})})
	if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", "", good); code != http.StatusOK {
		t.Fatalf("plain properties refused: %d", code)
	}
	for name, props := range map[string]map[string]any{
		"nested":          {"meta": map[string]any{"a": 1}},
		"a list":          {"tags": []any{"a"}},
		"null":            {"x": nil},
		"reserved name":   {"region": "eu"},
		"vBilling's name": {"client_event_id": "x"},
		"bad name":        {"Note": "x"},
	} {
		b, _ := json.Marshal([]any{ev("bad-"+strings.ReplaceAll(name, " ", "-"), props)})
		if code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", "", b); code != http.StatusBadRequest {
			t.Errorf("properties with %s: %d, want 400", name, code)
		}
	}

	var many []any
	for i := 0; i < 250; i++ {
		many = append(many, ev(fmt.Sprintf("x-%d", i), map[string]any{"note": "<b>"}))
	}
	b, _ := json.Marshal(many)
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "tok-acme", string(b))
	if code != http.StatusBadRequest || len(out["rejected"].([]any)) != maxRejections || out["rejected_count"].(float64) != 250 {
		t.Fatalf("250 refused events: %d, %d listed, count %v", code, len(out["rejected"].([]any)), out["rejected_count"])
	}
}

// Events sent with two tokens bound to the same tenant, under the same
// client and the same id, or without an id and with the same identity, get
// IDs of each token's own: a tenant's own token can never take the ID of an
// event the operator's gateway sends for it, so both copies are stored.
func TestEventsOfEachBoundTokenGetIDsOfTheirOwn(t *testing.T) {
	e := setup(t, func(c *config.Config) {
		c.IngestTokens = map[string]string{"tok-acme-gw": "acme", "tok-acme-own": "acme"}
	})
	ws := now.Add(-5 * time.Minute)
	for _, id := range []string{`"id":"evt-1",`, ""} {
		for _, tok := range []string{"tok-acme-own", "tok-acme-gw"} {
			q := map[string]string{"tok-acme-own": "0.000001", "tok-acme-gw": "8"}[tok]
			body := fmt.Sprintf(`[{%s"metric":%q,"quantity":%s,"sku":"NVIDIA-H100","window_start":%q,"window_end":%q}]`,
				id, usage.MetricGPUHours, q, ws.Format(time.RFC3339), ws.Add(time.Minute).Format(time.RFC3339))
			code, out := postClientJSON(t, e.srv.URL+"/api/v1/events", tok, "gateway", body)
			if code != http.StatusOK || out["accepted"] != float64(1) {
				t.Fatalf("%s %s: %d %v: the copy sent with another token bound to the tenant took this event's ID", tok, id, code, out)
			}
		}
	}
}

// An event a tenant-bound token sent with an id to v0.3.0, which scoped
// the id to the tenant and client only, counts once when retried after the
// upgrade.
func TestBoundEventRetriedAcrossTheUpgradeCountsOnce(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	ws := now.Add(-5 * time.Minute)
	old := usage.Event{ID: v030TenantEventID("acme", "api", "evt-1"), Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 1,
		SKU: "NVIDIA-H100", Region: "ap-southeast-2", WindowStart: ws, WindowEnd: ws.Add(time.Minute), Source: "ingest:api", RecordedAt: now}
	if _, _, err := e.sp.AppendIngest(old.Source, []usage.Event{old}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`[{"id":"evt-1","metric":%q,"quantity":1,"sku":"NVIDIA-H100","window_start":%q,"window_end":%q}]`,
		usage.MetricGPUHours, ws.Format(time.RFC3339), ws.Add(time.Minute).Format(time.RFC3339))
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "tok-acme", body); code != http.StatusOK || out["accepted"] != float64(0) || out["duplicates"] != float64(1) {
		t.Fatalf("retry across the upgrade: %d %v, want a duplicate", code, out)
	}
}

func postClientJSON(t *testing.T, url, token, client, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-VBilling-Client", client)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}
