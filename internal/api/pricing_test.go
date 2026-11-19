package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pricedDest prices H100 only.
type pricedDest struct{ recDest }

func (d *pricedDest) Name() string                    { return "metronome" }
func (d *pricedDest) PricePlan(t usage.Tenant) string { return "list-prices" }
func (d *pricedDest) PriceDims(string) []string       { return nil }
func (d *pricedDest) CheckPrices(_ context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	var out []destinations.PriceCoverage
	for _, s := range series {
		c := destinations.PriceCoverage{Series: s, Status: destinations.PriceCovered, Detail: "rate found"}
		if s.SKU == "NVIDIA-L40S" {
			c.Status, c.Detail, c.Hint = destinations.PriceMissing, "no rate", `add a rate for product "GPU Hours" (pricing group sku=NVIDIA-L40S)`
		}
		out = append(out, c)
	}
	return out, nil
}

func TestPricingEndpoint(t *testing.T) {
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	dest := &pricedDest{recDest{recorded: map[string]float64{}, reject: map[string]bool{}}}
	reg := pipeline.NewTenantRegistry()
	disp := pipeline.New(sp, []destinations.Destination{dest}, pipeline.Options{}, nil)
	svc := pricing.New(sp, []destinations.Destination{dest}, reg, nil, nil, nil, pricing.Options{})
	ws := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
	var evs []usage.Event
	for _, sku := range []string{"NVIDIA-H100", "NVIDIA-L40S"} {
		e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: sku, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
		e.Finalize()
		evs = append(evs, e)
	}
	svc.Observe(evs, time.Now())

	cfg := &config.Config{Adapters: []string{"metronome"}, APIToken: adminToken}
	srv := httptest.NewServer(New(cfg, sp, disp, reg, nil, nil, telemetry.New(), "test").WithPricing(svc).Handler())
	defer srv.Close()

	get := func(method, path string) pricing.Report {
		t.Helper()
		resp := do(t, method, srv.URL+path, adminToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
		var rep pricing.Report
		json.NewDecoder(resp.Body).Decode(&rep)
		return rep
	}

	rep := get("POST", "/api/v1/pricing/check")
	if rep.Mode != "warn" || rep.OK || rep.Unpriced != 1 || len(rep.Destinations) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	rep = get("GET", "/api/v1/pricing?status=unpriced")
	rows := rep.Destinations[0].Series
	if len(rows) != 1 || rows[0].SKU != "NVIDIA-L40S" || rows[0].Plan != "list-prices" || rows[0].Hint == "" {
		t.Fatalf("unpriced rows: %+v", rows)
	}
	if rep = get("GET", "/api/v1/pricing?destination=stripe"); len(rep.Destinations) != 0 {
		t.Fatalf("destination filter: %+v", rep)
	}

	off := httptest.NewServer(New(cfg, sp, disp, reg, nil, nil, telemetry.New(), "test").Handler())
	defer off.Close()
	resp := do(t, http.MethodGet, off.URL+"/api/v1/pricing", adminToken, nil)
	var o pricing.Report
	json.NewDecoder(resp.Body).Decode(&o)
	resp.Body.Close()
	if o.Mode != "off" || !o.OK {
		t.Fatalf("off: %+v", o)
	}
}
