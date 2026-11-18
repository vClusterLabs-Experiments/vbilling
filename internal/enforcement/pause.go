package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// annPaused marks a pause made with `vcluster pause`. vBilling never sets
// it (see discovery.AnnotationPaused) and never lifts a pause it did not make.
const annPaused = discovery.AnnotationPaused

// TaintPaused is the NoExecute taint vBilling puts on a paused cluster's
// own nodes: it evicts their workloads and keeps new ones off until resume.
const TaintPaused = "vbilling.vcluster.com/paused"

// labelManagedBy marks the pods a vCluster synced to the control plane
// cluster (what `vcluster pause` deletes).
const labelManagedBy = "vcluster.loft.sh/managed-by"

var pausedTaint = corev1.Taint{Key: TaintPaused, Value: "true", Effect: corev1.TaintEffectNoExecute}

// pauseCluster pauses one cluster, in this order:
//
//  1. mark it paused (vbilling.vcluster.com/paused-by, -at, -replicas):
//     capacity billing stops at this second;
//  2. inside the cluster, block new pods and drain its own nodes;
//  3. save its inventory, to meter its volumes from while its API is down;
//  4. scale its control plane to zero and delete the pods it synced to the
//     control plane cluster, as `vcluster pause` does.
//
// Every step is idempotent: a cluster resumed by anything else is paused
// again at the next resync.
func (e *Enforcer) pauseCluster(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	id := cl.ExternalID()
	if cl.External {
		return e.pauseExternal(ctx, cl, ts)
	}
	ann, replicas, err := e.workload(ctx, cl)
	if err != nil {
		return err
	}
	ours := ann[discovery.AnnotationPausedBy] == "vbilling"
	if (ours || ann[annPaused] == "true") && replicas == 0 {
		// Paused already (by vBilling or by an operator): keep it down.
		return e.deleteSyncedPods(ctx, cl)
	}
	if !ours {
		now := e.opts.Now().UTC()
		keep := replicas
		if keep < 1 {
			keep = 1
		}
		if err := e.patchWorkload(ctx, cl, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
			discovery.AnnotationPausedBy:       "vbilling",
			discovery.AnnotationPausedAt:       now.Format(time.RFC3339Nano),
			discovery.AnnotationPausedReplicas: strconv.Itoa(keep),
		}}}); err != nil {
			return fmt.Errorf("mark paused: %w", err)
		}
		e.began(id, now)
	}
	// Marked but still running: a pause interrupted by a restart, or one
	// scaled back up by hand. Drain and stop it.

	if e.opts.TenantClient != nil {
		kube, err := e.opts.TenantClient(ctx, cl)
		switch {
		case err != nil:
			log.Printf("[enforcement] %s: pause without draining its own nodes (no access to its API: %v)", id, err)
		case kube != nil:
			if err := e.policyInside(ctx, kube, ts); err != nil {
				log.Printf("[enforcement] %s: admission policy before pause: %v", id, err)
			}
			if left, err := e.drain(ctx, kube, false); err != nil {
				log.Printf("[enforcement] %s: drain before pause: %v", id, err)
			} else if left > 0 {
				log.Printf("[enforcement] %s: %d pod(s) on its own nodes tolerate the pause taint and keep running", id, left)
			}
		}
	}
	if e.opts.Freeze != nil {
		if err := e.opts.Freeze(ctx, cl); err != nil {
			log.Printf("[enforcement] %s: save inventory before pause: %v", id, err)
		}
	}
	if err := e.patchWorkload(ctx, cl, map[string]any{"spec": map[string]any{"replicas": 0}}); err != nil {
		return fmt.Errorf("scale control plane down: %w", err)
	}
	if err := e.deleteSyncedPods(ctx, cl); err != nil {
		return err
	}
	log.Printf("[enforcement] %s: paused (%s)", id, ts.Reason)
	e.opts.Telemetry.Add("vbilling_clusters_paused_total", "Clusters vBilling paused.", nil, 1)
	return nil
}

// pauseExternal pauses a cluster vBilling reaches only through its own API
// (vCluster Standalone, clusters elsewhere): its control plane is not
// vBilling's to stop, so the pause is the in-cluster policy plus drained
// nodes, re-applied at every resync.
func (e *Enforcer) pauseExternal(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	if e.opts.TenantClient == nil {
		return errors.New("pausing a cluster outside the control plane cluster needs ENFORCEMENT_IN_CLUSTER")
	}
	kube, err := e.opts.TenantClient(ctx, cl)
	if err != nil {
		return err
	}
	if kube == nil {
		return errors.New("no access to the cluster's API")
	}
	// Its API stays up, so it is metered live: only the span is recorded,
	// and metering is not told to read a saved inventory.
	if e.opts.Pauses != nil {
		if _, err := e.opts.Pauses.Begin(cl.ExternalID(), e.opts.Now().UTC(), "vbilling"); err != nil {
			log.Printf("[enforcement] %s: record pause: %v", cl.ExternalID(), err)
		}
	}
	if err := e.policyInside(ctx, kube, ts); err != nil {
		return err
	}
	_, err = e.drain(ctx, kube, true)
	return err
}

