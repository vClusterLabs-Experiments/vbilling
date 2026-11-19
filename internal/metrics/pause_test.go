package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func pausedCollector(t *testing.T, keep string, host []runtime.Object) *Collector {
	t.Helper()
	k, err := pause.ParseKeep(keep)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(fake.NewSimpleClientset(host...), nil, Options{
		Region: "ap-southeast-2", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		PauseKeep: k, Now: func() time.Time { return we.Add(2 * time.Second) },
	})
	c.UsePauses(pause.Memory())
	return c
}

func privateTenant() *TenantConn {
	return tenantConn(
		node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100", "node.kubernetes.io/instance-type": "h100x8"}, 8, true),
		boundPVC("ckpt", "100Gi"),
		lbService("serve"),
	)
}

func TestPausedTimeIsNotBilledForCapacity(t *testing.T) {
	// Paused at ws+30s and still paused: half of the window.
	half := []pause.Span{{From: ws.Add(30 * time.Second), By: "vbilling"}}
	for _, tc := range []struct {
		keep                         string
		nodeH, gpuH, storage, lb, cp float64
	}{
		// Default: nodes and the control plane stop, volumes and addresses keep billing.
		{pause.DefaultKeep, 0.5 / 60, 8 * 0.5 / 60, 100.0 / 60, 1.0 / 60, 0.5 / 60},
		{"nodes,storage,load_balancers,control_plane", 1.0 / 60, 8.0 / 60, 100.0 / 60, 1.0 / 60, 1.0 / 60},
		{"none", 0.5 / 60, 8 * 0.5 / 60, 50.0 / 60, 0.5 / 60, 0.5 / 60},
	} {
		c := pausedCollector(t, tc.keep, nil)
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
		tg := target("gpu", true)
		tg.Paused = half
		out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
		if err != nil {
			t.Fatal(err)
		}
		evs := out[we]
		for _, m := range []struct {
			metric string
			want   float64
		}{
			{usage.MetricPrivateNodeHours, tc.nodeH}, {usage.MetricGPUHours, tc.gpuH},
			{usage.MetricStorageGBHours, tc.storage}, {usage.MetricLBHours, tc.lb}, {usage.MetricInstanceHours, tc.cp},
		} {
			if got, _ := find(evs, m.metric); !near(got, usage.Round(m.want)) {
				t.Errorf("keep=%s: %s = %v, want %v", tc.keep, m.metric, got, usage.Round(m.want))
			}
		}
	}
}

func TestPausedClusterIsMeteredFromItsSavedInventory(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, nil)
	tg := target("gpu", false)
	// vBilling saves the inventory right before stopping the control plane.
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
	if err := c.Freeze(context.Background(), tg.Cluster); err != nil {
		t.Fatal(err)
	}
	// Then its API is gone, and it has been paused since before the window.
	h := health.New(nil)
	c.UseHealth(h)
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused"), conns: map[string]*TenantConn{"gpu": nil}})
	tg.Cluster.Paused = true
	tg.Paused = []pause.Span{{From: ws.Add(-time.Hour), By: "vbilling"}}
	out, st, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TenantAPIFailed) != 0 {
		t.Fatalf("a paused cluster was treated as an outage: %v", st.TenantAPIFailed)
	}
	evs := out[we]
	if got, _ := find(evs, usage.MetricStorageGBHours); !near(got, usage.Round(100.0/60)) {
		t.Errorf("storage while paused = %v", got)
	}
	if got, _ := find(evs, usage.MetricLBHours); !near(got, usage.Round(1.0/60)) {
		t.Errorf("load balancer while paused = %v", got)
	}
	for _, m := range []string{usage.MetricPrivateNodeHours, usage.MetricGPUHours, usage.MetricInstanceHours} {
		if got, _ := find(evs, m); got != 0 {
			t.Errorf("%s billed while paused: %v", m, got)
		}
	}
	src, ok := sourceNamed(h, "tenant-api/"+tg.Cluster.ExternalID())
	if !ok || src.Status != health.Paused || len(h.Problems()) != 0 {
		t.Fatalf("health = %+v (a pause is not a problem)", h.List())
	}

	// After it resumes the saved inventory is dropped.
	c.ForgetPaused(tg.Cluster.ExternalID())
	if _, ok := c.pauses.Inventory(tg.Cluster.ExternalID()); ok {
		t.Fatal("inventory kept after resume")
	}
}

