package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// recDest is a destination that records totals for reconciliation.
type recDest struct {
	mu       sync.Mutex
	recorded map[string]float64 // tenant|metric -> quantity
	reject   map[string]bool
	sent     int
}

func (d *recDest) Name() string                                       { return "fakebill" }
func (d *recDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (d *recDest) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (d *recDest) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (d *recDest) Accepts(m string) bool                              { return destinations.BillableOnly(m) }
func (d *recDest) SendEvents(_ context.Context, evs []usage.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rej := map[string]string{}
	for _, e := range evs {
		if d.reject[e.ID] {
			rej[e.ID] = "unknown customer"
			continue
		}
		d.recorded[e.Tenant+"|"+e.Metric] += e.Quantity
		d.sent++
	}
	if len(rej) > 0 {
		return &destinations.PartialError{Rejected: rej}
	}
	return nil
}
func (d *recDest) RecordedTotals(_ context.Context, q destinations.TotalsQuery) ([]destinations.Total, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []destinations.Total
	for _, t := range q.Tenants {
		for _, m := range q.Metrics {
			if v, ok := d.recorded[t.ID+"|"+m]; ok {
				out = append(out, destinations.Total{Tenant: t.ID, Metric: m, Quantity: v})
			}
		}
	}
	return out, nil
}

type env struct {
	srv  *httptest.Server
	sp   *spool.Spool
	dest *recDest
	disp *pipeline.Dispatcher
	enf  *enforcement.Enforcer
}

func setup(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	cfg := &config.Config{Region: "ap-southeast-2", ClusterName: "syd-1", Adapters: []string{"fakebill"}, CollectionInterval: time.Minute,
		IngestToken: "ingest-secret", EnforcementMode: "observe", StripeWebhookSecret: "whsec_api"}
	for _, m := range mutate {
		m(cfg)
	}
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	dest := &recDest{recorded: map[string]float64{}, reject: map[string]bool{}}
	reg := pipeline.NewTenantRegistry()
	reg.Upsert(usage.Tenant{ID: "acme", DisplayName: "Acme"})
	disp := pipeline.New(sp, []destinations.Destination{dest}, pipeline.Options{IdlePoll: 5 * time.Millisecond, MinBackoff: time.Millisecond}, nil)
	tel := telemetry.New()
	enf := enforcement.New(nil, enforcement.Options{Mode: "observe", StripeWebhookSecret: cfg.StripeWebhookSecret, Now: func() time.Time { return now },
		Telemetry: tel, Resolvers: map[string]destinations.CustomerResolver{"stripe": resolver{"cus_acme": "acme"}}})
	s := New(cfg, sp, disp, reg, nil, enf, tel, "test")
	s.now = func() time.Time { return now }
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{srv: srv, sp: sp, dest: dest, disp: disp, enf: enf}
}

type resolver map[string]string

func (r resolver) TenantForCustomer(_ context.Context, id string) (string, bool) {
	v, ok := r[id]
	return v, ok
}

func (e *env) commit(t *testing.T, windows int, start time.Time) {
	t.Helper()
	for i := 0; i < windows; i++ {
		ws := start.Add(time.Duration(i) * time.Minute)
		var evs []usage.Event
		for _, sku := range []string{"NVIDIA-H100", "NVIDIA-L40S"} {
			ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, SKU: sku, Region: "ap-southeast-2",
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: "vcluster-a-train"}, RecordedAt: now}
			ev.Finalize()
			evs = append(evs, ev)
		}
		util := usage.Event{Tenant: "acme", Metric: usage.MetricGPUUtilization, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
		util.Finalize()
		evs = append(evs, util)
		if err := e.sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) deliver(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go e.disp.Run(ctx)
	for e.sp.Cursor("fakebill") < e.sp.Committed() {
		if ctx.Err() != nil {
			t.Fatal("delivery timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestUsageLineItemsAndCSV(t *testing.T) {
	e := setup(t)
	e.sp.Cursor("fakebill")
	e.commit(t, 60, now.Add(-2*time.Hour))
	code, body := e.get(t, "/api/v1/usage?group_by=tenant,sku&metric="+usage.MetricGPUHours)
	if code != 200 {
		t.Fatalf("usage: %d %s", code, body)
	}
	var resp struct {
		Rows []map[string]any `json:"rows"`
	}
	json.Unmarshal([]byte(body), &resp)
	if len(resp.Rows) != 2 || resp.Rows[0]["sku"] != "NVIDIA-H100" || resp.Rows[0]["quantity"].(float64) != usage.Round(60*usage.Round(8.0/60)) {
		t.Fatalf("rows: %+v", resp.Rows)
	}
	code, body = e.get(t, "/api/v1/usage?format=csv&group_by=tenant,day")
	if code != 200 || !strings.HasPrefix(body, "tenant,day,metric,unit,quantity,events\n") || !strings.Contains(body, "acme,2026-10-05,vcluster_gpu_hours,gpu-hours,15.99999996,120") {
		t.Fatalf("csv: %d\n%s", code, body)
	}
	code, body = e.get(t, "/api/v1/events?format=csv&limit=3")
	if code != 200 || strings.Count(body, "\n") != 4 {
		t.Fatalf("event export: %d\n%s", code, body)
	}
}

func TestIngestValidatesDedupesAndAuthenticates(t *testing.T) {
	defer func() { recover() }()
	usage.RegisterCustom(usage.MetricDef{Code: "inference_output_tokens", Unit: "tokens", GroupKeys: []string{"model"}})
	e := setup(t)
	post := func(token, body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/events", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-VBilling-Client", "model-gateway")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	ws := now.Add(-5 * time.Minute).Format(time.RFC3339)
	we := now.Add(-4 * time.Minute).Format(time.RFC3339)
	good := fmt.Sprintf(`{"tenant":"acme","metric":"inference_output_tokens","quantity":125000,"window_start":%q,"window_end":%q,"dimensions":{"model":"llama-3-70b"}}`, ws, we)
	bad := fmt.Sprintf(`{"tenant":"acme","metric":"made_up","quantity":1,"window_start":%q,"window_end":%q}`, ws, we)
	future := fmt.Sprintf(`{"tenant":"acme","metric":"inference_output_tokens","quantity":1,"window_start":%q,"window_end":%q}`, we, now.Add(time.Hour).Format(time.RFC3339))

	if code, _ := post("wrong", `{"events":[]}`); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	code, out := post("ingest-secret", `{"events":[`+good+`,`+bad+`,`+future+`]}`)
	if code != 200 || out["accepted"].(float64) != 1 || len(out["rejected"].([]any)) != 2 {
		t.Fatalf("ingest: %d %+v", code, out)
	}
	code, out = post("ingest-secret", `[`+good+`]`) // resend without an ID: derived ID dedupes
	if code != 200 || out["accepted"].(float64) != 0 || out["duplicates"].(float64) != 1 {
		t.Fatalf("resend: %d %+v", code, out)
	}
	var stored *usage.Event
	e.sp.Scan(now.Add(-time.Hour), now, func(ev *usage.Event) error { stored = ev; return nil })
	if stored == nil || stored.Source != "ingest:model-gateway" || stored.Region != "ap-southeast-2" || stored.Unit != "tokens" {
		t.Fatalf("stored event: %+v", stored)
	}

	disabled := setup(t, func(c *config.Config) { c.IngestToken = "" })
	req, _ := http.NewRequest("POST", disabled.srv.URL+"/api/v1/events", strings.NewReader(good))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("ingest without a configured token must be disabled, got %d", resp.StatusCode)
	}
}

func TestReconcileMatchesAndExplainsDeadLetters(t *testing.T) {
	e := setup(t)
	e.sp.Cursor("fakebill")
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	e.commit(t, 30, day.Add(time.Hour))
	// Reject one event so it is dead-lettered; reconciliation must explain it.
	var victim usage.Event
	e.sp.Scan(day, day.Add(24*time.Hour), func(ev *usage.Event) error {
		if victim.ID == "" && ev.Metric == usage.MetricGPUHours {
			victim = *ev
		}
		return nil
	})
	e.dest.reject[victim.ID] = true
	e.deliver(t)

	code, body := e.get(t, "/api/v1/reconcile")
	if code != 200 {
		t.Fatalf("reconcile: %d %s", code, body)
	}
	var resp struct {
		Results []reconcileResult `json:"results"`
	}
	json.Unmarshal([]byte(body), &resp)
	if len(resp.Results) != 1 || !resp.Results[0].OK || len(resp.Results[0].Rows) != 1 {
		t.Fatalf("reconcile result: %s", body)
	}
	row := resp.Results[0].Rows[0]
	if row.DeadLettered == 0 || !row.Match || row.Metric != usage.MetricGPUHours {
		t.Fatalf("row: %+v", row)
	}

	// Drift in the backend shows up as a mismatch.
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] -= 1
	e.dest.mu.Unlock()
	_, body = e.get(t, "/api/v1/reconcile")
	json.Unmarshal([]byte(body), &resp)
	if resp.Results[0].OK || resp.Results[0].Rows[0].Match {
		t.Fatalf("drift not detected: %s", body)
	}

	// Fix the cause and replay the dead letter: it reconciles again.
	delete(e.dest.reject, victim.ID)
	e.dest.mu.Lock()
	e.dest.recorded["acme|"+usage.MetricGPUHours] += 1
	e.dest.mu.Unlock()
	resp2, _ := http.Post(e.srv.URL+"/api/v1/destinations/fakebill/dead-letters/replay", "application/json", nil)
	if resp2.StatusCode != 200 {
		t.Fatalf("replay status %d", resp2.StatusCode)
	}
	_, body = e.get(t, "/api/v1/reconcile")
	json.Unmarshal([]byte(body), &resp)
	if !resp.Results[0].OK || resp.Results[0].Rows[0].DeadLettered != 0 {
		t.Fatalf("after replay: %s", body)
	}
}

func TestVerifyDestinationsCursorAndWebhook(t *testing.T) {
	e := setup(t)
	e.commit(t, 3, now.Add(-time.Hour))
	if code, body := e.get(t, "/api/v1/ledger/verify"); code != 200 || !strings.Contains(body, `"ok": true`) {
		t.Fatalf("verify: %d %s", code, body)
	}
	resp, _ := http.Post(e.srv.URL+"/api/v1/destinations/fakebill/cursor", "application/json", strings.NewReader(`{"seq":0}`))
	if resp.StatusCode != 200 || e.sp.Cursor("fakebill") != 0 {
		t.Fatalf("cursor rewind: %d", resp.StatusCode)
	}

	body := []byte(`{"id":"evt_1","type":"invoice.payment_failed","data":{"object":{"customer":"cus_acme","attempt_count":1}}}`)
	req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", stripe.SignatureHeader(body, "whsec_api", now))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("webhook: %v %d", err, resp.StatusCode)
	}
	if e.enf.StateOf("acme").State != enforcement.Delinquent {
		t.Fatal("webhook did not update billing state")
	}
	req, _ = http.NewRequest("POST", e.srv.URL+"/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", "t=1,v1=bad")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	if code, _ := e.get(t, "/webhooks/metronome"); code != 405 && code != 404 {
		t.Fatalf("GET on webhook: %d", code)
	}
	code, mbody := e.get(t, "/metrics")
	if code != 200 || !strings.Contains(mbody, "vbilling_ledger_committed_seq") || !strings.Contains(mbody, `vbilling_tenant_billing_state{state="delinquent",tenant="acme"} 1`) {
		t.Fatalf("metrics: %s", mbody)
	}
}

func TestAPITokenProtectsReadEndpoints(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.APIToken = "read-secret" })
	if code, _ := e.get(t, "/api/v1/usage"); code != 401 {
		t.Fatalf("unauthenticated usage read: %d", code)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer read-secret")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("authenticated read: %d", resp.StatusCode)
	}
	if code, _ := e.get(t, "/healthz"); code != 200 {
		t.Fatal("healthz must stay open for probes")
	}
}
