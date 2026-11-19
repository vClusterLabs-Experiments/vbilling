package metronome

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Metronome receives estimates marked as such: accuracy=estimate on events
// of a metric the catalog marks as an estimate (a PromQL query), and on
// events that say so themselves (a metrics-server sample).
func TestEstimatesCarryAccuracyToMetronome(t *testing.T) {
	const code = "test_queried_tokens"
	if err := usage.RegisterCustom(usage.MetricDef{Code: code, Unit: "tokens", Billable: true, Accuracy: usage.AccuracyEstimate}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.RemoveCustom(code) })
	a := New(nil, &config.Config{}, kv.Memory(), nil)
	ws := now.Add(-time.Minute)
	mk := func(metric string, props map[string]any) usage.Event {
		e := usage.Event{Tenant: "acme", Metric: metric, Quantity: 2, WindowStart: ws, WindowEnd: now, Properties: props}
		e.Finalize()
		return e
	}
	queried := mk(code, nil)
	sampled := mk(usage.MetricCPUCoreHours, map[string]any{"basis": "usage", "estimate": true})
	exact := mk(usage.MetricCPUCoreHours, map[string]any{"basis": "requests"})
	if got := a.ToIngest(&queried).Properties["accuracy"]; got != "estimate" {
		t.Errorf("catalog estimate: accuracy %q", got)
	}
	if got := a.ToIngest(&sampled).Properties["accuracy"]; got != "estimate" {
		t.Errorf("sampled usage: accuracy %q", got)
	}
	if got, set := a.ToIngest(&exact).Properties["accuracy"]; set {
		t.Errorf("exact usage marked %q", got)
	}
}
