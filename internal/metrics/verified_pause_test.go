package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A pause vBilling only observed (vcluster pause, an operator, sleep mode,
// the tenant scaling its own control plane down) stops the control plane's
// hours, nothing else: nothing confirms the cluster's workloads stopped,
// so they and the nodes they run on keep billing. A pause vBilling verified
// stops capacity as PAUSE_KEEP_BILLING says.
func TestObservedPauseStopsOnlyTheControlPlane(t *testing.T) {
	for _, tc := range []struct {
		by                       string
		nodeH, gpuH, sharedH, cp float64
	}{
		// Paused at ws+30s: half of the window.
		{pause.ByExternal, 1.0 / 60, 8.0 / 60, 1.0 / 60, 0.5 / 60},
		{pause.ByVBilling, 0.5 / 60, 8 * 0.5 / 60, 0.5 / 60, 0.5 / 60},
	} {
		host := []runtime.Object{
			node("shared-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-L4"}, 1, true),
			pod("synced", "shared-1", withGPU("nvidia.com/gpu", 1)), // still running: the control plane went down without deleting it
		}
		c := pausedCollector(t, pause.DefaultKeep, host)
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
		tg := target("gpu", true)
		if _, _, err := c.Collect(context.Background(), []Target{tg}, []Window{{Start: ws.Add(-time.Minute), End: ws, Latest: true}}); err != nil {
			t.Fatal(err)
		}
		tg.Cluster.Paused, tg.Cluster.Ready = true, false
		tg.Paused = []pause.Span{{From: ws.Add(30 * time.Second), By: tc.by}}
		c.UseTenantAPI(fakeTenantAPI{err: errors.New("connection refused")})
		out, _, err := c.Collect(context.Background(), []Target{tg}, latest())
		if err != nil {
			t.Fatal(err)
		}
		evs := out[we]
		for _, m := range []struct {
			name      string
			got, want float64
		}{
			{"private node hours", sum(evs, usage.MetricPrivateNodeHours, ""), tc.nodeH},
			{"private GPU hours", sum(evs, usage.MetricGPUHours, "private_node"), tc.gpuH},
			{"shared GPU hours", sum(evs, usage.MetricGPUHours, "shared"), tc.sharedH},
			{"control plane hours", sum(evs, usage.MetricInstanceHours, ""), tc.cp},
		} {
			if !near(m.got, usage.Round(m.want)) {
				t.Errorf("%s pause: %s = %v, want %v", tc.by, m.name, m.got, usage.Round(m.want))
			}
		}
	}
}

func sum(evs []usage.Event, metric, mode string) float64 {
	if mode == "" {
		got, _ := find(evs, metric)
		return got
	}
	got, _ := find(evs, metric, usage.DimBillingMode, mode)
	return got
}

// OwnNodes tells a pause whether metering bills nodes of a cluster's own:
// unknown until the cluster was read, then as last read or saved.
func TestOwnNodesIsWhatMeteringLastSaw(t *testing.T) {
	c := pausedCollector(t, pause.DefaultKeep, []runtime.Object{node("cp-1", nil, 0, true, machine("4c4c4544-0001"))})
	private, shared := target("gpu", true), target("shared", true)
	if _, known := c.OwnNodes(private.Cluster); known {
		t.Fatal("OwnNodes known before the cluster was read")
	}
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{
		"gpu":    privateTenant(),
		"shared": tenantConn(node("cp-1", nil, 0, true, machine("4c4c4544-0001"))), // only the control plane cluster's node, seen from inside
	}})
	if _, _, err := c.Collect(context.Background(), []Target{private, shared}, latest()); err != nil {
		t.Fatal(err)
	}
	if has, known := c.OwnNodes(private.Cluster); !has || !known {
		t.Fatalf("private cluster: own nodes %t, known %t", has, known)
	}
	if has, known := c.OwnNodes(shared.Cluster); has || !known {
		t.Fatalf("shared cluster: own nodes %t, known %t", has, known)
	}
	// No credentials for a cluster: nothing inside it is metered.
	none := target("other", true)
	if _, _, err := c.Collect(context.Background(), []Target{none}, latest()); err != nil {
		t.Fatal(err)
	}
	if has, known := c.OwnNodes(none.Cluster); has || !known {
		t.Fatalf("cluster without credentials: own nodes %t, known %t", has, known)
	}
	// Freeze reads it again right before a pause.
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"shared": privateTenant()}})
	if err := c.Freeze(context.Background(), shared.Cluster); err != nil {
		t.Fatal(err)
	}
	if has, _ := c.OwnNodes(shared.Cluster); !has {
		t.Fatal("Freeze did not record the node that joined since the last collection")
	}
}
