// Package metrics turns the state of tenant clusters into usage events.
//
// Allocation metrics (GPUs, dedicated nodes, storage, load balancers,
// control plane hours, requests-based CPU/memory) are computed from object
// lifetimes overlapping each window, to the second. That is what makes
// backfill after a restart possible and keeps short jobs accurate.
// metrics-server CPU/memory is sampled for the latest window only.
// Prometheus-read usage (egress, DCGM utilization, operator metrics) is
// collected by PromSource under its own ledger source, so a Prometheus
// outage delays those windows instead of dropping them.
package metrics

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
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

const gib = 1024 * 1024 * 1024

// Options configure metering behavior.
type Options struct {
	Region             string
	RegionFromNode     bool
	Basis              string // usage | requests | max
	MeterControlPlane  bool
	MeterByNamespace   bool
	GPUResources       []string
	SKULabel           string
	CapacityTypeLabel  string
	UnhealthyTaints    []string
	DedicatedNodeLabel string
	PrometheusURL      string
	PromHeaders        map[string]string // extra request headers (tokens, X-Scope-OrgID)
	PromTokenFile      string            // bearer token file, re-read per query
	EgressQuery        string
	GPUUtilQuery       string // "none" disables GPU utilization
	GPUCountQuery      string
	PrometheusPreset   string       // "vcluster-platform": fleet observability label selectors
	PromMetrics        []PromMetric // operator-defined metrics from PromQL
	// Tenant clusters with their own nodes: "node" bills each private node
	// whole (default), "usage" bills the pods running on them instead.
	PrivateNodeBilling      string
	TenantExcludeNamespaces []string // usage mode: namespaces never billed (default kube-system)
	DRAGPUDrivers           []string // DRA drivers whose devices are GPUs (default gpu.nvidia.com, gpu.amd.com)
	// PauseKeep is the capacity that keeps billing while a cluster is
	// paused (PAUSE_KEEP_BILLING). Nil means pause.DefaultKeep.
	PauseKeep pause.Keep
	Now       func() time.Time
}

// Collector gathers billable state from the Kubernetes API.
type Collector struct {
	kube        kubernetes.Interface
	metrics     metricsclient.Interface // nil disables usage-based CPU/memory
	prom        *PromSource             // nil until NewPromSource
	promDefault *prometheusClient       // PROMETHEUS_URL; nil when unset
	graveyard   *Graveyard              // deletions seen between collections; nil = none
	tenants     TenantAPI               // tenant clusters' own APIs; nil = control plane cluster only
	dyn         dynamic.Interface       // DRA ResourceClaims and ResourceSlices; nil = none
	opts        Options

	excludedTenantNS map[string]bool
	health           *health.Registry // nil: not reported

	pauses  *pause.Store // inventories of paused clusters; nil = none
	invMu   sync.Mutex
	lastInv map[string]*tenantInventory // last inventory read from each cluster's own API
	// ownNodes records whether each cluster has nodes of its own that
	// metering bills (OwnNodes).
	ownNodes map[string]bool

	// nodeHistory is what vBilling first saw of each node of a cluster's
	// own, and the most GPUs it had (privateNodeHistory).
	nodeHistory *privateNodeHistory
	// syncedReported: clusters (by ID, "" for the list itself) whose
	// synced-namespaces source is reported (guarded by invMu).
	syncedReported map[string]bool
}

// UseGraveyard makes the collector bill pods and nodes deleted between
// collections (see Graveyard).
func (c *Collector) UseGraveyard(g *Graveyard) { c.graveyard = g }

// UseDynamic enables DRA (ResourceClaim) GPU metering on the control plane cluster.
func (c *Collector) UseDynamic(d dynamic.Interface) { c.dyn = d }

// UseHealth makes the collector report tenant API and metrics-server
// health.
func (c *Collector) UseHealth(h *health.Registry) { c.health = h }

// baseFn builds an event template for a tenant cluster and node.
type baseFn func(metric string, n *nodeInfo) usage.Event

// podEntry is a pod to meter; deletedAt is set for pods seen deleted.
type podEntry struct {
	pod       *corev1.Pod
	deletedAt time.Time
}

