package records

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A batch an emitter stored with v0.3.0 through a tenant-bound token and
// retries after the upgrade is not stored again: v0.3.1 derives bound
// records' IDs in their own namespace, and recognizes the ID v0.3.0 gave
// the same rows for 48 hours after their original arrival.
func TestBoundPushRetriedAcrossTheUpgradeCountsOnce(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(10 * time.Second)
	ws, we := usage.Window(at, time.Minute)
	v030 := usage.Event{ID: RecordID(Native, "acme", "req-1", "tokens_in"), Tenant: "acme", Metric: "tokens_in", Quantity: 1000, Unit: "tokens",
		WindowStart: ws, WindowEnd: we, Dimensions: map[string]string{}, Properties: map[string]any{"request_id": "req-1", "at": at.Format(time.RFC3339Nano)},
		Source: "records:" + Native, RecordedAt: at}
	v030.Finalize()
	r.clk.set(t0.Add(15 * time.Second))
	if _, err := r.records.AppendBatch("records:"+Native, []usage.Event{v030}); err != nil {
		t.Fatal(err)
	}
	r.close()
	r.clk.set(t0.Add(36 * time.Hour)) // a retry outside the old one-hour memory
	r.open(t)                         // v0.3.1 starts
	body := native("req-1", "acme", at, 1000, 0)
	res, err := r.in.Ingest(Native, Native, "acme", []byte(body))
	if err != nil || res.Accepted != 0 || res.AlreadyStored != 1 {
		t.Fatalf("the retried batch: %+v %v, want it already stored", res, err)
	}
	r.tick(t, t0.Add(36*time.Hour+time.Minute))
	if got := total(r.ledgerEvents(t), "tokens_in"); got != 1000 {
		t.Fatalf("billed %v tokens, want 1000", got)
	}
	// After the retry horizon the memory is gone, and new bound requests
	// are stored as usual.
	r.clk.set(t0.Add(85 * time.Hour))
	if res, err := r.in.Ingest(Native, Native, "acme", []byte(native("req-2", "acme", t0.Add(85*time.Hour-time.Second), 5, 0))); err != nil || res.Accepted != 1 {
		t.Fatalf("a new request: %+v %v", res, err)
	}
	if r.in.before.ids != nil {
		t.Fatal("the IDs stored before the start are kept past the retry horizon")
	}
}
