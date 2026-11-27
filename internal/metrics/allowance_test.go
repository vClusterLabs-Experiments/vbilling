package metrics

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// claimIn is a bound claim the cluster synced, in a storage class ("" for
// none), of gi GiB, bound since an hour before the window.
func claimIn(name, class string, gi int64) *corev1.PersistentVolumeClaim {
	pvc := boundClaim(name, gi, corev1.ClaimBound, ws.Add(-time.Hour))
	pvc.Spec.StorageClassName = nil
	if class != "" {
		pvc.Spec.StorageClassName = &class
	}
	return pvc
}

// dedicatedNode is a control plane cluster node dedicated to the cluster.
func dedicatedNode(name string, mods ...func(*corev1.Node)) *corev1.Node {
	return node(name, map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, true, mods...)
}

func createdAt(at time.Time) func(*corev1.Node) {
	return func(n *corev1.Node) { n.CreationTimestamp = metav1.NewTime(at) }
}

// meterAllowed meters windows with allowances and returns the events by
// window end. setup attaches a graveyard or cluster APIs.
func meterAllowed(t *testing.T, allowances []Allowance, host []runtime.Object, windows []Window, setup func(*Collector), targets ...Target) map[time.Time][]usage.Event {
	t.Helper()
	opts := Options{Region: "r1", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Allowances: allowances}
	return meterAllowedWith(t, opts, host, windows, setup, targets...)
}

func meterAllowedWith(t *testing.T, opts Options, host []runtime.Object, windows []Window, setup func(*Collector), targets ...Target) map[time.Time][]usage.Event {
	t.Helper()
	end := windows[len(windows)-1].End
	opts.Now = func() time.Time { return end.Add(2 * time.Second) }
	c := NewCollector(fake.NewSimpleClientset(host...), nil, opts)
	if setup != nil {
		setup(c)
	}
	if len(targets) == 0 {
		targets = []Target{target("gpu", true)}
	}
	out, st, err := c.Collect(context.Background(), targets, windows)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.TenantAPIFailed) > 0 {
		t.Fatalf("cluster API read failed: %v", st.TenantAPIFailed)
	}
	return out
}

// gibSec is the GiB-seconds of a metric's events (to the millisecond).
func gibSec(evs []usage.Event, metric string, filters ...string) float64 {
	q, _ := find(evs, metric, filters...)
	return math.Round(q*3600*1000) / 1000
}

func overage(evs []usage.Event, filters ...string) float64 {
	return gibSec(evs, usage.MetricStorageOverageGBHours, filters...)
}

func one(t *testing.T, evs []usage.Event, metric, sku string) usage.Event {
	t.Helper()
	var found []usage.Event
	for _, e := range evs {
		if e.Metric == metric && e.SKU == sku {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events of %s, want 1: %+v", len(found), metric, sku, evs)
	}
	return found[0]
}

func perClusterOf(gi float64, classes ...string) []Allowance {
	return []Allowance{{Metric: usage.MetricStorageGBHours, Included: gi, Per: PerCluster, StorageClasses: classes}}
}

// Each volume's first 32 GiB are free: a 20 GiB volume is all free, a 40
// GiB one bills 8 GiB. Both events say what was applied.
func TestAllowancePerVolume(t *testing.T) {
	al := []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerVolume}}
	evs := meterAllowed(t, al, []runtime.Object{claimIn("small", "fast", 20), claimIn("big", "fast", 40)}, latest(), nil)[we]
	if got := gibSec(evs, usage.MetricStorageGBHours); got != 60*60 {
		t.Errorf("storage = %v GiB-seconds, want 3600: the whole capacity is still recorded", got)
	}
	if got := overage(evs, "sku", "fast"); got != 8*60 {
		t.Errorf("overage = %v GiB-seconds, want 480: 8 GiB of the 40 GiB volume", got)
	}
	o := one(t, evs, usage.MetricStorageOverageGBHours, "fast")
	gross := one(t, evs, usage.MetricStorageGBHours, "fast")
	if o.Tenant != gross.Tenant || o.Project != gross.Project || o.Region != gross.Region || o.ResourceID != gross.ResourceID ||
		o.Dim(usage.DimTenantCluster) != gross.Dim(usage.DimTenantCluster) || o.Unit != "gib-hours" || o.Source != "collector" {
		t.Errorf("the overage event must carry the storage event's identity: %+v vs %+v", o, gross)
	}
	if o.ID == gross.ID || o.ID != usage.DeriveID(&o) {
		t.Errorf("overage ID %s: want its own deterministic ID", o.ID)
	}
	if o.Properties["included_gib"] != 32.0 || o.Properties["allowance_per"] != PerVolume || o.Properties["gross_gib_hours"] != 1.0 {
		t.Errorf("overage properties = %v, want included_gib 32, allowance_per volume, gross_gib_hours 1", o.Properties)
	}
	if gross.Properties["overage_gib_hours"] != usage.Round(8.0/60) || gross.Properties["included_gib"] != 32.0 || gross.Properties["allowance_per"] != PerVolume {
		t.Errorf("storage event properties = %v, want what was applied to it", gross.Properties)
	}
}

