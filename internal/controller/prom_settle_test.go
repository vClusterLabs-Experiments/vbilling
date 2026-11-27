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

// settlingProm is a Prometheus source that says whether a window's samples
// have arrived.
type settlingProm struct {
	recordingProm
	settled bool
	asked   []time.Time
}

func (s *settlingProm) Settled(_ context.Context, _ []metrics.Target, end time.Time) (bool, string) {
	s.asked = append(s.asked, end)
	return s.settled, "vcluster-team-a-train"
}

func promHarness(t *testing.T, watermark time.Time) (*harness, *settlingProm) {
	t.Helper()
	h := newHarness(t, func(c *config.Config) { c.PrometheusURL = "http://unused.test" })
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(context.Background())
	if err := h.sp.AppendWindow(metrics.SourcePrometheus, watermark, nil, ""); err != nil {
		t.Fatal(err)
	}
	prom := &settlingProm{}
	h.c.UsePrometheus(prom)
	return h, prom
}

// The newest Prometheus window waits until its samples have reached
// Prometheus, is looked at again a few seconds later, and is then read as
// the live window. Kubernetes metering never waits for it.
func TestPrometheusWindowWaitsForItsSamples(t *testing.T) {
	base := t0.Truncate(time.Hour)
	h, prom := promHarness(t, base.Add(4*time.Minute))
	ctx := context.Background()

	h.now = base.Add(5*time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	h.c.CollectPrometheusDue(ctx)
	if len(prom.calls) != 0 {
		t.Fatalf("read %v before its samples arrived", spans(prom.calls[0]))
	}
	if wm, _ := h.sp.Watermark(spool.SourceCollector); !wm.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("Kubernetes metering waited for Prometheus: watermark %s", wm.Format("15:04:05"))
	}
	if d := h.c.untilNextWindow(); d > promPoll {
		t.Errorf("next look in %s, want within %s", d, promPoll)
	}

	prom.settled = true
	h.now = base.Add(5*time.Minute + 10*time.Second)
	h.c.CollectPrometheusDue(ctx)
	if len(prom.calls) != 1 || len(prom.calls[0]) != 1 || spans(prom.calls[0])[0] != "10:04:00-10:05:00*" {
		t.Fatalf("read %v, want the window 10:04:00-10:05:00 as the live one", prom.calls)
	}
	if d := h.c.untilNextWindow(); d < 50*time.Second {
		t.Errorf("next look in %s although nothing waits", d)
	}
}

// A window never waits longer than metrics.PromMaxWait: what has not
// arrived by then is billed by a later window.
func TestPrometheusWindowIsReadAfterTheLongestWait(t *testing.T) {
	base := t0.Truncate(time.Hour)
	h, prom := promHarness(t, base.Add(4*time.Minute))
	ctx := context.Background()
	for _, at := range []time.Duration{5 * time.Second, 30 * time.Second, 55 * time.Second} {
		h.now = base.Add(5*time.Minute + at)
		h.c.CollectPrometheusDue(ctx)
		if len(prom.calls) != 0 {
			t.Fatalf("read %s after the window closed, before its samples arrived", at)
		}
	}
	h.now = base.Add(5*time.Minute + metrics.PromMaxWait)
	h.c.CollectPrometheusDue(ctx)
	if len(prom.calls) != 1 || spans(prom.calls[0])[0] != "10:04:00-10:05:00*" {
		t.Fatalf("read %v at the longest wait, want the window 10:04:00-10:05:00, still the live one", prom.calls)
	}
	if len(prom.asked) != 3 {
		t.Errorf("asked %d times, want 3 (not once the wait is over)", len(prom.asked))
	}
}

// After an outage the windows that are past their wait are read at once and
// marked as read late; only the newest one waits for its samples.
func TestPrometheusBacklogIsReadWhileTheNewestWindowWaits(t *testing.T) {
	base := t0.Truncate(time.Hour)
	h, prom := promHarness(t, base.Add(1*time.Minute))
	ctx := context.Background()
	h.now = base.Add(5*time.Minute + 5*time.Second)
	h.c.CollectPrometheusDue(ctx)
	if len(prom.calls) != 1 {
		t.Fatalf("runs: %v", prom.calls)
	}
	got := spans(prom.calls[0])
	if len(got) != 3 || got[0] != "10:01:00-10:02:00" || got[2] != "10:03:00-10:04:00" {
		t.Fatalf("read %v, want the three windows through 10:04:00, none of them live", got)
	}
	if wm, _ := h.sp.Watermark(metrics.SourcePrometheus); !wm.Equal(base.Add(4 * time.Minute)) {
		t.Errorf("watermark %s, want 10:04:00", wm.Format("15:04:05"))
	}
}

// A window that closed before this controller started metering was not
// watched as it happened: it is metered from what the API still shows and
// marked backfilled, like the windows before it, although it is the newest.
func TestWindowThatClosedBeforeTheControllerStartedIsBackfilled(t *testing.T) {
	base := t0.Truncate(time.Hour)
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	ctx := context.Background()
	h.c.Reconcile(ctx)
	if err := h.sp.AppendWindow(spool.SourceCollector, base.Add(2*time.Minute), nil, ""); err != nil {
		t.Fatal(err)
	}
	// Down from 10:02:30; started again at 10:05:10 and first collects at
	// 10:05:44: the windows 10:02 to 10:04 all closed while it was down.
	h.now = base.Add(5*time.Minute + 44*time.Second)
	h.c.liveSince = base.Add(5*time.Minute + 10*time.Second)
	h.c.CollectDue(ctx)
	got := spans(h.coll.calls[0])
	if len(got) != 3 || got[2] != "10:04:00-10:05:00" {
		t.Fatalf("collected %v, want three windows and none live", got)
	}
	// The next window closes while it runs: that one is live.
	h.now = base.Add(6*time.Minute + 5*time.Second)
	h.c.CollectDue(ctx)
	if got := spans(h.coll.calls[1]); len(got) != 1 || got[0] != "10:05:00-10:06:00*" {
		t.Fatalf("collected %v, want the live window 10:05:00-10:06:00", got)
	}
}
