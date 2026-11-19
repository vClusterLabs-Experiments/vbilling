package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// With vcluster.yaml sync.toHost.namespaces, vCluster syncs a cluster's
// pods, volumes and services to namespaces of the control plane cluster of
// their own, not to its control plane namespace. They carry the cluster's
// name in vcluster.loft.sh/managed-by, which vCluster's syncer sets (a
// tenant's own label of that name is renamed on the way). They are metered
// with the cluster's own namespace, when the name says whose they are
// (discovery.TenantCluster.SyncedHere).

// syncedObjects are the objects vCluster synced, on the whole control plane
// cluster, by the cluster name they carry, read once per collection.
type syncedObjects struct {
	pods     map[string][]corev1.Pod
	pvcs     map[string][]corev1.PersistentVolumeClaim
	services map[string][]corev1.Service
}

// listSynced reads every object that carries vcluster.loft.sh/managed-by.
// It returns nil, with no error, when vBilling may not list them
// everywhere (its source reports it).
func (c *Collector) listSynced(ctx context.Context) (*syncedObjects, error) {
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy}
	pods, err := c.kube.CoreV1().Pods("").List(ctx, opts)
	if err == nil {
		var pvcs *corev1.PersistentVolumeClaimList
		if pvcs, err = c.kube.CoreV1().PersistentVolumeClaims("").List(ctx, opts); err == nil {
			var svcs *corev1.ServiceList
			if svcs, err = c.kube.CoreV1().Services("").List(ctx, opts); err == nil {
				c.reportSynced(nil)
				s := &syncedObjects{pods: map[string][]corev1.Pod{}, pvcs: map[string][]corev1.PersistentVolumeClaim{}, services: map[string][]corev1.Service{}}
				for _, p := range pods.Items {
					s.pods[p.Labels[LabelManagedBy]] = append(s.pods[p.Labels[LabelManagedBy]], p)
				}
				for _, p := range pvcs.Items {
					s.pvcs[p.Labels[LabelManagedBy]] = append(s.pvcs[p.Labels[LabelManagedBy]], p)
				}
				for _, svc := range svcs.Items {
					s.services[svc.Labels[LabelManagedBy]] = append(s.services[svc.Labels[LabelManagedBy]], svc)
				}
				return s, nil
			}
		}
	}
	if apierrors.IsForbidden(err) {
		c.reportSynced(fmt.Errorf("objects clusters synced to other namespaces cannot be listed, so they are not billed: %w", err))
		return nil, nil
	}
	return nil, fmt.Errorf("list objects synced to the control plane cluster: %w", err)
}

// elsewhere is what one cluster synced outside its own namespace.
type elsewhere struct {
	pods     []corev1.Pod
	pvcs     []corev1.PersistentVolumeClaim
	services []corev1.Service
	// ambiguous counts objects outside its namespace and its namesakes'
	// that carry its name while another vCluster has it too: they could be
	// either's, so they are billed to neither.
	ambiguous int
}

func (e *elsewhere) count() int { return len(e.pods) + len(e.pvcs) + len(e.services) }

// of returns the objects cl synced outside its own namespace.
func (s *syncedObjects) of(cl discovery.TenantCluster) elsewhere {
	var out elsewhere
	if s == nil || cl.External {
		return out
	}
	own := map[string]bool{cl.Namespace: true}
	for _, ns := range cl.Namesakes {
		own[ns] = true
	}
	// whose: 1 = cl's, 0 = not about cl, -1 = may be cl's or a namesake's.
	whose := func(m metav1.ObjectMeta) int {
		if m.Labels[LabelManagedBy] != cl.Name || own[m.Namespace] {
			return 0
		}
		if !cl.SyncedHere(m.Namespace) {
			return -1
		}
		return 1
	}
	for _, p := range s.pods[cl.Name] {
		switch whose(p.ObjectMeta) {
		case 1:
			out.pods = append(out.pods, p)
		case -1:
			out.ambiguous++
		}
	}
	for _, p := range s.pvcs[cl.Name] {
		switch whose(p.ObjectMeta) {
		case 1:
			out.pvcs = append(out.pvcs, p)
		case -1:
			out.ambiguous++
		}
	}
	for _, svc := range s.services[cl.Name] {
		switch whose(svc.ObjectMeta) {
		case 1:
			out.services = append(out.services, svc)
		case -1:
			out.ambiguous++
		}
	}
	return out
}

