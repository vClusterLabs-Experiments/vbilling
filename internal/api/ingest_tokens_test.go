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
