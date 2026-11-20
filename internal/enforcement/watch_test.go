package enforcement

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// apiAfterStop is how a test cluster's API answers once its control plane
// was scaled to zero: "answers", "times out", "one more check" (then gone)
// or "gone".
type apiAfterStop struct {
	mu   sync.Mutex
	mode string
}

func (a *apiAfterStop) set(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = mode
}

// react answers a call the cluster's API gets once stopping says its
// control plane was scaled down.
func (a *apiAfterStop) react(stopping func() bool) ktesting.ReactionFunc {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	return func(act ktesting.Action) (bool, runtime.Object, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !stopping() {
			return false, nil, nil
		}
		switch a.mode {
		case "times out":
			return true, nil, apierrors.NewTimeoutError("list", 1)
		case "gone":
			return true, nil, transportErr(refused)
		case "one more check":
			if act.GetVerb() == "get" && act.GetResource().Resource == "validatingadmissionpolicybindings" {
				a.mode = "gone" // its last answer
			}
		}
		return false, nil, nil
	}
}

// pausedWith pauses a cluster with a workload on its own node gpu-1, whose
// control plane has ready replicas while it stops, and whose API answers
// as api says once that control plane is scaled down.
func pausedWith(t *testing.T, api *apiAfterStop, ready int32) (discovery.TenantCluster, *fake.Clientset, *fake.Clientset, *Enforcer, *pause.Store, *testClock) {
	t.Helper()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	cp := controlPlane(cl.Name, cl.Namespace, 1, nil)
	cp.Status.ReadyReplicas = ready
	host := fake.NewSimpleClientset(cp, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}}, podOn("inference", "default", "gpu-1"))
	var mu sync.Mutex
	stopping := false
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			mu.Lock()
			stopping = true
			mu.Unlock()
		}
		return false, nil, nil
	})
	inside.PrependReactor("*", "*", api.react(func() bool { mu.Lock(); defer mu.Unlock(); return stopping }))
	clock := newClock()
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	return cl, host, inside, e, spans, clock
}

// A clean pause whose checks keep timing out while its control plane stops
// is not verified by that pass, but not lost either: the next pass (Guard,
// every 15 seconds) that sees the API gone with the control plane down
// completes it. The span starts then, so the time in between bills.
func TestTimeoutsWhileTheControlPlaneStopsAreCompletedByALaterPass(t *testing.T) {
	api := &apiAfterStop{mode: "times out"}
	cl, _, _, e, spans, clock := pausedWith(t, api, 0)
	if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "could not be checked") {
		t.Fatalf("status %+v, want unverified while the checks time out", st)
	}
	if _, open := spans.Current(cl.ExternalID()); open {
		t.Fatal("a span opened on checks that all timed out")
	}
	clock.add(30 * time.Second)
	api.set("gone")
	e.Guard(context.Background())
	e.Wait()
	sp, open := spans.Current(cl.ExternalID())
	if !open || !sp.From.Equal(clock.Now()) {
		t.Fatalf("span %+v (open %t), want one from the pass that saw the API gone (%s); status %+v", sp, open, clock.Now(), e.Pauses())
	}
	if st := onlyStatus(t, e); !st.Verified {
		t.Fatalf("status %+v, want verified", st)
	}
}

// Checks that end while the cluster's API still answers (a control plane
// that takes longer than the drain timeout to stop) leave nothing a later
// pass may trust: it must find the own nodes empty itself before the API
// going away counts. A pod started in between is found then, and the pause
// stays unverified; an API gone before such a check leaves it unverified
// for good, and no pass looks again.
func TestChecksThatEndWhileTheAPIAnswersNeedAFreshCleanCheck(t *testing.T) {
	for _, tc := range []struct {
		name, later string
		started     bool
		why         string
	}{
		{"a fresh check, then the API goes", "one more check", false, ""},
		{"a pod started meanwhile", "one more check", true, "default/miner"},
		{"the API gone before a fresh check", "gone", false, "went away unwatched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &apiAfterStop{mode: "answers"}
			cl, host, inside, e, spans, clock := pausedWith(t, api, 1) // still ready while it stops
			if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "not stopped yet") {
				t.Fatalf("status %+v, want unverified while the control plane still stops", st)
			}
			if tc.started {
				if err := inside.Tracker().Add(podOn("miner", "default", "gpu-1", corev1.Toleration{Operator: corev1.TolerationOpExists})); err != nil {
					t.Fatal(err)
				}
			}
			stopped := sts(t, host, cl.Namespace, cl.Name).DeepCopy()
			stopped.Status.ReadyReplicas = 0
			if err := host.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("statefulsets"), stopped, cl.Namespace); err != nil {
				t.Fatal(err)
			}
			api.set(tc.later)
			clock.add(30 * time.Second)
			e.Guard(context.Background())
			e.Wait()
			_, open := spans.Current(cl.ExternalID())
			st := onlyStatus(t, e)
			if tc.why == "" {
				if !open || !st.Verified {
					t.Fatalf("span open %t, status %+v: a later pass found the own node empty and saw the API go away", open, st)
				}
				return
			}
			if open || st.Verified || !strings.Contains(st.Unverified, tc.why) || e.watching(cl.ExternalID()) {
				t.Fatalf("span open %t, status %+v, still watched %t: want unverified for good, saying %q", open, st, e.watching(cl.ExternalID()), tc.why)
			}
		})
	}
}