// withDeleted adds pods deleted since the last collection to a live list.
func withDeleted(live []corev1.Pod, gone []deleted[*corev1.Pod]) []podEntry {
	out := make([]podEntry, 0, len(live)+len(gone))
	seen := make(map[string]bool, len(live))
	for i := range live {
		out = append(out, podEntry{pod: &live[i]})
		seen[string(live[i].UID)] = true
	}
	for _, d := range gone {
		if !seen[string(d.obj.UID)] {
			out = append(out, podEntry{pod: d.obj, deletedAt: d.at})
		}
	}
	return out
}

func entryLabels(pods []podEntry) []map[string]string {
	out := make([]map[string]string, len(pods))
	for i := range pods {
		out[i] = pods[i].pod.Labels
	}
	return out
}

func NewCollector(kube kubernetes.Interface, mc metricsclient.Interface, opts Options) *Collector {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Basis == "" {
		opts.Basis = "requests"
	}
	if opts.PrivateNodeBilling == "" {
		opts.PrivateNodeBilling = "node"
	}
	if opts.TenantExcludeNamespaces == nil {
		opts.TenantExcludeNamespaces = []string{"kube-system"}
	}
	if opts.DRAGPUDrivers == nil {
		opts.DRAGPUDrivers = DefaultDRAGPUDrivers
	}
	if opts.PauseKeep == nil {
		opts.PauseKeep, _ = pause.ParseKeep(pause.DefaultKeep)
	}
	c := &Collector{kube: kube, metrics: mc, opts: opts, excludedTenantNS: map[string]bool{}, lastInv: map[string]*tenantInventory{},
		ownNodes: map[string]bool{}, nodeHistory: newPrivateNodeHistory(), syncedReported: map[string]bool{}}
	for _, ns := range opts.TenantExcludeNamespaces {
		c.excludedTenantNS[ns] = true
	}
	if opts.PrometheusURL != "" {
		c.promDefault = newPrometheusClient(opts.PrometheusURL, opts.PromHeaders, opts.PromTokenFile)
	}
	return c
}

// Window is a closed metering window. Latest marks the window being
// collected live (consumption metrics are only sampled for it).
type Window struct {
	Start, End time.Time
	Latest     bool
}

// Target is a tenant cluster with its resolved billing identity.
type Target struct {
	Cluster     discovery.TenantCluster
	Tenant      string
	Project     string
	TenantClass string
	// Paused are the times the cluster was paused. Capacity that does not
	// keep billing while paused (PauseKeep) is not billed for them.
	Paused []pause.Span
}

// pausesFor returns the pauses that stop billing capacity of the given
// kind (pause.Keep*) for a target: none when that kind keeps billing while
// paused. Only pauses vBilling verified (it confirmed the cluster's
// workloads were gone) stop billing nodes, workloads, volumes and load
// balancers. A pause it only observed (pause.ByExternal: vcluster pause, an
// operator, sleep mode, the tenant scaling its own control plane down)
// stops the control plane's hours alone: nothing confirms the workloads on
// the cluster's nodes stopped, so they and their nodes keep billing.
func (c *Collector) pausesFor(t Target, kind string) []pause.Span {
	if len(t.Paused) == 0 || c.opts.PauseKeep[kind] {
		return nil
	}
	if kind == pause.KeepControlPlane {
		return t.Paused
	}
	var out []pause.Span
	for _, sp := range t.Paused {
		if sp.Verified() {
			out = append(out, sp)
		}
	}
	return out
}

// pausedFor returns the part of [start, end) inside [ws, we) during which
// the target was paused, for capacity of the given kind (pause.Keep*). It
// is zero when that kind keeps billing while paused.
func (c *Collector) pausedFor(t Target, kind string, start, end, ws, we time.Time) time.Duration {
	spans := c.pausesFor(t, kind)
	if len(spans) == 0 {
		return 0
	}
	return pause.Overlap(spans, start, end, ws, we)
}

// clip returns a pausedFor bound to one target and kind.
func (c *Collector) clip(t Target, kind string) pausedFn {
	return func(start, end, ws, we time.Time) time.Duration { return c.pausedFor(t, kind, start, end, ws, we) }
}

