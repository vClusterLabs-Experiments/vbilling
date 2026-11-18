package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// LabelFakeNode marks the placeholder nodes vCluster creates for shared
// tenant clusters; LabelBillable=false exempts a node (for example hardware
// the tenant owns) from whole-node billing.
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
// late events, deduplicated by ID.
func (c *Collector) CollectTenantAPI(ctx context.Context, targets []Target, windows []Window) (map[time.Time][]usage.Event, Stats, error) {
	return c.collect(ctx, targets, windows, true)
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
func (c *Collector) collectTenant(ctx context.Context, t Target, hostNodes map[string]*nodeInfo, windows []Window, accs map[time.Time]*accumulator, st *Stats) {
	id := t.Cluster.ExternalID()
	tmp := map[time.Time]*accumulator{}
	for _, w := range windows {
		tmp[w.End] = newAccumulator(w)
	}
	if t.Cluster.Paused {
		if _, err := c.tenants.Conn(ctx, t.Cluster); errors.Is(err, ErrNoTenantAPI) {
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
		if err := c.meterTenant(ctx, t, &TenantConn{External: t.Cluster.External}, inv, hostNodes, windows, tmp, st); err != nil {
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
		return
	}
	if err == nil {
		err = c.collectTenantAPI(ctx, t, conn, hostNodes, windows, tmp, st)
	}
	if err != nil {
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
	// Pods and the time each deleted one was deleted (by UID), saved by
	// Freeze with PRIVATE_NODE_BILLING=usage: the window a pause cuts in
	// two still bills its workloads up to the pause.
	Pods        []corev1.Pod         `json:"pods,omitempty"`
	PodsDeleted map[string]time.Time `json:"podsDeleted,omitempty"`
}

// podEntries returns a stored inventory's pods with their deletion times.
// A pod that was terminating when it was saved ends at its deletion
// deadline at the latest.
func (inv *tenantInventory) podEntries() []podEntry {
	out := make([]podEntry, 0, len(inv.Pods))
	for i := range inv.Pods {
		p := &inv.Pods[i]
		e := podEntry{pod: p, deletedAt: inv.PodsDeleted[string(p.UID)]}
		if e.deletedAt.IsZero() && p.DeletionTimestamp != nil {
			e.deletedAt = p.DeletionTimestamp.Time
		}
		out = append(out, e)
	}
	return out
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

func (c *Collector) collectTenantAPI(ctx context.Context, t Target, conn *TenantConn, hostNodes map[string]*nodeInfo, windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	inv, err := readTenantInventory(ctx, conn.Kube, c.opts.Now())
	if err != nil {
		return err
	}
	conn.Graveyard.SeenNodes(inv.Nodes, c.opts.Now())
	c.invMu.Lock()
	c.lastInv[t.Cluster.ExternalID()] = inv
	c.invMu.Unlock()
	return c.meterTenant(ctx, t, conn, inv, hostNodes, windows, accs, st)
}

// meterTenant meters a cluster's own inventory. conn.Kube is nil for a
// paused cluster metered from its stored inventory.
func (c *Collector) meterTenant(ctx context.Context, t Target, conn *TenantConn, inv *tenantInventory, hostNodes map[string]*nodeInfo, windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	base := c.baseFor(t)
	private := map[string]*nodeInfo{}
	consider := func(n *corev1.Node, deletedAt time.Time) {
		if _, onHost := hostNodes[n.Name]; onHost && !conn.External {
			return // a control plane cluster node: metered there (shared or dedicated)
		}
		if n.Labels[LabelFakeNode] == "true" {
			return
		}
		ni := c.parseNodeLease(n, inv.Leases[n.Name])
		ni.deletedAt = deletedAt
		private[ni.name] = ni
	}
	live := map[string]bool{}
	for i := range inv.Nodes {
		live[inv.Nodes[i].Name] = true
		consider(&inv.Nodes[i], time.Time{})
	}
	for _, d := range conn.Graveyard.Nodes() {
		if !live[d.obj.Name] {
			consider(d.obj, d.at)
		}
	}
	c.prom.noteOwnNodes(t.Cluster.ExternalID(), len(private) > 0)
	if len(private) == 0 {
		return nil // a shared tenant cluster: everything is metered from the control plane cluster
	}
	dra := c.draFor(ctx, conn.Dynamic, "")
	applyDRANodes(private, dra)
	st.PrivateNodes += len(private)
	for _, n := range private {
		if n.down {
			st.NodesDown++
		}
	}

	if c.opts.PrivateNodeBilling == "usage" {
		if conn.Kube == nil {
			// A paused cluster: its pods as saved right before the pause.
			// Their usage ends at the pause (workloadPauses), so the window
			// the pause cuts in two still bills the part before it.
			include := func(p *corev1.Pod) bool {
				_, onPrivate := private[p.Spec.NodeName]
				return onPrivate && !c.excludedTenantNS[p.Namespace]
			}
			c.meterPods(inv.podEntries(), include, func(p *corev1.Pod) string { return p.Namespace },
				private, nil, dra, windows, accs, st, base, nil, "private_node", c.workloadPauses(t))
		} else {
			podList, err := conn.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("list pods: %w", err)
			}
			conn.Graveyard.SeenPods(podList.Items, c.opts.Now())
			pods := withDeleted(podList.Items, conn.Graveyard.Pods(""))
			var usageByPod map[string][2]float64
			if latest := windows[len(windows)-1]; latest.Latest && c.opts.Basis != "requests" && conn.Metrics != nil {
				usageByPod = podUsage(ctx, conn.Metrics, "")
			}
			include := func(p *corev1.Pod) bool {
				_, onPrivate := private[p.Spec.NodeName]
				return onPrivate && !c.excludedTenantNS[p.Namespace]
			}
			c.meterPods(pods, include, func(p *corev1.Pod) string { return p.Namespace },
				private, nil, dra, windows, accs, st, base, usageByPod, "private_node", c.workloadPauses(t))
		}
	} else {
		for name, n := range private {
			if n.labels[LabelBillable] == "false" {
				continue
			}
			c.meterWholeNode(name, n, windows, accs, base, "private_node", c.clip(t, pause.KeepNodes))
		}
	}

	// Volumes and load balancers of such a tenant cluster live in it too.
	meterVolumes(inv.PVCs, func(map[string]string) bool { return true }, windows, accs, base, c.clip(t, pause.KeepStorage))
	meterLoadBalancers(inv.Services, func(*corev1.Service) bool { return true }, windows, accs, base, c.clip(t, pause.KeepLoadBalancers))
	return nil
}

// UsePauses lets the collector keep the inventory of paused clusters.
func (c *Collector) UsePauses(p *pause.Store) { c.pauses = p }

// Freeze reads a cluster's own API now and stores its inventory, to meter
// it from while it is paused. vBilling calls it right before stopping the
// control plane. It is a no-op for clusters without tenant API access.
func (c *Collector) Freeze(ctx context.Context, cl discovery.TenantCluster) error {
	if c.tenants == nil || c.pauses == nil {
		return nil
	}
	conn, err := c.tenants.Conn(ctx, cl)
	if errors.Is(err, ErrNoTenantAPI) {
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
		pods, err := conn.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list pods: %w", err)
		}
		conn.Graveyard.SeenPods(pods.Items, c.opts.Now())
		inv.Pods = pods.Items
		for _, e := range withDeleted(pods.Items, conn.Graveyard.Pods("")) {
			if !e.deletedAt.IsZero() {
				if inv.PodsDeleted == nil {
					inv.PodsDeleted = map[string]time.Time{}
				}
				inv.Pods = append(inv.Pods, *e.pod)
				inv.PodsDeleted[string(e.pod.UID)] = e.deletedAt
			}
		}
	}
	return c.storeInventory(cl.ExternalID(), inv)
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
