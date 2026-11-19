package metronome

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// These tests run the adapter the way an AI Cloud runs Metronome: its portal
// creates customers with an identity alias and a compute alias (the vBilling
// tenant ID) and the contracts that hold prepaid commits, and Finance owns
// billable metrics, products and rate cards. METRONOME_PROVISION is off.

func newFake(t *testing.T) *metronometest.Server {
	f := metronometest.New(t)
	f.SetClock(func() time.Time { return now })
	return f
}

func newReadOnly(t *testing.T, f *metronometest.Server, mutate ...func(*config.Config)) *Adapter {
	cfg := &config.Config{MetronomeStripeCollect: "charge_automatically"}
	for _, m := range mutate {
		m(cfg)
	}
	a := New(NewClient(f.URL, metronometest.Token), cfg, kv.Memory(), nil)
	a.now = func() time.Time { return now }
	return a
}

// financeMetric adds the billable metric `vbilling metronome billable-metrics`
// prints for a metric, under a name Finance chose.
func financeMetric(t *testing.T, f *metronometest.Server, code, name string, mutate ...func(*BillableMetric)) string {
	t.Helper()
	def, ok := usage.Lookup(code)
	if !ok {
		t.Fatalf("no metric %s", code)
	}
	bm := BillableMetricFor(def)
	bm.Name = name
	for _, m := range mutate {
		m(&bm)
	}
	b, _ := json.Marshal(bm)
	return f.AddBillableMetric(b)
}

func gpuSeriesOf(a *Adapter, tenant, sku string) destinations.PriceSeries {
	ev := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, SKU: sku, Region: "ap-southeast-2",
		Dimensions: map[string]string{usage.DimCapacityType: usage.CapacityOnDemand, usage.DimBillingMode: "private_node"}}
	return destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: tenant}), &ev)
}

func coverageOf(t *testing.T, a *Adapter, series ...destinations.PriceSeries) []destinations.PriceCoverage {
	t.Helper()
	out, err := a.CheckPrices(context.Background(), series)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Without METRONOME_PROVISION nothing in Metronome is created or changed,
// even with a rate card, a pinned customer and a Stripe key configured:
// usage goes out under the tenant ID as ingest alias, and everything else is
// a read.
func TestDefaultModeNeverWritesToMetronome(t *testing.T) {
	f := newFake(t)
	financeMetric(t, f, usage.MetricGPUHours, "GPU hours") // Finance applies the billable metrics before vBilling starts
	a := newReadOnly(t, f, func(c *config.Config) { c.MetronomeRateCard = "payg"; c.StripeAPIKey = "sk_test_x" })
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatalf("bootstrap must not hold up delivery: %v", err)
	}
	pinned := usage.Tenant{ID: "tenant-acme", ProviderIDs: map[string]string{"metronome": "8f0e3c1a-6d3b-4c8e-9a51-1f0d7a2b9c44"}}
	if err := a.EnsureTenant(ctx, pinned); err != nil {
		t.Fatal(err)
	}
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-2*time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}
	// The portal signs the customer up afterwards; reads remember it.
	cus, err := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	if err != nil {
		t.Fatal(err)
	}
	coverageOf(t, a, gpuSeriesOf(a, "tenant-acme", "NVIDIA-H100"))
	day := now.Truncate(24 * time.Hour)
	if _, err := a.RecordedTotals(ctx, destinations.TotalsQuery{From: day, To: day.Add(24 * time.Hour),
		Tenants: []usage.Tenant{{ID: "tenant-acme"}}, Metrics: []string{usage.MetricGPUHours}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.TenantForCustomer(ctx, cus); !ok {
		t.Fatal("webhook customer not resolved")
	}
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}

	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("vBilling wrote to Metronome without METRONOME_PROVISION: %v", w)
	}
	evs := f.Events()
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	for _, e := range evs {
		if e.CustomerID != "tenant-acme" {
			t.Errorf("customer_id %q: usage must key on the ingest alias, not a customer ID", e.CustomerID)
		}
		if e.Customer != cus {
			t.Errorf("event not attributed to the portal's customer: %+v", e)
		}
	}
}