// workloadPauses returns the pause spans that end the billing of a
// target's pods (none when PAUSE_KEEP_BILLING keeps workloads).
func (c *Collector) workloadPauses(t Target) []pause.Span {
	return c.pausesFor(t, pause.KeepWorkloads)
}

// pausedFn returns how much of [start, end) inside [ws, we) is not billed
// because the cluster was paused.
type pausedFn func(start, end, ws, we time.Time) time.Duration

// Stats summarize one collection run.
type Stats struct {
	Pods         int
	GPUPods      int
	NodesDown    int
	Dedicated    int
	PromFailures int
	PrivateNodes int
	// UsageUnmeasured counts pod-windows whose CPU and memory were not
	// billed: CPU_MEMORY_BASIS=usage, and the window was collected late
	// (after a restart or outage), when metrics-server has no usage for it.
	UsageUnmeasured int
	// Unrequested counts running pods on shared nodes that request no CPU
	// or memory: on CPU_MEMORY_BASIS=requests they run unbilled. A vCluster
	// LimitRange (policies.limitRange) gives them default requests.
	Unrequested int
	// TenantAPIFailed lists tenant clusters (external IDs) whose own API
	// could not be read; nothing from it was metered for these windows.
	TenantAPIFailed []string
}

// accumulator merges observations with the same identity within one
// window into one event.
type accumulator struct {
	window Window
	events map[string]*usage.Event
	order  []string
}

func newAccumulator(w Window) *accumulator {
	return &accumulator{window: w, events: map[string]*usage.Event{}}
}

func (a *accumulator) add(tmpl usage.Event, qty float64, props map[string]any) {
	if qty <= 0 {
		return
	}
	tmpl.ID = ""
	tmpl.WindowStart, tmpl.WindowEnd = a.window.Start.UTC(), a.window.End.UTC()
	key := usage.DeriveID(&tmpl)
	ev, ok := a.events[key]
	if !ok {
		e := tmpl
		e.Properties = map[string]any{}
		a.events[key] = &e
		a.order = append(a.order, key)
		ev = &e
	}
	ev.Quantity += qty
	for k, v := range props {
		switch n := v.(type) {
		case int:
			prev, _ := ev.Properties[k].(int)
			ev.Properties[k] = prev + n
		case float64:
			prev, _ := ev.Properties[k].(float64)
			ev.Properties[k] = prev + n
		default:
			ev.Properties[k] = v
		}
	}
}

func (a *accumulator) list() []usage.Event {
	out := make([]usage.Event, 0, len(a.order))
	for _, k := range a.order {
		e := *a.events[k]
		if len(e.Properties) == 0 {
			e.Properties = nil
		}
		out = append(out, e)
	}
	return out
}

// Collect meters every target over every window. The result maps each
// window end to its events, ready to commit window by window.
func (c *Collector) Collect(ctx context.Context, targets []Target, windows []Window) (map[time.Time][]usage.Event, Stats, error) {
	return c.collect(ctx, targets, windows, false)
}

