package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// A long outage is metered in runs of at most c.batch windows, oldest
// first, each run committed before the next is read, and the next run is
// due a second later instead of at the next window boundary. Every window
// is metered exactly once and only the newest can be live.
func TestLongBackfillIsMeteredInBatches(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxBackfill = time.Hour })
	h.c.batch = 4
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	h.c.liveSince = h.now // as Run sets it

	h.c.CollectDue(ctx) // first run ever: the last closed window only
	first, _ := h.sp.Watermark(spool.SourceCollector)

	h.now = h.now.Add(10 * time.Minute) // vBilling stopped metering for 10 minutes
	latest := h.c.latestDue()
	var got []time.Time
	for run := 0; run < 10; run++ {
		before := len(h.coll.calls)
		h.c.CollectDue(ctx)
		if len(h.coll.calls) == before {
			break
		}
		call := h.coll.calls[len(h.coll.calls)-1]
		if len(call) > 4 {
			t.Fatalf("run %d read %d windows, want at most 4", run, len(call))
		}
		for i, w := range call {
			if w.Latest && !(i == len(call)-1 && w.End.Equal(latest)) {
				t.Fatalf("run %d: window %s is live, but only the newest window can be", run, w.End)
			}
			got = append(got, w.End)
		}
		wm, _ := h.sp.Watermark(spool.SourceCollector)
		if !wm.Equal(call[len(call)-1].End) {
			t.Fatalf("run %d: watermark %s, want the end of the run's last window %s", run, wm, call[len(call)-1].End)
		}
		h.c.mu.Lock()
		retry := h.c.collectionRetry
		h.c.mu.Unlock()
		if more := !wm.Equal(latest); more {
			if retry.IsZero() || retry.Sub(h.now) > time.Second {
				t.Fatalf("run %d: windows are left but the next run is not due within a second (retry %v)", run, retry)
			}
		} else if !retry.IsZero() {
			t.Fatalf("run %d: the backfill is done but a retry is still set for %s", run, retry)
		}
	}
	if len(got) != 10 {
		t.Fatalf("metered %d windows, want the 10 missed ones: %v", len(got), got)
	}
	for i, end := range got {
		if want := first.Add(time.Duration(i+1) * time.Minute); !end.Equal(want) {
			t.Fatalf("window %d ends %s, want %s (windows in order, none twice, none skipped)", i, end, want)
		}
	}
}

// Prometheus windows after a long outage are read in the same batches: the
// oldest first, each batch committed before the next, the next a second
// later, and the newest window still waits for its samples.
func TestLongPrometheusBackfillIsReadInBatches(t *testing.T) {
	base := t0.Truncate(time.Hour)
	h, prom := promHarness(t, base.Add(1*time.Minute))
	h.c.batch = 3
	ctx := context.Background()
	h.now = base.Add(9*time.Minute + 5*time.Second) // windows 10:01 to 10:09 are due; 10:08-10:09 is the newest

	h.c.CollectPrometheusDue(ctx)
	if len(prom.calls) != 1 || len(prom.calls[0]) != 3 {
		t.Fatalf("first run read %v, want the 3 oldest windows", prom.calls)
	}
	if got := spans(prom.calls[0]); got[0] != "10:01:00-10:02:00" || got[2] != "10:03:00-10:04:00" {
		t.Fatalf("first run read %v, want 10:01 to 10:04", got)
	}
	if len(prom.asked) != 0 {
		t.Fatalf("a backfill batch waited for the newest window's samples (%v)", prom.asked)
	}
	if d := h.c.untilNextWindow(); d > time.Second {
		t.Fatalf("next batch in %s, want within a second", d)
	}

	h.c.CollectPrometheusDue(ctx)
	if got := spans(prom.calls[1]); len(got) != 3 || got[0] != "10:04:00-10:05:00" || got[2] != "10:06:00-10:07:00" {
		t.Fatalf("second run read %v, want 10:04 to 10:07", got)
	}

	// The last batch holds the newest window: it waits for its samples, so
	// only the window before it is read now.
	h.c.CollectPrometheusDue(ctx)
	if got := spans(prom.calls[2]); len(got) != 1 || got[0] != "10:07:00-10:08:00" {
		t.Fatalf("third run read %v, want 10:07 to 10:08 while the newest window waits", got)
	}
	if wm, _ := h.sp.Watermark(metrics.SourcePrometheus); !wm.Equal(base.Add(8 * time.Minute)) {
		t.Fatalf("watermark %s, want 10:08:00", wm.Format("15:04:05"))
	}
	if d := h.c.untilNextWindow(); d > promPoll {
		t.Fatalf("the newest window is looked at again in %s, want within %s", d, promPoll)
	}
}

// The limit itself is 24 hours: an outage of a day is metered in full.
func TestBackfillReachesADay(t *testing.T) {
	if got := config.Load().MaxBackfill; got != 24*time.Hour {
		t.Fatalf("MAX_BACKFILL is %s, want 24h", got)
	}
}