// The cluster's bound capacity minus what it includes, at each instant.
func TestAllowancePerCluster(t *testing.T) {
	evs := meterAllowed(t, perClusterOf(32), []runtime.Object{claimIn("a", "fast", 20), claimIn("b", "fast", 20)}, latest(), nil)[we]
	if got := overage(evs, "sku", "fast"); got != 8*60 {
		t.Errorf("overage = %v GiB-seconds, want 480: 40 GiB held, 32 included", got)
	}
}

// The validation's case: storage with 32 GiB included per instance, two
// instances with a 20 GiB volume each, for 10 hours. A free tier in the
// billing platform applies 32 GiB once, to the customer's total, and bills
// 80 GiB-hours; per node nothing is above the allowance. The same volumes
// with 32 GiB per cluster bill 8 GiB for 10 hours.
func TestTheValidationsTwoInstancesExample(t *testing.T) {
	tenHours := []Window{{Start: ws, End: ws.Add(10 * time.Hour), Latest: true}}
	host := []runtime.Object{dedicatedNode("instance-1"), dedicatedNode("instance-2"), claimIn("vol-1", "standard", 20), claimIn("vol-2", "standard", 20)}

	perNode := []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerNode}}
	evs := meterAllowed(t, perNode, host, tenHours, nil)[ws.Add(10*time.Hour)]
	if got, _ := find(evs, usage.MetricPrivateNodeHours); !near(got, 20) {
		t.Fatalf("node-hours = %v, want 2 instances for 10 hours", got)
	}
	if got, _ := find(evs, usage.MetricStorageGBHours); !near(got, 400) {
		t.Errorf("storage = %v GiB-hours, want 400", got)
	}
	if _, n := find(evs, usage.MetricStorageOverageGBHours); n != 0 {
		t.Errorf("per node: %d overage events, want none: 40 GiB held, 64 included", n)
	}
	if gross := one(t, evs, usage.MetricStorageGBHours, "standard"); gross.Properties["overage_gib_hours"] != 0.0 {
		t.Errorf("the storage event must say nothing was above the allowance: %v", gross.Properties)
	}

	evs = meterAllowed(t, perClusterOf(32), host, tenHours, nil)[ws.Add(10*time.Hour)]
	if got, _ := find(evs, usage.MetricStorageOverageGBHours, "sku", "standard"); !near(got, 80) {
		t.Errorf("per cluster: overage = %v GiB-hours, want 80: 8 GiB for 10 hours", got)
	}
}

