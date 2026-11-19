package records

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// restartWith replaces a rig's ingester and aggregator with ones using
// another window size, as a restart with another COLLECTION_INTERVAL did
// before v0.3.1 fixed it at 60s.
func (r *rig) restartWith(t *testing.T, window time.Duration) {
	t.Helper()
	o := r.opts
	o.Now = r.clk.now
	o.Window = window
	formats, err := LoadFormats("", true)
	if err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r.in = NewIngester(r.records, formats, o, r.resolve, h, nil)
	r.agg = NewAggregator(r.records, r.ledger, o, h, nil)
}

func (r *rig) tickAt(t *testing.T, at time.Time) {
	t.Helper()
	r.clk.set(at)
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
}

// A request stored before the window size changed and retried after it
// sits in two overlapping windows of different sizes. It is billed once,
// whichever way the size changed and whether the retry is on time or late.
func TestRecordsRetriedAcrossAWindowSizeChangeCountOnce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after time.Duration
		retryAt       time.Duration // after t0
	}{
		{"5m to 60s, retry on time", 5 * time.Minute, time.Minute, 6*time.Minute + 20*time.Second},
		{"5m to 60s, retry late", 5 * time.Minute, time.Minute, 8 * time.Minute},
		{"60s to 5m, retry on time", time.Minute, 5 * time.Minute, 6*time.Minute + 20*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, Options{})
			defer r.close()
			r.restartWith(t, tc.before)
			// The request ran at 10:06:00 and was stored at 10:06:10.
			body := native("req-1", "acme", t0.Add(6*time.Minute), 0, 100)
			r.clk.set(t0.Add(6*time.Minute + 10*time.Second))
			r.post(t, Native, body)
			// vBilling restarts with the other window size before the request's
			// response reached the gateway, and the gateway retries it.
			r.restartWith(t, tc.after)
			r.clk.set(t0.Add(tc.retryAt))
			r.post(t, Native, body)
			for m := 7; m <= 30; m++ {
				r.tickAt(t, t0.Add(time.Duration(m)*time.Minute))
			}
			if got := total(r.ledgerEvents(t), "tokens_out"); got != 100 {
				t.Fatalf("billed %v output tokens for one request of 100", got)
			}
		})
	}
}

// After the window size changes, the windows committed from the old
// watermark realign with the new grid, and records stored with the old
// size are still billed once in their own windows.
func TestRecordsAfterAWindowSizeChange(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	r.restartWith(t, 5*time.Minute)
	// Stored with 5-minute windows: [10:05, 10:10) is still open.
	r.clk.set(t0.Add(7 * time.Minute))
	r.post(t, Native, native("old-bucket", "acme", t0.Add(6*time.Minute), 0, 100))
	r.tickAt(t, t0.Add(5*time.Minute+31*time.Second)) // commits through 10:05
	r.restartWith(t, time.Minute)
	r.clk.set(t0.Add(7*time.Minute + 5*time.Second))
	r.post(t, Native, native("new-bucket", "acme", t0.Add(7*time.Minute), 0, 10))
	for m := 8; m <= 12; m++ {
		r.tickAt(t, t0.Add(time.Duration(m)*time.Minute))
	}
	evs := r.ledgerEvents(t)
	if got := total(evs, "tokens_out"); got != 110 {
		t.Fatalf("billed %v output tokens, want 110 (100 stored with 5-minute windows, 10 with 1-minute windows)", got)
	}
	for _, e := range evs {
		if e.Metric != "tokens_out" {
			continue
		}
		want := map[float64][2]time.Time{100: {t0.Add(5 * time.Minute), t0.Add(10 * time.Minute)}, 10: {t0.Add(7 * time.Minute), t0.Add(8 * time.Minute)}}[e.Quantity]
		if !e.WindowStart.Equal(want[0]) || !e.WindowEnd.Equal(want[1]) {
			t.Errorf("event of %v billed for [%s, %s), want [%s, %s)", e.Quantity, e.WindowStart.Format("15:04"), e.WindowEnd.Format("15:04"), want[0].Format("15:04"), want[1].Format("15:04"))
		}
	}
	if wm, _ := r.ledger.Watermark(SourceBase); !wm.Equal(t0.Add(11 * time.Minute)) {
		t.Errorf("base watermark %s, want 10:11 on the 1-minute grid", wm.Format("15:04:05"))
	}
}