func (e *Enforcer) began(id string, at time.Time) {
	if e.opts.Pauses != nil {
		if _, err := e.opts.Pauses.Begin(id, at, "vbilling"); err != nil {
			log.Printf("[enforcement] %s: record pause: %v", id, err)
		}
	}
	if e.opts.OnPause != nil {
		e.opts.OnPause(id, true)
	}
}

func (e *Enforcer) ended(id string, at time.Time) {
	if e.opts.Pauses != nil {
		if _, err := e.opts.Pauses.End(id, at); err != nil {
			log.Printf("[enforcement] %s: record resume: %v", id, err)
		}
	}
	if e.opts.OnPause != nil {
		e.opts.OnPause(id, false)
	}
}

// resumeCluster resumes a cluster vBilling paused. Clusters paused by
// anyone else are left alone. It reports whether it scaled a control plane
// back up; the rest (lifting the taint, the admission policy) follows once
// that control plane answers.
func (e *Enforcer) resumeCluster(ctx context.Context, cl discovery.TenantCluster, ts TenantState) (bool, error) {
	id := cl.ExternalID()
	if cl.External {
		if e.opts.Pauses != nil {
			if sp, open := e.opts.Pauses.Current(id); open && sp.By == "vbilling" {
				if _, err := e.opts.Pauses.End(id, e.opts.Now().UTC()); err != nil {
					log.Printf("[enforcement] %s: record resume: %v", id, err)
				}
			}
		}
		return false, nil // reflectInside lifts the taint
	}
	ann, current, err := e.workload(ctx, cl)
	if err != nil {
		return false, err
	}
	if ann[discovery.AnnotationPausedBy] != "vbilling" {
		return false, nil
	}
	replicas, err := strconv.Atoi(ann[discovery.AnnotationPausedReplicas])
	if err != nil || replicas < 1 {
		replicas = 1
	}
	if current > 0 {
		replicas = current // already scaled back up by someone else: keep that
	}
	now := e.opts.Now().UTC()
	if err := e.patchWorkload(ctx, cl, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			discovery.AnnotationPausedBy: nil, discovery.AnnotationPausedAt: nil, discovery.AnnotationPausedReplicas: nil,
		}},
		"spec": map[string]any{"replicas": replicas},
	}); err != nil {
		return false, fmt.Errorf("scale control plane up: %w", err)
	}
	e.ended(id, now)
	log.Printf("[enforcement] %s: resumed (%d control plane replica(s))", id, replicas)
	e.opts.Telemetry.Add("vbilling_clusters_resumed_total", "Clusters vBilling resumed.", nil, 1)
	return true, nil
}

// finishResume waits for a resumed cluster's API, then lifts the pause
// taint from its own nodes and sets the admission policy for its tenant's
// current state. Resync does the same every few minutes if this gives up.
func (e *Enforcer) finishResume(ctx context.Context, tenant string, cl discovery.TenantCluster) error {
	ctx, cancel := context.WithTimeout(ctx, e.resumeTimeout)
	defer cancel()
	tick := time.NewTicker(e.resumeEvery)
	defer tick.Stop()
	cl.Paused = false
	for {
		ts := e.StateOf(tenant)
		if ts.State == Paused {
			return nil // paused again meanwhile: the worker loops
		}
		err := e.reflectInside(ctx, cl, ts)
		if err == nil {
			log.Printf("[enforcement] %s: workloads can run again", cl.ExternalID())
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("resumed control plane not reachable yet (%v); resync retries", err)
		case <-tick.C:
		}
	}
}

// PauseStatus is the progress of pausing or resuming one cluster.
type PauseStatus struct {
	Cluster string    `json:"cluster"`
	Tenant  string    `json:"tenant"`
	Phase   string    `json:"phase"` // pausing | paused | resuming | running
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

type worker struct{ running, dirty bool }

// kick converges a cluster's pause state in the background. Pausing waits
// for workloads to stop, so it never holds up an API call or a webhook.
// One worker per cluster serializes pauses and resumes: a resume never
// races a pause still in flight.
func (e *Enforcer) kick(tenant string, cl discovery.TenantCluster) {
	id := cl.ExternalID()
	e.wmu.Lock()
	w := e.workers[id]
	if w == nil {
		w = &worker{}
		e.workers[id] = w
	}
	if w.running {
		w.dirty = true
		e.wmu.Unlock()
		return
	}
	w.running = true
	e.wmu.Unlock()
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		for {
			e.work(tenant, cl)
			e.wmu.Lock()
			if !w.dirty {
				w.running = false
				e.wmu.Unlock()
				return
			}
			w.dirty = false
			e.wmu.Unlock()
		}
	}()
}

