package stripe

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func newTestAdapter(t *testing.T, f *fakeStripe, statePath string, mutate ...func(*config.Config)) *Adapter {
	t.Helper()
	cfg := &config.Config{
		StripeSplitMetersBy: []string{"sku"},
		DefaultPlanCode:     "standard",
		BillingCurrency:     "usd",
	}
	for _, m := range mutate {
		m(cfg)
	}
	state, err := kv.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	a := New(NewClient(f.srv.URL, "sk_test_123", "2026-09-30.endive", 0), cfg, state)
	a.now = func() time.Time { return now }
	return a
}

func gpuEvent(tenant, sku string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: q, SKU: sku, WindowStart: ws, WindowEnd: ws.Add(time.Minute),
		Dimensions: map[string]string{usage.DimGPUType: sku}}
	e.Finalize()
	return e
}

func TestBootstrapCreatesBillableMetersIdempotently(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	want := len(usage.Billable())
	if len(f.meters) != want {
		t.Fatalf("created %d meters, want %d billable", len(f.meters), want)
	}
	for _, m := range f.meters {
		if m.EventName == usage.MetricGPUUtilization || m.EventName == usage.MetricGPUDowntimeHours {
			t.Errorf("informational metric %s got a meter", m.EventName)
		}
	}
	// A fresh adapter (restart) finds existing meters instead of duplicating.
	f.meters[0].Status = "inactive"
	b := newTestAdapter(t, f, "")
	if err := b.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if len(f.meters) != want {
		t.Fatalf("second bootstrap created duplicates: %d meters", len(f.meters))
	}
	if f.meters[0].Status != "active" {
		t.Fatal("inactive meter was not reactivated")
	}
}

