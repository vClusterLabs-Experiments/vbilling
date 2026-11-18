package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// reviewPromCollector records the billing identity the controller supplies
// for each historical window, without needing a running Prometheus.
type reviewPromCollector struct{}

func (reviewPromCollector) Collect(_ context.Context, targets []metrics.Target, windows []metrics.Window) ([]metrics.PromWindow, error) {
	var out []metrics.PromWindow
	for _, w := range windows {
		pw := metrics.PromWindow{Window: w}
		for _, tg := range targets {
			ev := usage.Event{Tenant: tg.Tenant, Metric: usage.MetricNetworkEgressGB, Quantity: 1,
				WindowStart: w.Start, WindowEnd: w.End, Project: tg.Project,
				Dimensions: map[string]string{usage.DimTenantCluster: tg.Cluster.ExternalID()}}
			ev.Finalize()
			pw.Events = append(pw.Events, ev)
		}
		out = append(out, pw)
	}
	return out, nil
}

func (reviewPromCollector) Commit(results []metrics.PromWindow, commit func(metrics.Window, []usage.Event) error) error {
	for _, r := range results {
		if err := commit(r.Window, r.Events); err != nil {
			return err
		}
	}
	return nil
}

func TestReviewPrometheusBackfillUsesHistoricalOwner(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.TenantSource = "platform"
		c.PrometheusURL = "http://unused.test"
		c.MaxBackfill = 10 * time.Minute
	})
	ctx := context.Background()
	cl := cluster("vc-mover", "mover", nil)
	cl.Annotations = map[string]string{}
	cl.ApplyPlatform(discovery.PlatformBinding{Instance: "mover", InstanceUID: "uid-new", Project: "research", ProjectUID: "uid-research"})
	h.disc.clusters = []discovery.TenantCluster{cl}
	h.c.Reconcile(ctx)
	h.c.UsePrometheus(reviewPromCollector{})
	h.c.CollectPrometheusDue(ctx)

	start := h.now.Truncate(time.Minute)
	h.c.UseOwners(movedOwners{movedAt: start.Add(2 * time.Minute), before: discovery.PlatformBinding{
		Instance: "mover", InstanceUID: "uid-old", Project: "acme-train", ProjectUID: "uid-acme-train", Tenant: "acme", TenantUID: "uid-acme"}})
	h.now = h.now.Add(5 * time.Minute)
	h.c.CollectPrometheusDue(ctx)
	var checked int
	if err := h.sp.Scan(start.Add(time.Second), h.now, func(ev *usage.Event) error {
		wantTenant, wantUID := "project-research", "uid-new"
		if !ev.WindowEnd.After(start.Add(2 * time.Minute)) {
			wantTenant, wantUID = "tenant-acme", "uid-old"
		}
		checked++
		if ev.Tenant != wantTenant || ev.Properties["platform_instance_uid"] != wantUID {
			t.Errorf("window ending %s: billed %q (%v), want historical owner %q (%s)",
				ev.WindowEnd.Format("15:04"), ev.Tenant, ev.Properties["platform_instance_uid"], wantTenant, wantUID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if checked != 5 {
		t.Fatalf("checked %d windows, want 5", checked)
	}
}
