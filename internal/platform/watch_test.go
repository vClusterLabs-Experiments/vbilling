package platform

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform/platformtest"
)

// A live fake Platform (platformtest) serves the 4.12.7 fixtures: Project
// acme-train belongs to Tenant acme and holds instance train-1, which
// points at workload vc-train-1/train-1 on cp-1.

var windowStart = time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)

func newLiveResolver(t *testing.T, s *platformtest.Server, state string, clk *clock) (*Resolver, *health.Registry) {
	t.Helper()
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	h.SetClock(clk.now)
	r, err := New(Options{
		Client:    ClientOptions{URL: s.URL, AccessKey: testKey, UserAgent: "vbilling/test"},
		Cluster:   "cp-1",
		Refresh:   time.Minute,
		StatePath: state,
		Health:    h,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.settle, r.readGap = 200*time.Millisecond, 0
	return r, h
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func watchAll(t *testing.T, r *Resolver, s *platformtest.Server) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Watch(ctx)
	eventually(t, "the watches", func() bool {
		return s.Watching(platformtest.Projects) == 1 && s.Watching(platformtest.Tenants) == 1 && s.Watching(platformtest.Instances) == 1
	})
	return cancel
}

func handTo(s *platformtest.Server, project, tenant string) {
	s.Edit(platformtest.Projects, project, func(o map[string]any) { platformtest.SetLabel(o, TenantLegacyLabel, tenant) })
}

func noted(r *Resolver) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) > 0
}

func billedTo(c discovery.TenantCluster) string { return c.TenantID(discovery.TenantPerPlatform) }

// The tester's case: the Project is handed to another Tenant 15 seconds
// into a window. The watch reports it at once, the Platform is read, and
// the move is dated when the event arrived, not at the read: the window is
// split there. Before, the move was dated at the next read (up to a minute
// later), and the window it fell in stayed with the old Tenant.
func TestWatchDatesAProjectHandedToAnotherTenant(t *testing.T) {
	s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
	clk := newClock(windowStart.Add(10 * time.Second))
	state := filepath.Join(t.TempDir(), "platform-bindings.json")
	r, _ := newLiveResolver(t, s, state, clk)
	changed := make(chan struct{}, 4)
	r.OnOwnerChange(func() { changed <- struct{}{} })
	train := *bind(t, r, workload("vc-train-1", "train-1", old, nil))["train-1"]
	if billedTo(train) != "tenant-acme" {
		t.Fatalf("before: %s", billedTo(train))
	}
	watchAll(t, r, s)
	reads := s.Lists(platformtest.Instances)

	clk.add(5 * time.Second)
	moved := clk.now()
	handTo(s, "acme-train", "globex")
	eventually(t, "the event", func() bool { return noted(r) || len(r.ChangesIn(train, windowStart, windowStart.Add(time.Minute))) > 0 })
	clk.add(20 * time.Second) // the read comes later than the event
	eventually(t, "the move", func() bool { return len(r.ChangesIn(train, windowStart, windowStart.Add(time.Minute))) > 0 })

	if got := r.ChangesIn(train, windowStart, windowStart.Add(time.Minute)); len(got) != 1 || !got[0].Equal(moved) {
		t.Fatalf("changes in the window: %v, want the event's arrival %s", got, moved)
	}
	if s.Lists(platformtest.Instances) <= reads {
		t.Error("the event did not make vBilling read the Platform")
	}
	select {
	case <-changed:
	default:
		t.Error("OnOwnerChange was not called")
	}
	// The window's parts. Discovery has not run since: train still carries
	// Tenant acme, and the part after the move is globex's all the same.
	if c, ok := r.OwnerAt(train, moved); ok || billedTo(c) != "tenant-acme" {
		t.Errorf("part before the move: %s (%v)", billedTo(c), ok)
	}
	after, ok := r.OwnerAt(train, windowStart.Add(time.Minute))
	if !ok || billedTo(after) != "tenant-globex" || after.Platform.TenantUID != "uid-tenant-globex" {
		t.Errorf("part after the move: %s (%v)", billedTo(after), ok)
	}
	// vBilling restarts before the window is committed: the dated history
	// is on disk, and the backfilled window is split the same way.
	again, _ := newLiveResolver(t, s, state, clk)
	if got := again.ChangesIn(train, windowStart, windowStart.Add(time.Minute)); len(got) != 1 || !got[0].Equal(moved) {
		t.Errorf("after a restart: %v, want %s", got, moved)
	}
}

