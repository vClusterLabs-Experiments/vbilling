package enforcement

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

var podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

// testClock is a clock tests move by hand; pause workers read it in the
// background.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock { return &testClock{now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)} }

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// privateCluster is a cluster on the control plane cluster whose workloads
// run on one node of its own, gpu-1.
func privateCluster(pods ...runtime.Object) (discovery.TenantCluster, *fake.Clientset, *fake.Clientset) {
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(append([]runtime.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}}}, pods...)...)
	return cl, host, inside
}

func clientFor(kube kubernetes.Interface) func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) {
	return func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return kube, nil }
}

func onlyStatus(t *testing.T, e *Enforcer) PauseStatus {
	t.Helper()
	st := e.Pauses()
	if len(st) != 1 {
		t.Fatalf("pause statuses = %+v, want one", st)
	}
	return st[0]
}

// A pod that ignores SIGTERM with a year-long grace period is deleted with
// its grace capped at 30 seconds, and at the drain timeout without any. It
// counts as running while it terminates; once it is gone the pause is
// verified.
func TestDrainCapsGraceAndForceDeletesWhatIsLeft(t *testing.T) {
	year := int64(365 * 24 * 3600)
	slow := podOn("trainer", "ml", "gpu-1")
	slow.Spec.TerminationGracePeriodSeconds = &year
	cl, host, inside := privateCluster(slow)
	var mu sync.Mutex
	var graces []int64
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		da := a.(ktesting.DeleteActionImpl)
		g := int64(-1)
		if da.DeleteOptions.GracePeriodSeconds != nil {
			g = *da.DeleteOptions.GracePeriodSeconds
		}
		mu.Lock()
		graces = append(graces, g)
		mu.Unlock()
		if g == 0 {
			return false, nil, nil // deleted at once
		}
		// Terminating, and in no hurry.
		obj, err := inside.Tracker().Get(podsGVR, da.Namespace, da.Name)
		if err != nil {
			return true, nil, err
		}
		p := obj.(*corev1.Pod).DeepCopy()
		ts := metav1.Now()
		p.DeletionTimestamp, p.DeletionGracePeriodSeconds = &ts, &g
		return true, nil, inside.Tracker().Update(podsGVR, p, da.Namespace)
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 40 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	mu.Lock()
	got := append([]int64(nil), graces...)
	mu.Unlock()
	if len(got) < 2 || got[0] != maxGrace || got[len(got)-1] != 0 {
		t.Fatalf("delete grace periods = %v, want %d first (capped) and 0 last (forced at the drain timeout)", got, maxGrace)
	}
	for _, g := range got[1 : len(got)-1] {
		if g != 0 {
			t.Fatalf("a pod terminating within the cap was deleted again: grace periods %v", got)
		}
	}
	if _, open := spans.Current(cl.ExternalID()); !open {
		t.Fatalf("pause not verified once the pod was gone: status %+v", e.Pauses())
	}
	if st := onlyStatus(t, e); !st.Verified || st.Phase != "paused" {
		t.Fatalf("status = %+v, want paused and verified", st)
	}
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d, want 0", r)
	}
}

// A pod that stays (a finalizer, a kubelet that never confirms) is still
// there after its forced deletion: the control plane stops anyway, but no
// span opens and the status names the pod.
func TestPodThatNeverGoesLeavesThePauseUnverified(t *testing.T) {
	cl, host, inside := privateCluster(podOn("stuck", "ml", "gpu-1"))
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // accepted, never gone
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d: an unverified pause still stops the control plane", r)
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v for a pause whose pod survived", got)
	}
	st := onlyStatus(t, e)
	if st.Verified || !strings.Contains(st.Unverified, "ml/stuck") || st.Error != "" || st.Phase != "paused" {
		t.Fatalf("status = %+v, want paused, unverified, naming ml/stuck", st)
	}
}