// With METRONOME_PROVISION the adapter keeps creating what it needs.
func TestProvisionModeStillCreates(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f, func(c *config.Config) { c.MetronomeProvision = true })
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "tenant-acme", DisplayName: "Acme"}); err != nil {
		t.Fatal(err)
	}
	var creates, metrics int
	for _, w := range f.Writes() {
		switch w {
		case "POST /v1/customers":
			creates++
		case "POST /v1/billable-metrics/create":
			metrics++
		}
	}
	if creates != 1 || metrics != len(usage.Billable()) {
		t.Fatalf("customers %d, billable metrics %d (want 1, %d)", creates, metrics, len(usage.Billable()))
	}
}

// vBilling never creates a customer for an ingest alias another customer
// holds: an active holder is adopted, and an archived one (which Metronome
// does not list by default, but still answers 409 for) stops with an error
// that names it, instead of retrying a create forever.
func TestEnsureTenantNeverTakesAnAliasAnotherCustomerHolds(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f, func(c *config.Config) { c.MetronomeProvision = true })
	ctx := context.Background()

	portal, _ := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "tenant-acme"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.state.Get("cus/tenant-acme"); got != portal {
		t.Fatalf("adopted %q, want the portal's customer %q", got, portal)
	}

	old, _ := f.CreateCustomer("Churned", "tenant-old")
	f.ArchiveCustomer(old)
	err := a.EnsureTenant(ctx, usage.Tenant{ID: "tenant-old"})
	if err == nil || !destinations.IsPermanent(err) || !strings.Contains(err.Error(), old) {
		t.Fatalf("want a permanent error naming archived customer %s, got %v", old, err)
	}
	for _, w := range f.Writes() {
		if w == "POST /v1/customers" {
			t.Fatal("created a customer for an alias another customer holds")
		}
	}
}

// Price coverage without provisioning reads the rate card of the tenant's own
// customer's active contract, and finds billable metrics by event type and
// SUM of value, whatever they are called. METRONOME_RATE_CARD plays no part.
func TestPriceCoverageReadsTheTenantsOwnContract(t *testing.T) {
	f := newFake(t)
	bm := financeMetric(t, f, usage.MetricGPUHours, "Accelerator Hours")
	prod := f.AddProduct("Compute Instance Hours", bm, "sku")
	enterprise := f.AddRateCard("Enterprise AUD")
	payg := f.AddRateCard("PAYG AUD", "payg")
	f.AddRate(metronometest.Rate{RateCardID: enterprise, ProductID: prod, Price: 410, PricingGroupValues: map[string]string{"sku": "NVIDIA-H100"}})
	f.AddRate(metronometest.Rate{RateCardID: payg, ProductID: prod, Price: 450, PricingGroupValues: map[string]string{"sku": "NVIDIA-H100"}})
	f.AddRate(metronometest.Rate{RateCardID: payg, ProductID: prod, Price: 700, PricingGroupValues: map[string]string{"sku": "NVIDIA-B200"}})
	acme, _ := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	f.AddContract(metronometest.Contract{CustomerID: acme, RateCardID: enterprise, StartingAt: now.AddDate(0, -1, 0), PrepaidCommit: 50000})
	// A customer whose only contract ended.
	gone, _ := f.CreateCustomer("Globex", "org_9a9a", "tenant-globex")
	f.AddContract(metronometest.Contract{CustomerID: gone, RateCardID: payg, StartingAt: now.AddDate(0, -2, 0), EndingBefore: now.AddDate(0, 0, -3)})

	a := newReadOnly(t, f, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	h100, b200 := gpuSeriesOf(a, "tenant-acme", "NVIDIA-H100"), gpuSeriesOf(a, "tenant-acme", "NVIDIA-B200")
	newbie, globex := gpuSeriesOf(a, "tenant-newbie", "NVIDIA-H100"), gpuSeriesOf(a, "tenant-globex", "NVIDIA-H100")
	got := coverageOf(t, a, h100, b200, newbie, globex)

	if c := got[0]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, `"Enterprise AUD"`) || !strings.Contains(c.Detail, "flat 410") {
		t.Errorf("H100 on the customer's own rate card: %+v", c)
	}
	if c := got[1]; c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, `"Enterprise AUD"`) || !strings.Contains(c.Hint, "sku=NVIDIA-B200") {
		t.Errorf("B200 has a rate on METRONOME_RATE_CARD only, not on the customer's contract: %+v", c)
	}
	if c := got[2]; c.Status != destinations.PriceUnknown || !strings.Contains(c.Detail, "no customer for this alias yet") {
		t.Errorf("a tenant no customer has yet is a warning: %+v", c)
	}
	if c := got[3]; c.Status != destinations.PriceUnknown || !strings.Contains(c.Detail, "no active contract") {
		t.Errorf("a customer without an active contract: %+v", c)
	}
	// The tenant's series now share the rate card's plan; the others wait
	// under their alias until a customer or contract appears.
	if p := a.PricePlan(usage.Tenant{ID: "tenant-acme"}); p != enterprise {
		t.Errorf("plan after the check %q, want the contract's rate card %s", p, enterprise)
	}
	if p := a.PricePlan(usage.Tenant{ID: "tenant-newbie"}); p != aliasPlan+"tenant-newbie" {
		t.Errorf("plan of a tenant without customer %q", p)
	}
	if n := f.Calls("POST /v1/contract-pricing/rate-cards/getRates"); n != 1 {
		t.Errorf("rates read %d times: once per rate card in use", n)
	}

	// The portal signs the newcomer up on PAYG: the next check finds it.
	newbieCus, _ := f.CreateCustomer("Newbie", "org_0b0b", "tenant-newbie")
	f.AddContract(metronometest.Contract{CustomerID: newbieCus, RateCardID: payg, StartingAt: now.Add(-time.Hour)})
	if c := coverageOf(t, a, newbie)[0]; c.Status != destinations.PriceCovered || !strings.Contains(c.Detail, `"PAYG AUD"`) {
		t.Errorf("after signup: %+v", c)
	}
}

