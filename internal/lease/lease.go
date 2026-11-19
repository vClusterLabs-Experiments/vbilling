// Package lease keeps vBilling to one running instance.
//
// One vBilling process meters, takes gateway records, delivers usage and
// enforces billing states, with one ledger. Restarts are safe: the ledger
// and backfill resume where the previous process stopped. Two processes at
// once are not: each keeps its own ledger, records posted to the Service
// split between them, and billing platforms keep one copy of each event ID,
// so a share of every window is never invoiced. There is no standby either:
// a second replica has its own volume, so taking over would start from an
// empty ledger.
//
// So a process takes a coordination.k8s.io Lease in its own namespace before
// it does anything else. A second process waits, alive but not Ready (no
// Service routes records or webhooks to it), and takes over only once the
// Lease is released, when the first one stops, or expires, when it died.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Name is the Lease every vBilling process in a namespace competes for. It
// is fixed, so two installations in one namespace can never meter at once.
const Name = "vbilling"

// Timing. A holder renews every RetryPeriod; one that cannot renew for
// RenewDeadline stops (the process exits), RenewDeadline is well before
// LeaseDuration, after which another process may take the Lease. A short
// API server outage does not stop metering; a process that died is
// replaced within LeaseDuration, and its windows are backfilled.
const (
	LeaseDuration = 30 * time.Second
	RenewDeadline = 20 * time.Second
	RetryPeriod   = 4 * time.Second
)

// forbiddenAttempts is how many attempts in a row the Lease must be refused
// before vBilling runs without it: RBAC created in the same install as the
// pod can take a moment to apply.
const forbiddenAttempts = 5

// ErrForbidden means the process may not read or write its Lease.
var ErrForbidden = errors.New("no permission to use the Lease")

// Options configure Acquire. Zero values pick the defaults above.
type Options struct {
	Namespace string
	Name      string
	// Identity names this process in the Lease. Default: the hostname (the
	// pod name) plus a random suffix, so two processes never share one.
	Identity      string
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
	// Listen, when set, is served while the process waits: /healthz answers
	// 200 and every other path 503, so the pod stays alive but not Ready.
	Listen string
	Logf   func(format string, args ...any)
}

func (o *Options) defaults() {
	if o.Name == "" {
		o.Name = Name
	}
	if o.Identity == "" {
		host, _ := os.Hostname()
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		o.Identity = host + "_" + hex.EncodeToString(b)
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = LeaseDuration
	}
	if o.RenewDeadline <= 0 {
		o.RenewDeadline = RenewDeadline
	}
	if o.RetryPeriod <= 0 {
		o.RetryPeriod = RetryPeriod
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// Held is a Lease this process holds.
type Held struct {
	Identity string

	cancel    context.CancelFunc
	done      chan struct{} // the elector stopped
	lost      chan struct{} // closed when the Lease is lost, never on Release
	lostOnce  sync.Once
	releasing atomic.Bool
}

// Lost is closed if the process stops holding the Lease without Release:
// it could not renew it in time, so another process may take it. The caller
// must stop at once.
func (h *Held) Lost() <-chan struct{} { return h.lost }

// Release gives the Lease up. Call it after everything it guarded has
// stopped: another process can take it over at once.
func (h *Held) Release() {
	if h == nil {
		return
	}
	h.releasing.Store(true)
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
	}
}

// Acquire blocks until this process holds the Lease. It returns ctx's error
// if ctx ends first, and an error wrapping ErrForbidden when the process may
// not use Leases in the namespace.
func Acquire(ctx context.Context, kube kubernetes.Interface, o Options) (*Held, error) {
	o.defaults()
	leCtx, cancel := context.WithCancel(context.Background()) // ended by Release, not by ctx
	h := &Held{Identity: o.Identity, cancel: cancel, done: make(chan struct{}), lost: make(chan struct{})}
	acquired := make(chan struct{})
	forbidden := make(chan error, 1)
	lock := &watchedLock{
		Interface: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Namespace: o.Namespace, Name: o.Name},
			Client:     kube.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: o.Identity},
		},
		forbidden: forbidden,
	}
	var holder atomic.Value
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		Name:            o.Namespace + "/" + o.Name,
		LeaseDuration:   o.LeaseDuration,
		RenewDeadline:   o.RenewDeadline,
		RetryPeriod:     o.RetryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) { close(acquired) },
			OnStoppedLeading: func() {
				if !h.releasing.Load() {
					h.lostOnce.Do(func() { close(h.lost) })
				}
			},
			OnNewLeader: func(id string) {
				holder.Store(id)
				if id != "" && id != o.Identity {
					o.Logf("[lease] Lease %s/%s is held by %s: this vBilling waits, not Ready, until it is released or expires", o.Namespace, o.Name, id)
				}
			},
		},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		defer close(h.done)
		le.Run(leCtx)
	}()

	stop := serveWhileWaiting(o, func() string { s, _ := holder.Load().(string); return s })
	defer stop()
	select {
	case <-acquired:
		o.Logf("[lease] holding Lease %s/%s as %s", o.Namespace, o.Name, o.Identity)
		return h, nil
	case err := <-forbidden:
		h.releasing.Store(true)
		cancel()
		<-h.done
		return nil, fmt.Errorf("%w %s/%s: %v", ErrForbidden, o.Namespace, o.Name, err)
	case <-ctx.Done():
		h.releasing.Store(true)
		cancel()
		<-h.done
		return nil, ctx.Err()
	}
}

// watchedLock reports a Lease the API server keeps refusing.
type watchedLock struct {
	resourcelock.Interface
	forbidden chan error
	refused   atomic.Int32
}

func (l *watchedLock) note(err error) error {
	switch {
	case apierrors.IsForbidden(err):
		if l.refused.Add(1) >= forbiddenAttempts {
			select {
			case l.forbidden <- err:
			default:
			}
		}
	case err == nil:
		l.refused.Store(0)
	}
	return err
}

func (l *watchedLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	r, raw, err := l.Interface.Get(ctx)
	if apierrors.IsNotFound(err) {
		return r, raw, err // not created yet: Create follows
	}
	return r, raw, l.note(err)
}

func (l *watchedLock) Create(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	return l.note(l.Interface.Create(ctx, r))
}

func (l *watchedLock) Update(ctx context.Context, r resourcelock.LeaderElectionRecord) error {
	return l.note(l.Interface.Update(ctx, r))
}

// serveWhileWaiting answers health checks while the process waits for the
// Lease: alive (/healthz), never ready, and no records, events or webhooks
// accepted (503, so emitters retry). It returns a function that stops it.
func serveWhileWaiting(o Options, holder func() string) func() {
	if o.Listen == "" {
		return func() {}
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		o.Logf("[lease] cannot answer health checks on %s while waiting: %v", o.Listen, err)
		return func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok: waiting for the vBilling Lease\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "10")
		msg := "another vBilling instance holds the Lease " + o.Namespace + "/" + o.Name
		if id := holder(); id != "" {
			msg += " (" + id + ")"
		}
		http.Error(w, msg+"; this one does not meter or accept usage", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}
