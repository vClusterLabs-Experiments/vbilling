package metronome

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
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
