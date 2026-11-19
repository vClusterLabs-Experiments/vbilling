package metronome

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// testdata/openapi-pricing-schemas.json holds the request and 200-response
// schemas (with $refs resolved) of the endpoints the price check reads,
// extracted from the OpenAPI blocks of these docs.metronome.com pages:
//   - api-reference/billable-metrics/list-all-billable-metrics
//   - api-reference/products/list-products
//   - api-reference/rate-cards/list-rate-cards
//   - api-reference/rate-cards/get-rates
//
// The fake below validates every request vBilling sends against them, and
// every response it serves, so the fixtures (and the structs decoding them)
// use the documented field names.

var pricingSchemas map[string]struct {
	Path     string         `json:"path"`
	Method   string         `json:"method"`
	Request  map[string]any `json:"request"`
	Response map[string]any `json:"response"`
	Query    map[string]any `json:"query"`
}

func loadPricingSchemas(t *testing.T) {
	t.Helper()
	if pricingSchemas != nil {
		return
	}
	b, err := os.ReadFile("testdata/openapi-pricing-schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pricingSchemas); err != nil {
		t.Fatal(err)
	}
}

func schemaCheck(t *testing.T, name string, schema map[string]any, v any) {
	t.Helper()
	// Round-trip through JSON so numbers and maps look as they do on the wire.
	b, _ := json.Marshal(v)
	var wire any
	json.Unmarshal(b, &wire)
	for _, e := range validate(schema, wire, name) {
		t.Errorf("schema violation: %s", e)
	}
}

// fakePricing serves Metronome's pricing read endpoints from fixtures.
type fakePricing struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	metrics  []map[string]any
	products []map[string]any
	cards    []map[string]any
	rates    map[string][]map[string]any // rate card ID -> rates
	calls    map[string]int
	pageSize int // products per page (0: all)
}

