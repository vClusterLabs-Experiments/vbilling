package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Consumers can tell estimates from exact quantities: an event of a metric
// the catalog marks as an estimate, or one that says so itself, carries
// "accuracy": "estimate" in its properties.
func TestEstimatesCarryAccuracy(t *testing.T) {
	var got []CloudEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch []CloudEvent
		json.Unmarshal(body, &batch)
		got = append(got, batch...)
	}))
	defer srv.Close()
	a := New(&config.Config{WebhookURL: srv.URL})

	ws := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	mk := func(metric string, props map[string]any) usage.Event {
		e := usage.Event{Tenant: "acme", Metric: metric, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute), Properties: props}
		e.Finalize()
		return e
	}
	catalog := mk(usage.MetricGPUUtilization, nil)                                              // an estimate in the catalog
	sampled := mk(usage.MetricCPUCoreHours, map[string]any{"basis": "usage", "estimate": true}) // a sample
	exact := mk(usage.MetricGPUHours, map[string]any{"physical_gpus": 8.0})
	if err := a.SendEvents(context.Background(), []usage.Event{catalog, sampled, exact}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("events %d", len(got))
	}
	accuracy := func(ce CloudEvent) any {
		props, _ := ce.Data.(map[string]any)["properties"].(map[string]any)
		return props["accuracy"]
	}
	if accuracy(got[0]) != "estimate" || accuracy(got[1]) != "estimate" {
		t.Errorf("estimates not marked: %v, %v", accuracy(got[0]), accuracy(got[1]))
	}
	if v := accuracy(got[2]); v != nil {
		t.Errorf("exact quantity marked %v", v)
	}
	if _, set := sampled.Properties["accuracy"]; set {
		t.Error("the ledger's event was modified")
	}
}
