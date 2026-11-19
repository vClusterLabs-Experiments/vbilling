package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Fourth review: the ingest fixes made after the third review.

// v0.3.0 without API_TOKEN generated the API token into DATA_DIR/api-token,
// and INGEST_TOKEN defaulted to it, so gateways were configured with it.
// Upgrading, an operator who keeps the gateways as they are sets
// INGEST_TOKEN to the token they send. Validate compares INGEST_TOKEN with
// API_TOKEN before the API token is read from its file (it is "" then), and
// PrepareTokens only replaces an INGEST_TOKEN equal to the API token before
// it loads that file. vBilling starts with the shared ingest token equal to
// the API token, which security.html says it refuses.
func TestReview4IngestTokenSetToTheGeneratedAPITokenIsRefused(t *testing.T) {
	dir := t.TempDir()
	generated, err := loadOrCreateToken(filepath.Join(dir, TokenFile)) // what v0.3.0 left in DATA_DIR
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADAPTER", "noop")
	t.Setenv("DATA_DIR", dir)
	t.Setenv("API_TOKEN", "")
	t.Setenv("INGEST_TOKEN", generated)
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		return // refused at start: what security.html promises
	}
	if _, err := PrepareTokens(cfg); err != nil {
		return
	}
	if cfg.IngestToken == cfg.APIToken {
		e := setup(t, func(c *config.Config) { c.APIToken, c.IngestToken = cfg.APIToken, cfg.IngestToken })
		resp := do(t, http.MethodGet, e.srv.URL+"/api/v1/billing-states", cfg.IngestToken, nil)
		resp.Body.Close()
		t.Errorf("INGEST_TOKEN set to the API token vBilling keeps in DATA_DIR/api-token (API_TOKEN unset) passed Validate and PrepareTokens: "+
			"the shared ingest token that gateways hold is the API token (GET /api/v1/billing-states with it: %d)", resp.StatusCode)
	}
}

// The operator runs a LiteLLM for globex alone and gives it the token
// INGEST_TOKENS binds to globex ("A LiteLLM that serves one tenant can use
// a tenant-bound token instead"); globex also holds a second token bound to
// itself, for its own gateway. Every token bound to a tenant counts against
// one SKU cap (source "bound"), so globex fills it with 100 throwaway SKUs
// and the model the operator's LiteLLM reports next is stored as
// sku-limit-exceeded, which nothing prices. security.html: "a tenant's own
// token never uses up what the operator's gateways report for it".
func TestReview4TenantTokenFillsTheSKUCapOfTheOperatorsBoundGateway(t *testing.T) {
	e := setupRecords(t)
	e.server.cfg.IngestTokens["tok-globex-own"] = "globex" // gw-globex is the operator's LiteLLM that serves globex
	at := now.Add(-2 * time.Minute).Unix()
	junk := make([]string, maxSKUsPerTenantPerDay)
	for i := range junk {
		junk[i] = fmt.Sprintf(`{"id":"junk-%d","time":%d,"usage":{"inference_output_tokens":1},"sku":"junk-%d"}`, i, at, i)
	}
	if code, out := postJSON(t, e.srv.URL+"/api/v1/records", "tok-globex-own", "["+strings.Join(junk, ",")+"]", "X-VBilling-Client", "globex-gw"); code != http.StatusOK {
		t.Fatalf("globex's own token: %d %v", code, out)
	}
	litellm := fmt.Sprintf(`[{"id":"chatcmpl-1","litellm_call_id":"call-1","status":"success","model_group":"llama-4-maverick","prompt_tokens":512,`+
		`"completion_tokens":64,"endTime":%d,"metadata":{"user_api_key_team_id":"globex"}}]`, at)
	if code, out := postJSON(t, e.srv.URL+"/api/v1/records?format=litellm", "gw-globex", litellm); code != http.StatusOK {
		t.Fatalf("the operator's LiteLLM: %d %v", code, out)
	}
	stored := map[string]bool{}
	err := e.records.ScanRecords(now.Add(-time.Hour), now.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		if ev.Source == "records:litellm" {
			stored[ev.SKU] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stored["llama-4-maverick"] || stored[OverflowSKU] {
		t.Errorf("the operator's LiteLLM reported llama-4-maverick for globex with its globex-bound token; stored under SKUs %v: "+
			"globex's own bound token used up the cap the operator's gateway counts against", stored)
	}
}

// 1710360 checks the SKU, region, project, resource ID and dimensions of
// an ingested event, but not the other strings it stores: the unit, the
// properties (any JSON) and, for a tenant-bound token, the event's own id,
// kept in properties.client_event_id. The unit reaches Metronome as the
// unit property, and properties reach Lago as event properties (a "sku"
// property there for an event without a SKU, which the SKU cap and the
// price check never see).
func TestReview4EveryValueAnEventStoresIsChecked(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	ws := now.Add(-5 * time.Minute)
	ev := map[string]any{"id": "x\x07<b>" + strings.Repeat("i", 4096), "metric": usage.MetricGPUHours, "quantity": 0.5,
		"unit":         "<img src=x onerror=alert(1)>\x07" + strings.Repeat("u", 4096),
		"properties":   map[string]any{"sku": "free-tier", "note": "=HYPERLINK(\"https://example.invalid\")<script>"},
		"window_start": ws, "window_end": ws.Add(time.Minute)}
	body, _ := json.Marshal([]map[string]any{ev})
	code := postWithClient(t, e.srv.URL+"/api/v1/events", "tok-acme", "", body)
	var stored *usage.Event
	err := e.sp.ScanAll(now.Add(-time.Hour), now, func(ev *usage.Event) error {
		if ev.Source == "ingest:api" {
			c := *ev
			stored = &c
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code == http.StatusOK && stored != nil && (usage.CheckValue(stored.Unit) != nil || stored.Properties["sku"] != nil) {
		id, _ := stored.Properties["client_event_id"].(string)
		t.Errorf("a tenant-bound token stored unit %.40q... (%d bytes: markup and a control character), properties %v and a %d-byte client_event_id "+
			"with markup and a control character: none is checked like SKUs, regions and dimensions", stored.Unit, len(stored.Unit), stored.Properties["note"], len(id))
	}
}
