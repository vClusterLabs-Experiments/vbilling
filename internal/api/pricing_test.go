package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// pricedDest prices on the SKU and the capacity type: everything but spot
// L40S, and on-demand T4 at 0.
type pricedDest struct{ recDest }

func (d *pricedDest) Name() string                    { return "metronome" }
func (d *pricedDest) PricePlan(t usage.Tenant) string { return "list-prices" }
func (d *pricedDest) PriceDims(string) []string       { return []string{usage.DimCapacityType} }
func (d *pricedDest) CheckPrices(_ context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	var out []destinations.PriceCoverage
	for _, s := range series {
		c := destinations.PriceCoverage{Series: s, Status: destinations.PriceCovered, Detail: "rate found"}
		switch {
		case s.SKU == "NVIDIA-L40S" && s.Dims[usage.DimCapacityType] == usage.CapacitySpot:
			c.Status, c.Detail, c.Hint = destinations.PriceMissing, "no rate", `add a rate for product "GPU Hours" (pricing group capacity_type=spot, sku=NVIDIA-L40S)`
		case s.SKU == "NVIDIA-T4":
			c.Status, c.Detail = destinations.PriceZero, "priced at 0"
		}
		out = append(out, c)
	}
	return out, nil
}

// The dashboard lists price coverage one row per series, with the values of
// the series' other pricing keys (dims), its status, hint and note, and the
// destination's warnings: until v0.3.2 it only had a tile with a count.
func TestDashboardListsEachPricingSeriesWithItsPricingKeys(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("dashboard", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(src)
	for _, want := range []string{
		`<table id="pricing">`, `<th>Series</th>`, `<th>Plan</th>`, `<th>Status</th>`, // the table
		`json("/api/v1/pricing")`, `dimsText(s.dims)`, `s.hint`, `s.note`, `s.held_events`, `d.warnings`, `p.note`, // what it reads
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the dashboard lacks %s", want)
		}
	}
	for _, gone := range []string{"combinations", ".missing"} {
		if strings.Contains(page, gone) {
			t.Errorf("the dashboard still reads %q, which GET /api/v1/pricing no longer has", gone)
		}
	}
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
	for _, s := range [][2]string{{"NVIDIA-H100", usage.CapacityOnDemand}, {"NVIDIA-L40S", usage.CapacityOnDemand}, {"NVIDIA-L40S", usage.CapacitySpot}, {"NVIDIA-T4", usage.CapacityOnDemand}} {
		e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: s[0], Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute),
			Dimensions: map[string]string{usage.DimCapacityType: s[1]}}
		e.Finalize()
		evs = append(evs, e)
	}
	svc.Observe(evs, time.Now())

	cfg := &config.Config{Adapters: []string{"metronome"}, APIToken: adminToken}
	srv := httptest.NewServer(New(cfg, sp, disp, reg, nil, nil, telemetry.New(), "test").WithPricing(svc).Handler())
	defer srv.Close()

	body := func(method, base, path string) []byte {
		t.Helper()
		resp := do(t, method, base+path, adminToken, nil)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	get := func(method, path string) pricing.Report {
		t.Helper()
		var rep pricing.Report
		if err := json.Unmarshal(body(method, srv.URL, path), &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}

	rep := get("POST", "/api/v1/pricing/check")
	if rep.Mode != "warn" || rep.OK || rep.Unpriced != 1 || len(rep.Destinations) != 1 || rep.Note != "" {
		t.Fatalf("report: %+v", rep)
	}
	// One row per series: the L40S is priced on demand and unpriced as spot,
	// and the T4's rate of 0 is its own row and count.
	d := rep.Destinations[0]
	if d.Unpriced != 1 || d.Zero != 1 || d.Priced+d.Unpriced+d.Zero+d.Unknown != len(d.Series) {
		t.Fatalf("counts %+v do not count the %d rows", d, len(d.Series))
	}
	status := map[string]string{}
	for _, row := range d.Series {
		if !row.NoUsageYet {
			status[row.SKU+"/"+row.Dims[usage.DimCapacityType]] = row.Status
		}
	}
	want := map[string]string{"NVIDIA-H100/on-demand": "priced", "NVIDIA-L40S/on-demand": "priced", "NVIDIA-L40S/spot": "unpriced", "NVIDIA-T4/on-demand": "zero"}
	for k, v := range want {
		if status[k] != v {
			t.Errorf("series %s is %q, want %q (rows with usage: %v)", k, status[k], v, status)
		}
	}
	if len(status) != len(want) {
		t.Errorf("rows with usage %v, want one per series", status)
	}

	raw := string(body("GET", srv.URL, "/api/v1/pricing?status=unpriced"))
	if !strings.Contains(raw, `"capacity_type": "spot"`) || !strings.Contains(raw, `"dims"`) || strings.Contains(raw, `"combinations"`) || strings.Contains(raw, `"missing"`) {
		t.Fatalf("unpriced rows as JSON: %s", raw)
	}
	rep = get("GET", "/api/v1/pricing?status=unpriced")
	rows := rep.Destinations[0].Series
	if len(rows) != 1 || rows[0].SKU != "NVIDIA-L40S" || rows[0].Dims[usage.DimCapacityType] != usage.CapacitySpot || rows[0].Plan != "list-prices" || rows[0].Hint == "" {
		t.Fatalf("unpriced rows: %+v", rows)
	}
	if rows = get("GET", "/api/v1/pricing?status=zero").Destinations[0].Series; len(rows) != 1 || rows[0].SKU != "NVIDIA-T4" {
		t.Fatalf("zero rows: %+v", rows)
	}
	if rep = get("GET", "/api/v1/pricing?destination=stripe"); len(rep.Destinations) != 0 {
		t.Fatalf("destination filter: %+v", rep)
	}

	// PRICE_CHECK=off: the answer says in words that nothing is checked.
	off := httptest.NewServer(New(cfg, sp, disp, reg, nil, nil, telemetry.New(), "test").Handler())
	defer off.Close()
	for _, call := range [][2]string{{http.MethodGet, "/api/v1/pricing"}, {http.MethodPost, "/api/v1/pricing/check"}} {
		var o pricing.Report
		if err := json.Unmarshal(body(call[0], off.URL, call[1]), &o); err != nil {
			t.Fatal(err)
		}
		if o.Mode != "off" || !o.OK || o.Destinations == nil || len(o.Destinations) != 0 {
			t.Fatalf("off: %+v", o)
		}
		if !strings.Contains(o.Note, "checks no billing platform's prices") || !strings.Contains(o.Note, "delivered and rated at 0") {
			t.Fatalf("%s %s with PRICE_CHECK=off does not say that nothing is checked: %q", call[0], call[1], o.Note)
		}
	}
}