func TestPauseMadeElsewhereUsesTheLastInventoryRead(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, nil)
	tg := target("gpu", true)
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
	if _, _, err := c.Collect(context.Background(), []Target{tg}, latest()); err != nil {
		t.Fatal(err)
	}
	// vcluster pause by an operator: vBilling saved nothing, but it read the
	// cluster a window ago.
	c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	tg.Cluster.Paused, tg.Cluster.Ready = true, false
	tg.Paused = []pause.Span{{From: ws, By: "external"}}
	out, st, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil || len(st.TenantAPIFailed) != 0 {
		t.Fatalf("err %v, failed %v", err, st.TenantAPIFailed)
	}
	if got, _ := find(out[we], usage.MetricStorageGBHours); !near(got, usage.Round(100.0/60)) {
		t.Errorf("storage = %v", got)
	}
	if _, ok := c.pauses.Inventory(tg.Cluster.ExternalID()); !ok {
		t.Fatal("the inventory was not saved for the rest of the pause")
	}

	// Never read and paused: an outage, filled when it resumes.
	c2 := pausedCollector(t, pause.DefaultKeep, nil)
	c2.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
	_, st, err = c2.Collect(context.Background(), []Target{tg}, latest())
	if err != nil || len(st.TenantAPIFailed) != 1 {
		t.Fatalf("err %v, failed %v", err, st.TenantAPIFailed)
	}
}

func TestControlPlaneHoursStopAtTheSecondOfThePause(t *testing.T) {
	// Collected after the pause: the control plane is down (not ready) and
	// paused since ws+20s, so it ran for 20s of this window.
	c := pausedCollector(t, pause.DefaultKeep, nil)
	tg := target("gpu", false)
	tg.Cluster.Paused = true
	tg.Paused = []pause.Span{{From: ws.Add(-10 * time.Minute), To: ws.Add(-5 * time.Minute)}, {From: ws.Add(20 * time.Second)}}
	out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricInstanceHours); !near(got, usage.Round(20.0/3600)) {
		t.Fatalf("control plane hours = %v, want 20s", got)
	}
	// A cluster that is not ready for another reason (asleep) and was
	// paused in the past is not billed.
	tg.Cluster.Paused = false
	tg.Paused = tg.Paused[:1]
	out, _, _ = c.Collect(context.Background(), []Target{tg}, latest())
	if got, _ := find(out[we], usage.MetricInstanceHours); got != 0 {
		t.Fatalf("control plane hours while asleep = %v", got)
	}
}

func TestPausedWorkloadsStopBillingAtTheSecondOfThePause(t *testing.T) {
	// Found on a DGX Spark: paused at ws+20s, vLLM's GPU process exited a
	// second later, but its API server ignored SIGTERM and was killed when
	// the 30s grace period ended (ws+50s). The tenant pays until the pause.
	stopping := pod("vllm", "gpu-1", withGPU("nvidia.com/gpu", 1), withRequests("2", "8Gi"), finished(ws.Add(-time.Hour), ws.Add(50*time.Second)))
	stopping.Status.Phase = "Failed"
	host := []runtime.Object{node("gpu-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-GB10"}, 1, true), stopping}
	for _, tc := range []struct {
		keep      string
		gpu, cpus float64
	}{
		{pause.DefaultKeep, 20.0 / 3600, 2 * 20.0 / 3600},
		{"workloads", 50.0 / 3600, 2 * 50.0 / 3600}, // billed until it actually stopped
	} {
		c := pausedCollector(t, tc.keep, host)
		tg := target("gpu", false)
		tg.Cluster.Paused = true
		tg.Paused = []pause.Span{{From: ws.Add(20 * time.Second), By: "vbilling"}}
		out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := find(out[we], usage.MetricGPUHours, "sku", "NVIDIA-GB10"); !near(got, usage.Round(tc.gpu)) {
			t.Errorf("keep=%s: GPU hours = %v, want %v", tc.keep, got, usage.Round(tc.gpu))
		}
		if got, _ := find(out[we], usage.MetricCPUCoreHours); !near(got, usage.Round(tc.cpus)) {
			t.Errorf("keep=%s: CPU core-hours = %v, want %v", tc.keep, got, usage.Round(tc.cpus))
		}
	}

	// After a resume, the new pod bills from its own start; the earlier,
	// closed pause takes nothing from it.
	resumed := pod("vllm-2", "gpu-1", withGPU("nvidia.com/gpu", 1), startedAt(ws.Add(15*time.Second)))
	c := pausedCollector(t, pause.DefaultKeep, []runtime.Object{host[0], resumed})
	tg := target("gpu", true)
	tg.Paused = []pause.Span{{From: ws.Add(-3 * time.Minute), To: ws.Add(10 * time.Second), By: "vbilling"}}
	out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(45.0/3600)) {
		t.Errorf("GPU hours after resume = %v, want 45s", got)
	}
}