// Billable metrics are matched by what they count: a filter that leaves a
// SKU out, a COUNT on the event type, or nothing on it at all is a gap.
func TestPriceCoverageMatchesBillableMetricsByEventType(t *testing.T) {
	f := newFake(t)
	bm := financeMetric(t, f, usage.MetricGPUHours, "H100 hours only", func(m *BillableMetric) {
		m.PropertyFilters = append(m.PropertyFilters, PropertyFilter{Name: "sku", InValues: []string{"NVIDIA-H100"}})
	})
	financeMetric(t, f, usage.MetricCPUCoreHours, "CPU events", func(m *BillableMetric) {
		m.AggregationType, m.AggregationKey = "COUNT", ""
	})
	prod := f.AddProduct("GPU", bm)
	card := f.AddRateCard("List")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: prod, Price: 300})
	cus, _ := f.CreateCustomer("Acme", "tenant-acme")
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: now.AddDate(0, -1, 0)})

	a := newReadOnly(t, f)
	cpu := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricCPUCoreHours, Region: "ap-southeast-2"}
	storage := usage.Event{Tenant: "tenant-acme", Metric: usage.MetricStorageGBHours, SKU: "fast", Region: "ap-southeast-2"}
	plan := a.PricePlan(usage.Tenant{ID: "tenant-acme"})
	got := coverageOf(t, a, gpuSeriesOf(a, "tenant-acme", "NVIDIA-H100"), gpuSeriesOf(a, "tenant-acme", "NVIDIA-B200"),
		destinations.SeriesOf(a, plan, &cpu), destinations.SeriesOf(a, plan, &storage))
	if got[0].Status != destinations.PriceCovered {
		t.Errorf("H100 is counted by %q: %+v", "H100 hours only", got[0])
	}
	if c := got[1]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "property filters") {
		t.Errorf("B200 is filtered out of the only billable metric: %+v", c)
	}
	if c := got[2]; c.Status != destinations.PriceMissing || !strings.Contains(c.Detail, "COUNT") {
		t.Errorf("a COUNT cannot bill quantities: %+v", c)
	}
	if c := got[3]; c.Status != destinations.PriceMissing || !strings.Contains(c.Hint, "vbilling metronome billable-metrics") {
		t.Errorf("no billable metric for storage: %+v", c)
	}
}