func newFakePricing(t *testing.T) *fakePricing {
	loadPricingSchemas(t)
	f := &fakePricing{t: t, rates: map[string][]map[string]any{}, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePricing) endpoint(r *http.Request) string {
	for name, s := range pricingSchemas {
		if s.Method == r.Method && s.Path == r.URL.Path {
			return name
		}
	}
	return ""
}

func (f *fakePricing) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mtk_test" {
		w.WriteHeader(401)
		return
	}
	name := f.endpoint(r)
	if name == "" {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
		return
	}
	f.calls[name]++
	s := pricingSchemas[name]
	for k := range r.URL.Query() {
		if _, ok := s.Query[k]; !ok {
			f.t.Errorf("%s: undocumented query parameter %q", name, k)
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err != nil || n < 1 || n > 100 {
			f.t.Errorf("%s: limit %q outside 1..100", name, l)
		}
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	if s.Request != nil {
		if err := json.Unmarshal(body, &req); err != nil {
			f.t.Errorf("%s: body is not a JSON object: %q", name, body)
		}
		schemaCheck(f.t, name+" request", s.Request, req)
	} else if len(body) > 0 {
		f.t.Errorf("%s: unexpected body %q", name, body)
	}

	var resp map[string]any
	switch name {
	case "list_billable_metrics":
		resp = map[string]any{"data": f.metrics, "next_page": nil}
	case "list_products":
		if req["archive_filter"] != "NOT_ARCHIVED" {
			f.t.Errorf("products listed with archive_filter %v", req["archive_filter"])
		}
		from, _ := strconv.Atoi(r.URL.Query().Get("next_page"))
		to := len(f.products)
		var next any
		if f.pageSize > 0 && from+f.pageSize < to {
			to = from + f.pageSize
			next = strconv.Itoa(to)
		}
		resp = map[string]any{"data": f.products[from:to], "next_page": next}
	case "list_rate_cards":
		resp = map[string]any{"data": f.cards, "next_page": nil}
	case "get_rates":
		id, _ := req["rate_card_id"].(string)
		at, _ := time.Parse(time.RFC3339, req["at"].(string))
		if at.IsZero() {
			f.t.Errorf("get_rates: at %v is not RFC 3339", req["at"])
		}
		want := map[string]bool{}
		sel, _ := req["selectors"].([]any)
		for _, s := range sel {
			want[s.(map[string]any)["product_id"].(string)] = true
		}
		data := []map[string]any{}
		for _, rt := range f.rates[id] {
			if len(want) == 0 || want[rt["product_id"].(string)] {
				data = append(data, rt)
			}
		}
		resp = map[string]any{"data": data, "next_page": nil}
	}
	schemaCheck(f.t, name+" response", s.Response, resp)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (f *fakePricing) addMetric(def usage.MetricDef) string {
	id := uuidFor("bm-" + def.Code)
	f.metrics = append(f.metrics, map[string]any{"id": id, "name": metricName(def), "aggregation_type": "SUM",
		"aggregation_key": "value", "group_keys": [][]string{GroupKeys(def)}})
	return id
}

func (f *fakePricing) addProduct(name, metricID string, pricingKey ...string) string {
	id := uuidFor("product-" + name)
	state := map[string]any{"name": name, "created_at": "2026-10-01T00:00:00Z", "created_by": "finance",
		"billable_metric_id": metricID, "starting_at": "2026-10-01T00:00:00Z"}
	if len(pricingKey) > 0 {
		state["pricing_group_key"] = pricingKey
		state["presentation_group_key"] = []string{usage.DimTenantCluster}
	}
	f.products = append(f.products, map[string]any{"id": id, "type": "USAGE", "archived_at": nil,
		"initial": state, "current": state, "updates": []any{}})
	return id
}

func (f *fakePricing) addCard(name string, aliases ...map[string]any) string {
	id := uuidFor("card-" + name)
	if aliases == nil {
		aliases = []map[string]any{}
	}
	f.cards = append(f.cards, map[string]any{"id": id, "name": name, "created_at": "2026-10-01T00:00:00Z",
		"created_by": "finance", "aliases": aliases})
	return id
}

// addRate adds a flat rate (price in the rate card's credit type, e.g. cents).
func (f *fakePricing) addRate(card, product, productName string, price float64, group map[string]string, mutate ...func(map[string]any)) {
	r := map[string]any{"product_id": product, "product_name": productName, "product_tags": []string{},
		"product_custom_fields": map[string]string{}, "starting_at": "2026-10-01T00:00:00Z", "entitled": true,
		"rate": map[string]any{"rate_type": "FLAT", "price": price}}
	if group != nil {
		r["pricing_group_values"] = group
	}
	for _, m := range mutate {
		m(r)
	}
	f.rates[card] = append(f.rates[card], r)
}

// newPricingAdapter checks prices in provisioning mode: the rate card is
// METRONOME_RATE_CARD or the tenant's plan, and billable metrics are the ones
// vBilling created (stored ID, else name).
func newPricingAdapter(t *testing.T, f *fakePricing, rateCard string) *Adapter {
	cfg := &config.Config{MetronomeRateCard: rateCard, MetronomeProvision: true}
	a := New(NewClient(f.srv.URL, "mtk_test"), cfg, kv.Memory(), nil)
	a.now = func() time.Time { return now }
	return a
}

func gpuDef(t *testing.T) usage.MetricDef {
	def, ok := usage.Lookup(usage.MetricGPUHours)
	if !ok {
		t.Fatal("no gpu metric")
	}
	return def
}

func check(t *testing.T, a *Adapter, series ...destinations.PriceSeries) map[string]destinations.PriceCoverage {
	t.Helper()
	out, err := a.CheckPrices(context.Background(), series)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(series) {
		t.Fatalf("got %d results for %d series", len(out), len(series))
	}
	m := map[string]destinations.PriceCoverage{}
	for _, c := range out {
		m[c.Series.Key()] = c
	}
	return m
}

func gpuSeries(a *Adapter, plan, sku string, dims map[string]string) destinations.PriceSeries {
	ev := usage.Event{Metric: usage.MetricGPUHours, SKU: sku, Region: dims["region"]}
	for k, v := range dims {
		if k != "region" {
			if ev.Dimensions == nil {
				ev.Dimensions = map[string]string{}
			}
			ev.Dimensions[k] = v
		}
	}
	return destinations.SeriesOf(a, plan, &ev)
}

func TestPriceCheckPerSKU(t *testing.T) {
	f := newFakePricing(t)
	bm := f.addMetric(gpuDef(t))
	prod := f.addProduct("GPU Hours", bm, "sku")
	card := f.addCard("AI Cloud list prices", map[string]any{"name": "ai-cloud"})
	f.addRate(card, prod, "GPU Hours", 249, map[string]string{"sku": "NVIDIA-H100"})
	f.addRate(card, prod, "GPU Hours", 0, map[string]string{"sku": "NVIDIA-T4"})
	a := newPricingAdapter(t, f, "ai-cloud")

	h100 := gpuSeries(a, "ai-cloud", "NVIDIA-H100", map[string]string{"region": "us-east"})
	b200 := gpuSeries(a, "ai-cloud", "NVIDIA-B200", map[string]string{"region": "us-east"})
	t4 := gpuSeries(a, "ai-cloud", "NVIDIA-T4", map[string]string{"region": "us-east"})
	got := check(t, a, h100, b200, t4)

	if c := got[h100.Key()]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, "flat 249") {
		t.Errorf("H100: %+v", c)
	}
	c := got[b200.Key()]
	if c.Status != destinations.PriceMissing {
		t.Fatalf("B200: %+v", c)
	}
	if !strings.Contains(c.Hint, `product "GPU Hours" (pricing group sku=NVIDIA-B200)`) || !strings.Contains(c.Hint, `"ai-cloud"`) {
		t.Errorf("B200 hint should name the product, SKU and rate card: %q", c.Hint)
	}
	if c := got[t4.Key()]; c.Status != destinations.PriceZero {
		t.Errorf("T4 priced at 0 should be zero, got %+v", c)
	}
	if f.calls["list_rate_cards"] != 1 || f.calls["get_rates"] != 1 {
		t.Errorf("rate card read more than once per check: %v", f.calls)
	}
}

func TestPriceCheckOtherPricingKeys(t *testing.T) {
	f := newFakePricing(t)
	bm := f.addMetric(gpuDef(t))
	prod := f.addProduct("GPU Hours", bm, "region", "sku", "capacity_type")
	card := f.addCard("rates")
	f.addRate(card, prod, "GPU Hours", 300, map[string]string{"sku": "NVIDIA-H100", "region": "us-east", "capacity_type": "on-demand"})
	f.addRate(card, prod, "GPU Hours", 120, map[string]string{"sku": "NVIDIA-H100", "region": "us-east", "capacity_type": "none"})
	a := newPricingAdapter(t, f, card) // a rate card ID needs no alias lookup

	east := gpuSeries(a, card, "NVIDIA-H100", map[string]string{"region": "us-east", "capacity_type": "on-demand"})
	west := gpuSeries(a, card, "NVIDIA-H100", map[string]string{"region": "eu-west", "capacity_type": "on-demand"})
	// No capacity type on the event: Metronome receives the placeholder "none".
	bare := gpuSeries(a, card, "NVIDIA-H100", map[string]string{"region": "us-east"})
	got := check(t, a, east, west, bare)
	if got[east.Key()].Status != destinations.PriceCovered {
		t.Errorf("us-east: %+v", got[east.Key()])
	}
	if c := got[west.Key()]; c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, "region=eu-west") {
		t.Errorf("eu-west should be unpriced with the region in the hint: %+v", c)
	}
	if got[bare.Key()].Status != destinations.PriceCovered {
		t.Errorf("missing capacity type should match the rate for %q: %+v", placeholder, got[bare.Key()])
	}
	if f.calls["list_rate_cards"] != 0 {
		t.Error("a rate card ID was looked up by alias")
	}
}