// A volume resized mid-window: the allowance applies to each capacity for
// the time the volume had it.
func TestAllowanceFollowsAResize(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return ws.Add(30 * time.Second) })
	g.claimChanged(boundClaim("data", 20, corev1.ClaimBound, ws.Add(-time.Hour)), boundClaim("data", 50, corev1.ClaimBound, ws.Add(-time.Hour)))
	host := []runtime.Object{boundClaim("data", 50, corev1.ClaimBound, ws.Add(-time.Hour)), claimIn("small", "fast", 10)}
	withGraveyard := func(c *Collector) { c.UseGraveyard(g) }

	perVolume := []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerVolume}}
	evs := meterAllowed(t, perVolume, host, latest(), withGraveyard)[we]
	if got := gibSec(evs, usage.MetricStorageGBHours); got != 20*30+50*30+10*60 {
		t.Fatalf("storage = %v GiB-seconds", got)
	}
	if got := overage(evs); got != 18*30 {
		t.Errorf("per volume: overage = %v GiB-seconds, want 540: 18 GiB above 32 for the 30 s after the resize", got)
	}
	evs = meterAllowed(t, perClusterOf(32), host, latest(), withGraveyard)[we]
	if got := overage(evs); got != 28*30 {
		t.Errorf("per cluster: overage = %v GiB-seconds, want 840: 30 GiB held, then 60", got)
	}
}

// A volume deleted mid-window counts until the second it was deleted.
func TestAllowanceUntilAVolumeIsDeleted(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return we.Add(time.Second) })
	g.RecordVolume(boundClaim("old", 20, corev1.ClaimBound, ws.Add(-time.Hour)), ws.Add(45*time.Second))
	host := []runtime.Object{boundClaim("data", 30, corev1.ClaimBound, ws.Add(-time.Hour))}
	withGraveyard := func(c *Collector) { c.UseGraveyard(g) }

	evs := meterAllowed(t, perClusterOf(32), host, latest(), withGraveyard)[we]
	if got := gibSec(evs, usage.MetricStorageGBHours); got != 20*45+30*60 {
		t.Fatalf("storage = %v GiB-seconds", got)
	}
	if got := overage(evs); got != 18*45 {
		t.Errorf("per cluster: overage = %v GiB-seconds, want 810: 50 GiB held until 45 s, then 30", got)
	}
	perVolume := []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerVolume}}
	if _, n := find(meterAllowed(t, perVolume, host, latest(), withGraveyard)[we], usage.MetricStorageOverageGBHours); n != 0 {
		t.Error("per volume: neither volume is above 32 GiB")
	}
}

// Per node: 32 GiB for each of the cluster's own nodes billed at that
// instant. One node runs all window, one joins at 20 s and one is deleted
// at 40 s: 2, 3 and 2 nodes against 100 GiB held.
func TestAllowancePerNodeFollowsTheNodesBilled(t *testing.T) {
	g := NewGraveyard(time.Hour, func() time.Time { return we })
	g.RecordNode(node("leaving", nil, 0, true), ws.Add(40*time.Second))
	conn := tenantConn(node("staying", nil, 0, true), node("joining", nil, 0, true, createdAt(ws.Add(20*time.Second))), boundPVC("ckpt", "100Gi"))
	conn.Graveyard = g
	perNode := []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerNode}}
	evs := meterAllowed(t, perNode, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	})[we]
	if got, _ := find(evs, usage.MetricPrivateNodeHours); math.Round(got*3600) != 60+40+40 {
		t.Fatalf("node-seconds = %v, want 140", got*3600)
	}
	if got, want := overage(evs, "sku", "fast-nvme"), float64(36*20+4*20+36*20); got != want {
		t.Errorf("overage = %v GiB-seconds, want %v: 64 GiB included, then 96, then 64", got, want)
	}

	// A node that is not ready is not billed, and includes nothing then.
	down := tenantConn(node("flaky", nil, 0, false, func(n *corev1.Node) {
		n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ws.Add(30 * time.Second))
	}), boundPVC("ckpt", "40Gi"))
	evs = meterAllowed(t, perNode, nil, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": down}})
	})[we]
	if got := overage(evs); got != 8*30+40*30 {
		t.Errorf("overage = %v GiB-seconds, want 1440: 8 GiB above while the node is billed, all 40 once it is down", got)
	}

	// A cluster without nodes of its own includes nothing.
	evs = meterAllowed(t, perNode, []runtime.Object{claimIn("a", "fast", 20)}, latest(), nil)[we]
	if got := overage(evs); got != 20*60 {
		t.Errorf("shared nodes: overage = %v GiB-seconds, want all 1200", got)
	}
}