func TestEnsureTenantNeverDuplicatesCustomers(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	statePath := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, statePath)
	ctx := context.Background()
	tenant := usage.Tenant{ID: "acme", DisplayName: "Acme AI", Email: "billing@acme.test", Region: "ap-southeast-2", Clusters: []string{"vcluster-team-a-gpu"}}

	for i := 0; i < 3; i++ { // search is not yet consistent; local state must prevent duplicates
		if err := a.EnsureTenant(ctx, tenant); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.customers) != 1 {
		t.Fatalf("created %d customers", len(f.customers))
	}
	for _, c := range f.customers {
		if c.Metadata[metaTenant] != "acme" || c.Metadata["vbilling_region"] != "ap-southeast-2" || c.Email != "billing@acme.test" {
			t.Fatalf("customer metadata: %+v", c)
		}
	}

	// Lost local state (new PVC): search finds the existing customer.
	f.makeSearchable()
	b := newTestAdapter(t, f, "")
	if err := b.EnsureTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if len(f.customers) != 1 {
		t.Fatalf("lost state led to a duplicate customer: %d", len(f.customers))
	}

	// A pinned customer ID (tenant-cluster annotation) wins.
	pinned := &Customer{ID: "cus_pinned", Metadata: map[string]string{}}
	f.customers[pinned.ID] = pinned
	c := newTestAdapter(t, f, "")
	if err := c.EnsureTenant(ctx, usage.Tenant{ID: "beta", ProviderIDs: map[string]string{"stripe": "cus_pinned"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TenantForCustomer(ctx, "cus_pinned"); got != "beta" {
		t.Fatalf("pinned customer resolves to %q", got)
	}
}

func TestSendEventsSplitsMetersPerSKU(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	ws := now.Add(-10 * time.Minute)
	events := []usage.Event{
		gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", ws, 8.0/60),
		gpuEvent("acme", "NVIDIA-L40S", ws, 2.0/60),
		gpuEvent("beta", "NVIDIA-H100-80GB-HBM3", ws, 1.0/60),
	}
	cpu := usage.Event{Tenant: "acme", Metric: usage.MetricCPUCoreHours, Quantity: 0.5, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	cpu.Finalize()
	util := usage.Event{Tenant: "acme", Metric: usage.MetricGPUUtilization, Quantity: 40, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	util.Finalize()
	events = append(events, cpu, util)

	if err := a.SendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range f.events {
		got[e.EventName+"|"+e.Customer] = e.Value
		if e.Timestamp != ws.Unix() {
			t.Errorf("timestamp %d, want window start %d", e.Timestamp, ws.Unix())
		}
	}
	if len(f.events) != 4 {
		t.Fatalf("recorded %d meter events, want 4 (utilization is informational): %+v", len(f.events), f.events)
	}
	for _, name := range []string{"vcluster_gpu_hours__nvidia_h100_80gb_hbm3", "vcluster_gpu_hours__nvidia_l40s", "vcluster_cpu_core_hours"} {
		found := false
		for k := range got {
			if strings.HasPrefix(k, name+"|") {
				found = true
			}
		}
		if !found {
			t.Errorf("no events on meter %s (have %v)", name, got)
		}
	}
	for k, v := range got {
		if strings.HasPrefix(k, "vcluster_gpu_hours__nvidia_h100_80gb_hbm3|") && v != "0.133333333" && v != "0.016666667" {
			t.Errorf("unexpected H100 value %s", v)
		}
	}
	// Every event's identifier is the deterministic vBilling ID.
	for _, e := range f.events {
		if !strings.HasPrefix(e.Identifier, "vb1_") {
			t.Errorf("identifier %q is not the event ID", e.Identifier)
		}
	}
}

func TestRetryResendsOnlyWhatFailed(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	a.workers = 1 // deterministic order
	ctx := context.Background()
	ws := now.Add(-5 * time.Minute)
	var events []usage.Event
	for i := 0; i < 6; i++ {
		events = append(events, gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", ws.Add(time.Duration(i)*time.Minute), 0.1))
	}
	// Warm up customer + meter, then fail the 4th meter event.
	if err := a.SendEvents(ctx, events[:3]); err != nil {
		t.Fatal(err)
	}
	f.failNext["POST /v1/billing/meter_events"] = 1
	err := a.SendEvents(ctx, events)
	if err == nil || destinations.IsPermanent(err) {
		t.Fatalf("a 500 must be retryable, got %v", err)
	}
	if err := a.SendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 6 {
		t.Fatalf("recorded %d events, want exactly 6", len(f.events))
	}
	// A fresh adapter (lost sent cache) resending everything hits Stripe's
	// identifier dedupe, which counts as delivered.
	b := newTestAdapter(t, f, "")
	if err := b.SendEvents(ctx, events); err != nil {
		t.Fatalf("duplicate identifiers must not fail delivery: %v", err)
	}
	if len(f.events) != 6 {
		t.Fatalf("dedupe broken: %d events", len(f.events))
	}
}

func TestTooOldEventsAreRejectedNotRetried(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	old := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", now.Add(-40*24*time.Hour), 1)
	fresh := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", now.Add(-time.Hour), 1)
	err := a.SendEvents(context.Background(), []usage.Event{old, fresh})
	var partial *destinations.PartialError
	if !errors.As(err, &partial) || len(partial.Rejected) != 1 || partial.Rejected[old.ID] == "" {
		t.Fatalf("expected PartialError rejecting only the old event, got %v", err)
	}
	if len(f.events) != 1 {
		t.Fatalf("recorded %d events", len(f.events))
	}
}

func TestAutoSubscribeUsesPlanTaggedPrices(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "", func(c *config.Config) { c.StripeAutoSubscribe = true })
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	meterID := func(name string) string {
		for _, m := range f.meters {
			if m.EventName == name {
				return m.ID
			}
		}
		t.Fatalf("no meter %s", name)
		return ""
	}
	price := func(id, meter, plan, currency string) Price {
		p := Price{ID: id, Active: true, Currency: currency, Metadata: map[string]string{metaPlan: plan}}
		p.Recurring = &struct {
			UsageType string `json:"usage_type"`
			Meter     string `json:"meter"`
		}{UsageType: "metered", Meter: meter}
		return p
	}
	f.prices = []Price{
		price("price_gpu", meterID(usage.MetricGPUHours), "standard", "usd"),
		price("price_cpu", meterID(usage.MetricCPUCoreHours), "standard", "usd"),
		price("price_aud", meterID(usage.MetricGPUHours), "standard", "aud"),   // wrong currency
		price("price_ent", meterID(usage.MetricGPUHours), "enterprise", "usd"), // other plan
		price("price_foreign", "mtr_not_ours", "standard", "usd"),              // not a vBilling meter
	}
	tenant := usage.Tenant{ID: "acme"}
	if err := a.EnsureTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if len(f.subs) != 1 {
		t.Fatalf("subscriptions: %d", len(f.subs))
	}
	for _, s := range f.subs {
		var ids []string
		for _, it := range s.Items.Data {
			ids = append(ids, it.Price.ID)
		}
		if strings.Join(ids, ",") != "price_cpu,price_gpu" {
			t.Fatalf("subscription items %v", ids)
		}
	}
	// A price added to the plan later is added to the existing subscription.
	f.prices = append(f.prices, price("price_lb", meterID(usage.MetricLBHours), "standard", "usd"))
	a.pricesTime = time.Time{}
	if err := a.EnsureTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.subs {
		if len(s.Items.Data) != 3 {
			t.Fatalf("expected 3 items after plan change, got %d", len(s.Items.Data))
		}
	}
	if len(f.subs) != 1 {
		t.Fatal("plan change created a second subscription")
	}

	// A plan over Stripe's 20-item limit rejects only this tenant: the error
	// is permanent, so delivery for everyone else carries on.
	for i := 0; i < 21; i++ {
		f.prices = append(f.prices, price(fmt.Sprintf("price_big_%02d", i), meterID(usage.MetricGPUHours), "big", "usd"))
	}
	a.pricesTime = time.Time{}
	err := a.EnsureTenant(ctx, usage.Tenant{ID: "globex", Plan: "big"})
	if err == nil || !destinations.IsPermanent(err) {
		t.Fatalf("plan with 21 prices: got %v, want a permanent error", err)
	}
	if _, ok := a.state.Get("cus/globex"); !ok {
		t.Fatal("customer should still exist so usage can be delivered")
	}
}

func TestRecordedTotalsSumsSplitMeters(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	ws := now.Add(-30 * time.Minute)
	events := []usage.Event{
		gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", ws, 0.5),
		gpuEvent("acme", "NVIDIA-L40S", ws, 0.25),
		gpuEvent("acme", "NVIDIA-L40S", ws.Add(time.Minute), 0.25),
	}
	if err := a.SendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	totals, err := a.RecordedTotals(ctx, destinations.TotalsQuery{
		From: now.Add(-time.Hour), To: now, Tenants: []usage.Tenant{{ID: "acme"}}, Metrics: []string{usage.MetricGPUHours},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 1 || totals[0].Quantity != 1.0 {
		t.Fatalf("totals: %+v", totals)
	}
}

func TestMeterNameIsBoundedAndStable(t *testing.T) {
	a := &Adapter{cfg: &config.Config{StripeSplitMetersBy: []string{"sku", "region"}}}
	e := gpuEvent("acme", "NVIDIA-A100-SXM4-80GB-mig-1g.10gb", now, 1)
	e.Region = "ap-southeast-2"
	if got := a.MeterName(&e); got != "vcluster_gpu_hours__nvidia_a100_sxm4_80gb_mig_1g_10gb__ap_southeast_2" {
		t.Fatalf("meter name %q", got)
	}
	e.SKU = strings.Repeat("x", 200)
	if got := a.MeterName(&e); len(got) != 100 || got != a.MeterName(&e) {
		t.Fatalf("long meter name not bounded/stable: %d", len(got))
	}
}

func TestFormatValue(t *testing.T) {
	cases := map[float64]string{
		0:                    "0",
		1:                    "1",
		0.016666667:          "0.016666667",
		1.0 / 3600:           "0.000277778",
		8.0 / 60:             "0.133333333",
		123456.123456789:     "123456.123456789",
		1234567890.123456789: "1234567890.12346",
		12345678901234567890: "12345678901234567168",
	}
	for in, want := range cases {
		if got := FormatValue(in); got != want {
			t.Errorf("FormatValue(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"invoice.payment_failed"}`)
	secret := "whsec_test"
	header := SignatureHeader(body, secret, now)
	if err := VerifySignature(body, header, secret, DefaultTolerance, now); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature([]byte(`{"id":"evt_2"}`), header, secret, DefaultTolerance, now); err == nil {
		t.Fatal("tampered body accepted")
	}
	if err := VerifySignature(body, header, secret, DefaultTolerance, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale signature accepted")
	}
	rolled := header + ",v1=deadbeef"
	if err := VerifySignature(body, "v1=deadbeef,"+strings.TrimPrefix(rolled, ""), secret, DefaultTolerance, now); err != nil {
		t.Fatalf("multiple v1 signatures must be accepted when one matches: %v", err)
	}
	if err := VerifySignature(body, header, "whsec_other", DefaultTolerance, now); err == nil {
		t.Fatal("wrong secret accepted")
	}
}

func TestSearchResultsMustMatchTenantMetadata(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	// A customer that search returns but that belongs to someone else.
	f.customers["cus_other"] = &Customer{ID: "cus_other", Metadata: map[string]string{metaTenant: "acme-old"}}
	f.searchable["cus_other"] = true
	f.searchAll = true
	a := newTestAdapter(t, f, "")
	if err := a.EnsureTenant(context.Background(), usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.state.Get("cus/acme"); got == "cus_other" {
		t.Fatal("tenant attributed to a customer whose metadata does not match")
	}
}

// Stripe does not offer customer search in every region (accounts in India
// get a 400). Lost local state must still find existing customers, by listing.
func TestCustomerLookupWithoutSearch(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	f.noSearch = true
	for i := 0; i < 150; i++ { // more than one page
		id := fmt.Sprintf("cus_x%03d", i)
		f.customers[id] = &Customer{ID: id, Metadata: map[string]string{}}
	}
	f.customers["cus_same_email"] = &Customer{ID: "cus_same_email", Email: "billing@acme.test", Metadata: map[string]string{metaTenant: "acme-old"}}
	f.customers["cus_y_acme"] = &Customer{ID: "cus_y_acme", Email: "billing@acme.test", Metadata: map[string]string{metaTenant: "acme"}}
	f.customers["cus_y_beta"] = &Customer{ID: "cus_y_beta", Metadata: map[string]string{metaTenant: "beta"}}
	before := len(f.customers)

	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme", Email: "billing@acme.test"}); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "beta"}); err != nil { // no email: full scan, page 2
		t.Fatal(err)
	}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "gamma"}); err != nil {
		t.Fatal(err)
	}
	for tenant, want := range map[string]string{"acme": "cus_y_acme", "beta": "cus_y_beta"} {
		if got, _ := a.state.Get("cus/" + tenant); got != want {
			t.Errorf("tenant %s resolved to %q, want %s", tenant, got, want)
		}
	}
	if len(f.customers) != before+1 {
		t.Errorf("customers %d -> %d, want only gamma created", before, len(f.customers))
	}
	if n := f.count("GET /v1/customers/search"); n != 1 {
		t.Errorf("search tried %d times; once unavailable it must not be retried", n)
	}
}
