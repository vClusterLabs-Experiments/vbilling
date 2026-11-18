package controller

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// movedOwners: the cluster belonged to Project acme-train (Tenant acme)
// until movedAt, then to Project research.
type movedOwners struct {
	movedAt time.Time
	before  discovery.PlatformBinding
}

func (o movedOwners) OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool) {
	if cl.Platform == nil || end.After(o.movedAt) {
		return cl, false
	}
	cl.Annotations = map[string]string{}
	cl.ApplyPlatform(o.before)
	return cl, true
}

func TestBackfillBillsEachWindowToItsOwnerThen(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TenantSource = "platform"; c.MaxBackfill = 10 * time.Minute })
	ctx := context.Background()
	cl := cluster("vc-mover", "mover", nil)
	cl.Annotations = map[string]string{}
	cl.ApplyPlatform(discovery.PlatformBinding{Instance: "mover", InstanceUID: "uid-new", Project: "research", ProjectUID: "uid-research"})
	h.disc.clusters = []discovery.TenantCluster{cl}
	h.c.Reconcile(ctx)
	h.c.CollectDue(ctx)

	// vBilling was down for 5 minutes; the cluster moved 2 minutes in.
	start := h.now.Truncate(time.Minute)
	h.c.UseOwners(movedOwners{movedAt: start.Add(2 * time.Minute), before: discovery.PlatformBinding{
		Instance: "mover", InstanceUID: "uid-old", Project: "acme-train", ProjectUID: "uid-acme-train", Tenant: "acme", TenantUID: "uid-acme"}})
	h.now = h.now.Add(5 * time.Minute)
	h.c.CollectDue(ctx)

	calls := h.coll.calls[1:]
	if len(calls) != 2 || len(calls[0]) != 2 || len(calls[1]) != 3 {
		t.Fatalf("expected one run for the 2 windows before the move and one for the 3 after, got %+v", calls)
	}
	byWindow := map[time.Time]usage.Event{}
	if err := h.sp.Scan(start.Add(time.Second), h.now, func(ev *usage.Event) error {
		byWindow[ev.WindowEnd] = *ev
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		end := start.Add(time.Duration(i) * time.Minute)
		ev, ok := byWindow[end]
		wantTenant, wantUID := "project-research", "uid-new"
		if i <= 2 {
			wantTenant, wantUID = "tenant-acme", "uid-old"
		}
		if !ok || ev.Tenant != wantTenant || ev.Properties["platform_instance_uid"] != wantUID {
			t.Errorf("window ending %s: tenant %q (want %q), instance uid %v (want %s)", end.Format("15:04"), ev.Tenant, wantTenant, ev.Properties["platform_instance_uid"], wantUID)
		}
	}
	if ev := byWindow[start.Add(time.Minute)]; ev.Properties["platform_tenant_uid"] != "uid-acme" {
		t.Errorf("tenant UID not stamped: %v", ev.Properties)
	}

	// The customer record carries the Platform owner for traceability.
	tenant, _ := h.reg.Get("project-research")
	if tenant.Metadata["platform_account"] != "project" || tenant.Metadata["platform_project_uid"] != "uid-research" {
		t.Errorf("tenant metadata: %v", tenant.Metadata)
	}
}

func TestWithoutMovesOneCollectionRun(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxBackfill = 10 * time.Minute })
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", nil)}
	h.c.Reconcile(ctx)
	h.c.UseOwners(movedOwners{movedAt: t0.Add(time.Hour)}) // unbound clusters have no history
	h.c.CollectDue(ctx)
	h.now = h.now.Add(5 * time.Minute)
	h.c.CollectDue(ctx)
	if len(h.coll.calls) != 2 || len(h.coll.calls[1]) != 5 {
		t.Fatalf("calls: %+v", h.coll.calls)
	}
}

func TestOwnerResolutionKeepsPauses(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TenantSource = "platform" })
	h.c.UseOwners(movedOwners{movedAt: h.now.Add(time.Hour), before: discovery.PlatformBinding{
		Instance: "p", InstanceUID: "uid-p", Project: "acme-train", Tenant: "acme"}})
	cl := cluster("vc-p", "p", nil)
	cl.Annotations = map[string]string{}
	cl.ApplyPlatform(discovery.PlatformBinding{Instance: "p", InstanceUID: "uid-p", Project: "acme-train", Tenant: "acme"})
	cl.Paused = true
	spans := []pause.Span{{From: h.now.Add(-time.Minute), By: "vbilling"}}
	out, _ := h.c.targetsFor([]metrics.Target{{Cluster: cl, Tenant: "tenant-acme", Paused: spans}}, h.now)
	if len(out) != 1 || len(out[0].Paused) != 1 || !out[0].Cluster.Paused {
		t.Fatalf("owner resolution dropped the pause: %+v", out)
	}
}
