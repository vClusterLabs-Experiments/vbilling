package records

import (
	"testing"
	"time"
)

// A record sent with a token bound to a tenant never shares an ID with the
// gateway's own record of the same request, even when the tenant names the
// same client and the gateway's tenant value equals the tenant ID: the
// tenant could learn its request IDs from its own responses, and billing
// keeps only the first copy of an ID, so a cheaper copy sent first would
// replace the real one.
func TestBoundRecordsNeverTakeTheGatewaysIDs(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(10 * time.Second)
	if _, err := r.in.Ingest(Native, Native, "acme", []byte(native("req-1", "acme", at, 1, 0))); err != nil {
		t.Fatal(err)
	}
	if res, err := r.in.Ingest(Native, Native, "", []byte(native("req-1", "acme", at, 5000, 0))); err != nil || res.UsageRows != 1 {
		t.Fatalf("the gateway's record must be stored next to the bound copy: %+v %v", res, err)
	}
	r.clk.set(t0.Add(2 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	if got := total(r.ledgerEvents(t), "tokens_in"); got != 5001 {
		t.Fatalf("tokens_in billed %v: the gateway's 5000 must be billed whatever the bound copy says", got)
	}
}
