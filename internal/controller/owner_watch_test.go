package controller

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform/platformtest"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
)

// syncClock is a clock the resolver's watch goroutine reads while the test
// moves it.
type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) now() time.Time             { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *syncClock) set(t time.Time)            { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (h *harness) at(c *syncClock, t time.Time) { h.now = t; c.set(t) }

// End to end, as the tester ran it: TENANT_SOURCE=platform with the
// Platform watched. Project train's owner changes 15 seconds into a window;
// the watch reports it, the move is dated then, the tenant records follow,
// and the window is committed in two parts: 15 seconds for the old Tenant,
// 45 for the new one.
func TestWatchedMoveSplitsTheWindowItFallsIn(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123"
	s := platformtest.New(t, filepath.Join("..", "platform", "testdata", "platform-4.12.7"), key)
	h := newHarness(t, func(c *config.Config) { c.TenantSource = "platform" })
	h.c.coll = &billedColl{}
	clk := &syncClock{t: h.now}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := platform.New(platform.Options{
		Client:    platform.ClientOptions{URL: s.URL, AccessKey: key, UserAgent: "vbilling/test"},
		Cluster:   "cp-1",
		Refresh:   time.Minute,
		StatePath: filepath.Join(t.TempDir(), "platform-bindings.json"),
		Committed: func() (time.Time, bool) { return h.sp.Watermark(spool.SourceCollector) },
		Now:       clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-train-1"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "train-1", Namespace: "vc-train-1", UID: "uid-sts-train-1",
			Labels: map[string]string{"app": "vcluster"}, CreationTimestamp: metav1.NewTime(t0.Add(-24 * time.Hour))},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}},
	)
	h.c.disc = discovery.NewDiscoverer(kube, nil, nil, "cp-1").WithBinder(r)
	h.c.UseOwners(r)
	r.OnOwnerChange(h.c.ReconcileSoon)
	go r.Watch(ctx)

	start := t0.Truncate(time.Minute) // 10:00
	h.c.Reconcile(ctx)                // 10:00:30: bound to Tenant acme
	h.c.CollectDue(ctx)               // window 09:59-10:00
	for _, res := range []string{platformtest.Projects, platformtest.Tenants, platformtest.Instances} {
		res := res
		waitFor(t, "the watch of "+res, func() bool { return s.Watching(res) == 1 })
	}

	h.at(clk, start.Add(time.Minute+15*time.Second)) // 10:01:15
	s.Edit(platformtest.Projects, "acme-train", func(o map[string]any) {
		platformtest.SetLabel(o, platform.TenantLegacyLabel, "globex")
	})
	train := h.c.Clusters()[0]
	waitFor(t, "the move", func() bool { return len(r.ChangesIn(train, start.Add(time.Minute), start.Add(2*time.Minute))) == 1 })
	h.c.reconcileIfAsked(ctx) // what Run does before its next commit
	if g, _ := h.reg.Get("tenant-globex"); len(g.Clusters) != 1 {
		t.Errorf("the new owner's record does not list the cluster: %+v", g)
	}

	h.at(clk, start.Add(time.Minute+30*time.Second))
	h.c.CollectDue(ctx) // 10:00-10:01
	h.at(clk, start.Add(2*time.Minute+30*time.Second))
	h.c.CollectDue(ctx) // 10:01-10:02, the move's

	if got := seconds(ledgerWindow(t, h, start.Add(time.Minute))); got["tenant-acme"] != 60 || len(got) != 1 {
		t.Errorf("the window before: %v, want 60s for tenant-acme", got)
	}
	evs := ledgerWindow(t, h, start.Add(2*time.Minute))
	checkUnique(t, evs)
	if got := seconds(evs); got["tenant-acme"] != 15 || got["tenant-globex"] != 45 {
		t.Errorf("the window of the move: %v, want 15s for tenant-acme and 45s for tenant-globex", got)
	}
	for _, e := range evs {
		want := map[string]string{"tenant-acme": "uid-tenant-acme", "tenant-globex": "uid-tenant-globex"}[e.Tenant]
		if e.Properties["platform_tenant_uid"] != want {
			t.Errorf("%s's event carries tenant UID %v, want %s", e.Tenant, e.Properties["platform_tenant_uid"], want)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