func (c *Collector) collect(ctx context.Context, targets []Target, windows []Window, onlyTenantAPI bool) (map[time.Time][]usage.Event, Stats, error) {
	var st Stats
	out := map[time.Time][]usage.Event{}
	if len(windows) == 0 {
		return out, st, nil
	}
	nodeList, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, st, fmt.Errorf("list nodes: %w", err)
	}
	c.graveyard.SeenNodes(nodeList.Items, c.opts.Now())
	nodes := map[string]*nodeInfo{}
	renewed := leaseRenewals(ctx, c.kube, nodeList.Items)
	for i := range nodeList.Items {
		ni := c.parseNodeLease(&nodeList.Items[i], renewed[nodeList.Items[i].Name])
		nodes[ni.name] = ni
		if ni.down {
			st.NodesDown++
		}
	}
	for _, d := range c.graveyard.Nodes() {
		if _, live := nodes[d.obj.Name]; !live {
			ni := c.parseNode(d.obj)
			ni.deletedAt = d.at
			nodes[ni.name] = ni
		}
	}

	dra := c.draFor(ctx, c.dyn, "")
	applyDRANodes(nodes, dra)
	accs := map[time.Time]*accumulator{}
	for _, w := range windows {
		accs[w.End] = newAccumulator(w)
	}
	now := c.opts.Now().UTC()
	if r, ok := c.tenants.(interface {
		Retain([]discovery.TenantCluster)
	}); ok && !onlyTenantAPI {
		clusters := make([]discovery.TenantCluster, len(targets))
		for i := range targets {
			clusters[i] = targets[i].Cluster
		}
		r.Retain(clusters)
	}
	var synced *syncedObjects // objects synced to other namespaces (synced.go)
	if !onlyTenantAPI {
		if synced, err = c.listSynced(ctx); err != nil {
			return nil, st, err
		}
	}
	for _, t := range withNamesakes(targets) {
		// External tenant clusters have nothing on this control plane cluster.
		var syncedOn map[string]bool
		if !onlyTenantAPI && !t.Cluster.External {
			on, err := c.collectTarget(ctx, t, nodes, dra, synced.of(t.Cluster), windows, accs, &st)
			if err != nil {
				return nil, st, fmt.Errorf("meter %s: %w", t.Cluster.ExternalID(), err)
			}
			syncedOn = on
		}
		if c.tenants != nil {
			c.collectTenant(ctx, t, nodes, syncedOn, windows, accs, &st)
		}
	}
	c.saveNodeHistory()
	for _, w := range windows {
		var evs []usage.Event
		for _, ev := range accs[w.End].list() {
			ev.Source = "collector"
			ev.RecordedAt = now
			if !w.Latest {
				if ev.Properties == nil {
					ev.Properties = map[string]any{}
				}
				ev.Properties["backfilled"] = true
			}
			ev.Finalize()
			// Never commit an event the backends would reject: a bug here must
			// surface loudly rather than as dead letters downstream.
			if err := ev.Validate(); err != nil {
				return nil, st, fmt.Errorf("invalid event for %s/%s: %w", ev.Tenant, ev.Metric, err)
			}
			evs = append(evs, ev)
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].ID < evs[j].ID })
		out[w.End] = evs
	}
	return out, st, nil
}