func (e *Enforcer) work(tenant string, cl discovery.TenantCluster) {
	id := cl.ExternalID()
	ctx, cancel := context.WithTimeout(context.Background(), e.drainTimeout()+e.resumeTimeout+time.Minute)
	defer cancel()
	ts := e.StateOf(tenant)
	var err error
	if ts.State == Paused {
		e.setStatus(id, tenant, "pausing", nil)
		if err = e.pauseCluster(ctx, cl, ts); err == nil {
			e.setStatus(id, tenant, "paused", nil)
			return
		}
	} else {
		var resumed bool
		resumed, err = e.resumeCluster(ctx, cl, ts)
		if err == nil && resumed {
			e.setStatus(id, tenant, "resuming", nil)
			if e.opts.TenantClient != nil {
				err = e.finishResume(ctx, tenant, cl)
			}
		} else if err == nil && e.opts.TenantClient != nil && !cl.Paused {
			err = e.reflectInside(ctx, cl, ts)
		}
		if err == nil {
			e.setStatus(id, tenant, "running", nil)
			return
		}
	}
	log.Printf("[enforcement] %s: %v", id, err)
	e.setStatus(id, tenant, "", err)
}

func (e *Enforcer) setStatus(id, tenant, phase string, err error) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	st := e.status[id]
	st.Cluster, st.Tenant, st.At = id, tenant, e.opts.Now().UTC()
	if phase != "" {
		st.Phase = phase
	}
	st.Error = ""
	if err != nil {
		st.Error = err.Error()
	}
	e.status[id] = st
}

// Pauses returns the pause progress of clusters vBilling paused or resumed.
func (e *Enforcer) Pauses() []PauseStatus {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	out := make([]PauseStatus, 0, len(e.status))
	for _, st := range e.status {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// needsWorker reports whether a cluster has a pause to make or undo.
func (e *Enforcer) needsWorker(ctx context.Context, cl discovery.TenantCluster, ts TenantState) bool {
	if ts.State == Paused {
		return true
	}
	id := cl.ExternalID()
	e.wmu.Lock()
	w := e.workers[id]
	busy := w != nil && w.running
	e.wmu.Unlock()
	if busy {
		return true
	}
	if cl.External {
		if e.opts.Pauses != nil {
			sp, open := e.opts.Pauses.Current(id)
			return open && sp.By == "vbilling"
		}
		return false
	}
	ann, _, err := e.workload(ctx, cl)
	return err == nil && ann[discovery.AnnotationPausedBy] == "vbilling"
}

func (e *Enforcer) drainTimeout() time.Duration {
	if e.opts.DrainTimeout > 0 {
		return e.opts.DrainTimeout
	}
	return 2 * time.Minute
}

// Guard pauses again, at once, any cluster of a paused tenant that is
// running: one scaled back up by hand, woken by vCluster Platform, or
// created while the tenant is paused. It is cheap (one read per cluster of
// a paused tenant) and runs every few seconds; Resync covers the rest.
func (e *Enforcer) Guard(ctx context.Context) {
	if e.opts.Mode != "enforce" || e.kube == nil || e.opts.Clusters == nil {
		return
	}
	for _, ts := range e.States() {
		if ts.State != Paused {
			continue
		}
		for _, cl := range e.opts.Clusters(ts.Tenant) {
			if cl.External {
				continue // nothing to scale: Resync drains it again
			}
			ann, replicas, err := e.workload(ctx, cl)
			if err != nil {
				continue
			}
			if replicas > 0 || ann[discovery.AnnotationPausedBy] != "vbilling" && ann[annPaused] != "true" {
				log.Printf("[enforcement] %s: running while its tenant is paused, pausing it again", cl.ExternalID())
				e.kick(ts.Tenant, cl)
			}
		}
	}
}

// Wait waits for background pauses and resumes to finish (tests, shutdown).
func (e *Enforcer) Wait() { e.bg.Wait() }

// drain taints the cluster's own nodes NoExecute and deletes the pods on
// them that do not tolerate it, then waits up to DrainTimeout for them to
// stop. Nodes of the control plane cluster (shared nodes) are never
// touched: their pods are the synced ones the pause deletes there. It
// returns the pods still running that tolerate the taint.
func (e *Enforcer) drain(ctx context.Context, kube kubernetes.Interface, external bool) (int, error) {
	host := map[string]bool{}
	if !external && e.kube != nil {
		nodes, err := e.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, fmt.Errorf("list control plane cluster nodes: %w", err)
		}
		for _, n := range nodes.Items {
			host[n.Name] = true
		}
	}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	own := map[string]bool{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if host[n.Name] || n.Labels["vcluster.loft.sh/fake-node"] == "true" {
			continue
		}
		own[n.Name] = true
		if hasTaint(n, TaintPaused) {
			continue
		}
		if err := e.updateNode(ctx, kube, n.Name, func(n *corev1.Node) bool {
			if hasTaint(n, TaintPaused) {
				return false
			}
			n.Spec.Taints = append(n.Spec.Taints, pausedTaint)
			return true
		}); err != nil {
			return 0, fmt.Errorf("taint node %s: %w", n.Name, err)
		}
	}
	if len(own) == 0 {
		return 0, nil
	}
	timeout := e.drainTimeout()
	deadline := time.Now().Add(timeout)
	for {
		pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, fmt.Errorf("list pods: %w", err)
		}
		running, tolerating := 0, 0
		for i := range pods.Items {
			p := &pods.Items[i]
			if !own[p.Spec.NodeName] || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			if tolerates(p, &pausedTaint) {
				tolerating++
				continue
			}
			running++
			if p.DeletionTimestamp == nil {
				err := kube.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return 0, fmt.Errorf("delete pod %s/%s: %w", p.Namespace, p.Name, err)
				}
			}
		}
		if running == 0 {
			return tolerating, nil
		}
		if time.Now().After(deadline) {
			return tolerating, fmt.Errorf("%d pod(s) still stopping after %s", running, timeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(min(2*time.Second, timeout/10+time.Millisecond)):
		}
	}
}

