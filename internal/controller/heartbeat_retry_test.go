package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

func TestHeartbeatWaitRetriesBeforeNextBoundary(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = append(h.disc.clusters, cluster("a", "gpu", nil))
	h.c.Reconcile(context.Background())
	h.coll.err = &metrics.HeartbeatPending{Nodes: []string{"spot"}, Until: h.now.Add(30 * time.Second)}
	h.c.CollectDue(context.Background())
	if _, committed := h.sp.Watermark(spool.SourceCollector); committed {
		t.Fatal("uncertain window reached the ledger")
	}
	if delay := h.c.untilNextWindow(); delay != 5*time.Second {
		t.Fatalf("retry delay %s, want 5s", delay)
	}
	h.coll.err = nil
	h.now = h.now.Add(5 * time.Second)
	h.c.CollectDue(context.Background())
	if _, committed := h.sp.Watermark(spool.SourceCollector); !committed {
		t.Fatal("resolved heartbeat did not release the window")
	}
}
