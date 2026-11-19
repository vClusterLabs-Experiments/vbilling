package metrics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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
// tenant's own label of that name is renamed on the way), and vCluster
// labels each of those namespaces with the cluster's name and namespace.
// They are metered with the cluster's own namespace when
// discovery.HostNamespaces.Whose says they are the cluster's, and the
// namespaces they are in scope its Prometheus series too.

// syncedObjects are the objects vCluster synced, on the whole control plane
// cluster, by the cluster name they carry, read once per collection.
type syncedObjects struct {
	pods     map[string][]corev1.Pod
	pvcs     map[string][]corev1.PersistentVolumeClaim
	services map[string][]corev1.Service
}

// listSynced reads every object that carries vcluster.loft.sh/managed-by, a
// page at a time. When vBilling may not list them everywhere it returns no
// objects and, as denied, why.
func (c *Collector) listSynced(ctx context.Context) (objs *syncedObjects, denied, err error) {
	opts := metav1.ListOptions{LabelSelector: LabelManagedBy}
	pods, err := discovery.ListPaged(ctx, opts, c.kube.CoreV1().Pods("").List)
	if err == nil {
		var pvcs *corev1.PersistentVolumeClaimList
		if pvcs, err = discovery.ListPaged(ctx, opts, c.kube.CoreV1().PersistentVolumeClaims("").List); err == nil {
			var svcs *corev1.ServiceList
			if svcs, err = discovery.ListPaged(ctx, opts, c.kube.CoreV1().Services("").List); err == nil {
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
				return s, nil, nil
			}
		}
	}
	if apierrors.IsForbidden(err) {
		return nil, fmt.Errorf("objects clusters synced to other namespaces cannot be listed, so they are not billed: %w", err), nil
	}
	return nil, nil, fmt.Errorf("list objects synced to the control plane cluster: %w", err)
}

// elsewhere is what one cluster synced outside its own namespace.
type elsewhere struct {
	pods     []corev1.Pod
	gone     []deleted[*corev1.Pod] // its pods there deleted since the last collection
	pvcs     []corev1.PersistentVolumeClaim
	services []corev1.Service
	// namespaces are the other namespaces its objects (live or deleted)
	// are in: they scope its Prometheus series too.
	namespaces []string
	// uncertain counts, by reason, objects in other namespaces that carry
	// its name and may be its or another cluster's: they are billed to
	// neither. uncertainIn are their namespaces.
	uncertain   map[string]int
	uncertainIn []string
}

func (e *elsewhere) count() int { return len(e.pods) + len(e.pvcs) + len(e.services) }

// uncertainCount is how many objects may be the cluster's and are billed to
// no one (none for a nil e).
func (e *elsewhere) uncertainCount() int {
	if e == nil {
		return 0
	}
	n := 0
	for _, v := range e.uncertain {
		n += v
	}
	return n
}

// syncedNamespaces are the other namespaces the cluster's objects are in
// (none for a nil e).
func (e *elsewhere) syncedNamespaces() []string {
	if e == nil {
		return nil
	}
	return e.namespaces
}

// syncedView is what one read of the objects clusters synced found.
type syncedView struct {
	at time.Time
	// err: they could not be read, so nothing is known of any cluster's
	// objects in other namespaces.
	err error
	of  map[string]*elsewhere // by cluster external ID
}

// remembered is a namespace as vBilling last read it.
type remembered struct {
	labels map[string]string
	at     time.Time
}

// hostMemoryFor is how long vBilling remembers namespaces: as long as the
// graveyard keeps deleted pods, at least a day.
func (c *Collector) hostMemoryFor() time.Duration {
	if c.graveyard != nil && c.graveyard.retention > 24*time.Hour {
		return c.graveyard.retention
	}
	return 24 * time.Hour
}