// untaint lifts the pause taint from every node of a cluster.
func (e *Enforcer) untaint(ctx context.Context, kube kubernetes.Interface) error {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		if !hasTaint(&nodes.Items[i], TaintPaused) {
			continue
		}
		if err := e.updateNode(ctx, kube, nodes.Items[i].Name, func(n *corev1.Node) bool {
			kept := n.Spec.Taints[:0]
			for _, t := range n.Spec.Taints {
				if t.Key != TaintPaused {
					kept = append(kept, t)
				}
			}
			changed := len(kept) != len(n.Spec.Taints)
			n.Spec.Taints = kept
			return changed
		}); err != nil {
			return fmt.Errorf("lift pause taint from node %s: %w", nodes.Items[i].Name, err)
		}
	}
	return nil
}

// updateNode applies change to a node, retrying on conflicts.
func (e *Enforcer) updateNode(ctx context.Context, kube kubernetes.Interface, name string, change func(*corev1.Node) bool) error {
	for attempt := 0; ; attempt++ {
		n, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !change(n) {
			return nil
		}
		_, err = kube.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		if err == nil || !apierrors.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

func hasTaint(n *corev1.Node, key string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

func tolerates(p *corev1.Pod, taint *corev1.Taint) bool {
	for i := range p.Spec.Tolerations {
		if p.Spec.Tolerations[i].ToleratesTaint(taint) {
			return true
		}
	}
	return false
}

// workload returns the annotations and replica count of a cluster's
// control plane StatefulSet or Deployment.
func (e *Enforcer) workload(ctx context.Context, cl discovery.TenantCluster) (map[string]string, int, error) {
	if cl.Source == "deployment" {
		d, err := e.kube.AppsV1().Deployments(cl.Namespace).Get(ctx, cl.Name, metav1.GetOptions{})
		if err != nil {
			return nil, 0, fmt.Errorf("get deployment: %w", err)
		}
		r := 1
		if d.Spec.Replicas != nil {
			r = int(*d.Spec.Replicas)
		}
		return d.Annotations, r, nil
	}
	s, err := e.kube.AppsV1().StatefulSets(cl.Namespace).Get(ctx, cl.Name, metav1.GetOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("get statefulset: %w", err)
	}
	r := 1
	if s.Spec.Replicas != nil {
		r = int(*s.Spec.Replicas)
	}
	return s.Annotations, r, nil
}

func (e *Enforcer) patchWorkload(ctx context.Context, cl discovery.TenantCluster, patch map[string]any) error {
	b, _ := json.Marshal(patch)
	var err error
	if cl.Source == "deployment" {
		_, err = e.kube.AppsV1().Deployments(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, b, metav1.PatchOptions{})
	} else {
		_, err = e.kube.AppsV1().StatefulSets(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, b, metav1.PatchOptions{})
	}
	return err
}

// deleteSyncedPods deletes the pods a cluster synced to the control plane
// cluster, as `vcluster pause` does. They come back on resume: the cluster
// still has them and syncs them again.
func (e *Enforcer) deleteSyncedPods(ctx context.Context, cl discovery.TenantCluster) error {
	pods, err := e.kube.CoreV1().Pods(cl.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + cl.Name})
	if err != nil {
		return fmt.Errorf("list synced pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if err := e.kube.CoreV1().Pods(cl.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete synced pod %s: %w", p.Name, err)
		}
	}
	return nil
}