// A drain the cluster's API keeps refusing is retried with the control
// plane up; once it has failed for the drain timeout, the control plane is
// stopped anyway, unverified.
func TestDrainThatKeepsFailingStopsTheControlPlaneUnverified(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "inference", errBlocked)
	})
	clock := newClock()
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: time.Minute, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 1 {
		t.Fatalf("control plane replicas = %d after the first failed drain, want it kept up for a retry", r)
	}
	clock.add(30 * time.Second)
	e.Guard(ctx)
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 1 {
		t.Fatalf("control plane replicas = %d within the drain timeout, want 1", r)
	}
	clock.add(time.Minute)
	e.Guard(ctx)
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d after failing for longer than the drain timeout, want 0", r)
	}
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v: the workload was never stopped", got)
	}
	if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "could not be drained") {
		t.Fatalf("status = %+v, want it unverified because the drain failed", st)
	}
	if s := sts(t, host, cl.Namespace, cl.Name); s.Annotations[discovery.AnnotationPausedBy] != pause.ByVBilling {
		t.Fatalf("mark = %v: Guard keeps a paused control plane down by it", s.Annotations)
	}
	// A later pause starts its retries afresh.
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if err := e.Set(ctx, "acme", Paused, "wallet empty again", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 1 {
		t.Fatalf("control plane replicas = %d at the first failure of a new pause, want it kept up", r)
	}
}

var errBlocked = errors.New("admission webhook denied the request")

// A pod that starts on the cluster's own node while its control plane is
// stopping would run on with nothing to stop it: the pause stays
// unverified.
func TestPodStartedWhileTheControlPlaneStopsLeavesThePauseUnverified(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			_ = inside.Tracker().Add(podOn("late", "default", "gpu-1"))
		}
		return false, nil, nil
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
		t.Fatalf("pause spans %+v although a pod started as the control plane stopped", got)
	}
	if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "default/late") {
		t.Fatalf("status = %+v, want unverified naming default/late", st)
	}
}

// A paused cluster something brings back runs, so it bills from the last
// time vBilling saw it stopped; it is paused again and a new span opens
// once that pause is verified.
func TestWokenClusterBillsFromItsLastSighting(t *testing.T) {
	ns := "vc-w"
	host := fake.NewSimpleClientset(controlPlane("w", ns, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	cl := discovery.TenantCluster{Name: "w", Namespace: ns, Source: "statefulset"}
	clock := newClock()
	start := clock.Now()
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	clock.add(20 * time.Second)
	e.Guard(ctx) // sees it stopped
	if _, err := host.AppsV1().StatefulSets(ns).Patch(ctx, "w", "application/merge-patch+json", []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	clock.add(20 * time.Second)
	e.Guard(ctx) // sees it running
	e.Wait()
	got := spans.Spans(cl.ExternalID())
	if len(got) != 2 || !got[0].From.Equal(start) || !got[0].To.Equal(start.Add(20*time.Second)) ||
		!got[1].From.Equal(start.Add(40*time.Second)) || !got[1].Open() {
		t.Fatalf("spans = %+v, want one ending when it was last seen stopped and a new one from its second pause", got)
	}
	if r := *sts(t, host, ns, "w").Spec.Replicas; r != 0 {
		t.Fatalf("woken cluster not paused again: replicas %d", r)
	}
}

// Without write access to a cluster's API (ENFORCEMENT_IN_CLUSTER off) its
// own nodes cannot be drained: the pause is verified only when metering
// bills none of them.
func TestPauseWithoutAccessIsVerifiedOnlyWithoutOwnNodes(t *testing.T) {
	freeze := func(context.Context, discovery.TenantCluster) error { return nil }
	for _, tc := range []struct {
		name     string
		freeze   func(context.Context, discovery.TenantCluster) error
		own      func(discovery.TenantCluster) (bool, bool)
		client   func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error)
		verified bool
	}{
		{"metering never reads clusters' APIs", nil, nil, nil, true},
		{"metering saw no own node", freeze, func(discovery.TenantCluster) (bool, bool) { return false, true }, nil, true},
		{"metering saw own nodes", freeze, func(discovery.TenantCluster) (bool, bool) { return true, true }, nil, false},
		{"metering cannot tell", freeze, func(discovery.TenantCluster) (bool, bool) { return false, false }, nil, false},
		{"metering cannot tell, no hook", freeze, nil, nil, false},
		{"no credentials for the cluster", freeze, nil,
			func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return nil, nil }, true},
	} {
		ns := "vc-x"
		host := fake.NewSimpleClientset(controlPlane("x", ns, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cl := discovery.TenantCluster{Name: "x", Namespace: ns, Source: "statefulset"}
		spans := pause.Memory()
		e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
			Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
			Freeze:   tc.freeze, OwnNodes: tc.own, TenantClient: tc.client})
		if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
			t.Fatal(err)
		}
		e.Wait()
		_, open := spans.Current(cl.ExternalID())
		if r := *sts(t, host, ns, "x").Spec.Replicas; r != 0 || open != tc.verified || onlyStatus(t, e).Verified != tc.verified {
			t.Errorf("%s: replicas %d, span open %t, status %+v; want replicas 0 and verified %t", tc.name, r, open, e.Pauses(), tc.verified)
		}
	}
}

