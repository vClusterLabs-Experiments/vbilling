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

// Region, project, resource ID and dimensions of ingested events are
// checked like SKUs, and no dimension may take the name of a property of
// the event itself.
func TestIngestRefusesUnsafeFieldsAndReservedDimensions(t *testing.T) {
	e := setup(t)
	ws, we := now.Add(-5*time.Minute), now.Add(-4*time.Minute)
	event := func(id string, extra map[string]any) map[string]any {
		ev := map[string]any{"id": id, "tenant": "acme", "metric": pushedMetric, "quantity": 1, "window_start": ws, "window_end": we}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	many := map[string]string{}
	for i := 0; i <= usage.MaxDimensions; i++ {
		many[fmt.Sprintf("d%02d", i)] = "x"
	}
	for name, ev := range map[string]map[string]any{
		"region":       event("a", map[string]any{"region": "<b>"}),
		"project":      event("b", map[string]any{"project": "p\x00"}),
		"resource_id":  event("c", map[string]any{"resource_id": strings.Repeat("n", usage.MaxValueLen+1)}),
		"value":        event("d", map[string]any{"dimensions": map[string]string{"value": "0"}}),
		"sku":          event("e", map[string]any{"dimensions": map[string]string{"sku": "free-tier"}}),
		"window_start": event("f", map[string]any{"dimensions": map[string]string{"window_start": "x"}}),
		"too many":     event("g", map[string]any{"dimensions": many}),
	} {
		b, _ := json.Marshal([]any{ev})
		code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", string(b))
		if code != http.StatusBadRequest || out["accepted"] != float64(0) {
			t.Errorf("%s: %d %v, want 400", name, code, out)
		}
	}
	b, _ := json.Marshal([]any{event("ok", map[string]any{"region": "eu-west-1", "project": "ml", "resource_id": "node-1",
		"dimensions": map[string]string{"model": "llama"}})})
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", string(b)); code != http.StatusOK {
		t.Fatalf("a plain event: %d %v", code, out)
	}
}

// CSV exports escape cells a spreadsheet would run as a formula: tenants,
// IDs, SKUs and dimensions come from tenants and gateways.
func TestCSVExportsEscapeFormulas(t *testing.T) {
	e := setup(t)
	ws := now.Add(-5 * time.Minute)
	b, _ := json.Marshal([]map[string]any{{"id": "-1", "tenant": "=HYPERLINK(1)", "metric": pushedMetric, "quantity": 1, "sku": "@SUM(A1)",
		"window_start": ws, "window_end": ws.Add(time.Minute), "dimensions": map[string]string{"model": "+x"}}})
	if code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", string(b)); code != http.StatusOK {
		t.Fatalf("ingest: %d %v", code, out)
	}
	_, events := e.get(t, "/api/v1/events?format=csv&from="+ws.Add(-time.Hour).Format(time.RFC3339))
	_, rows := e.get(t, "/api/v1/usage?format=csv&group_by=tenant,sku,model&from="+ws.Add(-time.Hour).Format(time.RFC3339))
	for _, body := range []string{events, rows} {
		for _, cell := range strings.FieldsFunc(body, func(r rune) bool { return r == ',' || r == '\n' }) {
			if c := strings.Trim(cell, `"`); c != "" && strings.ContainsRune("=+-@", rune(c[0])) {
				t.Errorf("cell %q starts a formula:\n%s", c, body)
			}
		}
	}
	if !strings.Contains(events, "'=HYPERLINK(1)") || !strings.Contains(events, "'-1,") || !strings.Contains(rows, "'@SUM(A1)") || !strings.Contains(rows, "'+x") {
		t.Fatalf("escaped cells missing:\n%s\n%s", events, rows)
	}
}