// Without a watch, a move is dated at the read that shows it, as before:
// the Platform lists no watch verb, refuses the access key a watch, or the
// watch breaks.
func TestWithoutAWatchMovesAreDatedAtTheRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*platformtest.Server)
		check func(*testing.T, *Resolver, *platformtest.Server)
	}{
		{"no watch verb", func(s *platformtest.Server) { s.WithoutWatch() }, nil},
		{"watch refused", func(s *platformtest.Server) { s.RefuseWatch(true) }, nil},
		{"the watch breaks", nil, func(t *testing.T, r *Resolver, s *platformtest.Server) {
			watchAll(t, r, s)
			s.RefuseWatch(true)
			eventually(t, "the watch to break", func() bool { return s.Watching(platformtest.Projects) == 0 })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
			if tc.setup != nil {
				tc.setup(s)
			}
			clk := newClock(windowStart.Add(10 * time.Second))
			r, _ := newLiveResolver(t, s, "", clk)
			train := *bind(t, r, workload("vc-train-1", "train-1", old, nil))["train-1"]
			if tc.check != nil {
				tc.check(t, r, s)
			} else {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go r.Watch(ctx)
			}
			clk.add(5 * time.Second)
			handTo(s, "acme-train", "globex")
			time.Sleep(300 * time.Millisecond)
			if got := r.ChangesIn(train, windowStart, windowStart.Add(2*time.Minute)); len(got) != 0 {
				t.Fatalf("a move was recorded without a watch or a read: %v", got)
			}
			clk.add(57 * time.Second) // the next read is due
			seen := clk.now()
			moved := *bind(t, r, train)["train-1"]
			if billedTo(moved) != "tenant-globex" {
				t.Fatalf("after the read: %s", billedTo(moved))
			}
			if got := r.ChangesIn(moved, windowStart, windowStart.Add(2*time.Minute)); len(got) != 1 || !got[0].Equal(seen) {
				t.Errorf("changes: %v, want the read at %s", got, seen)
			}
			if tc.name == "no watch verb" && s.WatchRequests(platformtest.Projects) != 0 {
				t.Errorf("asked for a watch the Platform does not offer")
			}
		})
	}
}

// A move reverted inside one window is two changes: the window has three
// parts, the first and last for the old Tenant.
func TestWatchDatesAMoveAndItsReversal(t *testing.T) {
	s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
	clk := newClock(windowStart.Add(10 * time.Second))
	r, _ := newLiveResolver(t, s, "", clk)
	train := *bind(t, r, workload("vc-train-1", "train-1", old, nil))["train-1"]
	watchAll(t, r, s)
	in := func() []time.Time { return r.ChangesIn(train, windowStart, windowStart.Add(time.Minute)) }

	clk.add(5 * time.Second)
	out := clk.now()
	handTo(s, "acme-train", "globex")
	eventually(t, "the move", func() bool { return len(in()) == 1 })
	clk.add(25 * time.Second)
	back := clk.now()
	handTo(s, "acme-train", "acme")
	eventually(t, "the move back", func() bool { return len(in()) == 2 })

	if got := in(); !got[0].Equal(out) || !got[1].Equal(back) {
		t.Fatalf("changes: %v, want %s and %s", got, out, back)
	}
	for _, p := range []struct {
		end  time.Time
		want string
	}{{out, "tenant-acme"}, {back, "tenant-globex"}, {windowStart.Add(time.Minute), "tenant-acme"}} {
		if c, _ := r.OwnerAt(train, p.end); billedTo(c) != p.want {
			t.Errorf("part ending %s: %s, want %s", p.end.Format("15:04:05"), billedTo(c), p.want)
		}
	}
}

