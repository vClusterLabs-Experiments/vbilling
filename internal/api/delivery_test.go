package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
)

type receiptDestination struct {
	*recDest
	outcome string
}

func (d *receiptDestination) DeliveryReceipt(id string) (destinations.DeliveryReceipt, bool, error) {
	return destinations.DeliveryReceipt{Status: "pending"}, id == "event-1", nil
}

func (d *receiptDestination) ResolveDelivery(id, outcome, reason string) error {
	if id != "event-1" || outcome != "delivered" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("invalid reconciliation")
	}
	d.outcome = outcome
	return nil
}

func TestDeliveryRecoveryRequiresOperatorAuthentication(t *testing.T) {
	e := setup(t)
	dest := &receiptDestination{recDest: e.dest}
	e.server.disp = pipeline.New(e.sp, []destinations.Destination{dest}, pipeline.Options{}, nil)
	url := e.srv.URL + "/api/v1/destinations/fakebill/delivery/event-1"
	valid := `{"outcome":"delivered","reason":"provider reconciliation confirms acceptance"}`
	for _, token := range []string{"", "ingest-secret"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			r := do(t, method, url, token, strings.NewReader(valid))
			r.Body.Close()
			if r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s with token %q returned %d", method, token, r.StatusCode)
			}
		}
	}
	if dest.outcome != "" {
		t.Fatal("unauthorized request resolved a receipt")
	}
	for _, body := range []string{`{"outcome":"delivered"}`, valid + `{}`, `{"outcome":"delivered","reason":"ok","unknown":true}`} {
		r := do(t, http.MethodPut, url, adminToken, strings.NewReader(body))
		r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid recovery body returned %d", r.StatusCode)
		}
	}
	r := do(t, http.MethodPut, url, adminToken, strings.NewReader(valid))
	r.Body.Close()
	if r.StatusCode != http.StatusOK || dest.outcome != "delivered" {
		t.Fatalf("operator recovery: status %d, outcome %q", r.StatusCode, dest.outcome)
	}
}
