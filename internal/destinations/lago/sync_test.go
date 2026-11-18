package lago

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

const (
	h100 = "NVIDIA-H100-80GB-HBM3"
	l4   = "NVIDIA-L4"
	a100 = "NVIDIA-A100-80GB"
)

func newTestAdapter(t *testing.T, f *fakeLago, card PriceCard, managed ...string) *Adapter {
	t.Helper()
	state, err := kv.Open(filepath.Join(t.TempDir(), "state-lago.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DefaultPlanCode: "vcluster-standard", BillingCurrency: "USD", LagoManagedPlans: managed}
	return New(f.client(), cfg, state, card)
}

func builtinCatalog() []usage.MetricDef {
	var out []usage.MetricDef
	for _, d := range usage.Catalog() {
		if !d.Custom {
			out = append(out, d)
		}
	}
	return out
}

// seedLago creates vBilling's builtin billable metrics, and the default plan
// with a standard charge at "0" for each, as a previous vBilling start did.
// edit changes charges before they are stored.
func seedLago(f *fakeLago, edit func(metric string, charge map[string]any)) {
	var charges []map[string]any
	for _, m := range lagoMetrics(nil) {
		f.addMetric(m.Code, m.Name)
	}
	for _, m := range lagoMetrics(nil) {
		c := f.charge(m.Code, "0")
		if edit != nil {
			edit(m.Code, c)
		}
		charges = append(charges, c)
	}
	f.addPlan("vcluster-standard", charges...)
}

func gpuEvent(tenant, sku string, minute int) usage.Event {
	ws := time.Date(2026, 11, 18, 10, minute, 0, 0, time.UTC)
	e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: 1.0 / 60, SKU: sku, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	e.Finalize()
	return e
}

// planPut decodes a recorded plan update body.
func planPut(t *testing.T, r fakeReq) []map[string]any {
	t.Helper()
	var body struct {
		Plan struct {
			Charges []map[string]any `json:"charges"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Plan.Charges
}

func filterAmount(charge map[string]any, sku string) string {
	for _, fr := range charge["filters"].([]any) {
		fm := fr.(map[string]any)
		vs, _ := fm["values"].(map[string]any)["sku"].([]any)
		if len(vs) == 1 && vs[0] == sku {
			return fm["properties"].(map[string]any)["amount"].(string)
		}
	}
	return ""
}

func amount(charge map[string]any) string {
	return charge["properties"].(map[string]any)["amount"].(string)
}

func TestBootstrapAddsMissingChargesAndKeepsExistingOnes(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, func(metric string, c map[string]any) {
		if metric == MetricGPUHours { // priced by Finance in the Lago UI
			c["invoice_display_name"] = "GPU time"
			c["properties"] = map[string]any{"amount": "2.5", "grouped_by": []any{"tenant_cluster"}}
			c["min_amount_cents"] = 1000
			c["invoiceable"] = false
			c["taxes"] = []any{map[string]any{"code": "gst"}}
			c["filters"] = []any{map[string]any{"invoice_display_name": "H100", "properties": map[string]any{"amount": "3.1"}, "values": map[string]any{"sku": []any{h100}}}}
		}
	})
	f.metrics[MetricGPUHours].Filters = []MetricFilter{{Key: "sku", Values: []string{h100}}}
	before := f.planJSON("vcluster-standard")["charges"].([]any)

	tokens := usage.MetricDef{Code: "inference_output_tokens", Name: "Inference output tokens", Custom: true, Billable: true}
	a := newTestAdapter(t, f, PriceCard{"inference_output_tokens": {Amount: "0.000002"}})
	if err := a.Bootstrap(context.Background(), append(builtinCatalog(), tokens)); err != nil {
		t.Fatal(err)
	}

	puts := f.requests("PUT", "/api/v1/plans/")
	if len(puts) != 1 || puts[0].Path != "/api/v1/plans/vcluster-standard" {
		t.Fatalf("want one update of the default plan, got %d", len(puts))
	}
	sent := map[string]map[string]any{}
	var created []map[string]any
	for _, c := range planPut(t, puts[0]) {
		if id, ok := c["id"].(string); ok {
			sent[id] = c
		} else {
			created = append(created, c)
		}
	}
	// Every existing charge is resent with its id and its fields unchanged.
	for _, raw := range before {
		c := raw.(map[string]any)
		s := sent[c["lago_id"].(string)]
		if s == nil {
			t.Fatalf("charge %s (%s) omitted: Lago would delete it", c["lago_id"], c["billable_metric_code"])
		}
		for _, k := range []string{"invoice_display_name", "charge_model", "properties", "min_amount_cents", "invoiceable", "pay_in_advance", "prorated"} {
			if !reflect.DeepEqual(s[k], c[k]) {
				t.Errorf("%s: %s sent as %v, was %v", c["billable_metric_code"], k, s[k], c[k])
			}
		}
		if s["billable_metric_id"] != c["lago_billable_metric_id"] {
			t.Errorf("%s: billable_metric_id %v", c["billable_metric_code"], s["billable_metric_id"])
		}
		if f := c["filters"].([]any); len(f) > 0 && !reflect.DeepEqual(s["filters"], f) {
			t.Errorf("%s: filters sent as %v, were %v", c["billable_metric_code"], s["filters"], f)
		}
	}
	gpu := sent[f.chargeFor("vcluster-standard", MetricGPUHours)["lago_id"].(string)]
	if !reflect.DeepEqual(gpu["tax_codes"], []any{"gst"}) {
		t.Errorf("gpu tax_codes = %v, want [gst]", gpu["tax_codes"])
	}
	// The new metric gets one standard charge priced from the price card.
	if len(created) != 1 || created[0]["billable_metric_id"] != f.metrics["inference_output_tokens"].ID ||
		created[0]["charge_model"] != "standard" || amount(created[0]) != "0.000002" {
		t.Fatalf("created charges = %v", created)
	}

	// In Lago, every existing charge is exactly as it was.
	after := map[string]any{}
	for _, c := range f.planJSON("vcluster-standard")["charges"].([]any) {
		after[c.(map[string]any)["lago_id"].(string)] = c
	}
	for _, c := range before {
		if id := c.(map[string]any)["lago_id"].(string); !reflect.DeepEqual(after[id], c) {
			t.Errorf("charge changed in Lago:\nbefore %v\nafter  %v", c, after[id])
		}
	}
	if f.chargeFor("vcluster-standard", "inference_output_tokens") == nil {
		t.Fatal("no charge for the new metric in Lago")
	}

	// A second start changes nothing.
	f.resetRequests()
	if err := a.Bootstrap(context.Background(), append(builtinCatalog(), tokens)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/plans/")); n != 0 {
		t.Fatalf("second bootstrap updated the plan %d times", n)
	}

	// A charge Finance deletes in Lago is not added back.
	p := f.plans["vcluster-standard"]
	for i, c := range p.Charges {
		if c["billable_metric_code"] == MetricLBHours {
			p.Charges = append(p.Charges[:i], p.Charges[i+1:]...)
			break
		}
	}
	if err := a.Bootstrap(context.Background(), append(builtinCatalog(), tokens)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/plans/")); n != 0 || f.chargeFor("vcluster-standard", MetricLBHours) != nil {
		t.Fatal("a charge deleted in Lago was added back")
	}
}

func TestBootstrapNeverBlocksDeliveryOnARejectedPlanUpdate(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, nil)
	tokens := usage.MetricDef{Code: "inference_output_tokens", Name: "Inference output tokens", Custom: true, Billable: true}
	catalog := append(builtinCatalog(), tokens)
	a := newTestAdapter(t, f, nil)

	// Lago is down: Bootstrap fails, and the dispatcher retries it.
	f.setFail("PUT /api/v1/plans/vcluster-standard", 503)
	if err := a.Bootstrap(context.Background(), catalog); err == nil {
		t.Fatal("a 503 must fail Bootstrap so it is retried")
	}
	// Lago rejects the update: retrying cannot help, so Bootstrap succeeds
	// and delivery starts.
	f.setFail("PUT /api/v1/plans/vcluster-standard", 422)
	if err := a.Bootstrap(context.Background(), catalog); err != nil {
		t.Fatalf("a rejected plan update must not block delivery: %v", err)
	}
	if f.chargeFor("vcluster-standard", "inference_output_tokens") != nil {
		t.Fatal("charge added despite the rejection")
	}
	// Fixed in Lago: the next start adds the charge.
	f.setFail("PUT /api/v1/plans/vcluster-standard", 0)
	if err := a.Bootstrap(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if f.chargeFor("vcluster-standard", "inference_output_tokens") == nil {
		t.Fatal("charge not added once Lago accepted the update")
	}
}

func TestNewSKUUpdatesMetricAndPlanOnce(t *testing.T) {
	f := newFakeLago(t)
	f.addMetric(MetricGPUHours, "vCluster GPU Hours",
		MetricFilter{Key: "region", Values: []string{"us-east-1"}},
		MetricFilter{Key: "sku", Values: []string{l4}})
	f.addMetric(MetricCPUCoreHours, "vCluster CPU Core-Hours")
	gpu := f.charge(MetricGPUHours, "1.5")
	gpu["filters"] = []any{map[string]any{"invoice_display_name": nil, "properties": map[string]any{"amount": "0.8"},
		"values": map[string]any{"sku": []any{l4}, "region": []any{"us-east-1"}}}}
	f.addPlan("vcluster-standard", gpu, f.charge(MetricCPUCoreHours, "0.05"))
	f.addPlan("enterprise", f.charge(MetricGPUHours, "9")) // not managed
	beforeL4 := f.chargeFor("vcluster-standard", MetricGPUHours)["filters"]

	statePath := filepath.Join(t.TempDir(), "state-lago.json")
	state, _ := kv.Open(statePath)
	cfg := &config.Config{DefaultPlanCode: "vcluster-standard"}
	a := New(f.client(), cfg, state, nil)

	cpu := usage.Event{Tenant: "acme", Metric: usage.MetricCPUCoreHours, Quantity: 1, WindowStart: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	cpu.WindowEnd = cpu.WindowStart.Add(time.Minute)
	cpu.Finalize()
	events := []usage.Event{gpuEvent("acme", h100, 0), gpuEvent("acme", h100, 1), gpuEvent("globex", h100, 0), gpuEvent("acme", l4, 2), cpu}
	if err := a.SendEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != len(events) {
		t.Fatalf("delivered %d events, want %d", len(f.events), len(events))
	}

	mputs := f.requests("PUT", "/api/v1/billable_metrics/")
	if len(mputs) != 1 || mputs[0].Path != "/api/v1/billable_metrics/"+MetricGPUHours {
		t.Fatalf("want one billable metric update, got %d", len(mputs))
	}
	var mbody struct {
		BillableMetric struct {
			Filters []MetricFilter `json:"filters"`
		} `json:"billable_metric"`
	}
	json.Unmarshal(mputs[0].Body, &mbody)
	want := []MetricFilter{{Key: "region", Values: []string{"us-east-1"}}, {Key: "sku", Values: []string{l4, h100}}}
	if !reflect.DeepEqual(mbody.BillableMetric.Filters, want) {
		t.Fatalf("billable metric filters sent = %+v, want every key and value: %+v", mbody.BillableMetric.Filters, want)
	}

	pputs := f.requests("PUT", "/api/v1/plans/")
	if len(pputs) != 1 || pputs[0].Path != "/api/v1/plans/vcluster-standard" {
		t.Fatalf("want one update of the default plan, got %+v", pputs)
	}
	if len(planPut(t, pputs[0])) != 2 {
		t.Fatal("the plan update must resend every charge")
	}
	charge := f.chargeFor("vcluster-standard", MetricGPUHours)
	filters := charge["filters"].([]any)
	if len(filters) != 2 || !reflect.DeepEqual(filters[0], beforeL4.([]any)[0]) {
		t.Fatalf("existing filter must be kept as is: %v", filters)
	}
	nf := filters[1].(map[string]any)
	if nf["invoice_display_name"] != "vCluster GPU Hours - "+h100 || !reflect.DeepEqual(nf["values"], map[string]any{"sku": []any{h100}}) {
		t.Fatalf("new filter = %v", nf)
	}
	// No price card: the filter keeps the charge's amount, so its usage costs what it did.
	if got := filterAmount(charge, h100); got != "1.5" {
		t.Fatalf("new filter amount = %q, want the charge's 1.5", got)
	}
	if amount(f.chargeFor("vcluster-standard", MetricCPUCoreHours)) != "0.05" {
		t.Fatal("cpu charge changed")
	}
	if len(f.requests("GET", "/api/v1/plans/enterprise"))+len(f.requests("PUT", "/api/v1/plans/enterprise")) != 0 ||
		amount(f.chargeFor("enterprise", MetricGPUHours)) != "9" {
		t.Fatal("a plan outside LAGO_MANAGED_PLANS was touched")
	}

	// The same SKU again: nothing to do, not even a lookup.
	f.resetRequests()
	if err := a.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 3)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("GET", "/api/v1/")) + len(f.requests("PUT", "/api/v1/")); n != 0 {
		t.Fatalf("known SKU caused %d Lago calls", n)
	}
	// After a restart too: the state is on disk.
	state2, _ := kv.Open(statePath)
	b := New(f.client(), cfg, state2, nil)
	if err := b.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 4), gpuEvent("acme", l4, 4)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/")); n != 0 {
		t.Fatalf("restarted adapter redid known SKUs: %d updates", n)
	}
}

func TestPriceCardAmountsApplyOnlyWhenCreating(t *testing.T) {
	f := newFakeLago(t) // empty Lago
	card := PriceCard{
		MetricGPUHours:     {Amount: "0", SKU: map[string]Amount{h100: "2.49"}},
		MetricCPUCoreHours: {Amount: "0.05"},
	}
	a := newTestAdapter(t, f, card)
	if err := a.Bootstrap(context.Background(), builtinCatalog()); err != nil {
		t.Fatal(err)
	}
	// The new plan's charges use the price card, "0" for unlisted metrics.
	posts := f.requests("POST", "/api/v1/plans")
	if len(posts) != 1 {
		t.Fatalf("plan creations: %d", len(posts))
	}
	if got := amount(f.chargeFor("vcluster-standard", MetricCPUCoreHours)); got != "0.05" {
		t.Errorf("cpu amount = %s", got)
	}
	if got := amount(f.chargeFor("vcluster-standard", MetricMemoryGBHours)); got != "0" {
		t.Errorf("memory amount = %s", got)
	}
	// SKUs in the price card get their filter at bootstrap, before any usage.
	if got := filterAmount(f.chargeFor("vcluster-standard", MetricGPUHours), h100); got != "2.49" {
		t.Fatalf("H100 filter amount = %q, want 2.49", got)
	}

	// Finance changes prices in Lago.
	for _, c := range f.plans["vcluster-standard"].Charges {
		switch c["billable_metric_code"] {
		case MetricCPUCoreHours:
			c["properties"] = map[string]any{"amount": "0.07"}
		case MetricGPUHours:
			c["properties"] = map[string]any{"amount": "1.25"}
			c["filters"].([]any)[0].(map[string]any)["properties"] = map[string]any{"amount": "3.00"}
		}
	}
	// Restart, new SKUs and a metric missing from the plan: amounts in Lago stay.
	p := f.plans["vcluster-standard"]
	for i, c := range p.Charges {
		if c["billable_metric_code"] == MetricLBHours {
			p.Charges = append(p.Charges[:i], p.Charges[i+1:]...)
			break
		}
	}
	b := newTestAdapter(t, f, PriceCard{MetricGPUHours: {SKU: map[string]Amount{h100: "2.49", a100: "1.80"}}, MetricLBHours: {Amount: "0.02"}})
	if err := b.Bootstrap(context.Background(), builtinCatalog()); err != nil {
		t.Fatal(err)
	}
	if err := b.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 0), gpuEvent("acme", l4, 0)}); err != nil {
		t.Fatal(err)
	}
	gpu := f.chargeFor("vcluster-standard", MetricGPUHours)
	for sku, want := range map[string]string{h100: "3.00", a100: "1.80", l4: "1.25"} {
		if got := filterAmount(gpu, sku); got != want {
			t.Errorf("%s filter amount = %q, want %s", sku, got, want)
		}
	}
	if amount(gpu) != "1.25" || amount(f.chargeFor("vcluster-standard", MetricCPUCoreHours)) != "0.07" {
		t.Error("an amount set in Lago was overwritten")
	}
	if lb := f.chargeFor("vcluster-standard", MetricLBHours); lb == nil || amount(lb) != "0.02" {
		t.Errorf("re-created LB charge (fresh state) = %v, want the price card amount", lb)
	}
}

func TestSKUSyncFailureNeverFailsDelivery(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, nil)
	a := newTestAdapter(t, f, nil)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	ctx := context.Background()

	f.setFail("PUT /api/v1/billable_metrics/"+MetricGPUHours, 500)
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", h100, 0)}); err != nil {
		t.Fatalf("a Lago error in SKU sync must not fail delivery: %v", err)
	}
	if len(f.events) != 1 || len(f.requests("PUT", "/api/v1/billable_metrics/")) != 1 {
		t.Fatalf("events %d, metric updates %d", len(f.events), len(f.requests("PUT", "/api/v1/billable_metrics/")))
	}
	// Inside the backoff, later batches do not hammer Lago.
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", h100, 1)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/billable_metrics/")); n != 1 {
		t.Fatalf("retried inside the backoff: %d attempts", n)
	}
	// Lago recovers: a later batch sets the SKU up.
	f.setFail("PUT /api/v1/billable_metrics/"+MetricGPUHours, 0)
	now = now.Add(2 * time.Minute)
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", h100, 2)}); err != nil {
		t.Fatal(err)
	}
	if filterAmount(f.chargeFor("vcluster-standard", MetricGPUHours), h100) == "" {
		t.Fatal("SKU filter not added after Lago recovered")
	}

	// A rejected plan update (422) is not a delivery failure either, and
	// the retry skips the metric update that already went through.
	f.setFail("PUT /api/v1/plans/vcluster-standard", 422)
	f.resetRequests()
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", l4, 3)}); err != nil {
		t.Fatalf("plan update rejection failed delivery: %v", err)
	}
	f.setFail("PUT /api/v1/plans/vcluster-standard", 0)
	now = now.Add(2 * time.Minute)
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", l4, 4)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/billable_metrics/")); n != 1 {
		t.Errorf("metric updates = %d, want 1", n)
	}
	if filterAmount(f.chargeFor("vcluster-standard", MetricGPUHours), l4) == "" {
		t.Fatal("L4 filter not added on retry")
	}
	if len(f.events) != 5 {
		t.Fatalf("delivered %d events, want 5", len(f.events))
	}

	// A billing platform that is down fails the batch (retried by the
	// dispatcher) as before.
	f.setFail("POST /api/v1/events/batch", 503)
	if err := a.SendEvents(ctx, []usage.Event{gpuEvent("acme", a100, 5)}); err == nil {
		t.Fatal("event delivery failure must be returned")
	}
}

func TestManagedPlans(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, nil)
	f.addPlan("enterprise", f.charge(MetricGPUHours, "4"), f.charge(MetricCPUCoreHours, "0.04"))
	f.addPlan("custom", f.charge(MetricGPUHours, "7"))
	a := newTestAdapter(t, f, nil, "enterprise", "vcluster-standard", "missing")

	if err := a.Bootstrap(context.Background(), builtinCatalog()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.planJSON("enterprise")["charges"].([]any)); n != len(lagoMetrics(nil)) {
		t.Fatalf("enterprise plan has %d charges, want one per metric", n)
	}
	if amount(f.chargeFor("enterprise", MetricGPUHours)) != "4" {
		t.Fatal("enterprise GPU price changed")
	}
	if err := a.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 0)}); err != nil {
		t.Fatal(err)
	}
	for plan, want := range map[string]string{"vcluster-standard": "0", "enterprise": "4"} {
		if got := filterAmount(f.chargeFor(plan, MetricGPUHours), h100); got != want {
			t.Errorf("%s H100 filter = %q, want %q", plan, got, want)
		}
	}
	if len(f.requests("PUT", "/api/v1/plans/custom")) != 0 || len(f.requests("GET", "/api/v1/plans/custom")) != 0 {
		t.Fatal("unmanaged plan touched")
	}
	if len(f.requests("PUT", "/api/v1/plans/missing")) != 0 {
		t.Fatal("missing plan updated")
	}
	if got := a.managedPlans(); !reflect.DeepEqual(got, []string{"vcluster-standard", "enterprise", "missing"}) {
		t.Fatalf("managed plans = %v", got)
	}

	// A plan added to LAGO_MANAGED_PLANS later gets the SKUs already seen.
	f.resetRequests()
	b := New(f.client(), &config.Config{DefaultPlanCode: "vcluster-standard", LagoManagedPlans: []string{"enterprise", "custom"}}, a.state, nil)
	if err := b.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 1)}); err != nil {
		t.Fatal(err)
	}
	if got := filterAmount(f.chargeFor("custom", MetricGPUHours), h100); got != "7" {
		t.Fatalf("newly managed plan H100 filter = %q, want 7", got)
	}
	if n := len(f.requests("PUT", "/api/v1/plans/")); n != 1 {
		t.Fatalf("plan updates = %d, want only the newly managed plan", n)
	}
}

func TestSKUFiltersLeaveCustomPricingAlone(t *testing.T) {
	f := newFakeLago(t)
	seedLago(f, func(metric string, c map[string]any) {
		switch metric {
		case MetricGPUHours: // one filter already prices two SKUs
			c["filters"] = []any{map[string]any{"invoice_display_name": "Hopper and Ampere", "properties": map[string]any{"amount": "2"},
				"values": map[string]any{"sku": []any{h100, a100}}}}
		case MetricStorageGBHours: // graduated: filters would change the tiers
			c["charge_model"] = "graduated"
			c["properties"] = map[string]any{"graduated_ranges": []any{map[string]any{"from_value": 0, "to_value": nil, "per_unit_amount": "0.1", "flat_amount": "0"}}}
		}
	})
	f.metrics[MetricGPUHours].Filters = []MetricFilter{{Key: "sku", Values: []string{a100, h100}}}
	a := newTestAdapter(t, f, nil)
	ssd := usage.Event{Tenant: "acme", Metric: usage.MetricStorageGBHours, Quantity: 3, SKU: "premium-ssd", WindowStart: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	ssd.WindowEnd = ssd.WindowStart.Add(time.Minute)
	ssd.Finalize()
	if err := a.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 0), ssd}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("PUT", "/api/v1/plans/")); n != 0 {
		t.Fatalf("plan updated %d times; covered SKUs and graduated charges must be left alone", n)
	}
	// The storage SKU still becomes a filter value, for Finance to use in Lago.
	puts := f.requests("PUT", "/api/v1/billable_metrics/")
	if len(puts) != 1 || !strings.Contains(string(puts[0].Body), "premium-ssd") {
		t.Fatalf("metric updates = %v", puts)
	}
	// Done: later batches do not ask again.
	f.resetRequests()
	if err := a.SendEvents(context.Background(), []usage.Event{gpuEvent("acme", h100, 1)}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests("GET", "/api/v1/plans/")); n != 0 {
		t.Fatalf("plan read %d times for a finished SKU", n)
	}
}