// A move made while vBilling was down is dated at the first read after it
// starts: the windows backfilled before that read stay with the old owner.
func TestMoveWhileDownIsDatedAtTheFirstReadAfterTheStart(t *testing.T) {
	s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
	clk := newClock(windowStart.Add(10 * time.Second))
	state := filepath.Join(t.TempDir(), "platform-bindings.json")
	r, _ := newLiveResolver(t, s, state, clk)
	bind(t, r, workload("vc-train-1", "train-1", old, nil))

	handTo(s, "acme-train", "globex") // nothing watches: vBilling is down
	clk.add(30 * time.Minute)
	started := clk.now()
	again, _ := newLiveResolver(t, s, state, clk)
	train := *bind(t, again, workload("vc-train-1", "train-1", old, nil))["train-1"]
	if billedTo(train) != "tenant-globex" {
		t.Fatalf("after the start: %s", billedTo(train))
	}
	if got := again.ChangesIn(train, started.Truncate(time.Minute), started.Truncate(time.Minute).Add(time.Minute)); len(got) != 1 || !got[0].Equal(started) {
		t.Errorf("changes: %v, want the first read after the start, %s", got, started)
	}
	if c, ok := again.OwnerAt(train, windowStart.Add(10*time.Minute)); !ok || billedTo(c) != "tenant-acme" {
		t.Errorf("a backfilled window before the read: %s (%v), want tenant-acme", billedTo(c), ok)
	}
}

// A Tenant's billing ID changed through its setting: dated at the watch
// event too, and the customer record says when.
func TestWatchDatesABillingIDChange(t *testing.T) {
	s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
	clk := newClock(windowStart.Add(10 * time.Second))
	r, h := newLiveResolver(t, s, "", clk)
	train := *bind(t, r, workload("vc-train-1", "train-1", old, nil))["train-1"]
	watchAll(t, r, s)

	clk.add(5 * time.Second)
	at := clk.now()
	s.Edit(platformtest.Tenants, "acme", func(o map[string]any) {
		platformtest.SetAnnotation(o, discovery.AnnotationPrefix+"tenant", "acme-corp")
	})
	eventually(t, "the change", func() bool { return len(r.ChangesIn(train, windowStart, windowStart.Add(time.Minute))) == 1 })
	c, ok := r.OwnerAt(train, windowStart.Add(time.Minute))
	if !ok || billedTo(c) != "acme-corp" || c.Platform.PreviousTenantID != "tenant-acme" || !c.Platform.TenantIDChanged.Equal(at) {
		t.Fatalf("after the change: %s (was %q, changed %s), want acme-corp since %s", billedTo(c), c.Platform.PreviousTenantID, c.Platform.TenantIDChanged, at)
	}
	if c.PlatformMetadata()["tenant_id_changed_at"] != at.Format(time.RFC3339) {
		t.Errorf("customer record: %v", c.PlatformMetadata())
	}
	if status(h, "billing-id/acme-corp") == "" {
		t.Errorf("billing ID change not reported: %v", h.List())
	}
}

// Changes that decide no owner (an instance's phase, a Project annotation
// vBilling does not read) cause no read.
func TestWatchIgnoresChangesThatDecideNoOwner(t *testing.T) {
	s := platformtest.New(t, filepath.Join("testdata", "platform-4.12.7"), testKey)
	clk := newClock(windowStart.Add(10 * time.Second))
	r, _ := newLiveResolver(t, s, "", clk)
	bind(t, r, workload("vc-train-1", "train-1", old, nil))
	watchAll(t, r, s)
	reads := s.Lists(platformtest.Instances)
	s.Edit(platformtest.Instances, "train-1", func(o map[string]any) { o["status"].(map[string]any)["phase"] = "Sleeping" })
	s.Edit(platformtest.Projects, "acme-train", func(o map[string]any) { platformtest.SetAnnotation(o, "loft.sh/last-sync", "now") })
	time.Sleep(500 * time.Millisecond)
	if noted(r) || s.Lists(platformtest.Instances) != reads {
		t.Errorf("an unrelated change made vBilling read the Platform (%d reads)", s.Lists(platformtest.Instances)-reads)
	}
}
