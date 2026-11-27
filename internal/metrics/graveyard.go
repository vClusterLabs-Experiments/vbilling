package metrics

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// Graveyard keeps pods and nodes deleted between two collections. Windows
// are metered from a list taken when the window closes, so without it a pod
// deleted before then (a short job, a pod evicted with its node, an Auto
// Node scaled down) would lose its last partial window, or all of its life.
// Deleted objects are kept for the retention period, then dropped. Deletions
// while vBilling itself is down are not seen.
//
// A deletion noticed only after a watch reconnects (the informer reports it
// with an unknown final state) is dated to the last collection that still
// listed the object, never later: an outage can under-bill, not over-bill.
//
// It also keeps what a list cannot tell about objects that still exist,
// when the watch saw it happen: deleted volumes and load balancers, when a
// volume was bound and resized, when a load balancer got its address, and
// when a node that is ready now was not (see WatchCapacity and timeline).
type Graveyard struct {
	mu        sync.Mutex
	retention time.Duration
	now       func() time.Time
	pods      map[types.UID]deleted[*corev1.Pod]
	nodes     map[string]deleted[*corev1.Node]
	seenPods  map[types.UID]time.Time
	seenNodes map[string]time.Time

	pvcs      map[types.UID]deleted[*corev1.PersistentVolumeClaim]
	services  map[types.UID]deleted[*corev1.Service]
	bound     map[types.UID]time.Time // when a claim was seen becoming Bound
	addressed map[types.UID]time.Time // when a load balancer was seen getting its address
	sizes     map[types.UID][]sizeAt  // a claim's capacity over time, once it was seen changing
	nodeDowns map[string][]pause.Span // the times a node was seen not ready, now over
}

// sizeAt is a claim's capacity from At on (a zero At: before the first
// change the watch saw).
type sizeAt struct {
	At    time.Time
	Bytes int64
}

type deleted[T any] struct {
	obj T
	at  time.Time
}

func NewGraveyard(retention time.Duration, now func() time.Time) *Graveyard {
	if now == nil {
		now = time.Now
	}
	return &Graveyard{retention: retention, now: now,
		pods: map[types.UID]deleted[*corev1.Pod]{}, nodes: map[string]deleted[*corev1.Node]{},
		seenPods: map[types.UID]time.Time{}, seenNodes: map[string]time.Time{},
		pvcs: map[types.UID]deleted[*corev1.PersistentVolumeClaim]{}, services: map[types.UID]deleted[*corev1.Service]{},
		bound: map[types.UID]time.Time{}, addressed: map[types.UID]time.Time{}, sizes: map[types.UID][]sizeAt{},
		nodeDowns: map[string][]pause.Span{}}
}

// Watch records pod and node deletions seen by informers on client until ctx
// ends. podSelector narrows the pods watched (for example the label vCluster
// puts on synced pods); watchPods=false watches nodes only. It returns once
// the informers have synced.
func (g *Graveyard) Watch(ctx context.Context, client kubernetes.Interface, podSelector string, watchPods, watchNodes bool) {
	var synced []cache.InformerSynced
	if watchPods {
		podFactory := informers.NewSharedInformerFactoryWithOptions(client, 0,
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = podSelector }))
		inf := podFactory.Core().V1().Pods().Informer()
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{DeleteFunc: func(obj any) {
			obj, uncertain := finalState(obj)
			if p, ok := obj.(*corev1.Pod); ok {
				g.recordPod(p, g.now(), uncertain)
			}
		}})
		synced = append(synced, inf.HasSynced)
		podFactory.Start(ctx.Done())
	}
	if watchNodes {
		factory := informers.NewSharedInformerFactory(client, 0)
		inf := factory.Core().V1().Nodes().Informer()
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, obj any) {
				if was, ok := old.(*corev1.Node); ok {
					if n, ok := obj.(*corev1.Node); ok {
						g.nodeChanged(was, n)
					}
				}
			},
			DeleteFunc: func(obj any) {
				obj, uncertain := finalState(obj)
				if n, ok := obj.(*corev1.Node); ok {
					g.recordNode(n, g.now(), uncertain)
				}
			}})
		synced = append(synced, inf.HasSynced)
		factory.Start(ctx.Done())
	}
	cache.WaitForCacheSync(ctx.Done(), synced...)
}

