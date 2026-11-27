package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// ControlPlanes records when each cluster's control plane had a ready
// replica, from watch events on its StatefulSet or Deployment.
//
// Control plane hours are billed while a replica is ready. Discovery reads
// that every 30 seconds, so a control plane that stopped or started between
// two reads was billed by what the next read saw: up to 30 seconds of a
// stopped control plane, and none of the first seconds of a ready one. The
// watch sees each change within about a second, and the collector bills the
// ready time itself.
//
// What it did not watch it does not know. A window is "known" only when the
// timeline covers all of it: not before vBilling first saw the workload
// (unless it saw it being created), and not across a time vBilling was
// down if the workload's state changed meanwhile. The collector then falls
// back to what discovery reads, as before. The timeline is persisted, so a
// restart keeps what was seen.
type ControlPlanes struct {
	mu   sync.Mutex
	now  func() time.Time
	path string
	// planes by namespace/name of the workload.
	planes map[string]*planeTimeline
	// alive is when this process last recorded that it was watching.
	alive time.Time
	// retention is how long changes and deleted workloads are kept: as long
	// as a window can still be metered.
	retention time.Duration
	dirty     bool
	// listed are the workloads found by the list a watch started with.
	listed map[string]bool
}

// planeTimeline is what was seen of one control plane workload.
type planeTimeline struct {
	UID string `json:"uid"`
	// Changes are the states seen, oldest first: Ready (or not) from At
	// until the next change. Unknown marks a time the state is not known
	// from: vBilling was not watching and the state differed afterwards.
	Changes []planeChange `json:"changes"`
	// Deleted is when the workload was deleted (zero while it exists).
	Deleted time.Time `json:"deleted,omitempty"`
}

type planeChange struct {
	At      time.Time `json:"at"`
	Ready   bool      `json:"ready,omitempty"`
	Unknown bool      `json:"unknown,omitempty"`
}

type planesFile struct {
	Alive  time.Time                 `json:"alive"`
	Planes map[string]*planeTimeline `json:"planes"`
}

// NewControlPlanes loads the timeline at path (empty: in memory only).
func NewControlPlanes(path string, retention time.Duration, now func() time.Time) (*ControlPlanes, error) {
	if now == nil {
		now = time.Now
	}
	c := &ControlPlanes{now: now, path: path, planes: map[string]*planeTimeline{}, retention: retention}
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f planesFile
	if err := json.Unmarshal(b, &f); err != nil {
		// A timeline that cannot be read is only precision lost: start over.
		log.Printf("[metrics] control plane timeline %s is unreadable, starting a new one: %v", path, err)
		return c, nil
	}
	if f.Planes != nil {
		c.planes = f.Planes
	}
	c.alive = f.Alive
	return c, nil
}

// Watch records readiness changes of the control planes (StatefulSets and
// Deployments labeled app=vcluster) in namespaces (none: every namespace)
// until ctx ends. It returns once the informers have synced.
func (c *ControlPlanes) Watch(ctx context.Context, client kubernetes.Interface, namespaces []string) {
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	var synced []cache.InformerSynced
	for _, ns := range namespaces {
		factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithNamespace(ns),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = "app=vcluster" }))
		for _, inf := range []cache.SharedIndexInformer{factory.Apps().V1().StatefulSets().Informer(), factory.Apps().V1().Deployments().Informer()} {
			inf := inf
			_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
				// An object listed when the watch starts existed before:
				// nothing is known of it before now. One added later was
				// created under watch: it is known from its creation.
				AddFunc:    func(obj any) { c.seen(obj, !inf.HasSynced()) },
				UpdateFunc: func(_, obj any) { c.seen(obj, false) },
				DeleteFunc: func(obj any) {
					if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
						obj = tomb.Obj
					}
					if key, uid, _, _, ok := planeOf(obj); ok {
						c.deleted(key, uid)
					}
				},
			})
			synced = append(synced, inf.HasSynced)
		}
		factory.Start(ctx.Done())
	}
	if cache.WaitForCacheSync(ctx.Done(), synced...) {
		c.missing()
	}
	go c.keepAlive(ctx)
}

