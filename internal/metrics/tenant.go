package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Tenant clusters with their own nodes (vCluster Private Nodes, Auto Nodes,
// Standalone) run workloads the control plane cluster never sees: nothing is
// synced to it. Those tenant clusters are metered through their own API.

// ErrNoTenantAPI means no credentials are configured for a tenant cluster's
// own API; it is then metered from the control plane cluster only.
var ErrNoTenantAPI = errors.New("no tenant API credentials")

// LabelFakeNode marks the placeholder nodes vCluster creates for clusters on
// shared nodes. It is written inside the cluster, so it decides nothing
// (hostViews); it is read only from inventories saved by earlier versions.
// LabelBillable=false exempts a dedicated node of the control plane cluster
// (for example hardware the tenant owns), which the operator labels. On a
// cluster's own nodes it is ignored: whoever runs the cluster writes those.
const (
	LabelFakeNode = "vcluster.loft.sh/fake-node"
	LabelBillable = "vbilling.vcluster.com/billable"
)

// TenantConn is a connection to a tenant cluster's own API server.
type TenantConn struct {
	Kube      kubernetes.Interface
	Metrics   metricsclient.Interface // optional, usage-based CPU/memory in usage mode
	Dynamic   dynamic.Interface       // optional, DRA ResourceClaims
	Graveyard *Graveyard              // optional, nodes and pods deleted between collections
	// External tenant clusters (Standalone, or anything outside this control
	// plane cluster) share no nodes with it, so no node is excluded.
	External bool
}

// TenantAPI resolves connections to tenant clusters' own API servers.
type TenantAPI interface {
	Conn(ctx context.Context, cl discovery.TenantCluster) (*TenantConn, error)
}

// UseTenantAPI meters tenant clusters through their own API as well.
func (c *Collector) UseTenantAPI(t TenantAPI) { c.tenants = t }

// CollectTenantAPI meters only what tenant APIs see, for windows whose
// collection could not reach a tenant API. Its events go to the ledger as
// late events, deduplicated by ID. Their IDs are salted: a cluster's own
// volumes and load balancers have the same identity as events the control
// plane cluster side may already have committed for that window, and a fill
// must never be dropped as their duplicate. The salt derives from the
// event's identity, so a refilled window still deduplicates against itself.
func (c *Collector) CollectTenantAPI(ctx context.Context, targets []Target, windows []Window) (map[time.Time][]usage.Event, Stats, error) {
	out, st, err := c.collect(ctx, targets, windows, true)
	for _, evs := range out {
		for i := range evs {
			evs[i].ID = usage.DeriveIDWith(&evs[i], "fill")
		}
	}
	return out, st, err
}

// collectTenant runs the tenant-API part for one target. All-or-nothing: on
// error nothing is added, and the target is reported in Stats so its
// windows can be filled once the API is reachable again.
//
// A paused cluster's API is down by design. It is metered from the
// inventory read just before it paused (nothing in it can change while the
// control plane is stopped), so its volumes keep billing and its nodes stop,
// per PAUSE_KEEP_BILLING. Without a stored inventory its windows are filled
// after it resumes, like an outage.
//
// syncedOn are the control plane cluster nodes the cluster's synced pods run
// on (hostViews), nil when they were not listed.
func (c *Collector) collectTenant(ctx context.Context, t Target, hostNodes map[string]*nodeInfo, syncedOn map[string]bool, windows []Window, accs map[time.Time]*accumulator, st *Stats) {
	id := t.Cluster.ExternalID()
	tmp := map[time.Time]*accumulator{}
	for _, w := range windows {
		tmp[w.End] = newAccumulator(w)
	}
	if !t.Cluster.DeletedAt.IsZero() {
		// Deleted: it has no API any more, and that is not an outage to
		// fill later. What it had when it was last read is metered up to
		// its deletion (Target.Excluded ends everything there).
		c.invMu.Lock()
		inv := c.lastInv[id]
		c.invMu.Unlock()
		if inv == nil {
			return
		}
		if err := c.meterTenant(ctx, t, &TenantConn{External: t.Cluster.External}, inv, storedViews(inv, hostNodes, t.Cluster.External), windows, tmp, st); err != nil {
			log.Printf("[metrics] %s: metering its last inventory: %v", id, err)
			return
		}
		mergeAccs(tmp, accs)
		return
	}
	if t.Cluster.Paused {
		if _, err := c.tenants.Conn(ctx, t.Cluster); errors.Is(err, ErrNoTenantAPI) {
			c.noteOwnNodes(id, false)
			return // metered from the control plane cluster only
		}
		inv, ok := c.pausedInventory(id)
		if !ok {
			st.TenantAPIFailed = append(st.TenantAPIFailed, id)
			if c.health != nil {
				c.health.Set(health.Name("tenant-api", id), "tenant-api", id, health.Paused,
					"paused: its API is stopped and no inventory was saved before the pause",
					"its volumes and load balancers are metered when it resumes (windows within MAX_BACKFILL)")
			}
			return
		}
		if err := c.meterTenant(ctx, t, &TenantConn{External: t.Cluster.External}, inv, storedViews(inv, hostNodes, t.Cluster.External), windows, tmp, st); err != nil {
			log.Printf("[metrics] %s: paused inventory: %v", id, err)
			st.TenantAPIFailed = append(st.TenantAPIFailed, id)
			return
		}
		if c.health != nil {
			c.health.Set(health.Name("tenant-api", id), "tenant-api", id, health.Paused,
				"paused: metered from the inventory saved at "+inv.At.Format(time.RFC3339), "")
		}
		mergeAccs(tmp, accs)
		return
	}
	conn, err := c.tenants.Conn(ctx, t.Cluster)
	if errors.Is(err, ErrNoTenantAPI) {
		c.noteOwnNodes(id, false) // nothing inside it is metered
		return
	}
	if err == nil {
		err = c.collectTenantAPI(ctx, t, conn, hostNodes, syncedOn, windows, tmp, st)
	}
	if err != nil {
		var pending *HeartbeatPending
		if errors.As(err, &pending) {
			st.PendingHeartbeat = pending
			return
		}
		log.Printf("[metrics] %s: tenant API unavailable, its windows will be filled later: %v", id, err)
		st.TenantAPIFailed = append(st.TenantAPIFailed, id)
		if c.health != nil {
			c.health.Fail(health.Name("tenant-api", id), "tenant-api", id, err,
				"the cluster's own nodes, volumes and load balancers are not metered until its API answers; missed windows are filled then")
		}
		return
	}
	if c.health != nil {
		c.health.OK(health.Name("tenant-api", id), "tenant-api", id, 1)
	}
	mergeAccs(tmp, accs)
}

func mergeAccs(from, into map[time.Time]*accumulator) {
	for end, a := range from {
		for _, k := range a.order {
			ev := *a.events[k]
			qty, props := ev.Quantity, ev.Properties
			ev.Quantity, ev.Properties = 0, nil
			into[end].add(ev, qty, props)
		}
	}
}

// tenantInventory is what a cluster's own API holds that vBilling meters
// as capacity: its nodes, volumes and load balancers.
type tenantInventory struct {
	At       time.Time                      `json:"at"`
	Nodes    []corev1.Node                  `json:"nodes"`
	PVCs     []corev1.PersistentVolumeClaim `json:"pvcs"`
	Services []corev1.Service               `json:"services"`
	// Leases: last renewal of each node whose kubelet went silent.
	Leases map[string]time.Time `json:"leases,omitempty"`
	// Pods and the time each deleted one was deleted (by UID), read with
	// the rest when PRIVATE_NODE_BILLING=usage: metered from a stored
	// inventory, a paused cluster bills its workloads as they were when it
	// was read, whoever paused it.
	Pods        []corev1.Pod         `json:"pods,omitempty"`
	PodsDeleted map[string]time.Time `json:"podsDeleted,omitempty"`
	// ExcludedNamespaces are the cluster's namespaces labeled
	// vbilling.vcluster.com/exclude=true, read with the pods.
	ExcludedNamespaces []string `json:"excludedNamespaces,omitempty"`
	// Views are the nodes that were control plane cluster nodes seen through
	// the cluster when it was read (hostViews). Classified is false for an
	// inventory saved by an earlier version.
	Views      []string `json:"views,omitempty"`
	Classified bool     `json:"classified,omitempty"`
}

// podEntries returns an inventory's pods with their deletion times. From
// a stored inventory, a pod that was terminating when it was saved ends at
// its deletion deadline at the latest.
func (inv *tenantInventory) podEntries(stored bool) []podEntry {
	out := make([]podEntry, 0, len(inv.Pods))
	for i := range inv.Pods {
		p := &inv.Pods[i]
		e := podEntry{pod: p, deletedAt: inv.PodsDeleted[string(p.UID)]}
		if stored && e.deletedAt.IsZero() && p.DeletionTimestamp != nil {
			e.deletedAt = p.DeletionTimestamp.Time
		}
		out = append(out, e)
	}
	return out
}

// LabelExclude on a namespace inside a cluster keeps its pods out of
// PRIVATE_NODE_BILLING=usage, as kube-system is. (The same setting on a
// cluster's own namespace of the control plane cluster, its StatefulSet or
// its VirtualClusterInstance excludes the whole cluster; see discovery.)
const LabelExclude = discovery.AnnotationPrefix + "exclude"

// readPods adds a cluster's pods to its inventory, with those deleted since
// the last read and when they were deleted, and its namespaces labeled
// LabelExclude=true.
func (c *Collector) readPods(ctx context.Context, conn *TenantConn, inv *tenantInventory) error {
	excluded, err := c.excludedNamespaces(ctx, conn.Kube)
	if err != nil {
		return err
	}
	inv.ExcludedNamespaces = excluded
	list, err := conn.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	conn.Graveyard.SeenPods(list.Items, c.opts.Now())
	pods := list.Items
	var deleted map[string]time.Time
	for _, e := range withDeleted(list.Items, conn.Graveyard.Pods("")) {
		if !e.deletedAt.IsZero() {
			if deleted == nil {
				deleted = map[string]time.Time{}
			}
			pods = append(pods, *e.pod)
			deleted[string(e.pod.UID)] = e.deletedAt
		}
	}
	inv.Pods, inv.PodsDeleted = pods, deleted
	return nil
}

// excludedNamespaces lists a cluster's namespaces labeled LabelExclude=true.
// A reader role without access to namespaces excludes none (the cluster's
// pods are all billed) and is logged once.
func (c *Collector) excludedNamespaces(ctx context.Context, kube kubernetes.Interface) ([]string, error) {
	list, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelExclude + "=true"})
	if apierrors.IsForbidden(err) {
		c.invMu.Lock()
		warned := c.nsForbidden
		c.nsForbidden = true
		c.invMu.Unlock()
		if !warned {
			log.Printf("[metrics] a cluster's API refuses to list namespaces, so no namespace in it is excluded with %s=true: grant its reader role list on namespaces (%v)", LabelExclude, err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var out []string
	for _, ns := range list.Items {
		if ns.Labels[LabelExclude] == "true" {
			out = append(out, ns.Name)
		}
	}
	return out, nil
}

func readTenantInventory(ctx context.Context, kube kubernetes.Interface, at time.Time) (*tenantInventory, error) {
	inv := &tenantInventory{At: at.UTC()}
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	pvcs, err := kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pvcs: %w", err)
	}
	svcs, err := kube.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	inv.Nodes, inv.PVCs, inv.Services = nodes.Items, pvcs.Items, svcs.Items
	inv.Leases = leaseRenewals(ctx, kube, nodes.Items)
	return inv, nil
}

// compact drops what metering never reads, so a stored inventory stays small.
func (inv *tenantInventory) compact() {
	for i := range inv.Nodes {
		inv.Nodes[i].ManagedFields = nil
		inv.Nodes[i].Status.Images = nil
	}
	for i := range inv.PVCs {
		inv.PVCs[i].ManagedFields = nil
	}
	for i := range inv.Pods {
		inv.Pods[i].ManagedFields = nil
	}
	kept := inv.Services[:0]
	for _, svc := range inv.Services {
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			svc.ManagedFields = nil
			kept = append(kept, svc)
		}
	}
	inv.Services = kept
}