// Listed classes share one allowance, the first listed taking from it
// first; a class no entry lists has none. Without storageClasses, every
// class has an allowance of its own.
func TestAllowanceStorageClasses(t *testing.T) {
	host := []runtime.Object{claimIn("f", "fast", 20), claimIn("s", "standard", 40), claimIn("h1", "hdd", 15), claimIn("h2", "hdd", 5)}
	for _, tc := range []struct {
		name       string
		allowances []Allowance
		want       map[string]float64 // GiB above the allowance, all window
	}{
		{"listed classes share one", perClusterOf(32, "fast", "standard"), map[string]float64{"fast": 0, "standard": 28, "hdd": 20}},
		{"in the order listed", perClusterOf(32, "standard", "fast"), map[string]float64{"fast": 20, "standard": 8, "hdd": 20}},
		{"each class on its own", perClusterOf(32), map[string]float64{"fast": 0, "standard": 8, "hdd": 0}},
		{"an entry for some classes, one for the rest", []Allowance{
			{Metric: usage.MetricStorageGBHours, Included: 10, Per: PerVolume, StorageClasses: []string{"hdd"}},
			{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerCluster},
		}, map[string]float64{"fast": 0, "standard": 8, "hdd": 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := meterAllowed(t, tc.allowances, host, latest(), nil)[we]
			for class, gi := range tc.want {
				if got := overage(evs, "sku", class); got != gi*60 {
					t.Errorf("%s: overage = %v GiB-seconds, want %v", class, got, gi*60)
				}
				gross := one(t, evs, usage.MetricStorageGBHours, class)
				if want := usage.Round(gi / 60); gross.Properties["overage_gib_hours"] != want {
					t.Errorf("%s: overage_gib_hours = %v, want %v", class, gross.Properties["overage_gib_hours"], want)
				}
			}
		})
	}
	evs := meterAllowed(t, perClusterOf(32, "fast", "standard"), host, latest(), nil)[we]
	if hdd := one(t, evs, usage.MetricStorageOverageGBHours, "hdd"); hdd.Properties["included_gib"] != nil || hdd.Properties["gross_gib_hours"] != usage.Round(20.0/60) {
		t.Errorf("a class without an allowance is all overage, and says no allowance applied: %v", hdd.Properties)
	}
	if std := one(t, evs, usage.MetricStorageOverageGBHours, "standard"); std.Properties["allowance_storage_classes"] != "fast,standard" {
		t.Errorf("the overage of a shared allowance names the classes that share it: %v", std.Properties)
	}
}

// Windows cut the same lifetimes: a volume bound 30 s into the first
// window, resized at the boundary of the next and deleted 30 s into the
// third bills, window by window, what one window over all of them bills.
func TestAllowanceAcrossWindowBoundaries(t *testing.T) {
	windows := []Window{{Start: ws, End: ws.Add(time.Minute)}, {Start: ws.Add(time.Minute), End: ws.Add(2 * time.Minute)},
		{Start: ws.Add(2 * time.Minute), End: ws.Add(3 * time.Minute), Latest: true}}
	whole := []Window{{Start: ws, End: ws.Add(3 * time.Minute), Latest: true}}
	created := ws.Add(-10 * time.Second)
	graveyard := func() *Graveyard {
		at := ws.Add(30 * time.Second)
		g := NewGraveyard(time.Hour, func() time.Time { return at })
		g.claimChanged(boundClaim("data", 20, corev1.ClaimPending, created), boundClaim("data", 20, corev1.ClaimBound, created))
		at = ws.Add(time.Minute)
		g.claimChanged(boundClaim("data", 20, corev1.ClaimBound, created), boundClaim("data", 50, corev1.ClaimBound, created))
		g.RecordVolume(boundClaim("data", 50, corev1.ClaimBound, created), ws.Add(150*time.Second))
		return g
	}
	// A dedicated node that joins 30 s into the second window.
	host := []runtime.Object{dedicatedNode("late", createdAt(ws.Add(90*time.Second)))}
	for _, tc := range []struct {
		name   string
		al     Allowance
		byWind []float64 // GiB-seconds above the allowance in each window
	}{
		{"per volume", Allowance{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerVolume}, []float64{0, 18 * 60, 18 * 30}},
		{"per node", Allowance{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerNode}, []float64{20 * 30, 50*30 + 18*30, 18 * 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graveyard()
			out := meterAllowed(t, []Allowance{tc.al}, host, windows, func(c *Collector) { c.UseGraveyard(g) })
			total := 0.0
			for i, w := range windows {
				got := overage(out[w.End])
				if got != tc.byWind[i] {
					t.Errorf("window %d: overage = %v GiB-seconds, want %v", i+1, got, tc.byWind[i])
				}
				total += got
			}
			g = graveyard()
			if got := overage(meterAllowed(t, []Allowance{tc.al}, host, whole, func(c *Collector) { c.UseGraveyard(g) })[whole[0].End]); got != total {
				t.Errorf("one window over all three: %v GiB-seconds, the three windows: %v", got, total)
			}
		})
	}
}

