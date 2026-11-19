package records

import (
	"testing"
	"time"
)

// The SKU gate (the API's per-tenant SKU cap) applies to every stored
// row: rows past the cap are stored under the gate's SKU, never dropped.
func TestSKUGateAppliesToRecords(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	r.in.UseSKUGate(func(tenant, sku string) (string, bool) {
		if sku == "llama" {
			return "capped", true
		}
		return sku, false
	})
	res := r.post(t, Native, native("r-1", "acme", t0.Add(-30*time.Second), 3, 4))
	if res.Accepted != 1 || len(res.Warnings) == 0 {
		t.Fatalf("result: %+v", res)
	}
	if sku := storedSKUs(t, r)["r-1"]; sku != "capped" {
		t.Fatalf("stored SKU %q, want the gate's", sku)
	}
}