func (c *Collector) collectTenantAPI(ctx context.Context, t Target, conn *TenantConn, hostNodes map[string]*nodeInfo, syncedOn map[string]bool,
	windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	inv, err := readTenantInventory(ctx, conn.Kube, c.opts.Now())
	if err != nil {
		return err
	}
	proofs, heartbeatErr := readNodeHeartbeats(ctx, conn.Kube)
	if heartbeatErr == nil {
		inv.Leases = heartbeatRenewals(proofs)
		if pending := settleHeartbeats(inv.Nodes, proofs, windows, c.opts.Now(), c.apiUpSince(t)); pending != nil {
			return pending
		}
		if c.health != nil {
			c.health.OK(health.Name("node-heartbeats", t.Cluster.ExternalID()), "tenant-api", t.Cluster.ExternalID(), len(proofs))
		}
	} else if c.health != nil {
		c.health.Fail(health.Name("node-heartbeats", t.Cluster.ExternalID()), "tenant-api", t.Cluster.ExternalID(), heartbeatErr,
			"add list on leases in kube-node-lease to the tenant's reader role; until then use COLLECTION_DELAY=75s for sudden node failures")
	}
	conn.Graveyard.SeenNodes(inv.Nodes, c.opts.Now())
	if c.opts.PrivateNodeBilling == "usage" {
		// Kept with the rest: if the cluster is paused by anyone else
		// before the next read, its workloads bill from this inventory.
		if err := c.readPods(ctx, conn, inv); err != nil {
			return err
		}
	}
	views := c.newHostViews(t.Cluster, conn.External, hostNodes, syncedOn)
	if err := views.classify(ctx, inv); err != nil {
		return err
	}
	c.invMu.Lock()
	c.lastInv[t.Cluster.ExternalID()] = inv
	c.invMu.Unlock()
	return c.meterTenant(ctx, t, conn, inv, views.view, windows, accs, st)
}