// collectTarget meters what a cluster runs on the control plane cluster: in
// its own namespace, and what it synced to other namespaces (away). It
// returns the control plane cluster nodes its synced pods run on (hostViews).
func (c *Collector) collectTarget(ctx context.Context, t Target, nodes map[string]*nodeInfo, dra *draIndex, away elsewhere,
	windows []Window, accs map[time.Time]*accumulator, st *Stats) (map[string]bool, error) {
	cl := t.Cluster
	ns := cl.Namespace
	extID := cl.ExternalID()

	base := c.baseFor(t)

	podList, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	c.graveyard.SeenPods(podList.Items, c.opts.Now())
	pods := withDeleted(podList.Items, c.graveyard.Pods(ns))
	owned := ownedFilter(entryLabels(pods), cl.Name, c.opts.MeterControlPlane)
	include := func(p *corev1.Pod) bool { return owned(p.Labels) }
	syncedOn := map[string]bool{}
	syncedNodes(syncedOn, pods, func(p *corev1.Pod) bool { return !controlPlaneObject(p.Labels, cl.Name) && owned(p.Labels) })
	// Pods it synced to other namespaces: all of them are its workloads.
	c.graveyard.SeenPods(away.pods, c.opts.Now())
	if awayPods := withDeleted(away.pods, c.deletedElsewhere(cl)); len(awayPods) > 0 {
		pods = append(pods, awayPods...)
		include = func(p *corev1.Pod) bool { return p.Namespace != ns || owned(p.Labels) }
		syncedNodes(syncedOn, awayPods, func(*corev1.Pod) bool { return true })
	}
	c.reportElsewhere(cl, away)

	// Dedicated nodes: billed whole, so pods on them are not billed again.
	dedicated := map[string]*nodeInfo{}
	for name, n := range nodes {
		if c.dedicatedTo(n, cl.Name, ns, extID) {
			dedicated[name] = n
		}
	}
	st.Dedicated += len(dedicated)

	var usageByPod map[string][2]float64
	latest := windows[len(windows)-1]
	if latest.Latest && c.opts.Basis != "requests" && c.metrics != nil {
		usageByPod = podUsage(ctx, c.metrics, ns)
		for _, other := range namespacesOf(away.pods, ns) {
			for k, v := range podUsage(ctx, c.metrics, other) {
				usageByPod[k] = v
			}
		}
	}

	virtualNS := func(p *corev1.Pod) string { return p.Labels[LabelVirtualNamespace] }
	c.meterPods(pods, include, virtualNS,
		nodes, dedicated, dra, windows, accs, st, base, usageByPod, "shared", c.workloadPauses(t))

	// Dedicated nodes: full capacity, per node.
	for name, n := range dedicated {
		if n.labels[LabelBillable] == "false" {
			continue // the tenant's own hardware: neither the node nor its pods are billed
		}
		c.meterWholeNode(name, n, windows, accs, base, "dedicated_node", c.clip(t, pause.KeepNodes))
	}

	// Storage: bound PVCs synced from the tenant cluster, per storage class.
	pvcs, err := c.kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pvcs: %w", err)
	}
	meterVolumes(pvcs.Items, ownedFilter(pvcLabels(pvcs.Items), cl.Name, c.opts.MeterControlPlane), windows, accs, base, c.clip(t, pause.KeepStorage))
	meterVolumes(away.pvcs, func(map[string]string) bool { return true }, windows, accs, base, c.clip(t, pause.KeepStorage))

	// Load balancers with an assigned address.
	svcs, err := c.kube.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	meterLoadBalancers(svcs.Items, func(svc *corev1.Service) bool {
		return !controlPlaneObject(svc.Labels, cl.Name) || c.opts.MeterControlPlane
	}, windows, accs, base, c.clip(t, pause.KeepLoadBalancers))
	meterLoadBalancers(away.services, func(*corev1.Service) bool { return true }, windows, accs, base, c.clip(t, pause.KeepLoadBalancers))

	// Control plane hours, only while ready (no charge while provisioning,
	// asleep or paused, including paused time in backfilled windows). A
	// cluster that is paused now ran until the second its pause began.
	bill, until := cl.Ready, time.Time{}
	if n := len(t.Paused); !cl.Ready && cl.Paused && n > 0 && t.Paused[n-1].Open() {
		bill = true
		if !c.opts.PauseKeep[pause.KeepControlPlane] {
			until = t.Paused[n-1].From
		}
	}
	if bill {
		for _, w := range windows {
			ov := usage.Overlap(cl.CreatedAt, until, w.Start, w.End)
			ov -= c.pausedFor(t, pause.KeepControlPlane, cl.CreatedAt, until, w.Start, w.End)
			if ov < 0 {
				ov = 0
			}
			accs[w.End].add(base(usage.MetricInstanceHours, nil), ov.Hours(), nil)
		}
	}

	return syncedOn, nil
}

// podUsage returns current CPU cores and memory bytes per pod name.
func podUsage(ctx context.Context, mc metricsclient.Interface, ns string) map[string][2]float64 {
	out := map[string][2]float64{}
	list, err := mc.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[metrics] %s: metrics-server unavailable, usage-based CPU/memory skipped: %v", ns, err)
		return out
	}
	for _, pm := range list.Items {
		var cpu, mem float64
		for _, ctr := range pm.Containers {
			if q := ctr.Usage.Cpu(); q != nil {
				cpu += float64(q.MilliValue()) / 1000
			}
			if q := ctr.Usage.Memory(); q != nil {
				mem += float64(q.Value())
			}
		}
		out[pm.Namespace+"/"+pm.Name] = [2]float64{cpu, mem}
	}
	return out
}

func podLabels(pods []corev1.Pod) []map[string]string {
	out := make([]map[string]string, len(pods))
	for i := range pods {
		out[i] = pods[i].Labels
	}
	return out
}

func pvcLabels(pvcs []corev1.PersistentVolumeClaim) []map[string]string {
	out := make([]map[string]string, len(pvcs))
	for i := range pvcs {
		out[i] = pvcs[i].Labels
	}
	return out
}