// readSynced reads the objects clusters synced and decides, for each
// target, which of those outside its namespace are its
// (discovery.HostNamespaces.Whose). Two memories make up for what the
// control plane cluster no longer shows: the labels of namespaces read
// before (pods deleted with their namespace are its as they were when it
// existed), and the namespaces clusters ran in (what a deleted cluster
// left there is never another's).
func (c *Collector) readSynced(ctx context.Context, targets []Target) (*syncedView, error) {
	now := c.opts.Now().UTC()
	v := &syncedView{at: now, of: map[string]*elsewhere{}}
	objs, denied, err := c.listSynced(ctx)
	if err != nil {
		return nil, err
	}
	if denied != nil {
		return c.deniedSynced(v, denied), nil
	}
	gone := c.graveyard.Pods("")

	named := map[string][]discovery.TenantCluster{}
	current := map[string]bool{} // namespaces clusters run in
	for _, t := range targets {
		if t.Cluster.External {
			continue
		}
		named[t.Cluster.Name] = append(named[t.Cluster.Name], t.Cluster)
		current[t.Cluster.Namespace] = true
		for _, ns := range t.Cluster.Namesakes {
			current[ns] = true
		}
	}
	// The namespaces to read: those holding objects that carry a cluster's
	// name outside the namespace every cluster runs in.
	want := map[string]bool{}
	note := func(name, ns string) {
		if len(named[name]) > 0 && !current[ns] {
			want[ns] = true
		}
	}
	for name, l := range objs.pods {
		for i := range l {
			note(name, l[i].Namespace)
		}
	}
	for name, l := range objs.pvcs {
		for i := range l {
			note(name, l[i].Namespace)
		}
	}
	for name, l := range objs.services {
		for i := range l {
			note(name, l[i].Namespace)
		}
	}
	for _, d := range gone {
		note(d.obj.Labels[LabelManagedBy], d.obj.Namespace)
	}
	read := sortedKeys(want)
	host, err := c.hostView(ctx, read, current)
	if err != nil {
		return nil, fmt.Errorf("read the namespaces clusters synced to: %w", err)
	}
	if host.Err() != nil {
		return c.deniedSynced(v, fmt.Errorf("the namespaces clusters synced objects to cannot be read, so those objects are not billed: %w", host.Err())), nil
	}
	c.reportSynced(nil)

	for _, t := range targets {
		cl := t.Cluster
		if cl.External || v.of[cl.ExternalID()] != nil {
			continue
		}
		e := &elsewhere{uncertain: map[string]int{}}
		in, unsure := map[string]bool{}, map[string]bool{}
		// mine decides an object outside the cluster's namespace. A pod
		// deleted with a namespace vBilling never read is not reported:
		// nothing says it was this cluster's, and it no longer runs.
		mine := func(ns string, deletedObj bool) bool {
			if ns == cl.Namespace {
				return false // its own namespace: metered there
			}
			own, why := host.Whose(cl, ns)
			switch {
			case own == discovery.Owned:
				in[ns] = true
				return true
			case own == discovery.Uncertain && !(deletedObj && why == discovery.ReasonMissing):
				e.uncertain[why]++
				unsure[ns] = true
			}
			return false
		}
		for _, p := range objs.pods[cl.Name] {
			if mine(p.Namespace, false) {
				e.pods = append(e.pods, p)
			}
		}
		for _, p := range objs.pvcs[cl.Name] {
			if mine(p.Namespace, false) {
				e.pvcs = append(e.pvcs, p)
			}
		}
		for _, svc := range objs.services[cl.Name] {
			if mine(svc.Namespace, false) {
				e.services = append(e.services, svc)
			}
		}
		for _, d := range gone {
			if d.obj.Labels[LabelManagedBy] == cl.Name && mine(d.obj.Namespace, true) {
				e.gone = append(e.gone, d)
			}
		}
		e.namespaces, e.uncertainIn = sortedKeys(in), sortedKeys(unsure)
		v.of[cl.ExternalID()] = e
	}

	cutoff := now.Add(-c.hostMemoryFor())
	c.hostMu.Lock()
	for _, ns := range read {
		if exists, labels, known := host.Lookup(ns); known && exists {
			c.hostSeen[ns] = remembered{labels: labels, at: now}
		}
	}
	for ns := range current {
		c.hostRan[ns] = now
	}
	for _, ns := range host.ControlPlanes() {
		c.hostRan[ns] = now
	}
	for ns, r := range c.hostSeen {
		if !r.at.After(cutoff) {
			delete(c.hostSeen, ns)
		}
	}
	for ns, at := range c.hostRan {
		if !at.After(cutoff) {
			delete(c.hostRan, ns)
		}
	}
	c.synced = v
	c.hostMu.Unlock()
	return v, nil
}

// hostView reads the namespaces given (discovery.ReadHostNamespaces) and
// adds what vBilling remembers: a namespace gone since it was read is as it
// was then, and namespaces clusters ran in that are not in current (the
// ones they run in now) hold what those clusters left.
func (c *Collector) hostView(ctx context.Context, read []string, current map[string]bool) (*discovery.HostNamespaces, error) {
	host, err := discovery.ReadHostNamespaces(ctx, c.kube, read)
	if err != nil || host.Err() != nil {
		return host, err
	}
	cutoff := c.opts.Now().Add(-c.hostMemoryFor())
	former := map[string]bool{}
	c.hostMu.Lock()
	for ns, at := range c.hostRan {
		if at.After(cutoff) && !current[ns] {
			former[ns] = true
		}
	}
	for _, ns := range read {
		if exists, _, _ := host.Lookup(ns); !exists {
			if r, ok := c.hostSeen[ns]; ok && r.at.After(cutoff) {
				host.Note(ns, true, r.labels)
			}
		}
	}
	c.hostMu.Unlock()
	host.UseClusters(func(ns string) bool { return current[ns] }, func(ns string) bool { return former[ns] })
	return host, nil
}