// meterTenant meters a cluster's own inventory. conn.Kube is nil for a
// paused cluster metered from its stored inventory. isView decides which
// nodes are control plane cluster nodes seen through the cluster (hostViews).
func (c *Collector) meterTenant(ctx context.Context, t Target, conn *TenantConn, inv *tenantInventory, isView func(context.Context, *corev1.Node) (bool, error),
	windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	base := c.baseFor(t)
	private := map[string]*nodeInfo{}
	consider := func(n *corev1.Node, deletedAt time.Time) error {
		if view, err := isView(ctx, n); err != nil || view {
			return err // a control plane cluster node: metered there (shared or dedicated)
		}
		ni := c.parsePrivateNode(n, inv.Leases[n.Name])
		ni.deletedAt = deletedAt
		ni.downs = conn.Graveyard.NodeDowns(ni.name)
		private[ni.name] = ni
		return nil
	}
	live := map[string]bool{}
	for i := range inv.Nodes {
		live[inv.Nodes[i].Name] = true
		if err := consider(&inv.Nodes[i], time.Time{}); err != nil {
			return err
		}
	}
	for _, d := range conn.Graveyard.Nodes() {
		if !live[d.obj.Name] {
			if err := consider(d.obj, d.at); err != nil {
				return err
			}
		}
	}
	c.prom.noteOwnNodes(t.Cluster.ExternalID(), len(private) > 0)
	c.noteOwnNodes(t.Cluster.ExternalID(), len(private) > 0)
	if len(private) == 0 {
		return nil // a shared tenant cluster: everything is metered from the control plane cluster
	}
	dra := c.draFor(ctx, conn.Dynamic, "")
	applyDRANodes(private, dra)
	c.rememberPrivateNodes(t.Cluster.ExternalID(), private)
	st.PrivateNodes += len(private)
	for _, n := range private {
		if n.down {
			st.NodesDown++
		}
	}

	if c.opts.PrivateNodeBilling == "usage" {
		// Live, the pods collectTenantAPI just read. Paused (conn.Kube is
		// nil), the pods as last read: saved right before vBilling's own
		// pause, or at the last collection before a pause anyone else made.
		// Only a pause vBilling verified ends their usage (workloadPauses),
		// so the window it cuts in two still bills the part before it; after
		// any other pause they keep billing, because nothing confirms they
		// stopped.
		live := conn.Kube != nil
		var usageByPod map[string][2]float64
		if latest := windows[len(windows)-1]; live && latest.Latest && c.opts.Basis != "requests" && conn.Metrics != nil {
			usageByPod = podUsage(ctx, conn.Metrics, "")
		}
		// Pods in kube-system, and in namespaces the cluster labels
		// vbilling.vcluster.com/exclude=true, are not billed: in usage mode the
		// cluster decides which of its namespaces are workloads.
		excluded := map[string]bool{}
		for _, ns := range inv.ExcludedNamespaces {
			excluded[ns] = true
		}
		include := func(p *corev1.Pod) bool {
			_, onPrivate := private[p.Spec.NodeName]
			return onPrivate && !c.excludedTenantNS[p.Namespace] && !excluded[p.Namespace]
		}
		c.meterPods(inv.podEntries(!live), include, func(p *corev1.Pod) string { return p.Namespace },
			private, nil, dra, windows, accs, st, base, usageByPod, "private_node", c.workloadPauses(t))
	} else {
		// Whole, whatever the node's labels say: an exemption is the
		// operator's to give (LabelBillable counts on the control plane
		// cluster's dedicated nodes only).
		for name, n := range private {
			c.meterWholeNode(name, n, windows, accs, base, "private_node", c.pausesFor(t, pause.KeepNodes))
		}
	}

	// Volumes and load balancers of such a tenant cluster live in it too.
	// conn.Graveyard is nil for a stored inventory: what it holds is billed
	// as it was read.
	meterVolumes(conn.Graveyard.Volumes(inv.PVCs, ""), func(map[string]string) bool { return true }, windows, accs, base, c.clip(t, pause.KeepStorage))
	meterLoadBalancers(conn.Graveyard.LoadBalancers(inv.Services, ""), func(*corev1.Service) bool { return true }, windows, accs, base, c.clip(t, pause.KeepLoadBalancers))
	return nil
}