func TestPriceCheckProductWithoutPricingGroups(t *testing.T) {
	f := newFakePricing(t)
	bm := f.addMetric(gpuDef(t))
	prod := f.addProduct("GPU Hours", bm)
	card := f.addCard("rates", map[string]any{"name": "rc"})
	f.addRate(card, prod, "GPU Hours", 199, nil)
	a := newPricingAdapter(t, f, "rc")
	s := gpuSeries(a, "rc", "ANY-NEW-SKU", nil)
	if c := check(t, a, s)[s.Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("a product without pricing groups prices every SKU: %+v", c)
	}
}

func TestPriceCheckGaps(t *testing.T) {
	f := newFakePricing(t)
	gpu := gpuDef(t)
	cpu, _ := usage.Lookup(usage.MetricCPUCoreHours)
	storage, _ := usage.Lookup(usage.MetricStorageGBHours)
	gpuBM := f.addMetric(gpu)
	cpuBM := f.addMetric(cpu)
	// storage has no billable metric in Metronome at all
	gpuProd := f.addProduct("GPU Hours", gpuBM, "sku")
	card := f.addCard("rates", map[string]any{"name": "old", "ending_before": "2026-10-01T00:00:00Z"},
		map[string]any{"name": "current", "starting_at": "2026-10-01T00:00:00Z"})
	f.addRate(card, gpuProd, "GPU Hours", 249, map[string]string{"sku": "H100"}, func(r map[string]any) { r["entitled"] = false })
	f.addRate(card, gpuProd, "GPU Hours", 249, map[string]string{"sku": "A100"}, func(r map[string]any) { r["ending_before"] = "2026-10-02T00:00:00Z" })
	f.addRate(card, gpuProd, "GPU Hours", 249, map[string]string{"sku": "L40S"}, func(r map[string]any) { r["starting_at"] = "2026-12-01T00:00:00Z" })
	a := newPricingAdapter(t, f, "current")

	cases := []struct {
		name   string
		series destinations.PriceSeries
		status string
		detail string
	}{
		{"no billable metric", destinations.PriceSeries{Plan: "current", Metric: storage.Code, SKU: "ssd"}, destinations.PriceMissing, "no billable metric"},
		{"no product", destinations.PriceSeries{Plan: "current", Metric: cpu.Code}, destinations.PriceMissing, "no usage product meters billable metric"},
		{"not entitled", gpuSeries(a, "current", "H100", nil), destinations.PriceMissing, "not entitled"},
		{"rate ended", gpuSeries(a, "current", "A100", nil), destinations.PriceMissing, "has no rate for"},
		{"rate not started", gpuSeries(a, "current", "L40S", nil), destinations.PriceMissing, "has no rate for"},
		{"alias that ended", gpuSeries(a, "old", "H100", nil), destinations.PriceMissing, "no Metronome rate card"},
		{"no rate card configured", gpuSeries(a, "", "H100", nil), destinations.PriceUnknown, "no rate card is configured"},
	}
	var all []destinations.PriceSeries
	for _, c := range cases {
		all = append(all, c.series)
	}
	got := check(t, a, all...)
	for _, c := range cases {
		cov := got[c.series.Key()]
		if cov.Status != c.status || !strings.Contains(cov.Detail, c.detail) {
			t.Errorf("%s: got %s %q, want %s containing %q", c.name, cov.Status, cov.Detail, c.status, c.detail)
		}
		if cov.Status == destinations.PriceMissing && cov.Hint == "" {
			t.Errorf("%s: unpriced without a hint", c.name)
		}
	}
	_ = cpuBM
}

