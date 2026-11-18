package stripe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestLivePriceCard applies a price card to a real Stripe test account when
// VBILLING_LIVE_STRIPE_KEY_FILE names a file with an sk_test_ key. It uses a
// plan name of its own and archives the prices it created.
func TestLivePriceCard(t *testing.T) {
	keyFile := os.Getenv("VBILLING_LIVE_STRIPE_KEY_FILE")
	if keyFile == "" {
		t.Skip("set VBILLING_LIVE_STRIPE_KEY_FILE")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(b))
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("refusing a key that is not a test key")
	}
	plan := fmt.Sprintf("pc-live-%d", time.Now().Unix())
	cfg := &config.Config{StripeSplitMetersBy: []string{"sku"}, BillingCurrency: "usd", DefaultPlanCode: plan}
	c := NewClient("https://api.stripe.com", key, "2026-09-30.endive", 0)
	a := New(c, cfg, kv.Memory())
	a.card = &PriceCard{Currency: "usd", Interval: "month", Plans: map[string]map[string]CardPrice{plan: {
		"vcluster_gpu_hours":      {SKU: map[string]CardAmount{"NVIDIA-A100-SXM4-40GB": "2.20"}, Names: map[string]string{"NVIDIA-A100-SXM4-40GB": "A100 GPU-hour (live test)"}},
		"vcluster_lb_hours":       {Amount: "0.025", Name: "Load balancer hour (live test)"},
		"vcluster_instance_hours": {Amount: "0.60", Per: 1_000_000, Name: "Tiny per-million price (live test)"},
	}}}
	ctx := context.Background()
	mine := func() []Price {
		ps, err := c.ListMeteredPrices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []Price
		for _, p := range ps {
			if p.Metadata[metaPlan] == plan {
				out = append(out, p)
			}
		}
		return out
	}
	t.Cleanup(func() { archiveTestPrices(t, c, mine()) })
	if err := a.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	got := mine()
	if len(got) != 3 {
		t.Fatalf("created %d prices, want 3", len(got))
	}
	for _, p := range got {
		t.Logf("%s %s %s/unit on %s", p.ID, p.Metadata["vbilling_meter"], priceDecimal(p), p.Recurring.Meter)
		switch p.Metadata["vbilling_meter"] {
		case "vcluster_gpu_hours__nvidia_a100_sxm4_40gb":
			if priceDecimal(p) != "220" {
				t.Errorf("GPU price %s", priceDecimal(p))
			}
		case "vcluster_instance_hours":
			if priceDecimal(p) != "0.00006" {
				t.Errorf("per-million price %s", priceDecimal(p))
			}
		}
	}
	// A restart applies the card again: nothing new.
	a2 := New(c, cfg, kv.Memory())
	a2.card = a.card
	if err := a2.Bootstrap(ctx, usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	if n := len(mine()); n != 3 {
		t.Fatalf("second run: %d prices", n)
	}
}

// archiveTestPrices archives prices and their products. A price created
// with product_data is its product's default price, which Stripe refuses to
// archive until the product's default is unset.
func archiveTestPrices(t *testing.T, c *Client, prices []Price) {
	t.Helper()
	ctx := context.Background()
	for _, p := range prices {
		var full struct {
			Product string `json:"product"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/prices/"+p.ID, nil, "", &full); err != nil {
			t.Errorf("cleanup: read %s: %v", p.ID, err)
			continue
		}
		unset := url.Values{}
		unset.Set("default_price", "")
		archive := url.Values{}
		archive.Set("active", "false")
		for _, step := range []struct {
			path string
			form url.Values
		}{{"/v1/products/" + full.Product, unset}, {"/v1/prices/" + p.ID, archive}, {"/v1/products/" + full.Product, archive}} {
			if err := c.do(ctx, http.MethodPost, step.path, step.form, "", nil); err != nil {
				t.Errorf("cleanup %s: %v", step.path, err)
			}
		}
		// Read back: PASS must mean the sandbox is clean.
		var price struct {
			Active bool `json:"active"`
		}
		var product struct {
			Active       bool    `json:"active"`
			DefaultPrice *string `json:"default_price"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/prices/"+p.ID, nil, "", &price); err != nil || price.Active {
			t.Errorf("cleanup: price %s still active (read error %v)", p.ID, err)
		}
		if err := c.do(ctx, http.MethodGet, "/v1/products/"+full.Product, nil, "", &product); err != nil || product.Active || product.DefaultPrice != nil {
			t.Errorf("cleanup: product %s active=%v default_price=%v (read error %v)", full.Product, product.Active, product.DefaultPrice, err)
		}
	}
}

// TestLiveArchivePriceCardLeftovers archives prices of earlier live runs
// (plans named pc-live-*) when VBILLING_LIVE_STRIPE_CLEANUP=1.
func TestLiveArchivePriceCardLeftovers(t *testing.T) {
	keyFile := os.Getenv("VBILLING_LIVE_STRIPE_KEY_FILE")
	if keyFile == "" || os.Getenv("VBILLING_LIVE_STRIPE_CLEANUP") != "1" {
		t.Skip("set VBILLING_LIVE_STRIPE_KEY_FILE and VBILLING_LIVE_STRIPE_CLEANUP=1")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(b))
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("refusing a key that is not a test key")
	}
	c := NewClient("https://api.stripe.com", key, "2026-09-30.endive", 0)
	ps, err := c.ListMeteredPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var old []Price
	for _, p := range ps {
		if strings.HasPrefix(p.Metadata[metaPlan], "pc-live-") && p.Metadata["vbilling_price_card"] == "true" {
			old = append(old, p)
		}
	}
	archiveTestPrices(t, c, old)
	t.Logf("archived %d leftover price(s)", len(old))
}