// UsePauses lets the collector keep the inventory of paused clusters.
func (c *Collector) UsePauses(p *pause.Store) { c.pauses = p }

// stoppedAt returns when vBilling scaled a cluster's control plane down for a
// pause whose span is not open yet.
func (c *Collector) stoppedAt(cl discovery.TenantCluster) (time.Time, bool) {
	if c.pauses == nil {
		return time.Time{}, false
	}
	return c.pauses.Stopped(cl.ExternalID())
}

// Freeze reads a cluster's own API now and stores its inventory, to meter
// it from while it is paused. vBilling calls it right before stopping the
// control plane. It is a no-op for clusters without tenant API access.
func (c *Collector) Freeze(ctx context.Context, cl discovery.TenantCluster) error {
	if c.tenants == nil || c.pauses == nil {
		return nil
	}
	conn, err := c.tenants.Conn(ctx, cl)
	if errors.Is(err, ErrNoTenantAPI) {
		c.noteOwnNodes(cl.ExternalID(), false) // nothing inside it is metered
		return nil
	}
	if err != nil {
		return err
	}
	inv, err := readTenantInventory(ctx, conn.Kube, c.opts.Now())
	if err != nil {
		return err
	}
	if c.opts.PrivateNodeBilling == "usage" {
		if err := c.readPods(ctx, conn, inv); err != nil {
			return err
		}
	}
	// Whether it has nodes of its own, as of right now: a pause vBilling
	// cannot drain stops their billing only if it has none (OwnNodes).
	// The rules are meterTenant's (hostViews): a node of the control plane
	// cluster, or a placeholder vCluster made for one, is metered there.
	host := map[string]*nodeInfo{}
	if !conn.External {
		nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list control plane cluster nodes: %w", err)
		}
		for i := range nodes.Items {
			host[nodes.Items[i].Name] = c.parseNode(&nodes.Items[i])
		}
	}
	if err := c.newHostViews(cl, conn.External, host, nil).classify(ctx, inv); err != nil {
		return err
	}
	c.noteOwnNodes(cl.ExternalID(), len(inv.Views) < len(inv.Nodes))
	return c.storeInventory(cl.ExternalID(), inv)
}