// ownedFilter decides which host objects belong to a tenant cluster. When
// the syncer's managed-by label is present in the namespace it is used
// strictly (several tenant clusters can share a namespace); otherwise every
// non-control-plane object counts, as in v0.1.
func ownedFilter(all []map[string]string, clusterName string, includeControlPlane bool) func(map[string]string) bool {
	labeled := false
	for _, l := range all {
		if l[LabelManagedBy] != "" {
			labeled = true
			break
		}
	}
	return func(l map[string]string) bool {
		if controlPlaneObject(l, clusterName) {
			return includeControlPlane
		}
		if labeled {
			return l[LabelManagedBy] == clusterName
		}
		return true
	}
}

// meterPods meters GPU, CPU and memory time of pods outside the skipped
// (whole-node billed) nodes. mode is the billing_mode dimension.
func (c *Collector) meterPods(pods []podEntry, include func(*corev1.Pod) bool, nsOf func(*corev1.Pod) string,
	nodes, skip map[string]*nodeInfo, dra *draIndex, windows []Window, accs map[time.Time]*accumulator, st *Stats,
	base baseFn, usageByPod map[string][2]float64, mode string, paused []pause.Span) {
	for _, e := range pods {
		p := e.pod
		if !include(p) {
			continue
		}
		start, end, started := podLifetime(p)
		if !started {
			continue
		}
		// A pod deleted while running stops being billed when it was deleted.
		if !e.deletedAt.IsZero() && (end.IsZero() || end.After(e.deletedAt)) {
			end = e.deletedAt
		}
		st.Pods++
		n := nodes[p.Spec.NodeName]
		if _, ok := skip[p.Spec.NodeName]; ok {
			continue
		}
		capType := usage.CapacityOnDemand
		gpuType := "unknown"
		if n != nil {
			capType, gpuType = n.capacityType, n.gpuType
		}
		vns := nsOf(p)
		withNS := func(e usage.Event) usage.Event {
			if c.opts.MeterByNamespace && vns != "" {
				e.Dimensions[usage.DimNamespace] = vns
			}
			return e
		}
		allocs := append(c.gpuAllocs(p, n), dra.allocs(p.UID)...)
		if gpuType == "unknown" && dra != nil && dra.gpuType[p.UID] != "" {
			gpuType = dra.gpuType[p.UID]
		}
		if len(allocs) > 0 {
			st.GPUPods++
		}
		reqCPU, reqMem := podRequests(p)
		if c.opts.Basis == "requests" && reqCPU == 0 && reqMem == 0 && end.IsZero() {
			st.Unrequested++
		}
		// A pod of a paused cluster stops billing at the second of the
		// pause, even while it takes its grace period to stop.
		parts := [][2]time.Time{{start, end}}
		if len(paused) > 0 {
			parts = pause.Outside(paused, start, end)
		}
		for _, w := range windows {
			acc := accs[w.End]
			var billable, down time.Duration
			for _, pt := range parts {
				b, d := n.split(pt[0], pt[1], w.Start, w.End)
				billable, down = billable+b, down+d
			}
			for _, a := range allocs {
				sku := gpuType
				if n != nil && n.sku != "" {
					sku = n.sku
				}
				if a.profile != "full" {
					sku += "-" + a.profile
				}
				ev := withNS(base(usage.MetricGPUHours, n))
				ev.SKU = sku
				ev.Dimensions[usage.DimGPUType] = gpuType
				ev.Dimensions[usage.DimGPUProfile] = a.profile
				ev.Dimensions[usage.DimCapacityType] = capType
				ev.Dimensions[usage.DimBillingMode] = mode
				gpuEq := a.count * a.fraction
				acc.add(ev, gpuEq*billable.Hours(), map[string]any{"gpu_devices": a.count, "pods": 1})
				if down > 0 {
					dt := base(usage.MetricGPUDowntimeHours, n)
					dt.SKU = sku
					dt.Dimensions[usage.DimGPUType] = gpuType
					dt.Dimensions[usage.DimNode] = p.Spec.NodeName
					acc.add(dt, gpuEq*down.Hours(), map[string]any{"reason": "node not ready or unhealthy"})
				}
			}

			var cpu, mem float64
			sampled := false // a quantity came from a metrics-server sample
			switch c.opts.Basis {
			case "requests":
				cpu, mem = reqCPU, reqMem
			case "usage", "max":
				if !w.Latest {
					// metrics-server only knows usage now: a window
					// collected late has none.
					if c.opts.Basis == "max" {
						cpu, mem = reqCPU, reqMem
					} else if st != nil {
						st.UsageUnmeasured++
					}
					break
				}
				u := usageByPod[p.Namespace+"/"+p.Name]
				cpu, mem = u[0], u[1]
				sampled = cpu > 0 || mem > 0
				if c.opts.Basis == "max" {
					sampled = cpu > reqCPU || mem > reqMem
					cpu, mem = max(cpu, reqCPU), max(mem, reqMem)
				}
			}
			for _, m := range []struct {
				metric string
				qty    float64
			}{{usage.MetricCPUCoreHours, cpu}, {usage.MetricMemoryGBHours, mem / gib}} {
				if m.qty <= 0 {
					continue
				}
				ev := withNS(base(m.metric, n))
				ev.Dimensions[usage.DimCapacityType] = capType
				ev.Dimensions[usage.DimBillingMode] = mode
				props := map[string]any{"basis": c.opts.Basis}
				if sampled {
					props["estimate"] = true
				}
				acc.add(ev, m.qty*billable.Hours(), props)
			}
		}
	}
}

