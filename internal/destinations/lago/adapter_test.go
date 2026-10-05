package lago

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestToEventCarriesDimensionsForChargeFilters(t *testing.T) {
	ws := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	e := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, SKU: "NVIDIA-H100", Region: "ap-southeast-2",
		WindowStart: ws, WindowEnd: ws.Add(time.Minute),
		Dimensions: map[string]string{usage.DimTenantCluster: "vcluster-a-train", usage.DimCapacityType: usage.CapacitySpot}}
	e.Finalize()
	le := ToEvent(&e)
	if le.TransactionID != e.ID || le.ExternalSubscriptionID != "sub-acme" || le.Code != usage.MetricGPUHours || le.Timestamp != ws.Unix() {
		t.Fatalf("event: %+v", le)
	}
	if le.Properties["gpu_hours"] != 0.133333333 {
		t.Errorf("gpu_hours = %v, want the number 0.133333333", le.Properties["gpu_hours"])
	}
	for k, want := range map[string]string{"sku": "NVIDIA-H100", "region": "ap-southeast-2", "capacity_type": "spot", "vcluster": "vcluster-a-train"} {
		if le.Properties[k] != want {
			t.Errorf("property %s = %v, want %s", k, le.Properties[k], want)
		}
	}
}

func TestDuplicateTransactionIDCountsAsDelivered(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Events []Event `json:"events"`
		}
		json.Unmarshal(body, &req)
		if len(req.Events) == 1 && strings.HasSuffix(req.Events[0].TransactionID, "dup") {
			w.WriteHeader(422)
			io.WriteString(w, `{"status":422,"error":"Unprocessable Entity","code":"validation_errors","error_details":{"transaction_id":["value_already_exist"]}}`)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	a := &Adapter{client: NewClient(srv.URL, "key"), cfg: &config.Config{}}
	ev := usage.Event{ID: "vb1_dup", Tenant: "acme", Metric: usage.MetricCPUCoreHours, Quantity: 1, WindowStart: time.Now().Add(-time.Minute), WindowEnd: time.Now()}
	if err := a.SendEvents(context.Background(), []usage.Event{ev}); err != nil {
		t.Fatalf("duplicate must be treated as delivered: %v", err)
	}
	// A 422 for another reason is a permanent rejection the dispatcher isolates.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		io.WriteString(w, `{"code":"validation_errors","error_details":{"code":["metric_not_found"]}}`)
	}))
	defer srv2.Close()
	b := &Adapter{client: NewClient(srv2.URL, "key"), cfg: &config.Config{}}
	if err := b.SendEvents(context.Background(), []usage.Event{ev}); !destinations.IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if b.Accepts(usage.MetricGPUDowntimeHours) || !b.Accepts(usage.MetricGPUUtilization) {
		t.Fatal("lago filter: downtime is informational; utilization is kept for v0.1 plans")
	}
}