// While a cluster is paused its volumes keep billing (PAUSE_KEEP_BILLING
// keeps storage by default) and its nodes do not: an allowance per node
// includes nothing then, one per cluster still applies. Where storage stops
// billing too, the allowance covers nothing of the paused time either.
func TestAllowanceWhilePaused(t *testing.T) {
	tg := target("gpu", true)
	tg.Paused = []pause.Span{{From: ws.Add(30 * time.Second), By: "vbilling"}}
	for _, tc := range []struct {
		keep          string
		per           string
		storage, want float64 // GiB-seconds
	}{
		{pause.DefaultKeep, PerNode, 100 * 60, 68*30 + 100*30},
		{pause.DefaultKeep, PerCluster, 100 * 60, 68 * 60},
		{"none", PerNode, 100 * 30, 68 * 30},
		{"none", PerCluster, 100 * 30, 68 * 30},
		{"none", PerVolume, 100 * 30, 68 * 30},
	} {
		keep, err := pause.ParseKeep(tc.keep)
		if err != nil {
			t.Fatal(err)
		}
		opts := Options{Region: "r1", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", PauseKeep: keep,
			Allowances: []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: tc.per}}}
		evs := meterAllowedWith(t, opts, nil, latest(), func(c *Collector) {
			c.UsePauses(pause.Memory())
			c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
		}, tg)[we]
		if got := gibSec(evs, usage.MetricStorageGBHours); got != tc.storage {
			t.Fatalf("keep %s, %s: storage = %v GiB-seconds, want %v", tc.keep, tc.per, got, tc.storage)
		}
		if got := overage(evs); got != tc.want {
			t.Errorf("keep %s, %s: overage = %v GiB-seconds, want %v", tc.keep, tc.per, got, tc.want)
		}
	}
}

