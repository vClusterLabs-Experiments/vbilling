package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type recEnv struct {
	server  *Server
	srv     *httptest.Server
	records *spool.Spool
	health  *health.Registry
}

func setupRecords(t *testing.T) *recEnv {
	t.Helper()
	cfg := &config.Config{Region: "eu-west-1", CollectionInterval: time.Minute, APIToken: adminToken, IngestToken: "ingest-secret",
		IngestTokens: map[string]string{"gw-globex": "globex"}}
	dir := t.TempDir()
	clock := func() time.Time { return now }
	sp, err := spool.Open(filepath.Join(dir, "ledger"), spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := spool.Open(filepath.Join(dir, "records"), spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close(); rs.Close() })
	formats, _ := records.LoadFormats("", true)
	if err := formats.Declare(); err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	reg := pipeline.NewTenantRegistry()
	reg.Upsert(usage.Tenant{ID: "acme"})
	resolve := func(v string) (string, bool) {
		if v == "team-acme" {
			return "acme", true
		}
		_, ok := reg.Get(v)
		return v, ok
	}
	tel := telemetry.New()
	in := records.NewIngester(rs, formats, records.Options{Window: time.Minute, Region: cfg.Region, Now: clock}, resolve, h, tel)
	s := New(cfg, sp, nil, reg, nil, nil, tel, "test").WithHealth(h).WithRecords(in)
	s.now = clock
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &recEnv{server: s, srv: srv, records: rs, health: h}
}