func TestPriceCheckCatalogAndPaging(t *testing.T) {
	f := newFakePricing(t)
	f.pageSize = 1
	gpu := gpuDef(t)
	lb, _ := usage.Lookup(usage.MetricLBHours)
	gpuProd := f.addProduct("GPU Hours", f.addMetric(gpu), "sku")
	f.addProduct("Unrelated", uuidFor("other-metric"))
	f.addProduct("LB Hours", f.addMetric(lb))
	card := f.addCard("rates", map[string]any{"name": "rc"})
	f.addRate(card, gpuProd, "GPU Hours", 0, map[string]string{"sku": "H100"})
	f.addRate(card, gpuProd, "GPU Hours", 249, map[string]string{"sku": "B200"})
	a := newPricingAdapter(t, f, "rc")

	gpuCat := destinations.PriceSeries{Plan: "rc", Metric: gpu.Code, Catalog: true}
	lbCat := destinations.PriceSeries{Plan: "rc", Metric: lb.Code, Catalog: true}
	got := check(t, a, gpuCat, lbCat)
	if got[gpuCat.Key()].Status != destinations.PriceCovered {
		t.Errorf("gpu catalog: %+v", got[gpuCat.Key()])
	}
	if c := got[lbCat.Key()]; c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, `"LB Hours"`) {
		t.Errorf("lb catalog: %+v", c)
	}
	if f.calls["list_products"] != 3 {
		t.Errorf("products should be read page by page: %d calls", f.calls["list_products"])
	}
}

