package records

import (
	"fmt"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// storedSKUs returns the SKUs in the records ledger, by request ID.
func storedSKUs(t *testing.T, r *rig) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := r.records.ScanRecords(t0.Add(-2*time.Hour), t0.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		out[fmt.Sprint(ev.Properties["request_id"])] = ev.SKU
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A pushed record with markup in its SKU or a dimension is refused; the
// same record read from a gateway's own log is stored with a safe SKU,
// because the request was served and its usage must be billed.
func TestRecordValuesRefusedWhenPushedRepairedWhenPulled(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second).Format(time.RFC3339)
	bad := fmt.Sprintf(`{"id":"r-1","tenant":"acme","time":%q,"usage":{"tokens_out":5},"sku":"<b>x</b>","dimensions":{"tier":"a\u0007b"}}`, at)
	res := r.post(t, Native, bad)
	if res.Accepted != 0 || len(res.Rejected) != 1 {
		t.Fatalf("pushed: %+v", res)
	}
	obj := mustParse(t, bad)
	pres, err := r.in.ingestPulled(Native, Native, "", []map[string]any{obj}, nil)
	if err != nil || pres.Accepted != 1 {
		t.Fatalf("pulled: %+v %v", pres, err)
	}
	if sku := storedSKUs(t, r)["r-1"]; sku != "_b_x_/b_" || usage.CheckValue(sku) != nil {
		t.Fatalf("pulled SKU stored as %q", sku)
	}
}