func postJSON(t *testing.T, url, token, body string, headers ...string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestRecordsEndpoint(t *testing.T) {
	e := setupRecords(t)
	at := float64(now.Add(-2 * time.Minute).Unix())
	litellm := fmt.Sprintf(`[{"id":"c-1","status":"success","model_group":"llama-70b","prompt_tokens":512,"completion_tokens":64,"endTime":%v,
	  "metadata":{"user_api_key_team_id":"team-acme"}}]`, at)

	if code, _ := postJSON(t, e.srv.URL+"/api/v1/records?format=litellm", "", litellm); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	code, out := postJSON(t, e.srv.URL+"/api/v1/records?format=litellm", "ingest-secret", litellm)
	if code != 200 || out["accepted"].(float64) != 1 || out["usage_rows"].(float64) != 2 {
		t.Fatalf("litellm: %d %+v", code, out)
	}
	if code, out := postJSON(t, e.srv.URL+"/api/v1/records?format=nope", "ingest-secret", litellm); code != 400 || !strings.Contains(out["error"].(string), "unknown format") {
		t.Fatalf("unknown format: %d %+v", code, out)
	}
	// A bound token bills its own tenant whatever the payload says.
	native := fmt.Sprintf(`{"id":"n-1","tenant":"acme","time":%v,"usage":{"inference_output_tokens":9}}`, at)
	code, out = postJSON(t, e.srv.URL+"/api/v1/records", "gw-globex", native, "X-VBilling-Client", "edge-gw")
	if code != 200 || out["accepted"].(float64) != 1 {
		t.Fatalf("bound token: %d %+v", code, out)
	}
	tenants := map[string]string{}
	e.records.Scan(now.Add(-time.Hour), now.Add(time.Hour), func(ev *usage.Event) error {
		tenants[ev.Properties["request_id"].(string)] = ev.Tenant
		if ev.Region != "eu-west-1" {
			t.Errorf("region = %q", ev.Region)
		}
		return nil
	})
	if tenants["c-1"] != "acme" || tenants["n-1"] != "globex" {
		t.Errorf("tenants = %v", tenants)
	}
	// Everything rejected is a 400 with the reasons.
	code, out = postJSON(t, e.srv.URL+"/api/v1/records", "ingest-secret", `{"tenant":"acme","usage":{"inference_output_tokens":1}}`)
	if code != 400 || len(out["rejected"].([]any)) != 1 {
		t.Fatalf("all rejected: %d %+v", code, out)
	}
}

// Events without IDs that share an identity are rejected instead of being
// silently deduplicated into one.
func TestEventsWithoutIDsDoNotCollapse(t *testing.T) {
	usage.RegisterCustom(usage.MetricDef{Code: "collapse_tokens", Unit: "tokens", Billable: true})
	e := setup(t)
	ws := now.Add(-5 * time.Minute).Format(time.RFC3339)
	we := now.Add(-4 * time.Minute).Format(time.RFC3339)
	ev := func(q int) string {
		return fmt.Sprintf(`{"tenant":"acme","metric":"collapse_tokens","quantity":%d,"window_start":%q,"window_end":%q}`, q, ws, we)
	}
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", `[`+ev(10)+`,`+ev(20)+`]`)
	if code != 200 || out["accepted"].(float64) != 1 {
		t.Fatalf("batch: %d %+v", code, out)
	}
	rej := out["rejected"].([]any)
	if len(rej) != 1 || !strings.Contains(rej[0].(map[string]any)["error"].(string), "/api/v1/records") {
		t.Fatalf("the second event must be rejected with a pointer to the records API: %+v", rej)
	}
	// A later request with the same identity and no ID is reported, not hidden.
	code, out = postJSON(t, e.srv.URL+"/api/v1/events", "ingest-secret", "["+ev(30)+"]")
	if code != 200 || out["duplicates"].(float64) != 1 || out["warnings"] == nil {
		t.Fatalf("cross-batch duplicate: %d %+v", code, out)
	}
}

func TestStatusShowsSourceProblems(t *testing.T) {
	e := setupRecords(t)
	e.health.Empty("prometheus/tokens/vcluster-a-b", "prometheus", "vcluster-a-b", "no series", "use {{selector}}")
	e.health.Fail("tenant-api/vcluster-a-c", "tenant-api", "vcluster-a-c", errors.New("forbidden"), "")
	e.health.OK("records/litellm", "records", "litellm", 3)
	resp := do(t, http.MethodGet, e.srv.URL+"/api/v1/status", adminToken, nil)
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	warnings, _ := st["warnings"].([]any)
	if st["healthy"] != false || len(warnings) != 2 || !strings.Contains(fmt.Sprint(warnings), "use {{selector}}") {
		t.Fatalf("status = %+v", st)
	}
	resp = do(t, http.MethodGet, e.srv.URL+"/api/v1/sources?kind=prometheus", adminToken, nil)
	var src struct {
		Sources []health.Source `json:"sources"`
	}
	json.NewDecoder(resp.Body).Decode(&src)
	resp.Body.Close()
	if len(src.Sources) != 1 || src.Sources[0].Status != health.Empty {
		t.Fatalf("sources = %+v", src)
	}
}

type startingCtrl struct{ ready bool }

func (c *startingCtrl) Ready() bool                         { return c.ready }
func (c *startingCtrl) Clusters() []discovery.TenantCluster { return nil }
func (c *startingCtrl) LastWindow() time.Time               { return time.Time{} }

func TestRecordsWaitForTheFirstDiscovery(t *testing.T) {
	env := setupRecords(t)
	ctrl := &startingCtrl{}
	env.server.ctrl = ctrl
	rec := `{"id":"r1","tenant":"team-acme","time":"2026-10-05T11:59:10Z","usage":{"inference_input_tokens":5}}`
	// Unbound: the alias cannot be mapped yet, so the gateway must retry.
	if code, body := postJSON(t, env.srv.URL+"/api/v1/records", "ingest-secret", rec); code != http.StatusServiceUnavailable {
		t.Fatalf("before discovery: %d %v", code, body)
	}
	// A token bound to a tenant needs no mapping.
	if code, body := postJSON(t, env.srv.URL+"/api/v1/records", "gw-globex", rec); code != http.StatusOK {
		t.Fatalf("bound token before discovery: %d %v", code, body)
	}
	ctrl.ready = true
	if code, body := postJSON(t, env.srv.URL+"/api/v1/records", "ingest-secret", rec); code != http.StatusOK {
		t.Fatalf("after discovery: %d %v", code, body)
	}
	// Discovery that never succeeds does not block ingest for good.
	ctrl.ready = false
	env.server.started = time.Now().Add(-ingestStartupGrace - time.Second)
	if code, body := postJSON(t, env.srv.URL+"/api/v1/records", "ingest-secret", strings.Replace(rec, "r1", "r2", 1)); code != http.StatusOK {
		t.Fatalf("after the grace: %d %v", code, body)
	}
}