func TestPriceCheckUsesStoredBillableMetricID(t *testing.T) {
	f := newFakePricing(t)
	gpu := gpuDef(t)
	// The billable metric was renamed in Metronome: only the stored ID finds it.
	id := uuidFor("renamed")
	f.metrics = append(f.metrics, map[string]any{"id": id, "name": "GPU (renamed)"})
	prod := f.addProduct("GPU Hours", id, "sku")
	card := f.addCard("rates", map[string]any{"name": "rc"})
	f.addRate(card, prod, "GPU Hours", 249, map[string]string{"sku": "H100"})
	a := newPricingAdapter(t, f, "rc")
	a.state.Set("bm/"+gpu.Code, id)
	s := gpuSeries(a, "rc", "H100", nil)
	if c := check(t, a, s)[s.Key()]; c.Status != destinations.PriceCovered {
		t.Errorf("%+v", c)
	}
}

func TestPricePlanAndDims(t *testing.T) {
	a := New(nil, &config.Config{MetronomeRateCard: "default-rc", MetronomeProvision: true}, kv.Memory(), nil)
	if p := a.PricePlan(usage.Tenant{ID: "t"}); p != "default-rc" {
		t.Errorf("plan %q", p)
	}
	if p := a.PricePlan(usage.Tenant{ID: "t", Plan: "enterprise"}); p != "enterprise" {
		t.Errorf("plan %q", p)
	}
	// Without provisioning neither the rate card setting nor the tenant's
	// plan decides: the tenant's own customer's contracts do, once read.
	ro := New(nil, &config.Config{MetronomeRateCard: "default-rc"}, kv.Memory(), nil)
	for _, tn := range []usage.Tenant{{ID: "t"}, {ID: "t", Plan: "enterprise"}} {
		if p := ro.PricePlan(tn); p != aliasPlan+"t" {
			t.Errorf("default mode plan %q, want %q", p, aliasPlan+"t")
		}
	}
	if p := ro.PricePlan(usage.Tenant{}); p != "" {
		t.Errorf("default mode plan of no tenant %q", p)
	}
	dims := a.PriceDims(usage.MetricGPUHours)
	for _, d := range dims {
		if d == "sku" || d == usage.DimTenantCluster {
			t.Errorf("dims %v include %s", dims, d)
		}
	}
	if len(dims) != 3 { // region, capacity_type, billing_mode
		t.Errorf("dims %v", dims)
	}
}

func TestPricingSchemasCatchTypos(t *testing.T) {
	loadPricingSchemas(t)
	bad := map[string]any{"rate_card": uuidFor("x"), "at": "2026-10-05T12:00:00Z"}
	var wire any
	b, _ := json.Marshal(bad)
	json.Unmarshal(b, &wire)
	if errs := validate(pricingSchemas["get_rates"].Request, wire, "get_rates"); len(errs) == 0 {
		t.Fatal("validator accepted a misspelled getRates field")
	}
	if errs := validate(pricingSchemas["list_rate_cards"].Request, map[string]any{"limit": 5.0}, "list_rate_cards"); len(errs) == 0 {
		t.Fatal("validator accepted a field in the rate card list body, which takes none")
	}
}