// WatchCapacity records, from informers on client until ctx ends, what a
// list taken when a window closes cannot tell about volumes and load
// balancers: those deleted since (with when), when a claim became Bound and
// when its capacity changed, and when a load balancer got its address.
// selector narrows what is watched (for example the label vCluster puts on
// synced objects; empty: everything). It returns once the informers have
// synced. Without it, or for what happened while vBilling was not watching,
// metering falls back to what the objects themselves say: a volume or load
// balancer deleted before a window closes is not billed for it, one is
// billed from its creation in the window it becomes billable in, and a
// resized volume at its new size for the whole window.
func (g *Graveyard) WatchCapacity(ctx context.Context, client kubernetes.Interface, selector string) {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }))
	claims := factory.Core().V1().PersistentVolumeClaims().Informer()
	_, _ = claims.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, obj any) {
			was, ok1 := old.(*corev1.PersistentVolumeClaim)
			pvc, ok2 := obj.(*corev1.PersistentVolumeClaim)
			if ok1 && ok2 {
				g.claimChanged(was, pvc)
			}
		},
		DeleteFunc: func(obj any) {
			obj, uncertain := finalState(obj)
			if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Status.Phase == corev1.ClaimBound && !uncertain {
				// A deletion only noticed after a watch reconnected is not
				// dated: nothing says how long the volume lived.
				g.RecordVolume(pvc, g.now())
			}
		},
	})
	services := factory.Core().V1().Services().Informer()
	_, _ = services.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, obj any) {
			was, ok1 := old.(*corev1.Service)
			svc, ok2 := obj.(*corev1.Service)
			if ok1 && ok2 && !hasAddress(was) && hasAddress(svc) {
				g.mu.Lock()
				g.addressed[svc.UID] = g.now()
				g.mu.Unlock()
			}
		},
		DeleteFunc: func(obj any) {
			obj, uncertain := finalState(obj)
			if svc, ok := obj.(*corev1.Service); ok && hasAddress(svc) && !uncertain {
				g.RecordLoadBalancer(svc, g.now())
			}
		},
	})
	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), claims.HasSynced, services.HasSynced)
}

// RecordVolume remembers a deleted claim and when it was deleted.
func (g *Graveyard) RecordVolume(pvc *corev1.PersistentVolumeClaim, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pvcs[pvc.UID] = deleted[*corev1.PersistentVolumeClaim]{obj: pvc, at: at}
}

// RecordLoadBalancer remembers a deleted Service and when it was deleted.
func (g *Graveyard) RecordLoadBalancer(svc *corev1.Service, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.services[svc.UID] = deleted[*corev1.Service]{obj: svc, at: at}
}

// hasAddress reports whether a Service is a load balancer with an address.
func hasAddress(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer && len(svc.Status.LoadBalancer.Ingress) > 0
}

// claimBytes is a claim's capacity: what it was given, else what it asked for.
func claimBytes(pvc *corev1.PersistentVolumeClaim) int64 {
	size := pvc.Status.Capacity[corev1.ResourceStorage]
	if size.IsZero() {
		size = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	return size.Value()
}

func (g *Graveyard) claimChanged(was, pvc *corev1.PersistentVolumeClaim) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if was.Status.Phase != corev1.ClaimBound && pvc.Status.Phase == corev1.ClaimBound {
		g.bound[pvc.UID] = now
	}
	if pvc.Status.Phase != corev1.ClaimBound || was.Status.Phase != corev1.ClaimBound {
		return
	}
	if before, after := claimBytes(was), claimBytes(pvc); before != after {
		if len(g.sizes[pvc.UID]) == 0 {
			g.sizes[pvc.UID] = []sizeAt{{Bytes: before}}
		}
		g.sizes[pvc.UID] = append(g.sizes[pvc.UID], sizeAt{At: now, Bytes: after})
	}
}

// nodeNotReady returns whether a node is not ready and since when, as its
// Ready condition says. A node whose kubelet went silent is down from its
// last status heartbeat, not from the Unknown mark that follows 40 to 50
// seconds later, as when it is metered while still down.
func nodeNotReady(n *corev1.Node) (bool, time.Time) {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			since := cond.LastTransitionTime.Time
			if cond.Status == corev1.ConditionUnknown {
				since = lastProofOfLife(cond.LastHeartbeatTime.Time, time.Time{}, since)
			}
			return cond.Status != corev1.ConditionTrue, since
		}
	}
	return false, time.Time{}
}