// missing marks the workloads a timeline was loaded for that no longer
// exist: they were deleted while vBilling was not watching. When is not
// known, so they count as gone from when vBilling last watched: a deletion
// during an outage can under-bill, never over-bill.
func (c *ControlPlanes) missing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.planes {
		if c.listed[key] || !p.Deleted.IsZero() || len(p.Changes) == 0 {
			continue
		}
		at := c.alive
		if last := p.Changes[len(p.Changes)-1]; last.At.After(at) {
			at = last.At
		}
		if at.IsZero() {
			at = c.now().UTC()
		}
		p.Deleted = at
		if last := p.Changes[len(p.Changes)-1]; last.Unknown || last.Ready {
			p.Changes = append(p.Changes, planeChange{At: at})
		}
		c.dirty = true
	}
	c.listed = nil
}

// keepAlive saves the timeline when it changed and records, once a minute,
// that vBilling is watching, so a restart knows since when it was not.
func (c *ControlPlanes) keepAlive(ctx context.Context) {
	save := time.NewTicker(2 * time.Second)
	defer save.Stop()
	beat := time.NewTicker(time.Minute)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			c.flush(true)
			return
		case <-beat.C:
			c.flush(true)
		case <-save.C:
			c.flush(false)
		}
	}
}

func planeOf(obj any) (key, uid string, ready bool, created time.Time, ok bool) {
	switch o := obj.(type) {
	case *appsv1.StatefulSet:
		return o.Namespace + "/" + o.Name, string(o.UID), o.Status.ReadyReplicas > 0, o.CreationTimestamp.Time, true
	case *appsv1.Deployment:
		return o.Namespace + "/" + o.Name, string(o.UID), o.Status.ReadyReplicas > 0, o.CreationTimestamp.Time, true
	}
	return "", "", false, time.Time{}, false
}

func (c *ControlPlanes) seen(obj any, listed bool) {
	key, uid, ready, created, ok := planeOf(obj)
	if !ok {
		return
	}
	c.observe(key, uid, ready, created, listed)
}

// observe records the state of a control plane. listed is true when the
// object was found by the list a watch starts with.
func (c *ControlPlanes) observe(key, uid string, ready bool, created time.Time, listed bool) {
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if listed {
		if c.listed == nil {
			c.listed = map[string]bool{}
		}
		c.listed[key] = true
	}
	p := c.planes[key]
	if p != nil && p.UID != uid {
		p = nil // another workload took the name: a new timeline
	}
	if p == nil {
		p = &planeTimeline{UID: uid}
		c.planes[key] = p
		at := now
		if !listed && !created.IsZero() && created.Before(now) {
			// Created under watch: not ready from its creation on.
			p.Changes = append(p.Changes, planeChange{At: created.UTC()})
		}
		p.Changes = append(p.Changes, planeChange{At: at, Ready: ready})
		c.dirty = true
		return
	}
	p.Deleted = time.Time{}
	last := p.Changes[len(p.Changes)-1]
	if listed && !c.alive.IsZero() && (last.Unknown || last.Ready != ready) {
		// Its state changed while vBilling was not watching: when is not
		// known, so the time since vBilling last watched is not either.
		if c.alive.After(last.At) {
			p.Changes = append(p.Changes, planeChange{At: c.alive, Unknown: true})
		} else {
			p.Changes[len(p.Changes)-1].Unknown = true
		}
		p.Changes = append(p.Changes, planeChange{At: now, Ready: ready})
		c.dirty = true
		return
	}
	if last.Unknown || last.Ready != ready {
		p.Changes = append(p.Changes, planeChange{At: now, Ready: ready})
		c.dirty = true
	}
}

func (c *ControlPlanes) deleted(key, uid string) {
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.planes[key]
	if p == nil || p.UID != uid || !p.Deleted.IsZero() {
		return
	}
	p.Deleted = now
	if last := p.Changes[len(p.Changes)-1]; last.Unknown || last.Ready {
		p.Changes = append(p.Changes, planeChange{At: now})
	}
	c.dirty = true
}

