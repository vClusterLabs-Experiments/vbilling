package metrics

import (
	"context"
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
func (c *Collector) collectTenant(ctx context.Context, t Target, hostNodes map[string]*nodeInfo, windows []Window, accs map[time.Time]*accumulator, st *Stats) {
	conn, err := c.tenants.Conn(ctx, t.Cluster)
	if errors.Is(err, ErrNoTenantAPI) {
		return
	}
	tmp := map[time.Time]*accumulator{}
	for _, w := range windows {
		tmp[w.End] = newAccumulator(w)
	}
	if err == nil {
		err = c.collectTenantAPI(ctx, t, conn, hostNodes, windows, tmp, st)
	}
	if err != nil {
		log.Printf("[metrics] %s: tenant API unavailable, its windows will be filled later: %v", t.Cluster.ExternalID(), err)
		st.TenantAPIFailed = append(st.TenantAPIFailed, t.Cluster.ExternalID())
		return
	}
	for end, a := range tmp {
		for _, k := range a.order {
			ev := *a.events[k]
			qty, props := ev.Quantity, ev.Properties
			ev.Quantity, ev.Properties = 0, nil
			accs[end].add(ev, qty, props)
		}
	}
}

func (c *Collector) collectTenantAPI(ctx context.Context, t Target, conn *TenantConn, hostNodes map[string]*nodeInfo, windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	base := c.baseFor(t)
	list, err := conn.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	conn.Graveyard.SeenNodes(list.Items, c.opts.Now())
	private := map[string]*nodeInfo{}
	consider := func(n *corev1.Node, deletedAt time.Time) {
		if _, onHost := hostNodes[n.Name]; onHost && !conn.External {
			return // a control plane cluster node: metered there (shared or dedicated)
		}
		if n.Labels[LabelFakeNode] == "true" {
			return
		}
		ni := c.parseNode(n)
		ni.deletedAt = deletedAt
		private[ni.name] = ni
	}
	live := map[string]bool{}
	for i := range list.Items {
		live[list.Items[i].Name] = true
		consider(&list.Items[i], time.Time{})
	}
	for _, d := range conn.Graveyard.Nodes() {
		if !live[d.obj.Name] {
			consider(d.obj, d.at)
		}
	}
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
			private, nil, dra, windows, accs, st, base, usageByPod, "private_node")
	} else {
		for name, n := range private {
			if n.labels[LabelBillable] == "false" {
				continue
			}
			c.meterWholeNode(name, n, windows, accs, base, "private_node")
		}
	}

	// Volumes and load balancers of such a tenant cluster live in it too.
	pvcs, err := conn.Kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pvcs: %w", err)
	}
	meterVolumes(pvcs.Items, func(map[string]string) bool { return true }, windows, accs, base)
	svcs, err := conn.Kube.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	meterLoadBalancers(svcs.Items, func(*corev1.Service) bool { return true }, windows, accs, base)
	return nil
}
