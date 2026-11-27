package platform

import (
	"context"
	"fmt"
	"log"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// Watching the Platform dates owner changes to the second. The Platform
// serves Projects, Tenants and VirtualClusterInstances like a Kubernetes
// API, with the watch verb in its API discovery (captured from a live
// Platform 4.9.0 in testdata). vBilling watches each of them that
// discovery lists with that verb. A watch event that changes what decides
// an owner (a Project's Tenant labels, a Tenant's or Project's
// vbilling.vcluster.com/ settings, an instance created, moved or deleted)
// is noted with the time it arrived, and the Platform is read right away:
// an owner change that read shows is dated at the arrival, and recorded
// before the window it falls in is committed.
//
// The reads every PLATFORM_REFRESH go on regardless. They decide alone
// whenever a watch is not available: the Platform does not list the verb,
// the access key may not watch, or the watch keeps failing. Changes are
// then dated at the read that shows them, as before.

// watchSettle is how long a burst of events (a move between projects is
// three within milliseconds) gets before the read. Reads the watch asks for
// are at least forceGap apart.
const watchSettle = time.Second

// A watch that ends sooner than minWatch failed; retries back off up to
// maxWatchBackoff.
const (
	minWatch        = 30 * time.Second
	maxWatchBackoff = 5 * time.Minute
)

// watchState is what vBilling last said about the watch of one resource.
type watchState struct {
	up     bool
	logged string // the last failure logged, so a failing watch is not logged every retry
}

// watched are the resources whose changes decide owners.
var watched = []schema.GroupVersionResource{projectsGVR, tenantsGVR, instancesGVR}

// Watch watches the Platform's Projects, Tenants and VirtualClusterInstances
// until ctx ends, starting once the Platform has answered a read. It
// returns when ctx ends.
func (r *Resolver) Watch(ctx context.Context) {
	r.mu.Lock()
	r.watchCtx = ctx
	snap := r.snap
	r.mu.Unlock()
	if snap != nil {
		r.startWatches(snap)
	}
	var last time.Time // when the last read the watch asked for began
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		// A steady stream of changes elsewhere on the Platform reads it at
		// most every readGap: the events keep their arrival times meanwhile.
		wait := r.settle
		if !last.IsZero() {
			wait = max(wait, r.readGap-time.Since(last))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		select {
		case <-r.wake: // events of the same burst: this read covers them
		default:
		}
		last = time.Now()
		now := r.opts.Now()
		r.mu.Lock()
		r.lastTry = now
		r.mu.Unlock()
		r.read(ctx, now)
	}
}

// startWatches starts a watch for each resource the Platform offers one
// for, once Watch was called.
func (r *Resolver) startWatches(snap *Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx := r.watchCtx
	if ctx == nil {
		return
	}
	for _, gvr := range watched {
		res := gvr.Resource
		if res == tenantsGVR.Resource && !snap.HasTenants() {
			continue // no Tenants before Platform 4.12
		}
		if r.watches[res] != nil {
			continue
		}
		r.watches[res] = &watchState{}
		if !snap.Watchable[res] {
			log.Printf("[platform] the Platform offers no watch of %s: owner changes there are dated at the read that shows them, every %s", res, r.opts.Refresh)
			continue
		}
		go r.watchLoop(ctx, gvr, snap.Versions[res])
	}
}

// watchLoop keeps a watch of one resource open, from resource version rv.
func (r *Resolver) watchLoop(ctx context.Context, gvr schema.GroupVersionResource, rv string) {
	backoff := time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := r.watchOnce(ctx, gvr, &rv)
		if ctx.Err() != nil {
			return
		}
		if err == nil && time.Since(began) >= minWatch {
			backoff = time.Second // ran until the Platform ended it: the next one starts now
			continue
		}
		if err == nil {
			err = fmt.Errorf("the Platform ends it within %s", minWatch)
		}
		r.watchDown(gvr.Resource, err)
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, maxWatchBackoff)
	}
}