// meterWholeNode bills a node's full capacity (node-hours, whole GPUs, CPU
// and memory) to one tenant cluster: dedicated nodes of the control plane
// cluster and private nodes of the tenant cluster itself.
func (c *Collector) meterWholeNode(name string, n *nodeInfo, windows []Window, accs map[time.Time]*accumulator, base baseFn, mode string, paused pausedFn) {
	for _, w := range windows {
		acc := accs[w.End]
		billable, down := n.split(n.created, n.deletedAt, w.Start, w.End)
		if paused != nil {
			// A paused cluster's nodes are idle and unusable: their paused
			// time is neither usage nor downtime.
			from := n.downFrom(n.created, n.deletedAt)
			billable -= paused(n.created, from, w.Start, w.End)
			if down > 0 {
				down -= paused(from, n.deletedAt, w.Start, w.End)
			}
			billable, down = max(billable, 0), max(down, 0)
		}
		h := billable.Hours()
		sku := nodeSKU(n)
		nodeEv := base(usage.MetricPrivateNodeHours, n)
		nodeEv.ResourceID = name
		nodeEv.SKU = sku
		nodeEv.Dimensions[usage.DimCapacityType] = n.capacityType
		nodeEv.Dimensions[usage.DimBillingMode] = mode
		nodeEv.Dimensions[usage.DimNode] = name
		if n.instanceType != "" {
			nodeEv.Dimensions[usage.DimInstanceType] = n.instanceType
		}
		// vCPUs, memory, GPUs and public IP of the instance: not group keys,
		// so nothing is priced on them unless a billing platform chooses to.
		for k, v := range n.instanceAttributes() {
			nodeEv.Dimensions[k] = v
		}
		acc.add(nodeEv, h, nil)
		if gpus := n.physicalGPUs(); gpus > 0 {
			gev := base(usage.MetricGPUHours, n)
			gev.ResourceID = name
			gev.SKU = n.gpuType
			if n.sku != "" {
				gev.SKU = n.sku
			}
			gev.Dimensions[usage.DimGPUType] = n.gpuType
			gev.Dimensions[usage.DimGPUProfile] = "full"
			gev.Dimensions[usage.DimCapacityType] = n.capacityType
			gev.Dimensions[usage.DimBillingMode] = mode
			gev.Dimensions[usage.DimNode] = name
			acc.add(gev, gpus*h, map[string]any{"gpu_devices": float64(n.gpuDevices()), "physical_gpus": gpus})
			if down > 0 {
				dt := base(usage.MetricGPUDowntimeHours, n)
				dt.ResourceID = name
				dt.SKU = gev.SKU
				dt.Dimensions[usage.DimGPUType] = n.gpuType
				dt.Dimensions[usage.DimNode] = name
				acc.add(dt, gpus*down.Hours(), map[string]any{"reason": "node not ready or unhealthy"})
			}
		} else if lost := n.expectedGPUs(); lost > 0 && (h > 0 || down > 0) {
			// Installed but not allocatable: GPU downtime, never usage.
			dt := base(usage.MetricGPUDowntimeHours, n)
			dt.ResourceID = name
			dt.SKU = n.gpuType
			if n.sku != "" {
				dt.SKU = n.sku
			}
			dt.Dimensions[usage.DimGPUType] = n.gpuType
			dt.Dimensions[usage.DimNode] = name
			reason := "GPUs installed but not allocatable"
			if h == 0 {
				reason = "node not ready or unhealthy"
			}
			acc.add(dt, lost*(h+down.Hours()), map[string]any{"reason": reason})
		}
		for _, m := range []struct {
			metric string
			qty    float64
		}{{usage.MetricCPUCoreHours, n.cpuCapacity}, {usage.MetricMemoryGBHours, n.memCapacity / gib}} {
			ev := base(m.metric, n)
			ev.ResourceID = name
			ev.Dimensions[usage.DimCapacityType] = n.capacityType
			ev.Dimensions[usage.DimBillingMode] = mode
			ev.Dimensions[usage.DimNode] = name
			acc.add(ev, m.qty*h, nil)
		}
	}
}