// Ready returns the times within [ws, we) the control plane of cl had a
// ready replica. known is false when the timeline does not cover the whole
// window, and the caller must decide from what it reads itself.
func (c *ControlPlanes) Ready(cl discovery.TenantCluster, ws, we time.Time) (ready [][2]time.Time, known bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.planes[cl.Namespace+"/"+cl.Name]
	if p == nil || (cl.UID != "" && p.UID != cl.UID) || len(p.Changes) == 0 {
		return nil, false
	}
	if p.Changes[0].At.After(ws) {
		// Seen only from later on. A workload created under watch starts
		// its timeline at its creation: before it, there was nothing.
		if !(cl.CreatedAt.Equal(p.Changes[0].At) || cl.CreatedAt.After(p.Changes[0].At)) {
			return nil, false
		}
	}
	for i, ch := range p.Changes {
		end := we
		if i+1 < len(p.Changes) {
			end = p.Changes[i+1].At
		}
		from := ch.At
		if from.Before(ws) {
			from = ws
		}
		if end.After(we) {
			end = we
		}
		if !end.After(from) {
			continue
		}
		if ch.Unknown {
			return nil, false
		}
		if ch.Ready {
			ready = append(ready, [2]time.Time{from, end})
		}
	}
	return ready, true
}

// Gone returns when the control plane workload of cl was deleted, if it was
// seen being deleted.
func (c *ControlPlanes) Gone(cl discovery.TenantCluster) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.planes[cl.Namespace+"/"+cl.Name]
	if p == nil || (cl.UID != "" && p.UID != cl.UID) || p.Deleted.IsZero() {
		return time.Time{}, false
	}
	return p.Deleted, true
}

// ReadySince returns when the control plane of cl last became ready, if it
// is ready and that was seen.
func (c *ControlPlanes) ReadySince(cl discovery.TenantCluster) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.planes[cl.Namespace+"/"+cl.Name]
	if p == nil || (cl.UID != "" && p.UID != cl.UID) || len(p.Changes) == 0 {
		return time.Time{}, false
	}
	last := p.Changes[len(p.Changes)-1]
	if !last.Ready || last.Unknown {
		return time.Time{}, false
	}
	return last.At, true
}

// Tracks reports whether the workload of cl is being watched (it was seen
// and is not deleted).
func (c *ControlPlanes) Tracks(cl discovery.TenantCluster) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.planes[cl.Namespace+"/"+cl.Name]
	return p != nil && (cl.UID == "" || p.UID == cl.UID) && p.Deleted.IsZero()
}

// flush drops what no window can need any more and saves the timeline when
// it changed (always with beat, which records that vBilling is watching).
func (c *ControlPlanes) flush(beat bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	if beat {
		c.alive = now
		c.pruneLocked(now.Add(-c.retention))
	} else if !c.dirty {
		return
	}
	c.dirty = false
	if c.path == "" {
		return
	}
	b, err := json.Marshal(planesFile{Alive: c.alive, Planes: c.planes})
	if err == nil {
		err = writeAtomic(c.path, b)
	}
	if err != nil {
		log.Printf("[metrics] save the control plane timeline: %v (kept in memory)", err)
		c.dirty = true
	}
}

// pruneLocked forgets workloads deleted before cutoff, and changes that
// ended before it (the state at the cutoff is kept).
func (c *ControlPlanes) pruneLocked(cutoff time.Time) {
	for key, p := range c.planes {
		if !p.Deleted.IsZero() && p.Deleted.Before(cutoff) {
			delete(c.planes, key)
			continue
		}
		first := sort.Search(len(p.Changes), func(i int) bool { return !p.Changes[i].At.Before(cutoff) })
		if first > 1 {
			p.Changes = append([]planeChange(nil), p.Changes[first-1:]...)
		}
	}
}
