package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// recordingProm records the windows of every Collect call.
type recordingProm struct {
	reviewPromCollector
	mu    sync.Mutex
	calls [][]metrics.Window
}

func (r *recordingProm) Collect(ctx context.Context, targets []metrics.Target, windows []metrics.Window) ([]metrics.PromWindow, error) {
	r.mu.Lock()
	r.calls = append(r.calls, windows)
	r.mu.Unlock()
	return r.reviewPromCollector.Collect(ctx, targets, windows)
}

func spans(windows []metrics.Window) []string {
	var out []string
	for _, w := range windows {
		s := w.Start.Format("15:04:05") + "-" + w.End.Format("15:04:05")
		if w.Latest {
			s += "*"
		}
		out = append(out, s)
	}
	return out
}

// An install that ran v0.3.0 with another COLLECTION_INTERVAL (45s here)
// left its watermarks off the 60s grid. The next run meters one partial
// window up to the grid and continues on it: the windows tile the time
// exactly, the newest one is the live (Latest) one, and the partial window
// is collected in a run of its own, since Prometheus queries take the
// window length from a run's first window.
func TestOffGridWatermarkRealignsWithAPartialWindow(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.PrometheusURL = "http://unused.test" })
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	base := t0.Truncate(time.Hour)
	off := base.Add(2*time.Minute + 15*time.Second) // a 45s window ended here
	for _, src := range []string{spool.SourceCollector, metrics.SourcePrometheus} {
		if err := h.sp.AppendWindow(src, off, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	prom := &recordingProm{}
	h.c.UsePrometheus(prom)
	h.now = base.Add(5*time.Minute + 10*time.Second)
	h.c.CollectDue(ctx)
	h.c.CollectPrometheusDue(ctx)

	want := [][]string{{"10:02:15-10:03:00"}, {"10:03:00-10:04:00", "10:04:00-10:05:00*"}}
	for name, calls := range map[string][][]metrics.Window{"collector": h.coll.calls, "prometheus": prom.calls} {
		if len(calls) != len(want) {
			t.Fatalf("%s runs: %d (%v), want %v", name, len(calls), calls, want)
		}
		for i := range want {
			if got := spans(calls[i]); len(got) != len(want[i]) || got[0] != want[i][0] || got[len(got)-1] != want[i][len(want[i])-1] {
				t.Errorf("%s run %d: %v, want %v", name, i, got, want[i])
			}
		}
	}
	for _, src := range []string{spool.SourceCollector, metrics.SourcePrometheus} {
		if wm, _ := h.sp.Watermark(src); !wm.Equal(base.Add(5 * time.Minute)) {
			t.Errorf("%s watermark %s, want 10:05:00 on the grid", src, wm.Format("15:04:05"))
		}
	}
}

// A tenant API outage during the partial window is filled with exactly the
// windows that were committed: never a whole window reaching back into one
// that was metered normally.
func TestFillAfterRealignUsesTheCommittedWindows(t *testing.T) {
	h := newHarness(t)
	gc := &gapColl{tenantDown: true}
	h.c.coll = gc
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-p", "private", nil)}
	h.c.Reconcile(ctx)
	base := t0.Truncate(time.Hour)
	if err := h.sp.AppendWindow(spool.SourceCollector, base.Add(2*time.Minute+15*time.Second), nil, ""); err != nil {
		t.Fatal(err)
	}
	h.now = base.Add(4*time.Minute + 10*time.Second)
	h.c.CollectDue(ctx) // 10:02:15-10:03 and 10:03-10:04, the cluster's API down
	gc.tenantDown = false
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx)
	var filled []metrics.Window
	for _, f := range gc.fills { // the partial window is a run of its own
		filled = append(filled, f...)
	}
	if got := spans(filled); len(got) != 2 || got[0] != "10:02:15-10:03:00" || got[1] != "10:03:00-10:04:00" {
		t.Fatalf("filled %v, want the two windows committed while the API was down", got)
	}
}