// With METER_CONTROL_PLANE the control plane's own volume is billed as
// storage. It is not the tenant's storage, so no allowance covers it.
func TestTheControlPlaneVolumeIsNotCovered(t *testing.T) {
	controlPlane := claimIn("data-gpu-0", "", 5)
	controlPlane.Labels = map[string]string{"app": "vcluster", "release": "gpu"}
	opts := Options{Region: "r1", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", MeterControlPlane: true, Allowances: perClusterOf(32)}
	evs := meterAllowedWith(t, opts, []runtime.Object{claimIn("work", "", 20), controlPlane}, latest(), nil)[we]
	if got := gibSec(evs, usage.MetricStorageGBHours, "sku", "default"); got != 25*60 {
		t.Fatalf("storage = %v GiB-seconds, want 1500", got)
	}
	if got := overage(evs, "sku", "default"); got != 5*60 {
		t.Errorf("overage = %v GiB-seconds, want 300: the control plane's 5 GiB", got)
	}
}

// Without allowances nothing changes: no overage events, nothing added to
// storage events.
func TestNoAllowancesNoOverage(t *testing.T) {
	evs := meterAllowed(t, nil, []runtime.Object{claimIn("big", "fast", 400)}, latest(), nil)[we]
	if _, n := find(evs, usage.MetricStorageOverageGBHours); n != 0 {
		t.Error("an overage event without an allowance")
	}
	if p := one(t, evs, usage.MetricStorageGBHours, "fast").Properties; len(p) != 1 || p["volumes"] != 1 {
		t.Errorf("storage properties = %v, want only volumes", p)
	}
}

// Windows filled after an outage of a cluster's API get their overage, with
// fill IDs like every other event of the fill.
func TestFilledWindowsGetTheirOverage(t *testing.T) {
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Allowances: perClusterOf(32), Now: func() time.Time { return we.Add(time.Hour) }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": privateTenant()}})
	out, _, err := c.CollectTenantAPI(context.Background(), []Target{target("gpu", true)}, latest())
	if err != nil {
		t.Fatal(err)
	}
	o := one(t, out[we], usage.MetricStorageOverageGBHours, "fast-nvme")
	if math.Round(o.Quantity*3600) != 68*60 {
		t.Errorf("filled overage = %v GiB-seconds, want 4080", o.Quantity*3600)
	}
	if o.ID != usage.DeriveIDWith(&o, "fill") {
		t.Errorf("filled overage ID %s, want the fill ID", o.ID)
	}
}

// An allowance is computed once per window from both sides of a cluster's
// metering, so the window's total matches the sum of its parts: here 60
// one-minute windows against one of an hour, with volumes and nodes coming
// and going at odd seconds.
func TestAllowanceSumsOverWindows(t *testing.T) {
	var minutes []Window
	for i := 0; i < 60; i++ {
		minutes = append(minutes, Window{Start: ws.Add(time.Duration(i) * time.Minute), End: ws.Add(time.Duration(i+1) * time.Minute), Latest: i == 59})
	}
	hour := []Window{{Start: ws, End: ws.Add(time.Hour), Latest: true}}
	graveyard := func() *Graveyard {
		g := NewGraveyard(2*time.Hour, func() time.Time { return ws.Add(17*time.Minute + 13*time.Second) })
		g.RecordVolume(claimIn("scratch", "fast", 45), ws.Add(17*time.Minute+13*time.Second))
		g.RecordNode(dedicatedNode("spot-1"), ws.Add(41*time.Minute+7*time.Second))
		return g
	}
	late := claimIn("late", "fast", 25)
	late.CreationTimestamp = metav1.NewTime(ws.Add(23*time.Minute + 31*time.Second))
	host := []runtime.Object{claimIn("base", "fast", 30), late, claimIn("cold", "hdd", 12),
		dedicatedNode("od-1", createdAt(ws.Add(5*time.Minute+5*time.Second)))}
	for _, al := range [][]Allowance{
		{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerNode}},
		perClusterOf(40, "hdd", "fast"),
		{{Metric: usage.MetricStorageGBHours, Included: 28, Per: PerVolume}},
	} {
		g := graveyard()
		byMinute := meterAllowed(t, al, host, minutes, func(c *Collector) { c.UseGraveyard(g) })
		var sum, gross float64
		for _, w := range minutes {
			q, _ := find(byMinute[w.End], usage.MetricStorageOverageGBHours)
			sum += q
			q, _ = find(byMinute[w.End], usage.MetricStorageGBHours)
			gross += q
		}
		g = graveyard()
		byHour := meterAllowed(t, al, host, hour, func(c *Collector) { c.UseGraveyard(g) })[hour[0].End]
		want, _ := find(byHour, usage.MetricStorageOverageGBHours)
		if math.Abs(sum-want) > 1e-6 {
			t.Errorf("%s: 60 windows bill %v GiB-hours above the allowance, one window of the hour %v", al[0].Per, sum, want)
		}
		if g, _ := find(byHour, usage.MetricStorageGBHours); math.Abs(gross-g) > 1e-6 || want <= 0 || want >= g {
			t.Errorf("%s: gross %v (by minute %v), overage %v", al[0].Per, g, gross, want)
		}
	}
}

