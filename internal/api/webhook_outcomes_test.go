package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// What became of every billing webhook shows in /metrics as
// vbilling_webhooks_total, and a payment that did not unlock its tenant
// shows as a source (GET /api/v1/sources, and the status is not healthy)
// until the hold is lifted. In the live validation of v0.3.2 such a payment
// left a log line and nothing an operator could alert on.
func TestWebhookOutcomesShowInMetricsAndSources(t *testing.T) {
	e := setup(t)
	sources := health.New(nil)
	sources.SetLogger(func(string, ...any) {})
	e.enf.UseHealth(sources)
	e.server.WithHealth(sources)
	post := func(body []byte, signature string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", e.srv.URL+"/webhooks/stripe", bytes.NewReader(body))
		req.Header.Set("Stripe-Signature", signature)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	signed := func(body []byte) string { return stripe.SignatureHeader(body, "whsec_api", now) }
	metric := func(result string) string {
		return fmt.Sprintf(`vbilling_webhooks_total{result=%q,source="stripe"}`, result)
	}

	// Every series is there before the first webhook.
	if _, body := e.get(t, "/metrics"); !strings.Contains(body, metric("applied")+" 0") || !strings.Contains(body, metric("unsettled")+" 0") {
		t.Fatalf("no vbilling_webhooks_total series before the first webhook:\n%s", body)
	}

	failed := []byte(fmt.Sprintf(`{"id":"evt_failed","type":"invoice.payment_failed","created":%d,"data":{"object":{"customer":"cus_acme","attempt_count":1}}}`, now.Unix()-60))
	if code := post(failed, signed(failed)); code != 200 {
		t.Fatalf("a failed payment: %d", code)
	}
	if code := post(failed, signed(failed)); code != 200 {
		t.Fatalf("its redelivery: %d", code)
	}
	if code := post(failed, "t=1,v1=bad"); code != 400 {
		t.Fatalf("a bad signature: %d", code)
	}
	// The payment arrives, and nothing confirms that nothing is owed (this
	// Stripe cannot be read again): it is applied, and the hold stays.
	paid := []byte(fmt.Sprintf(`{"id":"evt_paid","type":"invoice.paid","created":%d,"data":{"object":{"object":"invoice","id":"in_1","customer":"cus_acme"}}}`, now.Unix()))
	if code := post(paid, signed(paid)); code != 200 {
		t.Fatalf("a payment that lifts nothing: %d, want 200 (it is applied)", code)
	}
	if st := e.enf.StateOf("acme"); st.State != enforcement.Delinquent {
		t.Fatalf("test setup: state %s, want the hold kept", st.State)
	}
	_, body := e.get(t, "/metrics")
	for _, want := range []string{metric("applied") + " 1", metric("duplicate") + " 1", metric("invalid") + " 1", metric("refused") + " 1", metric("error") + " 0"} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics lacks %s:\n%s", want, body)
		}
	}

	code, body := e.get(t, "/api/v1/sources?kind=billing-unlock")
	if code != 200 || !strings.Contains(body, `"name": "billing-unlock/acme"`) || !strings.Contains(body, `"status": "failing"`) || !strings.Contains(body, "invoice.paid") {
		t.Fatalf("sources: %d %s", code, body)
	}
	if _, body := e.get(t, "/api/v1/status"); !strings.Contains(body, `"healthy": false`) || !strings.Contains(body, "billing-unlock/acme") {
		t.Fatalf("status with a payment that did not unlock: %s", body)
	}

	// An operator lifts the hold: the source is ok again.
	resp := do(t, http.MethodPut, e.srv.URL+"/api/v1/billing-states/acme", adminToken, strings.NewReader(`{"state":"active","hold":"billing","reason":"paid by wire"}`))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("lift the hold: %d", resp.StatusCode)
	}
	if _, body := e.get(t, "/api/v1/status"); !strings.Contains(body, `"healthy": true`) {
		t.Fatalf("status after the hold was lifted: %s", body)
	}
}
