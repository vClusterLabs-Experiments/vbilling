package stripe

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestMeterIdentityPreservesOriginalSplitValues(t *testing.T) {
	a := &Adapter{cfg: &config.Config{StripeSplitMetersBy: []string{"sku", "region"}}}
	seen := map[string]bool{}
	for _, e := range []usage.Event{
		{Metric: "inference_input_tokens", SKU: "org/model"},
		{Metric: "inference_input_tokens", SKU: "org-model"},
		{Metric: "inference_input_tokens", SKU: "ORG/model"},
		{Metric: "inference_input_tokens", SKU: "same"},
		{Metric: "inference_input_tokens", Region: "same"},
	} {
		name := a.MeterName(&e)
		if seen[name] || len(name) > 100 {
			t.Fatalf("colliding or oversized meter %s for %+v", name, e)
		}
		seen[name] = true
	}
}

func TestLegacyMeterRouteSurvivesUpgradeAndRestart(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	a := newTestAdapter(t, f, statePath)
	e := gpuEvent("acme", "org/model", now.Add(-time.Minute), 1)
	legacy := a.legacyMeterName(&e)
	if _, err := a.ensureMeter(context.Background(), e.Metric, legacy, "existing price's meter"); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(filepath.Join(dir, "ledger"), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AppendWindow(spool.SourceCollector, e.WindowEnd, []usage.Event{e}, ""); err != nil {
		t.Fatal(err)
	}
	a.UseLedger(s)
	if err := a.Bootstrap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	other := e
	other.SKU = "org-model"
	if a.MeterName(&e) != legacy || a.MeterName(&other) == legacy {
		t.Fatalf("upgrade routes: existing %s, collider %s", a.MeterName(&e), a.MeterName(&other))
	}
	b := newTestAdapter(t, f, statePath)
	b.UseLedger(s)
	if err := b.Bootstrap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if b.MeterName(&e) != legacy || b.MeterName(&other) != a.MeterName(&other) {
		t.Fatal("persisted routes changed across restart")
	}
}

func TestLongMetricCodesReconcileSeparatelyAfterRestart(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	path := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, path)
	metrics := []string{strings.Repeat("long_metric_", 7) + "a", strings.Repeat("long_metric_", 7) + "b"}
	for i, metric := range metrics {
		if err := usage.RegisterCustom(usage.MetricDef{Code: metric, Unit: "tokens", Billable: true}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { usage.RemoveCustom(metric) })
		e := gpuEvent("acme", "same-model", now.Add(-time.Minute), float64(i+1))
		e.Metric, e.ID = metric, ""
		e.Finalize()
		if err := a.SendEvents(context.Background(), []usage.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	b := newTestAdapter(t, f, path)
	totals, err := b.RecordedTotals(context.Background(), destinations.TotalsQuery{From: now.Add(-time.Hour), To: now, Tenants: []usage.Tenant{{ID: "acme"}}, Metrics: metrics})
	if err != nil || len(totals) != 2 {
		t.Fatalf("long metric totals: %+v %v", totals, err)
	}
	for i, total := range totals {
		if total.Metric != metrics[i] || total.Quantity != float64(i+1) {
			t.Fatalf("metric identities conflated: %+v", totals)
		}
	}
}