// nodeChanged records the downtime of a node that is ready again: from when
// it was marked not ready to when it was marked ready. A list taken later
// only shows a ready node.
func (g *Graveyard) nodeChanged(was, n *corev1.Node) {
	down, since := nodeNotReady(was)
	stillDown, readyAt := nodeNotReady(n)
	if !down || stillDown || since.IsZero() || !readyAt.After(since) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodeDowns[n.Name] = append(g.nodeDowns[n.Name], pause.Span{From: since, To: readyAt})
}

// NodeDowns returns the times a node was seen not ready that are over.
func (g *Graveyard) NodeDowns(name string) []pause.Span {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]pause.Span(nil), g.nodeDowns[name]...)
}

// volumeEntry is a volume to meter: from when it is billed (when it was
// seen becoming Bound, else its creation), until when (zero: it exists),
// and its capacity over time when that was seen changing.
type volumeEntry struct {
	pvc       *corev1.PersistentVolumeClaim
	from      time.Time
	deletedAt time.Time
	sizes     []sizeAt
}

// lbEntry is a load balancer to meter: from when it is billed (when it was
// seen getting its address, else its creation) and until when.
type lbEntry struct {
	svc       *corev1.Service
	from      time.Time
	deletedAt time.Time
}

// Volumes returns live claims and those of namespace ("" = all) deleted
// since, with what the watch knows of each. A nil Graveyard knows nothing:
// every live claim is billed from its creation at its current size.
func (g *Graveyard) Volumes(live []corev1.PersistentVolumeClaim, namespace string) []volumeEntry {
	return g.volumes(live, func(ns string) bool { return namespace == "" || ns == namespace })
}

// KnownVolumes is Volumes without the deleted ones: for claims whose
// namespace is not one cluster's alone.
func (g *Graveyard) KnownVolumes(live []corev1.PersistentVolumeClaim) []volumeEntry {
	return g.volumes(live, func(string) bool { return false })
}

func (g *Graveyard) volumes(live []corev1.PersistentVolumeClaim, deletedIn func(namespace string) bool) []volumeEntry {
	out := make([]volumeEntry, 0, len(live))
	if g == nil {
		for i := range live {
			out = append(out, volumeEntry{pvc: &live[i], from: live[i].CreationTimestamp.Time})
		}
		return out
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	seen := make(map[types.UID]bool, len(live))
	entry := func(pvc *corev1.PersistentVolumeClaim) volumeEntry {
		e := volumeEntry{pvc: pvc, from: pvc.CreationTimestamp.Time, sizes: g.sizes[pvc.UID]}
		if at, ok := g.bound[pvc.UID]; ok && at.After(e.from) {
			e.from = at
		}
		return e
	}
	for i := range live {
		seen[live[i].UID] = true
		out = append(out, entry(&live[i]))
	}
	for uid, d := range g.pvcs {
		if !seen[uid] && deletedIn(d.obj.Namespace) {
			e := entry(d.obj)
			e.deletedAt = d.at
			out = append(out, e)
		}
	}
	return out
}

// LoadBalancers returns live Services and those of namespace ("" = all)
// deleted since, with what the watch knows of each.
func (g *Graveyard) LoadBalancers(live []corev1.Service, namespace string) []lbEntry {
	return g.loadBalancers(live, func(ns string) bool { return namespace == "" || ns == namespace })
}

// KnownLoadBalancers is LoadBalancers without the deleted ones.
func (g *Graveyard) KnownLoadBalancers(live []corev1.Service) []lbEntry {
	return g.loadBalancers(live, func(string) bool { return false })
}

func (g *Graveyard) loadBalancers(live []corev1.Service, deletedIn func(namespace string) bool) []lbEntry {
	out := make([]lbEntry, 0, len(live))
	if g == nil {
		for i := range live {
			out = append(out, lbEntry{svc: &live[i], from: live[i].CreationTimestamp.Time})
		}
		return out
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	seen := make(map[types.UID]bool, len(live))
	entry := func(svc *corev1.Service) lbEntry {
		e := lbEntry{svc: svc, from: svc.CreationTimestamp.Time}
		if at, ok := g.addressed[svc.UID]; ok && at.After(e.from) {
			e.from = at
		}
		return e
	}
	for i := range live {
		seen[live[i].UID] = true
		out = append(out, entry(&live[i]))
	}
	for uid, d := range g.services {
		if !seen[uid] && deletedIn(d.obj.Namespace) {
			e := entry(d.obj)
			e.deletedAt = d.at
			out = append(out, e)
		}
	}
	return out
}

func finalState(obj any) (any, bool) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return tomb.Obj, true
	}
	return obj, false
}