// Synced pods that take a while to go keep the pause unverified until a
// later pass finds them gone; the span opens then, not before.
func TestSyncedPodsMustBeGoneBeforeTheSpanOpens(t *testing.T) {
	ns := "vc-s"
	host := fake.NewSimpleClientset(controlPlane("s", ns, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		syncedPod("job-x-default-x-s", ns, "s"))
	var mu sync.Mutex
	stuck := true
	host.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		return stuck, nil, nil
	})
	cl := discovery.TenantCluster{Name: "s", Namespace: ns, Source: "statefulset"}
	clock := newClock()
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if _, open := spans.Current(cl.ExternalID()); open {
		t.Fatal("span opened while a synced pod still ran")
	}
	if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "job-x-default-x-s") {
		t.Fatalf("status = %+v", st)
	}
	mu.Lock()
	stuck = false
	mu.Unlock()
	clock.add(2 * time.Minute)
	e.Resync(ctx) // deletes it
	e.Wait()
	e.Resync(ctx) // finds it gone
	e.Wait()
	sp, open := spans.Current(cl.ExternalID())
	if !open || !sp.From.Equal(clock.Now()) {
		t.Fatalf("span = %+v %t, want it open from when the synced pods were found gone (%s)", sp, open, clock.Now())
	}
}

// An external cluster's API stays up, so a cluster admin inside can start
// workloads again between two passes. A pass that finds them ends the span
// at the last pass that found none; a pass that cannot check drops what it
// cannot vouch for. Guard checks every 15 seconds.
func TestExternalPauseSpansCoverOnlyVerifiedTime(t *testing.T) {
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}}, podOn("infer", "ml", "edge-1"))
	var mu sync.Mutex
	deny := false
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if deny {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errBlocked)
		}
		return false, nil, nil
	})
	cl := discovery.TenantCluster{Name: "edge", External: true}
	clock := newClock()
	start := clock.Now()
	spans := pause.Memory()
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	clock.add(15 * time.Second)
	e.Guard(ctx) // nothing runs: the span holds
	e.Wait()
	clock.add(15 * time.Second)
	_ = inside.Tracker().Add(podOn("again", "ml", "edge-1")) // the tenant starts something
	e.Guard(ctx)
	e.Wait()
	got := spans.Spans(cl.ExternalID())
	if len(got) != 2 || !got[0].From.Equal(start) || !got[0].To.Equal(start.Add(15*time.Second)) ||
		!got[1].From.Equal(start.Add(30*time.Second)) || !got[1].Open() {
		t.Fatalf("spans = %+v, want the first ending at the last pass that found nothing and a new one from now", got)
	}
	mu.Lock()
	deny = true
	mu.Unlock()
	clock.add(15 * time.Second)
	_ = inside.Tracker().Add(podOn("third", "ml", "edge-1"))
	e.Guard(ctx)
	e.Wait()
	// The pass that opened the second span is the last one that found
	// nothing: none of the second span can be vouched for, so it goes.
	got = spans.Spans(cl.ExternalID())
	if len(got) != 1 || !got[0].To.Equal(start.Add(15*time.Second)) {
		t.Fatalf("spans after a pass that could not stop the workloads = %+v, want only the first, closed", got)
	}
	if st := onlyStatus(t, e); st.Verified || st.Error == "" {
		t.Fatalf("status = %+v, want the failed drain", st)
	}
}

