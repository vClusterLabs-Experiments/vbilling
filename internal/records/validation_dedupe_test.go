package records

import (
	"testing"
	"time"
)

func TestRejectedCopyDoesNotReserveRequestIDs(t *testing.T) {
	for name, invalid := range map[string]string{
		"unknown metric":    `{"id":"retry","tenant":"acme","time":"2026-11-18T10:00:10Z","usage":{"tokens_in":100,"unknown_metric":1},"sku":"llama"}`,
		"invalid dimension": `{"id":"retry","tenant":"acme","time":"2026-11-18T10:00:10Z","usage":{"tokens_in":100},"sku":"llama","dimensions":{"invalid-key":"value"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, Options{})
			defer r.close()
			r.clk.set(t0.Add(40 * time.Second))
			valid := native("retry", "acme", t0.Add(10*time.Second), 100, 0)
			res := r.post(t, Native, "["+invalid+","+valid+","+valid+"]")
			if res.Accepted != 2 || res.RejectedCount != 1 || res.UsageRows != 1 || res.DuplicatesInBatch != 1 {
				t.Fatalf("result: %+v", res)
			}
			r.clk.set(t0.Add(2 * time.Minute))
			if err := r.agg.Tick(); err != nil {
				t.Fatal(err)
			}
			if got := total(r.ledgerEvents(t), "tokens_in"); got != 100 {
				t.Fatalf("billed %v tokens, want 100", got)
			}
		})
	}
}
