package records

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Third review: upgrade from v0.3.0. A gateway that serves one tenant
// pushes with a token bound to it, and vBilling pulls the same gateway's log
// pinned to that tenant, as usage-sources.html asks. v0.3.0 gave both
// copies RecordID(client, tenant, request, metric). v0.3.1 moves bound
// records into their own namespace ("bound\x00"+client), with nothing that
// recognizes the old IDs: the first pulls after the upgrade re-read their
// lookback (an hour) and store every request v0.3.0 already billed again,
// under new IDs, and the aggregator bills them a second time as late
// records. (Ingested events got IngestedRecently for the same change.)
func TestReview3UpgradeBillsBoundRequestsTwice(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(10 * time.Second)

	// v0.3.0 stored the bound push of req-1 (client vbilling, tenant acme).
	ws, we := usage.Window(at, time.Minute)
	v030 := usage.Event{ID: RecordID(Native, "acme", "req-1", "tokens_in"), Tenant: "acme", Metric: "tokens_in", Quantity: 1000, Unit: "tokens",
		WindowStart: ws, WindowEnd: we, SKU: "llama", Dimensions: map[string]string{},
		Properties: map[string]any{"request_id": "req-1", "at": at.Format(time.RFC3339Nano)}, Source: "records:" + Native, RecordedAt: at}
	v030.Finalize()
	if _, err := r.records.AppendBatch("records:"+Native, []usage.Event{v030}); err != nil {
		t.Fatal(err)
	}
	r.clk.set(t0.Add(2 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	if got := total(r.ledgerEvents(t), "tokens_in"); got != 1000 {
		t.Fatalf("test setup: v0.3.0 billed %v", got)
	}

	// v0.3.1 starts; its pinned pull re-reads the same request.
	obj := map[string]any{"id": "req-1", "tenant": "acme", "time": at.Format(time.RFC3339Nano),
		"usage": map[string]any{"tokens_in": 1000.0}, "sku": "llama"}
	if _, err := r.in.ingestPulled(Native, Native, "acme", []map[string]any{obj}, nil); err != nil {
		t.Fatal(err)
	}
	r.clk.set(t0.Add(4 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	if got := total(r.ledgerEvents(t), "tokens_in"); got != 1000 {
		t.Fatalf("one request of 1000 tokens is billed %v tokens after the upgrade: its pinned pull stored it again under a new ID", got)
	}
}