// RecordPod remembers a deleted pod and when it was deleted.
func (g *Graveyard) RecordPod(p *corev1.Pod, at time.Time) { g.recordPod(p, at, false) }

// RecordNode remembers a deleted node and when it was deleted.
func (g *Graveyard) RecordNode(n *corev1.Node, at time.Time) { g.recordNode(n, at, false) }

func (g *Graveyard) recordPod(p *corev1.Pod, at time.Time, uncertain bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if seen, ok := g.seenPods[p.UID]; uncertain && ok && seen.Before(at) {
		at = seen
	}
	g.pods[p.UID] = deleted[*corev1.Pod]{obj: p, at: at}
}

func (g *Graveyard) recordNode(n *corev1.Node, at time.Time, uncertain bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if seen, ok := g.seenNodes[n.Name]; uncertain && ok && seen.Before(at) {
		at = seen
	}
	g.nodes[n.Name] = deleted[*corev1.Node]{obj: n, at: at}
}

// SeenNodes records that a collection listed these nodes at time at.
func (g *Graveyard) SeenNodes(nodes []corev1.Node, at time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range nodes {
		g.seenNodes[nodes[i].Name] = at
	}
}

// SeenPods records that a collection listed these pods at time at.
func (g *Graveyard) SeenPods(pods []corev1.Pod, at time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range pods {
		g.seenPods[pods[i].UID] = at
	}
}

// Pods returns deleted pods of a namespace ("" = all) still in retention.
func (g *Graveyard) Pods(namespace string) []deleted[*corev1.Pod] {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	var out []deleted[*corev1.Pod]
	for _, d := range g.pods {
		if namespace == "" || d.obj.Namespace == namespace {
			out = append(out, d)
		}
	}
	return out
}

// Nodes returns deleted nodes still in retention.
func (g *Graveyard) Nodes() []deleted[*corev1.Node] {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked()
	out := make([]deleted[*corev1.Node], 0, len(g.nodes))
	for _, d := range g.nodes {
		out = append(out, d)
	}
	return out
}

func (g *Graveyard) pruneLocked() {
	cutoff := g.now().Add(-g.retention)
	for k, at := range g.seenPods {
		if at.Before(cutoff) {
			delete(g.seenPods, k)
		}
	}
	for k, at := range g.seenNodes {
		if at.Before(cutoff) {
			delete(g.seenNodes, k)
		}
	}
	for k, d := range g.pods {
		if d.at.Before(cutoff) {
			delete(g.pods, k)
		}
	}
	for k, d := range g.nodes {
		if d.at.Before(cutoff) {
			delete(g.nodes, k)
		}
	}
	for k, d := range g.pvcs {
		if d.at.Before(cutoff) {
			delete(g.pvcs, k)
			delete(g.bound, k)
			delete(g.sizes, k)
		}
	}
	for k, d := range g.services {
		if d.at.Before(cutoff) {
			delete(g.services, k)
			delete(g.addressed, k)
		}
	}
	// A time older than every window that can still be metered changes
	// nothing any more: the object is billed for all of each window.
	for k, at := range g.bound {
		if at.Before(cutoff) {
			delete(g.bound, k)
		}
	}
	for k, at := range g.addressed {
		if at.Before(cutoff) {
			delete(g.addressed, k)
		}
	}
	for k, sz := range g.sizes {
		if n := len(sz); n > 0 && sz[n-1].At.Before(cutoff) {
			delete(g.sizes, k)
		}
	}
	for k, spans := range g.nodeDowns {
		kept := spans[:0]
		for _, sp := range spans {
			if !sp.To.Before(cutoff) {
				kept = append(kept, sp)
			}
		}
		if len(kept) == 0 {
			delete(g.nodeDowns, k)
		} else {
			g.nodeDowns[k] = kept
		}
	}
}