// noteOwnNodes records whether a cluster has nodes of its own that
// metering bills (OwnNodes).
func (c *Collector) noteOwnNodes(id string, has bool) {
	c.invMu.Lock()
	defer c.invMu.Unlock()
	c.ownNodes[id] = has
}

// OwnNodes reports whether metering bills nodes of a cluster's own (nodes
// that joined the cluster rather than the control plane cluster's), as of
// the last time it read the cluster's API or saved its inventory: has is
// false for a cluster whose API it has no credentials for, since nothing
// inside it is metered. known is false until it has read the cluster once.
func (c *Collector) OwnNodes(cl discovery.TenantCluster) (has, known bool) {
	c.invMu.Lock()
	defer c.invMu.Unlock()
	has, known = c.ownNodes[cl.ExternalID()]
	return has, known
}

func (c *Collector) storeInventory(id string, inv *tenantInventory) error {
	inv.compact()
	b, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	return c.pauses.SaveInventory(id, b)
}

// pausedInventory returns the stored inventory of a paused cluster. A
// cluster paused by someone else (vcluster pause) has none stored; the
// last inventory read before the pause is stored then, if there is one.
func (c *Collector) pausedInventory(id string) (*tenantInventory, bool) {
	if c.pauses == nil {
		return nil, false
	}
	if b, ok := c.pauses.Inventory(id); ok {
		var inv tenantInventory
		if json.Unmarshal(b, &inv) == nil {
			return &inv, true
		}
	}
	c.invMu.Lock()
	inv := c.lastInv[id]
	c.invMu.Unlock()
	if inv == nil {
		return nil, false
	}
	if err := c.storeInventory(id, inv); err != nil {
		log.Printf("[metrics] %s: store paused inventory: %v", id, err)
	}
	return inv, true
}

// ForgetPaused drops the stored inventory of a cluster that resumed.
func (c *Collector) ForgetPaused(id string) {
	if c.pauses != nil {
		if err := c.pauses.DropInventory(id); err != nil {
			log.Printf("[metrics] %s: drop paused inventory: %v", id, err)
		}
	}
}

// apiUpSince is when a cluster's own API last came up, as far as vBilling
// knows: the end of its last pause, or when its control plane was last seen
// becoming ready. Zero when neither is known.
func (c *Collector) apiUpSince(t Target) time.Time {
	var since time.Time
	for _, sp := range t.Paused {
		if sp.To.After(since) {
			since = sp.To
		}
	}
	if at, ok := c.planes.ReadySince(t.Cluster); ok && at.After(since) {
		since = at
	}
	return since
}