func TestParseAllowances(t *testing.T) {
	good := `
- metric: vcluster_storage_gb_hours
  included: 32
  per: node
  storageClasses: [fast-ssd, standard]
- {metric: vcluster_storage_gb_hours, included: 10.5, per: volume}
`
	list, err := ParseAllowances([]byte(good), "allowances.yaml")
	if err != nil || len(list) != 2 || list[0].Included != 32 || list[1].Per != PerVolume || strings.Join(list[0].StorageClasses, ",") != "fast-ssd,standard" {
		t.Fatalf("ParseAllowances = %+v, %v", list, err)
	}
	if list, err := ParseAllowances([]byte("[]"), "f"); err != nil || len(list) != 0 {
		t.Fatalf("an empty list: %+v, %v", list, err)
	}
	for _, tc := range []struct{ yaml, want string }{
		{"- {included: 32, per: node}", "f: allowance 1: metric is required: vcluster_storage_gb_hours"},
		{"- {metric: vcluster_gpu_hours, included: 1, per: node}", `f: allowance 1: metric "vcluster_gpu_hours" takes no allowance: only vcluster_storage_gb_hours does`},
		{"- {metric: vcluster_storage_gb_hours, included: 0, per: node}", "f: allowance 1: included must be a number of GiB above 0, not 0"},
		{"- {metric: vcluster_storage_gb_hours, included: -5, per: node}", "f: allowance 1: included must be a number of GiB above 0, not -5"},
		{"- {metric: vcluster_storage_gb_hours, per: node}", "f: allowance 1: included must be a number of GiB above 0, not 0"},
		{"- {metric: vcluster_storage_gb_hours, included: .nan, per: node}", "f: error converting YAML to JSON: json: unsupported value: NaN"},
		{"- {metric: vcluster_storage_gb_hours, included: .inf, per: node}", "f: error converting YAML to JSON: json: unsupported value: +Inf"},
		{"- {metric: vcluster_storage_gb_hours, included: 32Gi, per: node}", "f: error unmarshaling JSON"},
		{"- {metric: vcluster_storage_gb_hours, included: 32}", "f: allowance 1: per is required: node, cluster or volume"},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: instance}", `f: allowance 1: per "instance" must be node, cluster or volume`},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node, storageClasses: []}", "f: allowance 1: storageClasses is empty"},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node, storageClasses: [fast, fast]}", `f: allowance 1: storageClasses lists "fast" twice`},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node, storageClasses: [' ']}", "f: allowance 1: a storage class in storageClasses is empty"},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node, storageClasses: [fast]}\n- {metric: vcluster_storage_gb_hours, included: 1, per: volume, storageClasses: [hdd, fast]}",
			`f: allowance 2: storage class "fast" is in allowance 1 too: a class has one allowance`},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node}\n- {metric: vcluster_storage_gb_hours, included: 1, per: volume}",
			"f: allowances 1 and 2 both apply to every storage class: list the classes of all but one in storageClasses"},
		{"- {metric: vcluster_storage_gb_hours, included: 32, per: node, free: true}", `f: error unmarshaling JSON: while decoding JSON: json: unknown field "free"`},
	} {
		if _, err := ParseAllowances([]byte(tc.yaml), "f"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s:\n  got  %v\n  want %s", tc.yaml, err, tc.want)
		}
	}
	if list, err := LoadAllowances(""); err != nil || list != nil {
		t.Errorf("no file: %v, %v", list, err)
	}
	if _, err := LoadAllowances("/nonexistent/allowances.yaml"); err == nil {
		t.Error("a file that is not there must fail")
	}
}

// A claim of a quantity Kubernetes reports in decimal units still bills its
// exact bytes, and the allowance compares GiB with GiB.
func TestAllowanceComparesGiB(t *testing.T) {
	pvc := claimIn("decimal", "fast", 0)
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40G")} // 37.25 GiB
	evs := meterAllowed(t, []Allowance{{Metric: usage.MetricStorageGBHours, Included: 32, Per: PerVolume}}, []runtime.Object{pvc}, latest(), nil)[we]
	want := (40e9/gib - 32) / 60
	if got, _ := find(evs, usage.MetricStorageOverageGBHours); !near(got, usage.Round(want)) {
		t.Errorf("overage = %v GiB-hours, want %v", got, want)
	}
}