// meterVolumes bills bound volumes per storage class.
func meterVolumes(pvcs []corev1.PersistentVolumeClaim, include func(map[string]string) bool, windows []Window, accs map[time.Time]*accumulator, base baseFn, paused pausedFn) {
	for _, pvc := range pvcs {
		if pvc.Status.Phase != corev1.ClaimBound || !include(pvc.Labels) {
			continue
		}
		size := pvc.Status.Capacity[corev1.ResourceStorage]
		if size.IsZero() {
			size = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		}
		class := "default"
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			class = *pvc.Spec.StorageClassName
		}
		for _, w := range windows {
			ov := usage.Overlap(pvc.CreationTimestamp.Time, time.Time{}, w.Start, w.End)
			if paused != nil {
				ov = max(ov-paused(pvc.CreationTimestamp.Time, time.Time{}, w.Start, w.End), 0)
			}
			ev := base(usage.MetricStorageGBHours, nil)
			ev.SKU = class
			accs[w.End].add(ev, float64(size.Value())/gib*ov.Hours(), map[string]any{"volumes": 1})
		}
	}
}

// meterLoadBalancers bills LoadBalancer services that have an address.
func meterLoadBalancers(svcs []corev1.Service, include func(*corev1.Service) bool, windows []Window, accs map[time.Time]*accumulator, base baseFn, paused pausedFn) {
	for i := range svcs {
		svc := &svcs[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || len(svc.Status.LoadBalancer.Ingress) == 0 || !include(svc) {
			continue
		}
		for _, w := range windows {
			ov := usage.Overlap(svc.CreationTimestamp.Time, time.Time{}, w.Start, w.End)
			if paused != nil {
				ov = max(ov-paused(svc.CreationTimestamp.Time, time.Time{}, w.Start, w.End), 0)
			}
			accs[w.End].add(base(usage.MetricLBHours, nil), ov.Hours(), map[string]any{"load_balancers": 1})
		}
	}
}

// baseFor returns the event template builder for a tenant cluster.
func (c *Collector) baseFor(t Target) baseFn {
	extID := t.Cluster.ExternalID()
	return func(metric string, n *nodeInfo) usage.Event {
		e := usage.Event{
			Tenant: t.Tenant, Metric: metric, Project: t.Project, ResourceID: extID,
			Region:     n.regionOr(c.opts.Region, c.opts.RegionFromNode),
			Dimensions: map[string]string{usage.DimTenantCluster: extID},
		}
		if t.TenantClass != "" {
			e.Dimensions[usage.DimTenantClass] = t.TenantClass
		}
		if n != nil && n.zone != "" {
			e.Dimensions[usage.DimZone] = n.zone
		}
		return e
	}
}
