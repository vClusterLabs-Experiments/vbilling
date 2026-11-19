package lease

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fast keeps tests short. A Lease records whole seconds; two seconds keep
// a holder's renewals ahead of expiry even when -race and parallel test
// binaries starve it for a moment.
func fast(id string) Options {
	return Options{Namespace: "vbilling-system", Identity: id, LeaseDuration: 2 * time.Second,
		RenewDeadline: 1500 * time.Millisecond, RetryPeriod: 200 * time.Millisecond, Logf: func(string, ...any) {}}
}

func acquireAsync(ctx context.Context, t *testing.T, kube *fake.Clientset, o Options) <-chan *Held {
	t.Helper()
	ch := make(chan *Held, 1)
	go func() {
		h, err := Acquire(ctx, kube, o)
		if err != nil {
			close(ch)
			return
		}
		ch <- h
	}()
	return ch
}

// A second process waits while the first holds the Lease, and takes over
// as soon as the first releases it.
func TestSecondInstanceWaitsForTheLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := Acquire(ctx, kube, fast("a"))
	if err != nil {
		t.Fatal(err)
	}
	b := acquireAsync(ctx, t, kube, fast("b"))
	select {
	case <-b:
		t.Fatal("b took the Lease while a holds it")
	case <-time.After(4500 * time.Millisecond): // more than two lease durations: a keeps renewing
	}
	released := time.Now()
	a.Release()
	select {
	case h := <-b:
		if h == nil {
			t.Fatal("b failed to take the Lease")
		}
		defer h.Release()
		if waited := time.Since(released); waited > 2*time.Second {
			t.Fatalf("b took %s to take a released Lease", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("b never took the released Lease")
	}
	l, err := kube.CoordinationV1().Leases("vbilling-system").Get(ctx, Name, metav1.GetOptions{})
	if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity != "b" {
		t.Fatalf("Lease after the handover: %+v %v", l, err)
	}
	select {
	case <-a.Lost():
		t.Fatal("a release must not report the Lease as lost")
	default:
	}
}

// A process that died (no release) keeps the Lease until it expires: the
// next process takes over only then.
func TestTakesOverOnlyOnceTheLeaseExpires(t *testing.T) {
	now := metav1.NewMicroTime(time.Now())
	holder, secs := "dead_0001", int32(2)
	kube := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: "vbilling-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, AcquireTime: &now, RenewTime: &now},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := fast("next")
	o.LeaseDuration = 2 * time.Second
	start := time.Now()
	h, err := Acquire(ctx, kube, o)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if waited := time.Since(start); waited < 1800*time.Millisecond {
		t.Fatalf("took the Lease of a holder that may still run after %s, before it expired", waited)
	}
}

// Losing the Lease (renewals refused) is reported, so the process stops
// before another one can take over.
func TestLosingTheLeaseIsReported(t *testing.T) {
	kube := fake.NewSimpleClientset()
	var refuse atomic.Bool
	kube.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	h, err := Acquire(context.Background(), kube, fast("a"))
	if err != nil {
		t.Fatal(err)
	}
	refuse.Store(true)
	select {
	case <-h.Lost():
	case <-time.After(6 * time.Second):
		t.Fatal("a Lease that cannot be renewed was not reported lost")
	}
	h.Release()
}

// Without RBAC for Leases the process gives up on the Lease (and runs
// without the guard) instead of waiting forever.
func TestForbiddenLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("*", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, Name, errors.New("RBAC"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Acquire(ctx, kube, fast("a")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// While it waits, a process is alive but not Ready, and takes no usage.
func TestWaitingInstanceIsAliveButNotReady(t *testing.T) {
	kube := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := Acquire(ctx, kube, fast("a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	o := fast("b")
	o.Listen = addr
	acquireAsync(ctx, t, kube, o)
	get := func(method, path string) int {
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest(method, "http://"+addr+path, strings.NewReader("{}"))
			if res, err := http.DefaultClient.Do(req); err == nil {
				res.Body.Close()
				return res.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s %s: nothing answers on %s", method, path, addr)
		return 0
	}
	if got := get("GET", "/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 (alive)", got)
	}
	for _, p := range [][2]string{{"GET", "/readyz"}, {"POST", "/api/v1/records"}, {"POST", "/webhooks/stripe"}} {
		if got := get(p[0], p[1]); got != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 while another instance holds the Lease", p[0], p[1], got)
		}
	}
}