// In hold mode a tenant without a customer is never held (Metronome
// attributes its usage once the portal creates the customer); a SKU missing
// from the customer's own rate card is.
func TestHoldModeNeverHoldsUsageWithoutCustomer(t *testing.T) {
	f := newFake(t)
	bm := financeMetric(t, f, usage.MetricGPUHours, "GPU Hours")
	prod := f.AddProduct("GPU Hours", bm, "sku")
	card := f.AddRateCard("List")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: prod, Price: 300, PricingGroupValues: map[string]string{"sku": "NVIDIA-H100-80GB-HBM3"}})
	cus, _ := f.CreateCustomer("Acme", "org_1", "tenant-acme")
	f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: now.AddDate(0, -1, 0)})

	a := newReadOnly(t, f)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	svc := pricing.New(sp, []destinations.Destination{a}, nil, nil, nil, nil, pricing.Options{Mode: pricing.ModeHold, DecideWait: 10 * time.Second})
	ctx := context.Background()

	priced := gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)
	unpriced := gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)
	unpriced.SKU, unpriced.ID = "NVIDIA-B200", ""
	unpriced.Finalize()
	early := gpu("tenant-newbie", "c9", now.Add(-time.Minute), 1)
	dec := svc.Gate().Decide(ctx, "metronome", []usage.Event{priced, unpriced, early})
	if dec[0].Hold || !dec[1].Hold || dec[2].Hold {
		t.Fatalf("holds: priced %v, unpriced %v, no customer yet %v (%+v)", dec[0].Hold, dec[1].Hold, dec[2].Hold, dec)
	}
	// Once the tenant's plan is its rate card, the same events decide the same way.
	svc.CheckNow(ctx)
	dec = svc.Gate().Decide(ctx, "metronome", []usage.Event{priced, unpriced, early})
	if dec[0].Hold || !dec[1].Hold || dec[2].Hold {
		t.Fatalf("after the plan moved to the rate card: %+v", dec)
	}
}

// Webhooks resolve through every alias of the customer: the one vBilling
// knows as a tenant, never just the first. Aliases are cached for a minute.
func TestCustomerAliasesAndWebhookResolution(t *testing.T) {
	f := newFake(t)
	cus, _ := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme") // identity alias first
	stranger, _ := f.CreateCustomer("Not ours", "org_7777")
	a := newReadOnly(t, f)
	ctx := context.Background()
	if err := a.SendEvents(ctx, []usage.Event{gpu("tenant-acme", "c1", now.Add(-time.Minute), 1)}); err != nil {
		t.Fatal(err)
	}

	aliases, err := a.CustomerAliases(ctx, cus)
	if err != nil || strings.Join(aliases, ",") != "org_1f2e,tenant-acme" {
		t.Fatalf("aliases %v, %v", aliases, err)
	}
	if tenant, ok := a.TenantForCustomer(ctx, cus); !ok || tenant != "tenant-acme" {
		t.Fatalf("resolved %q %v, want the compute alias tenant-acme, not the identity alias", tenant, ok)
	}
	if tenant, ok := a.TenantForCustomer(ctx, stranger); ok {
		t.Fatalf("a customer with no vBilling tenant resolved to %q", tenant)
	}
	if n := f.Calls("GET /v1/customers/{id}"); n != 2 {
		t.Fatalf("customer reads %d: cached per customer", n)
	}

	// A migration swaps the compute alias; the cache expires within a minute.
	if err := f.SetIngestAliases(cus, "org_1f2e", "tenant-acme-2"); err != nil {
		t.Fatal(err)
	}
	if aliases, _ := a.CustomerAliases(ctx, cus); strings.Join(aliases, ",") != "org_1f2e,tenant-acme" {
		t.Fatalf("within the minute the cached aliases stay: %v", aliases)
	}
	a.now = func() time.Time { return now.Add(2 * time.Minute) }
	if aliases, _ := a.CustomerAliases(ctx, cus); strings.Join(aliases, ",") != "org_1f2e,tenant-acme-2" {
		t.Fatalf("after a minute the aliases are read again: %v", aliases)
	}
	if _, ok := a.TenantForCustomer(ctx, cus); ok {
		t.Fatal("the customer no longer holds a vBilling tenant's alias")
	}

	if aliases, err := a.CustomerAliases(ctx, "00000000-0000-4000-8000-000000000000"); err != nil || aliases != nil {
		t.Fatalf("unknown customer: %v %v", aliases, err)
	}
	a.now = func() time.Time { return now.Add(10 * time.Minute) } // the cached entry expired
	f.FailNext("GET /v1/customers/{id}", 1)
	if _, err := a.CustomerAliases(ctx, stranger); err == nil {
		t.Fatal("a failed read must be an error, so the webhook is retried")
	}
}