// deletedElsewhere returns the pods cl synced outside its own namespace
// that were deleted since the last collection.
func (c *Collector) deletedElsewhere(cl discovery.TenantCluster) []deleted[*corev1.Pod] {
	var out []deleted[*corev1.Pod]
	for _, d := range c.graveyard.Pods("") {
		if m := d.obj.ObjectMeta; m.Namespace != cl.Namespace && m.Labels[LabelManagedBy] == cl.Name && cl.SyncedHere(m.Namespace) {
			out = append(out, d)
		}
	}
	return out
}

// withNamesakes adds to each target's namesakes the other targets with its
// name, for clusters handed in without discovery's view.
func withNamesakes(targets []Target) []Target {
	byName := map[string][]string{}
	for _, t := range targets {
		if !t.Cluster.External {
			byName[t.Cluster.Name] = append(byName[t.Cluster.Name], t.Cluster.Namespace)
		}
	}
	out := make([]Target, len(targets))
	for i, t := range targets {
		out[i] = t
		if t.Cluster.External || len(byName[t.Cluster.Name]) < 2 {
			continue
		}
		seen := map[string]bool{t.Cluster.Namespace: true}
		ns := append([]string(nil), t.Cluster.Namesakes...)
		for _, n := range ns {
			seen[n] = true
		}
		for _, n := range byName[t.Cluster.Name] {
			if !seen[n] {
				seen[n] = true
				ns = append(ns, n)
			}
		}
		sort.Strings(ns)
		out[i].Cluster.Namesakes = ns
	}
	return out
}

// reportElsewhere reports, per cluster, objects synced to other namespaces:
// billed (ok), or not billed because they may be another cluster's
// (failing). A cluster with none is reported only to clear a report.
func (c *Collector) reportElsewhere(cl discovery.TenantCluster, e elsewhere) {
	id := cl.ExternalID()
	c.invMu.Lock()
	was := c.syncedReported[id]
	c.syncedReported[id] = e.count() > 0 || e.ambiguous > 0
	c.invMu.Unlock()
	if c.health == nil || !was && e.count() == 0 && e.ambiguous == 0 {
		return
	}
	src := health.Name("synced-namespaces", id)
	if e.ambiguous > 0 {
		why := "namespace syncing with a cluster name used twice: " + fmt.Sprint(e.ambiguous) +
			" object(s) in other namespaces carry " + LabelManagedBy + "=" + cl.Name + ", which another vCluster on the control plane cluster also has (in " +
			strings.Join(cl.Namesakes, ", ") + "); they are billed to neither"
		if cl.NamesakesUnknown {
			why = fmt.Sprint(e.ambiguous) + " object(s) in other namespaces carry " + LabelManagedBy + "=" + cl.Name +
				", and vBilling could not list every vCluster to know they are this cluster's; they are not billed"
		}
		c.health.Fail(src, "synced-namespaces", id, fmt.Errorf("%s", why),
			"give each vCluster that syncs to other namespaces (sync.toHost.namespaces) a name no other vCluster on the control plane cluster has")
		return
	}
	c.health.OK(src, "synced-namespaces", id, e.count())
}

// reportSynced reports whether objects synced to other namespaces can be
// listed at all (reported only when they cannot, and once when they can
// again).
func (c *Collector) reportSynced(err error) {
	c.invMu.Lock()
	was := c.syncedReported[""]
	c.syncedReported[""] = err != nil
	c.invMu.Unlock()
	if c.health == nil {
		return
	}
	src := health.Name("synced-namespaces")
	if err != nil {
		c.health.Fail(src, "synced-namespaces", "", err, "grant vBilling list on pods, persistentvolumeclaims and services in every namespace (the chart's ClusterRole does)")
		return
	}
	if was {
		c.health.OK(src, "synced-namespaces", "", 0)
	}
}
