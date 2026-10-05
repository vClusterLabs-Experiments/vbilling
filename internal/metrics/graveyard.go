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
type Graveyard struct {
	mu        sync.Mutex
	retention time.Duration
	now       func() time.Time
	pods      map[types.UID]deleted[*corev1.Pod]
	nodes     map[string]deleted[*corev1.Node]
	seenPods  map[types.UID]time.Time
	seenNodes map[string]time.Time
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
		seenPods: map[types.UID]time.Time{}, seenNodes: map[string]time.Time{}}
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
		_, _ = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{DeleteFunc: func(obj any) {
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
}