// syncedPodsOf returns the pods cl synced to the control plane cluster,
// deleted ones included, decided as readSynced decides: in its namespace,
// and in the other namespaces that are its. fieldSelector narrows the live
// pods listed (deleted ones are not narrowed).
func (c *Collector) syncedPodsOf(ctx context.Context, cl discovery.TenantCluster, fieldSelector string) ([]*corev1.Pod, error) {
	live, err := discovery.ListPaged(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + cl.Name, FieldSelector: fieldSelector},
		c.kube.CoreV1().Pods("").List)
	if err != nil {
		return nil, fmt.Errorf("list pods synced to the control plane cluster: %w", err)
	}
	entries := withDeleted(live.Items, c.graveyard.Pods(""))
	other := map[string]bool{}
	for _, e := range entries {
		if p := e.pod; p.Labels[LabelManagedBy] == cl.Name && p.Namespace != cl.Namespace {
			other[p.Namespace] = true
		}
	}
	host, err := c.hostView(ctx, sortedKeys(other), nil)
	if err != nil {
		return nil, err
	}
	var out []*corev1.Pod
	for _, e := range entries {
		if p := e.pod; p.Labels[LabelManagedBy] == cl.Name {
			if own, _ := host.Whose(cl, p.Namespace); own == discovery.Owned {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

// deniedSynced records a read of synced objects that vBilling was not
// allowed to make: nothing is known of any cluster's objects elsewhere.
func (c *Collector) deniedSynced(v *syncedView, why error) *syncedView {
	c.reportSynced(why)
	v.err = why
	c.hostMu.Lock()
	c.synced = v
	c.hostMu.Unlock()
	return v
}

// awayOf returns what a cluster synced outside its namespace (nothing when
// that could not be read).
func (v *syncedView) awayOf(cl discovery.TenantCluster) *elsewhere {
	if v != nil {
		if e := v.of[cl.ExternalID()]; e != nil {
			return e
		}
	}
	return &elsewhere{}
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// syncedFor returns a read of the objects clusters synced that covers the
// targets and was made after fresh (the end of the windows being read):
// the collection's own, or a new one.
func (c *Collector) syncedFor(ctx context.Context, targets []Target, fresh time.Time) (*syncedView, error) {
	c.hostMu.Lock()
	v := c.synced
	c.hostMu.Unlock()
	if v != nil && !v.at.Before(fresh) {
		covered := true
		for _, t := range targets {
			if _, ok := v.of[t.Cluster.ExternalID()]; !ok && !t.Cluster.External && v.err == nil {
				covered = false
				break
			}
		}
		if covered {
			return v, nil
		}
	}
	return c.readSynced(ctx, targets)
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
// billed (ok), or not billed because nothing tells they are its (failing).
// A cluster with none is reported only to clear a report.
func (c *Collector) reportElsewhere(cl discovery.TenantCluster, e *elsewhere) {
	id := cl.ExternalID()
	unsure := e.uncertainCount()
	c.invMu.Lock()
	was := c.syncedReported[id]
	c.syncedReported[id] = e.count() > 0 || unsure > 0
	c.invMu.Unlock()
	if c.health == nil || !was && e.count() == 0 && unsure == 0 {
		return
	}
	src := health.Name("synced-namespaces", id)
	if unsure > 0 {
		c.health.Fail(src, "synced-namespaces", id, fmt.Errorf("%s", e.why(cl)),
			"vCluster labels each namespace it syncs a cluster's objects into with "+discovery.LabelSyncedName+" and "+
				discovery.LabelSyncedNamespace+"; restore those labels, or give the cluster a name no other cluster on the control plane cluster has")
		return
	}
	c.health.OK(src, "synced-namespaces", id, e.count())
}

// why explains the objects that are not billed.
func (e *elsewhere) why(cl discovery.TenantCluster) string {
	reasons := make([]string, 0, len(e.uncertain))
	for r := range e.uncertain {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	var parts []string
	for _, r := range reasons {
		n := e.uncertain[r]
		switch r {
		case discovery.ReasonNameShared:
			if cl.NamesakesUnknown {
				parts = append(parts, fmt.Sprintf("%d in namespaces without vCluster's labels, and vBilling could not list every cluster to know they are this cluster's", n))
			} else {
				parts = append(parts, fmt.Sprintf("%s: %d in namespaces without vCluster's labels, and another cluster on the control plane cluster has the name (in %s)",
					r, n, strings.Join(cl.Namesakes, ", ")))
			}
		default:
			parts = append(parts, fmt.Sprintf("%d %s", n, r))
		}
	}
	return fmt.Sprintf("%d object(s) in other namespaces (%s) carry %s=%s and are billed to neither, nor is their Prometheus usage: %s",
		e.uncertainCount(), strings.Join(e.uncertainIn, ", "), LabelManagedBy, cl.Name, strings.Join(parts, "; "))
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
		c.health.Fail(src, "synced-namespaces", "", err, "grant vBilling list on pods, persistentvolumeclaims and services and get on namespaces in every namespace (the chart's ClusterRole does)")
		return
	}
	if was {
		c.health.OK(src, "synced-namespaces", "", 0)
	}
}