// watchOnce runs one watch until the Platform ends it or it fails, and
// moves rv along with the events.
func (r *Resolver) watchOnce(ctx context.Context, gvr schema.GroupVersionResource, rv *string) error {
	// A connection that stops answering is given up a minute after the
	// Platform should have ended the watch.
	ctx, cancel := context.WithTimeout(ctx, watchTimeout+time.Minute)
	defer cancel()
	w, err := r.client.watchFrom(ctx, gvr, *rv)
	if err != nil {
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			*rv = "" // too old: the next watch starts with the current state
		}
		return err
	}
	defer w.Stop()
	// It works once it has held for minWatch (said once, not at every
	// retry of a watch the Platform keeps ending), or the first time.
	r.watchUp(gvr.Resource, true)
	held := time.NewTimer(minWatch)
	defer held.Stop()
	for {
		var ev watch.Event
		select {
		case <-held.C:
			r.watchUp(gvr.Resource, false)
			continue
		case e, open := <-w.ResultChan():
			if !open {
				return nil
			}
			ev = e
		}
		switch ev.Type {
		case watch.Error:
			err := apierrors.FromObject(ev.Object)
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				*rv = ""
			}
			return err
		case watch.Bookmark:
			if u, ok := ev.Object.(*unstructured.Unstructured); ok {
				*rv = u.GetResourceVersion()
			}
		case watch.Added, watch.Modified, watch.Deleted:
			arrived := r.opts.Now()
			u, ok := ev.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			*rv = u.GetResourceVersion()
			o, err := decode(gvr, u)
			if err != nil {
				log.Printf("[platform] watch of %s: %v", gvr.Resource, err)
				continue
			}
			r.noteEvent(gvr.Resource, ev.Type, &o, arrived)
		}
	}
}

// noteEvent notes when a change that may move a cluster to another owner
// arrived, and asks for a read. Changes to anything else (status, phase,
// other annotations) are ignored.
func (r *Resolver) noteEvent(resource string, typ watch.EventType, o *Object, arrived time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snap != nil {
		have := r.snap.object(resource, o)
		switch {
		case typ == watch.Deleted && have == nil:
			return // gone before the last read
		case typ != watch.Deleted && have != nil && ownership(have) == ownership(o):
			return // nothing that decides an owner changed
		}
	}
	uid := string(o.UID)
	if at, ok := r.pending[uid]; !ok || arrived.Before(at) {
		r.pending[uid] = arrived
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// reportedAt returns when the watch first reported a change to an object
// that decides between two bindings (their instances, Projects and
// Tenants) that no read had covered yet.
func (r *Resolver) reportedAt(was, now *discovery.PlatformBinding) (time.Time, bool) {
	var first time.Time
	for _, uid := range []string{was.InstanceUID, now.InstanceUID, was.ProjectUID, now.ProjectUID, was.TenantUID, now.TenantUID} {
		if at, ok := r.pending[uid]; uid != "" && ok && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return first, !first.IsZero()
}

// watchUp notes that the watch of res works: as soon as it opens the first
// time (first), and after a failure once a watch has held for minWatch.
func (r *Resolver) watchUp(res string, first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.watches[res]
	if st == nil || st.up || first && st.logged != "" {
		return
	}
	st.up = true
	if st.logged == "" {
		log.Printf("[platform] watching %s: owner changes there are dated when the Platform reports them", res)
	} else {
		log.Printf("[platform] watching %s again: owner changes there are dated when the Platform reports them", res)
	}
	st.logged = "up"
}

func (r *Resolver) watchDown(res string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.watches[res]
	if st == nil {
		return
	}
	st.up = false
	if msg := err.Error(); msg != st.logged {
		st.logged = msg
		log.Printf("[platform] cannot watch %s (%v): owner changes there are dated at the read that shows them, every %s, until the watch works again",
			res, err, r.opts.Refresh)
	}
}
