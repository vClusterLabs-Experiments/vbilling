package records

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestIndependentCollectorsPreserveBothQuantities(t *testing.T) {
	collect := func(source, request string, q float64) usage.Event {
		r := newRig(t, Options{Region: "same-region", MeteringSource: source})
		defer r.close()
		r.clk.set(t0.Add(40 * time.Second))
		r.post(t, Native, native(request, "acme", t0.Add(10*time.Second), q, 0))
		r.close()
		r.open(t)
		r.tick(t, t0.Add(2*time.Minute))
		for _, e := range r.ledgerEvents(t) {
			if e.Metric == "tokens_in" {
				return e
			}
		}
		t.Fatal("missing aggregate")
		return usage.Event{}
	}
	a := collect("collector-a", "request-a", 100)
	b := collect("collector-b", "request-b", 200)
	if a.ID == b.ID || a.Quantity+b.Quantity != 300 {
		t.Fatalf("independent aggregates collide: %+v %+v", a, b)
	}
	if same := collect("collector-a", "request-a", 100); same.ID != a.ID {
		t.Fatalf("same source and window changed ID: %s != %s", same.ID, a.ID)
	}
}

func TestLegacySourceDimensionsCannotSplitTheSameAggregate(t *testing.T) {
	r := newRig(t, Options{MeteringSource: "real-collector"})
	defer r.close()
	r.clk.set(t0.Add(30 * time.Second))
	for i, claim := range []string{"emitter-a", "emitter-b"} {
		e := usage.Event{ID: claim, Tenant: "acme", Metric: "tokens_in", Quantity: float64(i+1) * 100, Unit: "tokens",
			WindowStart: t0, WindowEnd: t0.Add(time.Minute), Dimensions: map[string]string{usage.DimMeteringSource: claim}}
		e.Finalize()
		if _, err := r.records.AppendBatch("records:"+Native, []usage.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	r.tick(t, t0.Add(2*time.Minute))
	events := r.ledgerEvents(t)
	if len(events) != 1 || total(events, "tokens_in") != 300 {
		t.Fatalf("source claims split the aggregate: %+v", events)
	}
	for _, e := range events {
		if e.Dimensions[usage.DimMeteringSource] != "real-collector" {
			t.Fatal("emitter selected the collector identity")
		}
	}
}
