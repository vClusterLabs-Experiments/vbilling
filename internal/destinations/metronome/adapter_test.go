package metronome

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// fakeMetronome validates every request body against Metronome's OpenAPI
// request schema and models dedupe, alias conflicts and uniqueness keys.
type fakeMetronome struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	customers map[string]*Customer // id -> customer
	metrics   []BillableMetric
	events    map[string]IngestEvent // transaction_id -> first accepted
	ingests   int
	contracts map[string]CreateContractRequest // uniqueness key -> request
	ended     map[string]string                // uniqueness key -> ending_before
	archived  map[string]bool                  // uniqueness key -> archived
	billing   map[string][]BillingProviderConfig
	billSets  int // setCustomerBillingProviderConfigurations calls
	failNext  map[string]int
	hideOnce  bool // next alias lookup misses (eventual consistency), forcing a 409 on create
	conflicts int
}

func newFakeMetronome(t *testing.T) *fakeMetronome {
	f := &fakeMetronome{t: t, customers: map[string]*Customer{}, events: map[string]IngestEvent{},
		contracts: map[string]CreateContractRequest{}, ended: map[string]string{}, archived: map[string]bool{},
		billing: map[string][]BillingProviderConfig{}, failNext: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func uuidFor(s string) string {
	h := sha256.Sum256([]byte(s))
	x := fmt.Sprintf("%x", h[:16])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

func (f *fakeMetronome) reply(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeMetronome) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mtk_test" {
		f.reply(w, 401, map[string]string{"message": "Unauthorized"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	if n := f.failNext[key]; n > 0 {
		f.failNext[key] = n - 1
		f.reply(w, 503, map[string]string{"message": "unavailable"})
		return
	}
	switch key {
	case "POST /v1/ingest":
		assertMatchesSchema(f.t, "ingest", body)
		var evs []IngestEvent
		json.Unmarshal(body, &evs)
		f.ingests++
		for _, e := range evs {
			for k, v := range e.Properties {
				if v == "" {
					f.t.Errorf("empty property %s on %s", k, e.TransactionID)
				}
			}
			if _, dup := f.events[e.TransactionID]; !dup { // first accepted wins
				f.events[e.TransactionID] = e
			}
		}
		w.WriteHeader(200)

	case "GET /v1/customers":
		alias := r.URL.Query().Get("ingest_alias")
		var out []Customer
		if f.hideOnce {
			f.hideOnce = false
			f.reply(w, 200, map[string]any{"data": out, "next_page": nil})
			return
		}
		for _, c := range f.customers {
			for _, a := range c.IngestAliases {
				if a == alias {
					out = append(out, *c)
				}
			}
		}
		f.reply(w, 200, map[string]any{"data": out, "next_page": nil})

	case "POST /v1/customers":
		assertMatchesSchema(f.t, "create_customer", body)
		var req CreateCustomerRequest
		json.Unmarshal(body, &req)
		for _, c := range f.customers {
			for _, a := range c.IngestAliases {
				if a == req.IngestAliases[0] {
					f.conflicts++
					f.reply(w, 409, map[string]string{"message": "ingest alias already in use"})
					return
				}
			}
		}
		c := &Customer{ID: uuidFor(req.IngestAliases[0]), Name: req.Name, IngestAliases: req.IngestAliases}
		f.customers[c.ID] = c
		f.billing[c.ID] = req.BillingProviderConfigs
		f.reply(w, 200, map[string]any{"data": c})

	case "GET /v1/billable-metrics":
		f.reply(w, 200, map[string]any{"data": f.metrics, "next_page": nil})

	case "POST /v1/billable-metrics/create":
		assertMatchesSchema(f.t, "create_billable_metric", body)
		var m BillableMetric
		json.Unmarshal(body, &m)
		m.ID = uuidFor(m.Name)
		f.metrics = append(f.metrics, m)
		f.reply(w, 200, map[string]any{"data": map[string]string{"id": m.ID}})

	case "POST /v1/contracts/create":
		assertMatchesSchema(f.t, "create_contract", body)
		var req CreateContractRequest
		json.Unmarshal(body, &req)
		if _, used := f.contracts[req.UniquenessKey]; used {
			f.reply(w, 409, map[string]string{"message": "This uniqueness key has already been used."})
			return
		}
		f.contracts[req.UniquenessKey] = req
		f.reply(w, 200, map[string]any{"data": map[string]string{"id": uuidFor(req.UniquenessKey)}})

	case "POST /v2/contracts/list":
		assertMatchesSchema(f.t, "list_contracts_v2", body)
		var req listContractsRequest
		json.Unmarshal(body, &req)
		var keys []string
		for k, c := range f.contracts {
			if c.CustomerID == req.CustomerID && (req.IncludeArchived || !f.archived[k]) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		from, _ := strconv.Atoi(req.Cursor)
		to := from + req.Limit
		var cursor any
		if to < len(keys) {
			cursor = strconv.Itoa(to)
		} else {
			to = len(keys)
		}
		var data []map[string]any
		for _, k := range keys[from:to] {
			c := map[string]any{"id": uuidFor(k), "uniqueness_key": k, "starting_at": f.contracts[k].StartingAt, "customer_id": req.CustomerID}
			if e := f.ended[k]; e != "" {
				c["ending_before"] = e
			}
			if f.archived[k] {
				c["archived_at"] = now.Format(time.RFC3339)
			}
			data = append(data, c)
		}
		f.reply(w, 200, map[string]any{"data": data, "cursor": cursor})

	case "POST /v1/contracts/updateEndDate", "POST /v1/contracts/archive":
		var req struct {
			CustomerID   string `json:"customer_id"`
			ContractID   string `json:"contract_id"`
			EndingBefore string `json:"ending_before"`
		}
		json.Unmarshal(body, &req)
		key := ""
		for k, c := range f.contracts {
			if uuidFor(k) == req.ContractID && c.CustomerID == req.CustomerID {
				key = k
			}
		}
		if key == "" {
			f.reply(w, 404, map[string]string{"message": "contract not found"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/archive") {
			assertMatchesSchema(f.t, "archive_contract", body)
			f.archived[key] = true
		} else {
			assertMatchesSchema(f.t, "update_contract_end_date", body)
			if req.EndingBefore <= f.contracts[key].StartingAt {
				f.reply(w, 400, map[string]string{"message": "ending_before must be after starting_at"})
				return
			}
			f.ended[key] = req.EndingBefore
		}
		f.reply(w, 200, map[string]any{"data": map[string]string{"id": req.ContractID}})

	case "POST /v1/getCustomerBillingProviderConfigurations":
		assertMatchesSchema(f.t, "get_customer_billing_provider_configurations", body)
		var req struct {
			CustomerID string `json:"customer_id"`
		}
		json.Unmarshal(body, &req)
		var data []map[string]any
		for i, c := range f.billing[req.CustomerID] {
			data = append(data, map[string]any{"id": uuidFor(fmt.Sprint(req.CustomerID, i)), "billing_provider": c.BillingProvider,
				"customer_id": req.CustomerID, "configuration": c.Configuration, "delivery_method": c.DeliveryMethod})
		}
		f.reply(w, 200, map[string]any{"data": data})

	case "POST /v1/setCustomerBillingProviderConfigurations":
		assertMatchesSchema(f.t, "set_customer_billing_provider_configurations", body)
		var req struct {
			Data []struct {
				CustomerID string `json:"customer_id"`
				BillingProviderConfig
			} `json:"data"`
		}
		json.Unmarshal(body, &req)
		f.billSets++
		for _, d := range req.Data {
			f.billing[d.CustomerID] = append(f.billing[d.CustomerID], d.BillingProviderConfig)
		}
		f.reply(w, 200, map[string]any{"data": []any{}})

	case "POST /v1/usage":
		assertMatchesSchema(f.t, "usage", body)
		var req UsageRequest
		json.Unmarshal(body, &req)
		start, _ := time.Parse(time.RFC3339, req.StartingOn)
		end, _ := time.Parse(time.RFC3339, req.EndingBefore)
		var rows []map[string]any
		for _, cid := range req.CustomerIDs {
			for _, bmRef := range req.BillableMetrics {
				bmID := bmRef["id"].(string)
				var metric BillableMetric
				for _, m := range f.metrics {
					if m.ID == bmID {
						metric = m
					}
				}
				var sum float64
				for _, e := range f.events {
					ts, _ := time.Parse(time.RFC3339, e.Timestamp)
					owner := e.CustomerID
					if c, ok := f.customers[cid]; ok && len(c.IngestAliases) > 0 && owner == c.IngestAliases[0] {
						owner = cid
					}
					if owner == cid && e.EventType == metric.EventTypeFilter.InValues[0] && !ts.Before(start) && ts.Before(end) {
						v, _ := strconv.ParseFloat(e.Properties[metric.AggregationKey], 64)
						sum += v
					}
				}
				rows = append(rows, map[string]any{"customer_id": cid, "billable_metric_id": bmID, "value": sum})
			}
		}
		f.reply(w, 200, map[string]any{"data": rows, "next_page": nil})

	default:
		if strings.HasPrefix(key, "GET /v1/customers/") {
			c, ok := f.customers[strings.TrimPrefix(r.URL.Path, "/v1/customers/")]
			if !ok {
				f.reply(w, 404, map[string]string{"message": "not found"})
				return
			}
			f.reply(w, 200, map[string]any{"data": c})
			return
		}
		f.reply(w, 404, map[string]string{"message": "unhandled " + key})
	}
}

func newAdapter(t *testing.T, f *fakeMetronome, link *stripe.Adapter, mutate ...func(*config.Config)) *Adapter {
	cfg := &config.Config{MetronomeStripeCollect: "charge_automatically"}
	for _, m := range mutate {
		m(cfg)
	}
	a := New(NewClient(f.srv.URL, "mtk_test"), cfg, kv.Memory(), link)
	a.now = func() time.Time { return now }
	return a
}

func gpu(tenant, cluster string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: q, SKU: "NVIDIA-H100-80GB-HBM3",
		Region: "ap-southeast-2", WindowStart: ws, WindowEnd: ws.Add(time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: cluster, usage.DimCapacityType: usage.CapacityOnDemand}}
	e.Finalize()
	return e
}

func TestBootstrapCreatesGroupedSumMetrics(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if len(f.metrics) != len(usage.Billable()) {
		t.Fatalf("created %d billable metrics", len(f.metrics))
	}
	var gpuMetric BillableMetric
	for _, m := range f.metrics {
		if m.EventTypeFilter.InValues[0] == usage.MetricGPUHours {
			gpuMetric = m
		}
	}
	if gpuMetric.AggregationType != "SUM" || gpuMetric.AggregationKey != "value" {
		t.Fatalf("gpu metric: %+v", gpuMetric)
	}
	if got := strings.Join(gpuMetric.GroupKeys[0], ","); got != "region,sku,capacity_type,billing_mode,tenant_cluster" {
		t.Fatalf("compound group key = %s", got)
	}
	filtered := map[string]bool{}
	for _, pf := range gpuMetric.PropertyFilters {
		filtered[pf.Name] = pf.Exists != nil && *pf.Exists
	}
	for _, k := range append([]string{"value"}, gpuMetric.GroupKeys[0]...) {
		if !filtered[k] {
			t.Errorf("group key %s lacks an exists property filter (Metronome requires it)", k)
		}
	}

	// Restart with lost state: metrics are matched by name, not recreated.
	b := newAdapter(t, f, nil)
	if err := b.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if len(f.metrics) != len(usage.Billable()) {
		t.Fatalf("second bootstrap duplicated metrics: %d", len(f.metrics))
	}
}

func TestEnsureTenantCustomerAndContract(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg-aud" })
	ctx := context.Background()
	tenant := usage.Tenant{ID: "acme", DisplayName: "Acme AI"}
	for i := 0; i < 2; i++ {
		if err := a.EnsureTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.customers) != 1 || len(f.contracts) != 1 {
		t.Fatalf("customers=%d contracts=%d", len(f.customers), len(f.contracts))
	}
	for _, c := range f.contracts {
		if c.RateCardAlias != "payg-aud" || c.StartingAt != "2026-10-05T00:00:00Z" || c.UsageStatementSchedule.Day != "FIRST_OF_MONTH" {
			t.Fatalf("contract: %+v", c)
		}
	}
	// Lost state: alias lookup finds the customer; the uniqueness key stops a
	// second contract (409 is treated as "already exists").
	b := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg-aud" })
	if err := b.EnsureTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if len(f.customers) != 1 || len(f.contracts) != 1 {
		t.Fatalf("after state loss: customers=%d contracts=%d", len(f.customers), len(f.contracts))
	}
	// A per-tenant plan that looks like a UUID is passed as rate_card_id.
	rc := "8f0e3c1a-6d3b-4c8e-9a51-1f0d7a2b9c44"
	if err := b.EnsureTenant(ctx, usage.Tenant{ID: "beta", Plan: rc}); err != nil {
		t.Fatal(err)
	}
	if got := f.contracts["vbilling:"+rc+"@2026-10-05:"+shortHash("beta")]; got.RateCardID != rc || got.RateCardAlias != "" {
		t.Fatalf("uuid rate card: %+v", got)
	}
}

func TestEnsureTenantConflictResolvesExistingAlias(t *testing.T) {
	f := newFakeMetronome(t)
	pre := &Customer{ID: uuidFor("acme"), Name: "pre-existing", IngestAliases: []string{"acme"}}
	f.customers[pre.ID] = pre
	a := newAdapter(t, f, nil)
	f.hideOnce = true // the first lookup misses, so creation hits the 409 path
	if err := a.EnsureTenant(context.Background(), usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if f.conflicts != 1 {
		t.Fatalf("expected one 409, got %d", f.conflicts)
	}
	if got, _ := a.state.Get("cus/acme"); got != pre.ID {
		t.Fatalf("resolved %q, want %q", got, pre.ID)
	}
}

// stripeStub implements the customer endpoints the Stripe link uses.
func stripeStub(t *testing.T) (*httptest.Server, *int) {
	created := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch {
		case r.URL.Path == "/v1/customers/search":
			json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "has_more": false})
		case r.Method == "POST" && r.URL.Path == "/v1/customers":
			created++
			if r.Form.Get("metadata[vbilling_tenant_id]") == "" {
				t.Error("stripe customer missing tenant metadata")
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "cus_linked"})
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/customers/"):
			json.NewEncoder(w).Encode(map[string]any{"id": "cus_linked"})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &created
}

func TestStripeLinkedCustomerForInvoicing(t *testing.T) {
	f := newFakeMetronome(t)
	ss, created := stripeStub(t)
	cfg := &config.Config{}
	link := stripe.New(stripe.NewClient(ss.URL, "sk_test_x", "2026-09-30.endive", 0), cfg, kv.Memory())
	a := newAdapter(t, f, link, func(c *config.Config) { c.MetronomeRateCard = "payg-aud" })
	if err := a.EnsureTenant(context.Background(), usage.Tenant{ID: "acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	if *created != 1 {
		t.Fatalf("stripe customers created: %d", *created)
	}
	cfgs := f.billing[uuidFor("acme")]
	if len(cfgs) != 1 || cfgs[0].BillingProvider != "stripe" || cfgs[0].DeliveryMethod != "direct_to_billing_provider" ||
		cfgs[0].Configuration["stripe_customer_id"] != "cus_linked" || cfgs[0].Configuration["stripe_collection_method"] != "charge_automatically" {
		t.Fatalf("billing provider config: %+v", cfgs)
	}
	for _, c := range f.contracts {
		if c.BillingProviderConfig == nil || c.BillingProviderConfig.BillingProvider != "stripe" {
			t.Fatalf("contract not routed to stripe: %+v", c)
		}
	}
}

func TestSendEventsWireFormatBatchingAndDedupe(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	var events []usage.Event
	for i := 0; i < 250; i++ {
		events = append(events, gpu("acme", "vcluster-team-a-train", now.Add(-time.Duration(i+1)*time.Minute), 8.0/60))
	}
	cpu := usage.Event{Tenant: "beta", Metric: usage.MetricCPUCoreHours, Quantity: 0.25, WindowStart: now.Add(-time.Minute), WindowEnd: now}
	cpu.Finalize()
	events = append(events, cpu)

	f.failNext["POST /v1/ingest"] = 1
	if err := a.SendEvents(ctx, events); err == nil || destinations.IsPermanent(err) {
		t.Fatalf("503 must be a retryable error, got %v", err)
	}
	if err := a.SendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := a.SendEvents(ctx, events); err != nil { // full replay: deduped by transaction_id
		t.Fatal(err)
	}
	if len(f.events) != 251 {
		t.Fatalf("stored %d events, want 251", len(f.events))
	}
	if f.ingests != 6 { // 3 batches of <=100, twice
		t.Fatalf("ingest calls = %d", f.ingests)
	}
	e := f.events[events[0].ID]
	if e.CustomerID != uuidFor("acme") || e.EventType != usage.MetricGPUHours || e.Timestamp != rfc3339(events[0].WindowStart) {
		t.Fatalf("wire event: %+v", e)
	}
	if e.Properties["value"] != "0.133333333" || e.Properties["sku"] != "NVIDIA-H100-80GB-HBM3" || e.Properties["billing_mode"] != placeholder {
		t.Fatalf("properties: %+v", e.Properties)
	}
	// Unknown customers are sent under their ingest alias so Metronome can
	// attribute them once the customer exists.
	if got := f.events[cpu.ID].CustomerID; got != "beta" {
		t.Fatalf("alias customer_id = %q", got)
	}
}

func TestOldEventsRejectedWithReason(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil)
	old := gpu("acme", "c", now.Add(-40*24*time.Hour), 1)
	err := a.SendEvents(context.Background(), []usage.Event{old, gpu("acme", "c", now.Add(-time.Hour), 1)})
	var partial *destinations.PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Rejected[old.ID], "34-day") {
		t.Fatalf("expected age rejection, got %v", err)
	}
}

func TestRecordedTotalsWholeDays(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	day := now.Truncate(24 * time.Hour)
	var events []usage.Event
	for i := 0; i < 60; i++ {
		events = append(events, gpu("acme", "c1", day.Add(time.Duration(i)*time.Minute), 8.0/60))
	}
	if err := a.SendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	q := destinations.TotalsQuery{From: day, To: day.Add(24 * time.Hour), Tenants: []usage.Tenant{{ID: "acme"}}, Metrics: []string{usage.MetricGPUHours}}
	totals, err := a.RecordedTotals(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].Quantity != usage.Round(60*usage.Round(8.0/60)) {
		t.Fatalf("totals: %+v", totals)
	}
	q.From = day.Add(time.Hour)
	if _, err := a.RecordedTotals(ctx, q); err == nil {
		t.Fatal("non-midnight bounds must be rejected")
	}
}

func TestVerifySignatureOfficialVector(t *testing.T) {
	// From docs.metronome.com/guides/platform-configuration/setup-webhooks.
	body := []byte("{\n  \"id\": \"b2c9e307-624e-4e7d-a5a4-1b74107d78c4\",\n  \"type\": \"widget_created\",\n  \"properties\": {\n    \"customer_id\": \"5f794d50-085a-4db6-8d15-286e518b7225\",\n    \"widget_id\": \"0891458d-b6f0-4fdd-a41e-380aae1a1e38\"\n  }\n}")
	date := "Mon, 02 Jan 2006 22:04:05 GMT"
	sig := "b82652fa2246cf1d8a27e591f155c865f68b46c19b9213fd9c052f2419b4742b"
	if err := VerifySignature(body, date, sig, "correct-horse-battery-staple", 0, now); err != nil {
		t.Fatalf("official test vector rejected: %v", err)
	}
	if err := VerifySignature(body, date, sig, "correct-horse-battery-staple", DefaultTolerance, now); err == nil {
		t.Fatal("2006 timestamp should fail the freshness check")
	}
	if err := VerifySignature(append(body, ' '), date, sig, "correct-horse-battery-staple", 0, now); err == nil {
		t.Fatal("modified body accepted")
	}
	fresh := now.Format(http.TimeFormat)
	if err := VerifySignature(body, fresh, Sign(body, fresh, "s"), "s", DefaultTolerance, now.Add(time.Minute)); err != nil {
		t.Fatalf("fresh signature rejected: %v", err)
	}
}

// contract finds a contract's uniqueness key by rate card and start day.
func (f *fakeMetronome) contract(t *testing.T, rateCard, day string) string {
	t.Helper()
	for k, c := range f.contracts {
		if c.RateCardAlias == rateCard && c.StartingAt == day+"T00:00:00Z" {
			return k
		}
	}
	t.Fatalf("no contract on %s from %s in %v", rateCard, day, f.contracts)
	return ""
}

func TestRateCardChangeHandsOverContract(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	ctx := context.Background()
	at := func(day int) { a.now = func() time.Time { return now.AddDate(0, 0, day) } }
	ensure := func(plan string) {
		t.Helper()
		if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme", Plan: plan}); err != nil {
			t.Fatal(err)
		}
	}

	ensure("") // payg from 2026-10-05
	payg := f.contract(t, "payg", "2026-10-05")

	at(1) // moved to a committed rate card the next day
	ensure("committed")
	committed := f.contract(t, "committed", "2026-10-06")
	if f.ended[payg] != "2026-10-06T00:00:00Z" || f.ended[committed] != "" {
		t.Fatalf("handover: payg ends %q, committed ends %q", f.ended[payg], f.ended[committed])
	}
	ensure("committed") // steady state: no API calls, nothing changes
	if len(f.contracts) != 2 {
		t.Fatalf("contracts: %d", len(f.contracts))
	}

	at(3) // back to payg: its old key is used up, so a new contract starts
	ensure("payg")
	payg2 := f.contract(t, "payg", "2026-10-08")
	if f.ended[committed] != "2026-10-08T00:00:00Z" || f.ended[payg] != "2026-10-06T00:00:00Z" || f.ended[payg2] != "" {
		t.Fatalf("second handover: %v", f.ended)
	}

	ensure("committed") // and straight back the same day: payg2 never ran, so it is archived
	if !f.archived[payg2] {
		t.Fatal("same-day contract not archived")
	}
	committed2 := f.contract(t, "committed", "2026-10-08")
	if f.archived[committed2] || f.ended[committed2] != "" {
		t.Fatal("new committed contract not open")
	}
	ensure("payg") // a third switch that day needs a fresh key: "#2"
	var fresh int
	for k, c := range f.contracts {
		if c.RateCardAlias == "payg" && !f.archived[k] && f.ended[k] == "" {
			fresh++
			if !strings.Contains(k, "@2026-10-08#2:") {
				t.Fatalf("expected a #2 key, got %s", k)
			}
		}
	}
	if fresh != 1 || !f.archived[committed2] {
		t.Fatalf("open payg contracts %d, committed2 archived %v", fresh, f.archived[committed2])
	}

	// A second replica (or lost state) adopts the open contract instead of creating one.
	b := newAdapter(t, f, nil)
	b.now = a.now
	n := len(f.contracts)
	if err := b.EnsureTenant(ctx, usage.Tenant{ID: "acme", Plan: "payg"}); err != nil {
		t.Fatal(err)
	}
	if len(f.contracts) != n {
		t.Fatal("second replica created a contract")
	}
}

func TestUpgradeAdoptsLegacyContractAndEndsOverlap(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	cus := uuidFor("acme")
	f.customers[cus] = &Customer{ID: cus, Name: "acme", IngestAliases: []string{"acme"}}
	// v0.2.0 left two open contracts after a plan change, next to one vBilling
	// does not own and enough unrelated ones to need several pages.
	f.contracts["vbilling-acme-payg"] = CreateContractRequest{CustomerID: cus, StartingAt: "2026-09-20T00:00:00Z", RateCardAlias: "payg", UniquenessKey: "vbilling-acme-payg"}
	f.contracts["vbilling-acme-committed"] = CreateContractRequest{CustomerID: cus, StartingAt: "2026-09-28T00:00:00Z", RateCardAlias: "committed", UniquenessKey: "vbilling-acme-committed"}
	f.contracts["sales-deal-7"] = CreateContractRequest{CustomerID: cus, StartingAt: "2026-09-01T00:00:00Z", RateCardAlias: "enterprise", UniquenessKey: "sales-deal-7"}
	for i := 0; i < 25; i++ {
		k := fmt.Sprintf("old-%02d", i)
		f.contracts[k] = CreateContractRequest{CustomerID: cus, StartingAt: "2026-01-01T00:00:00Z", UniquenessKey: k}
		f.ended[k] = "2026-02-01T00:00:00Z"
	}
	n := len(f.contracts)

	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme", Plan: "committed"}); err != nil {
		t.Fatal(err)
	}
	if len(f.contracts) != n {
		t.Fatal("a legacy contract on the current rate card should be adopted, not replaced")
	}
	if f.ended["vbilling-acme-payg"] != "2026-10-05T00:00:00Z" {
		t.Fatalf("overlapping legacy contract ends %q", f.ended["vbilling-acme-payg"])
	}
	if f.ended["vbilling-acme-committed"] != "" || f.ended["sales-deal-7"] != "" || f.archived["sales-deal-7"] {
		t.Fatal("touched the adopted contract or one vBilling does not own")
	}
}

func TestStripeLinkAddedToExistingCustomer(t *testing.T) {
	f := newFakeMetronome(t)
	ss, created := stripeStub(t)
	link := stripe.New(stripe.NewClient(ss.URL, "sk_test_x", "2026-09-30.endive", 0), &config.Config{}, kv.Memory())
	ctx := context.Background()
	// Created before the Stripe link was turned on.
	pre := &Customer{ID: uuidFor("acme"), Name: "acme", IngestAliases: []string{"acme"}}
	f.customers[pre.ID] = pre
	a := newAdapter(t, f, link)
	for i := 0; i < 2; i++ {
		if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
			t.Fatal(err)
		}
	}
	cfgs := f.billing[pre.ID]
	if f.billSets != 1 || len(cfgs) != 1 || cfgs[0].BillingProvider != "stripe" || cfgs[0].Configuration["stripe_customer_id"] != "cus_linked" {
		t.Fatalf("link: sets=%d configs=%+v", f.billSets, cfgs)
	}
	if *created != 1 {
		t.Fatalf("stripe customers created: %d", *created)
	}
	// A customer that already has a Stripe configuration is left as it is.
	other := &Customer{ID: uuidFor("globex"), Name: "globex", IngestAliases: []string{"globex"}}
	f.customers[other.ID] = other
	f.billing[other.ID] = []BillingProviderConfig{{BillingProvider: "stripe", DeliveryMethod: "direct_to_billing_provider",
		Configuration: map[string]string{"stripe_customer_id": "cus_theirs"}}}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "globex"}); err != nil {
		t.Fatal(err)
	}
	if f.billSets != 1 || len(f.billing[other.ID]) != 1 {
		t.Fatal("re-linked a customer that already had Stripe")
	}
}