// After a restart vBilling no longer knows what the pause pass found on a
// stopped cluster's own nodes, and its API is down: the pause counts as
// verified only if metering bills none of its own nodes.
func TestRestartBeforeVerificationKeepsBilling(t *testing.T) {
	for _, tc := range []struct {
		own      bool
		verified bool
	}{{own: true, verified: false}, {own: false, verified: true}} {
		ns := "vc-r"
		host := fake.NewSimpleClientset(controlPlane("r", ns, 0, map[string]string{discovery.AnnotationPausedBy: pause.ByVBilling,
			discovery.AnnotationPausedReplicas: "1"}), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cl := discovery.TenantCluster{Name: "r", Namespace: ns, Source: "statefulset"}
		spans := pause.Memory()
		own := tc.own
		e := New(host, Options{Mode: "enforce", State: kvWith(t, "acme", Paused), DrainTimeout: 20 * time.Millisecond, Pauses: spans,
			Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
			Freeze:       func(context.Context, discovery.TenantCluster) error { return nil },
			OwnNodes:     func(discovery.TenantCluster) (bool, bool) { return own, true },
			TenantClient: clientFor(fake.NewSimpleClientset())})
		e.Resync(context.Background())
		e.Wait()
		if _, open := spans.Current(cl.ExternalID()); open != tc.verified {
			t.Errorf("own nodes %t: span open %t, want %t (status %+v)", tc.own, open, tc.verified, e.Pauses())
		}
	}
}

func TestKeptOnPauseOnlyNodeInfrastructure(t *testing.T) {
	ds := func(ns string, mutate ...func(*corev1.Pod)) *corev1.Pod {
		p := podOn("x", ns, "gpu-1")
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "x"}}
		p.Spec.Containers = []corev1.Container{{Name: "c"}}
		for _, m := range mutate {
			m(p)
		}
		return p
	}
	env := func(name, value string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: name, Value: value})
		}
	}
	e := New(nil, Options{})
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		kept bool
	}{
		{"CNI DaemonSet", ds("kube-system"), true},
		{"GPU driver hiding devices", ds("gpu-operator", env("NVIDIA_VISIBLE_DEVICES", "void")), true},
		{"no devices", ds("gpu-operator", env("NVIDIA_VISIBLE_DEVICES", "none")), true},
		{"empty", ds("gpu-operator", env("CUDA_VISIBLE_DEVICES", "")), true},
		{"every GPU", ds("kube-system", env("NVIDIA_VISIBLE_DEVICES", "all")), false},
		{"one GPU", ds("kube-system", env("CUDA_VISIBLE_DEVICES", "0")), false},
		{"AMD", ds("kube-system", env("ROCR_VISIBLE_DEVICES", "0,1")), false},
		{"HIP", ds("kube-system", env("HIP_VISIBLE_DEVICES", "0")), false},
		{"AMD visible", ds("kube-system", env("AMD_VISIBLE_DEVICES", "all")), false},
		{"value from a ConfigMap", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", ValueFrom: &corev1.EnvVarSource{}}}
		}), false},
		{"envFrom", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{}}}
		}), false},
		{"init container", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "i", Env: []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"}}}}
		}), false},
		{"ephemeral container", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name: "debug", Env: []corev1.EnvVar{{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"}}}}}
		}), false},
		{"GPU request", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		}), false},
		{"DRA claim", ds("kube-system", func(p *corev1.Pod) {
			p.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu"}}
		}), false},
		{"other namespace", ds("ml"), false},
		{"not a DaemonSet", podOn("x", "kube-system", "gpu-1"), false},
		{"static pod", func() *corev1.Pod {
			p := podOn("x", "kube-system", "gpu-1")
			p.OwnerReferences = []metav1.OwnerReference{{Kind: "Node", Name: "gpu-1"}}
			return p
		}(), false},
	} {
		if got := e.keptOnPause(tc.pod); got != tc.kept {
			t.Errorf("%s: kept = %t, want %t", tc.name, got, tc.kept)
		}
	}
	none := New(nil, Options{KeepNamespaces: []string{}}) // PAUSE_KEEP_NAMESPACES=none
	if none.keptOnPause(ds("kube-system")) {
		t.Error("PAUSE_KEEP_NAMESPACES=none kept a DaemonSet")
	}
}
