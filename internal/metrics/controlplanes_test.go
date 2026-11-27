package metrics

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// planeClock drives a ControlPlanes timeline in tests.
type planeClock struct{ at time.Time }

func (p *planeClock) now() time.Time { return p.at }

func instanceHours(t *testing.T, c *Collector, tg Target, windows []Window) map[time.Time]float64 {
	t.Helper()
	out, _, err := c.Collect(context.Background(), []Target{tg}, windows)
	if err != nil {
		t.Fatal(err)
	}
	got := map[time.Time]float64{}
	for end, evs := range out {
		for _, e := range evs {
			if e.Metric == usage.MetricInstanceHours {
				got[end.Add(-time.Minute)] += e.Quantity
			}
		}
	}
	return got
}

// seconds turns billed hours into seconds, to the millisecond (hours are
// rounded to nine decimals).
func seconds(h float64) float64 { return math.Round(h*3600*1000) / 1000 }

// A control plane scaled to zero outside vBilling, ten seconds before a
// window closes, and ready again two and a half minutes later. Discovery
// sees both 30 seconds late at worst; before, the control plane was billed
// by what it saw: 26.76 seconds of a window with no ready replica at all,
// the last 10 seconds of the window before, and 6.7 seconds before it was
// ready again. Watched, it is billed for the time a replica was ready.
func TestControlPlaneHoursFollowTheTimeAReplicaWasReady(t *testing.T) {
	clock := &planeClock{at: ws.Add(-10 * time.Minute)}
	planes, err := NewControlPlanes("", time.Hour, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	cl := discovery.TenantCluster{Name: "own", Namespace: ns, UID: "uid-1", Ready: true, CreatedAt: ws.Add(-48 * time.Hour)}
	key := ns + "/own"
	planes.observe(key, "uid-1", true, cl.CreatedAt, true) // found ready when the watch started
	clock.at = ws.Add(51 * time.Second)                    // 10:00:51: its last replica is gone
	planes.observe(key, "uid-1", false, cl.CreatedAt, false)
	clock.at = ws.Add(3*time.Minute + 34*time.Second) // 10:03:34: ready again
	planes.observe(key, "uid-1", true, cl.CreatedAt, false)

	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", Now: func() time.Time { return ws.Add(5 * time.Minute) }})
	c.UseControlPlanes(planes)
	// What discovery knows, 30 seconds late: a pause it recorded from
	// 10:01:26 to 10:03:26, and a cluster that is ready now.
	tg := Target{Cluster: cl, Tenant: "acme", Paused: []pause.Span{{From: ws.Add(86 * time.Second), To: ws.Add(206 * time.Second), By: pause.ByExternal}}}
	got := instanceHours(t, c, tg, windowsFrom(ws, 5))
	for i, want := range []float64{51, 0, 0, 26, 60} {
		w := ws.Add(time.Duration(i) * time.Minute)
		if s := seconds(got[w]); s != want {
			t.Errorf("window %s billed %v s of control plane, want %v s (the time a replica was ready)", w.Format("15:04"), s, want)
		}
	}
}

// What was not watched is not known: a window from before vBilling first
// saw the control plane is billed by what discovery reads, as before.
func TestControlPlaneHoursBeforeTheWatchFallBackToDiscovery(t *testing.T) {
	clock := &planeClock{at: ws.Add(90 * time.Second)} // first seen at 10:01:30
	planes, _ := NewControlPlanes("", time.Hour, clock.now)
	cl := discovery.TenantCluster{Name: "own", Namespace: ns, UID: "uid-1", Ready: true, CreatedAt: ws.Add(-48 * time.Hour)}
	planes.observe(ns+"/own", "uid-1", true, cl.CreatedAt, true)
	if _, known := planes.Ready(cl, ws, we); known {
		t.Fatal("a window from before the control plane was first seen is not known")
	}
	if _, known := planes.Ready(cl, ws.Add(time.Minute), ws.Add(2*time.Minute)); known {
		t.Fatal("the window it was first seen in is not known in full")
	}
	ready, known := planes.Ready(cl, ws.Add(2*time.Minute), ws.Add(3*time.Minute))
	if !known || len(ready) != 1 || ready[0][1].Sub(ready[0][0]) != time.Minute {
		t.Fatalf("a window after it was first seen: ready %v known %t, want the whole window", ready, known)
	}
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", Now: func() time.Time { return ws.Add(5 * time.Minute) }})
	c.UseControlPlanes(planes)
	got := instanceHours(t, c, Target{Cluster: cl, Tenant: "acme"}, windowsFrom(ws, 3))
	for i := 0; i < 3; i++ {
		if w := ws.Add(time.Duration(i) * time.Minute); seconds(got[w]) != 60 {
			t.Errorf("window %s billed %v s, want 60 (ready as discovery read it)", w.Format("15:04"), seconds(got[w]))
		}
	}
}

// A cluster created under watch is known from its creation: nothing is
// billed while its control plane is still starting.
func TestNewControlPlaneIsBilledFromWhenItIsReady(t *testing.T) {
	created := ws.Add(10 * time.Second)
	clock := &planeClock{at: created.Add(time.Second)}
	planes, _ := NewControlPlanes("", time.Hour, clock.now)
	cl := discovery.TenantCluster{Name: "new", Namespace: ns, UID: "uid-2", Ready: true, CreatedAt: created}
	planes.observe(ns+"/new", "uid-2", false, created, false) // created, not ready yet
	clock.at = ws.Add(40 * time.Second)
	planes.observe(ns+"/new", "uid-2", true, created, false) // ready 30 seconds later
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", Now: func() time.Time { return ws.Add(5 * time.Minute) }})
	c.UseControlPlanes(planes)
	got := instanceHours(t, c, Target{Cluster: cl, Tenant: "acme"}, windowsFrom(ws, 2))
	if s := seconds(got[ws]); s != 20 {
		t.Errorf("first window billed %v s, want 20: from when it was ready, not from its creation", s)
	}
	if s := seconds(got[ws.Add(time.Minute)]); s != 60 {
		t.Errorf("second window billed %v s, want 60", s)
	}
}

// The timeline survives a restart. A control plane whose state is the same
// afterwards is known throughout; one whose state changed while vBilling
// was down is not known for that time; one that is gone was deleted no
// later than when vBilling last watched it.
func TestControlPlaneTimelineAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-planes.json")
	clock := &planeClock{at: ws.Add(-time.Hour)}
	first, err := NewControlPlanes(path, 6*time.Hour, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	created := ws.Add(-48 * time.Hour)
	for _, name := range []string{"same", "changed", "gone"} {
		first.observe(ns+"/"+name, "uid-"+name, true, created, true)
	}
	clock.at = ws // vBilling stops at 10:00:00
	first.flush(true)

	clock.at = ws.Add(5 * time.Minute) // and starts again at 10:05:00
	second, err := NewControlPlanes(path, 6*time.Hour, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	second.observe(ns+"/same", "uid-same", true, created, true)
	second.observe(ns+"/changed", "uid-changed", false, created, true)
	second.missing()

	cluster := func(name string) discovery.TenantCluster {
		return discovery.TenantCluster{Name: name, Namespace: ns, UID: "uid-" + name, CreatedAt: created}
	}
	down := [2]time.Time{ws.Add(2 * time.Minute), ws.Add(3 * time.Minute)} // a window while it was down
	if ready, known := second.Ready(cluster("same"), down[0], down[1]); !known || len(ready) != 1 {
		t.Errorf("unchanged control plane: ready %v known %t, want ready and known", ready, known)
	}
	if _, known := second.Ready(cluster("changed"), down[0], down[1]); known {
		t.Error("a control plane that stopped while vBilling was down: when is not known")
	}
	if ready, known := second.Ready(cluster("changed"), ws.Add(-10*time.Minute), ws.Add(-9*time.Minute)); !known || len(ready) != 1 {
		t.Errorf("before vBilling stopped it was watched: ready %v known %t", ready, known)
	}
	if ready, known := second.Ready(cluster("changed"), ws.Add(6*time.Minute), ws.Add(7*time.Minute)); !known || len(ready) != 0 {
		t.Errorf("after the restart it is watched again: ready %v known %t, want not ready and known", ready, known)
	}
	at, gone := second.Gone(cluster("gone"))
	if !gone || !at.Equal(ws) {
		t.Errorf("deleted while vBilling was down: gone %t at %s, want gone from when vBilling last watched (10:00:00)", gone, at.Format("15:04:05"))
	}
	if _, gone := second.Gone(cluster("same")); gone {
		t.Error("a control plane that still exists is not gone")
	}
	// Another workload under the same name is another cluster.
	other := cluster("same")
	other.UID = "uid-other"
	if _, known := second.Ready(other, down[0], down[1]); known {
		t.Error("a new workload under an old name must not inherit its timeline")
	}
}

// With PAUSE_KEEP_BILLING=control_plane a paused control plane keeps
// billing: its paused time counts although no replica is ready.
func TestKeptControlPlaneBillsWhilePaused(t *testing.T) {
	clock := &planeClock{at: ws.Add(-10 * time.Minute)}
	planes, _ := NewControlPlanes("", time.Hour, clock.now)
	cl := discovery.TenantCluster{Name: "own", Namespace: ns, UID: "uid-1", Paused: true, CreatedAt: ws.Add(-48 * time.Hour)}
	planes.observe(ns+"/own", "uid-1", true, cl.CreatedAt, true)
	clock.at = ws.Add(20 * time.Second)
	planes.observe(ns+"/own", "uid-1", false, cl.CreatedAt, false)                              // scaled down at 10:00:20
	span := []pause.Span{{From: ws.Add(45 * time.Second), By: pause.ByVBilling, Drained: true}} // verified at 10:00:45
	for _, tc := range []struct {
		keep string
		want float64
	}{{pause.DefaultKeep, 20}, {pause.KeepControlPlane, 35}} { // 20 s ready, plus 15 s paused when kept
		keep, _ := pause.ParseKeep(tc.keep)
		c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", PauseKeep: keep, Now: func() time.Time { return ws.Add(5 * time.Minute) }})
		c.UseControlPlanes(planes)
		got := instanceHours(t, c, Target{Cluster: cl, Tenant: "acme", Paused: span}, windowsFrom(ws, 1))
		if s := seconds(got[ws]); s != tc.want {
			t.Errorf("PAUSE_KEEP_BILLING=%s: billed %v s, want %v s", tc.keep, s, tc.want)
		}
	}
}

// A cluster deleted 44 seconds into a window is metered one last time: its
// control plane is billed until the deletion, where the watch saw it, and
// nothing of it after.
func TestDeletedControlPlaneIsBilledUntilItsDeletion(t *testing.T) {
	clock := &planeClock{at: ws.Add(-10 * time.Minute)}
	planes, _ := NewControlPlanes("", time.Hour, clock.now)
	cl := discovery.TenantCluster{Name: "ghost", Namespace: ns, UID: "uid-ghost", Ready: true, CreatedAt: ws.Add(-48 * time.Hour)}
	planes.observe(ns+"/ghost", "uid-ghost", true, cl.CreatedAt, true)
	deletedAt := ws.Add(44 * time.Second)
	clock.at = deletedAt
	planes.deleted(ns+"/ghost", "uid-ghost")
	if at, gone := planes.Gone(cl); !gone || !at.Equal(deletedAt) {
		t.Fatalf("gone %t at %s, want deleted at %s", gone, at.Format("15:04:05"), deletedAt.Format("15:04:05"))
	}
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", Now: func() time.Time { return ws.Add(5 * time.Minute) }})
	c.UseControlPlanes(planes)
	// As the controller meters a deleted cluster: not ready any more, and
	// nothing billed from its deletion on.
	cl.Ready, cl.DeletedAt = false, deletedAt
	tg := Target{Cluster: cl, Tenant: "acme", Excluded: []pause.Span{{From: deletedAt, By: pause.ByExcluded}}}
	got := instanceHours(t, c, tg, windowsFrom(ws, 2))
	if s := seconds(got[ws]); s != 44 {
		t.Errorf("the window it was deleted in billed %v s of control plane, want 44", s)
	}
	if s := seconds(got[ws.Add(time.Minute)]); s != 0 {
		t.Errorf("the window after its deletion billed %v s, want 0", s)
	}
}
