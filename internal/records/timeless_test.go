package records

import (
	"fmt"
	"testing"
	"time"
)

func timeless(id, tenant string, out float64) string {
	return fmt.Sprintf(`{"id":%q,"tenant":%q,"usage":{"tokens_out":%v},"sku":"llama"}`, id, tenant, out)
}

// A record without a time is dated when it first arrives. Its retry gets
// that same time, so it lands in the same window and its request ID is a
// duplicate there, even when the retry arrives after the window boundary,
// after the window was committed, or after a restart.
func TestRecordsWithoutATimeRetriedAcrossAWindowBoundaryCountOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retryAt time.Duration
		restart bool
	}{
		{"just after the boundary", time.Minute + 500*time.Millisecond, false},
		{"after the window was committed", 3 * time.Minute, false},
		{"after a restart", 2 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			defer r.close()
			r.clk.set(t0.Add(59*time.Second + 900*time.Millisecond))
			if res := r.post(t, Native, timeless("req-1", "acme", 100)); len(res.Warnings) == 0 {
				t.Fatalf("no warning for a record without a time: %+v", res)
			}
			if tc.retryAt >= 2*time.Minute {
				r.tickAt(t, t0.Add(tc.retryAt-time.Second)) // the first copy's window is committed
			}
			if tc.restart {
				r.close()
				r.open(t)
			}
			r.clk.set(t0.Add(tc.retryAt))
			r.post(t, Native, timeless("req-1", "acme", 100)) // the emitter retries
			for m := 2; m <= 6; m++ {
				r.tickAt(t, t0.Add(time.Duration(m)*time.Minute))
			}
			if got := total(r.ledgerEvents(t), "tokens_out"); got != 100 {
				t.Fatalf("billed %v output tokens for one request of 100", got)
			}
		})
	}
}