// Checks the API throttles (TooManyRequests, which API Priority and
// Fairness inside a tenant's cluster can be set to answer vBilling with)
// say the API is there and will not answer: the pause stays unverified,
// and no later pass completes it.
func TestThrottledChecksLeaveThePauseUnverified(t *testing.T) {
	cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
	var mu sync.Mutex
	stopping := false
	host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
			mu.Lock()
			stopping = true
			mu.Unlock()
		}
		return false, nil, nil
	})
	inside.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if stopping {
			return true, nil, apierrors.NewTooManyRequests("the server has received too many requests", 1)
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
	e.Resync(context.Background())
	e.Wait()
	if _, open := spans.Current(cl.ExternalID()); open {
		t.Fatalf("pause verified on throttled checks: %+v", e.Pauses())
	}
	if st := onlyStatus(t, e); st.Verified || !strings.Contains(st.Unverified, "could not be checked") || e.watching(cl.ExternalID()) {
		t.Fatalf("status %+v, want unverified for good", st)
	}
}

// Through vCluster Platform's proxy (TENANT_SOURCE=platform), a stopped
// cluster's API never refuses: the proxy holds each call until it times
// out. A timeout counts as the API gone once the control plane cluster
// shows no control plane pod left, so the pause of a cluster with its own
// nodes is verified, and its nodes stop billing, in the pass that stops it.
// While a control plane pod is still there (terminating, say), a timeout
// proves nothing; the pass after it is gone completes the pause.
func TestTimeoutsThroughAProxyCountOnceTheControlPlanePodsAreGone(t *testing.T) {
	for _, podLeft := range []bool{false, true} {
		name := "no control plane pod left"
		if podLeft {
			name = "a control plane pod still terminating"
		}
		t.Run(name, func(t *testing.T) {
			api := &apiAfterStop{mode: "times out"}
			cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
			cp := controlPlane(cl.Name, cl.Namespace, 1, nil)
			cp.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "vcluster", "release": cl.Name}}
			objs := []runtime.Object{cp, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}}}
			cpPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cl.Name + "-0", Namespace: cl.Namespace,
				Labels: map[string]string{"app": "vcluster", "release": cl.Name}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
			if podLeft {
				objs = append(objs, cpPod)
			}
			host := fake.NewSimpleClientset(objs...)
			inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}}, podOn("inference", "default", "gpu-1"))
			var mu sync.Mutex
			stopping := false
			host.PrependReactor("patch", "statefulsets", func(a ktesting.Action) (bool, runtime.Object, error) {
				if strings.Contains(string(a.(ktesting.PatchActionImpl).Patch), `"replicas":0`) {
					mu.Lock()
					stopping = true
					mu.Unlock()
				}
				return false, nil, nil
			})
			inside.PrependReactor("*", "*", api.react(func() bool { mu.Lock(); defer mu.Unlock(); return stopping }))
			clock := newClock()
			spans := pause.Memory()
			e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
				Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
			if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "metronome", "alerts.low_remaining_commit_balance_reached"); err != nil {
				t.Fatal(err)
			}
			e.Wait()
			if !podLeft {
				sp, open := spans.Current(cl.ExternalID())
				if st := onlyStatus(t, e); !open || !st.Verified || !sp.From.Equal(clock.Now()) {
					t.Fatalf("span %+v (open %t), status %+v: want the pause verified in the pass that stopped it", sp, open, st)
				}
				return
			}
			if _, open := spans.Current(cl.ExternalID()); open {
				t.Fatalf("a span opened while a control plane pod was still there: status %+v", e.Pauses())
			}
			if st := onlyStatus(t, e); st.Verified {
				t.Fatalf("status %+v: verified while a control plane pod was still there", st)
			}
			if err := host.CoreV1().Pods(cl.Namespace).Delete(context.Background(), cpPod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			clock.add(15 * time.Second)
			e.Guard(context.Background())
			e.Wait()
			sp, open := spans.Current(cl.ExternalID())
			if st := onlyStatus(t, e); !open || !st.Verified || !sp.From.Equal(clock.Now()) {
				t.Fatalf("span %+v (open %t), status %+v: want it verified by the pass after the pod was gone", sp, open, st)
			}
		})
	}
}

func TestUnreachableSaysNoAnswer(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{errors.New(`Get "https://loft.vcluster-platform.svc/kubernetes/project/default/virtualcluster/pilot-a100/api/v1/nodes": context deadline exceeded`), true},
		{errors.New("net/http: request canceled (Client.Timeout exceeded while awaiting headers)"), true},
		{apierrors.NewTimeoutError("list", 1), true},
		{apierrors.NewForbidden(corev1.Resource("pods"), "", errors.New("revoked")), false},
		{apierrors.NewTooManyRequests("throttled", 1), false},
		{nil, false},
	} {
		if got := unreachable(tc.err); got != tc.want {
			t.Errorf("unreachable(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}
