package stripe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestUnitAmountDecimal(t *testing.T) {
	for _, tc := range []struct {
		amount   CardAmount
		per      int64
		currency string
		want     string
	}{
		{"2.20", 1, "usd", "220"},
		{"0.10", 1, "usd", "10"},
		{"0.60", 1_000_000, "usd", "0.00006"},
		{"0.014", 1, "usd", "1.4"},
		{"300", 1, "jpy", "300"},
		{"0", 1, "usd", "0"},
	} {
		got, err := unitAmountDecimal(tc.amount, tc.per, tc.currency)
		if err != nil || got != tc.want {
			t.Errorf("%s per %d %s = %q %v, want %q", tc.amount, tc.per, tc.currency, got, err, tc.want)
		}
	}
	if _, err := unitAmountDecimal("0.0000001", 1_000_000_000, "usd"); err == nil {
		t.Error("an amount finer than 12 decimals of a cent must be refused")
	}
}

func writeCard(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "card.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPriceCardChecks(t *testing.T) {
	for name, body := range map[string]string{
		"no plans":        "currency: usd\n",
		"bad interval":    "interval: hour\nplans: {p: {vcluster_lb_hours: {amount: \"1\"}}}\n",
		"negative amount": "plans: {p: {vcluster_lb_hours: {amount: \"-1\"}}}\n",
		"no amounts":      "plans: {p: {vcluster_lb_hours: {name: x}}}\n",
		"unknown field":   "plans: {p: {vcluster_lb_hours: {amont: \"1\"}}}\n",
	} {
		if _, err := LoadPriceCard(writeCard(t, body), "USD"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	pc, err := LoadPriceCard(writeCard(t, "plans: {p: {vcluster_lb_hours: {amount: 0.025}}}\n"), "USD")
	if err != nil || pc.Currency != "usd" || pc.Interval != "month" || pc.Plans["p"]["vcluster_lb_hours"].Amount != "0.025" {
		t.Fatalf("defaults: %+v %v", pc, err)
	}
}

// The price card creates every missing meter and price once, never
// touches an existing price, and the prices join the plan's subscriptions.
func TestPriceCardCreatesMissingPricesOnce(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	card := writeCard(t, `currency: usd
plans:
  dedicated-a100:
    vcluster_gpu_hours:
      sku: {NVIDIA-A100-SXM4-40GB: "2.20"}
      names: {NVIDIA-A100-SXM4-40GB: A100 40GB GPU-hour}
    vcluster_instance_hours: {amount: "0.10", name: Cluster control plane hour}
  serverless:
    vcluster_lb_hours: {amount: "0.025"}
`)
	a := newTestAdapter(t, f, "", func(c *config.Config) { c.StripeAutoSubscribe = true; c.DefaultPlanCode = "dedicated-a100" })
	pc, err := LoadPriceCard(card, "usd")
	if err != nil {
		t.Fatal(err)
	}
	a.card = pc
	ctx := context.Background()
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if len(f.prices) != 3 {
		t.Fatalf("created %d prices, want 3", len(f.prices))
	}
	var gpu Price
	for _, p := range f.prices {
		if p.Metadata["vbilling_meter"] == "vcluster_gpu_hours__nvidia_a100_sxm4_40gb" {
			gpu = p
		}
	}
	if gpu.ID == "" || *gpu.UnitAmountDecimal != "220" || gpu.Metadata[metaPlan] != "dedicated-a100" {
		t.Fatalf("GPU price %+v", gpu)
	}
	// Again (a restart): nothing new. A changed amount is logged, not applied.
	pc.Plans["dedicated-a100"]["vcluster_instance_hours"] = CardPrice{Amount: "0.20"}
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if len(f.prices) != 3 {
		t.Fatalf("a second run created prices: %d", len(f.prices))
	}
	// A tenant on the plan is subscribed to the card's prices, and its GPU
	// usage reaches the SKU meter the card priced.
	if err := a.EnsureTenant(destinations.WithUsageStart(ctx, now.Add(-time.Hour)), usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	sub := onlySub(t, f)
	if len(sub.Items.Data) != 2 {
		t.Fatalf("subscription has %d prices, want the plan's 2", len(sub.Items.Data))
	}
	ev := gpuEvent("acme", "NVIDIA-A100-SXM4-40GB", now.Add(-time.Minute), 1)
	if err := a.SendEvents(ctx, []usage.Event{ev}); err != nil {
		t.Fatal(err)
	}
	if got := f.events[len(f.events)-1].EventName; got != gpu.Metadata["vbilling_meter"] {
		t.Fatalf("usage went to %s, the card priced %s", got, gpu.Metadata["vbilling_meter"])
	}
	if !strings.HasPrefix(gpu.ID, "price") {
		t.Fatal(gpu.ID)
	}
}
