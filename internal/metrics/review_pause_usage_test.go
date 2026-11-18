package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestReviewPausePreservesPrivateWorkloadUsageBeforePause(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, nil)
	c.opts.PrivateNodeBilling = "usage"
	tg := target("gpu", true)
	conn := tenantConn(
		node("private-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 1, true),
		pod("infer", "private-gpu", withGPU("nvidia.com/gpu", 1), withRequests("2", "8Gi")),
	)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	if err := c.Freeze(context.Background(), tg.Cluster); err != nil {
		t.Fatal(err)
	}
	// The first 30 seconds are billable. The API goes away after the
	// snapshot and before the minute closes, as a normal billing pause does.
	tg.Cluster.Paused, tg.Cluster.Ready = true, false
	tg.Paused = []pause.Span{{From: ws.Add(30 * time.Second), By: "vbilling"}}
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	out, st, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil || len(st.TenantAPIFailed) != 0 {
		t.Fatalf("collect failed: %v, tenant failures: %v", err, st.TenantAPIFailed)
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(30.0/3600)) {
		t.Errorf("GPU hours = %v, want 30 seconds of usage before the pause", got)
	}
	if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(2*30.0/3600)) {
		t.Errorf("CPU core-hours = %v, want 2 cores for 30 seconds before the pause", got)
	}
}

// A window that starts after the pause bills none of the saved pods.
func TestPausedWindowBillsNoSavedPods(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, nil)
	c.opts.PrivateNodeBilling = "usage"
	tg := target("gpu", true)
	conn := tenantConn(
		node("private-gpu", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 1, true),
		pod("infer", "private-gpu", withGPU("nvidia.com/gpu", 1), withRequests("2", "8Gi")),
	)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	if err := c.Freeze(context.Background(), tg.Cluster); err != nil {
		t.Fatal(err)
	}
	tg.Cluster.Paused, tg.Cluster.Ready = true, false
	tg.Paused = []pause.Span{{From: ws.Add(-time.Minute), By: "vbilling"}}
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{usage.MetricGPUHours, usage.MetricCPUCoreHours, usage.MetricMemoryGBHours} {
		if got, n := find(out[we], m); n != 0 || got != 0 {
			t.Errorf("%s = %v (%d events) in a window after the pause, want none", m, got, n)
		}
	}
}
