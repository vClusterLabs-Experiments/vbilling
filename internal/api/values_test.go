package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// SKUs and dimension values with markup, quotes or control characters, or
// longer than the cap, are refused at ingest with 400.
func TestIngestRefusesUnsafeValues(t *testing.T) {
	e := setup(t)
	ws, we := now.Add(-5*time.Minute).Format(time.RFC3339), now.Add(-4*time.Minute).Format(time.RFC3339)
	event := func(id, sku string, dims map[string]string) map[string]any {
		return map[string]any{"id": id, "tenant": "acme", "metric": pushedMetric, "quantity": 1, "sku": sku,
			"window_start": ws, "window_end": we, "dimensions": dims}
	}
	for name, ev := range map[string]map[string]any{
		"markup":    event("a", `<script>`, nil),
		"quote":     event("b", `x"y`, nil),
		"ampersand": event("c", "a&b", nil),
		"control":   event("d", "ok", map[string]string{"model": "a\nb"}),
		"long":      event("e", strings.Repeat("x", usage.MaxValueLen+1), nil),
	} {
		b, _ := json.Marshal([]any{ev})
		code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", string(b))
		if code != http.StatusBadRequest || out["accepted"] != float64(0) {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	// A batch keeps its valid events.
	b, _ := json.Marshal([]any{event("ok-1", "NVIDIA-H100", map[string]string{"model": "llama-3.1-70b"}), event("bad-1", "<b>", nil)})
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", string(b)); code != 200 || out["accepted"] != float64(1) {
		t.Fatalf("mixed batch: %d %v", code, out)
	}

	r := setupRecords(t)
	at := now.Add(-2 * time.Minute).Format(time.RFC3339)
	rec := fmt.Sprintf(`{"id":"r-1","tenant":"acme","time":%q,"usage":{"inference_output_tokens":9},"sku":"<img src=x>"}`, at)
	if code, out := postJSON(t, r.srv.URL+"/api/v1/records", "ingest-secret", rec); code != http.StatusBadRequest {
		t.Fatalf("records: %d %v, want 400", code, out)
	}
}
