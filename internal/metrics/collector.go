// Package metrics turns the state of tenant clusters into usage events.
//
// Allocation metrics (GPUs, dedicated nodes, storage, load balancers,
// control plane hours, requests-based CPU/memory) are computed from object
// lifetimes overlapping each window, to the second. That is what makes
// backfill after a restart possible and keeps short jobs accurate.
// Consumption metrics (metrics-server CPU/memory, Prometheus egress and
// DCGM utilization) are sampled for the latest window only.
package metrics

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
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
	Now                     func() time.Time
}

// Collector gathers billable state from the Kubernetes API.
type Collector struct {
	kube      kubernetes.Interface
	metrics   metricsclient.Interface // nil disables usage-based CPU/memory
	prom      *prometheusClient
	graveyard *Graveyard        // deletions seen between collections; nil = none
	tenants   TenantAPI         // tenant clusters' own APIs; nil = control plane cluster only
	dyn       dynamic.Interface // DRA ResourceClaims and ResourceSlices; nil = none
	opts      Options

	excludedTenantNS map[string]bool
}

// UseGraveyard makes the collector bill pods and nodes deleted between
// collections (see Graveyard).
func (c *Collector) UseGraveyard(g *Graveyard) { c.graveyard = g }

// UseDynamic enables DRA (ResourceClaim) GPU metering on the control plane cluster.
func (c *Collector) UseDynamic(d dynamic.Interface) { c.dyn = d }

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
		opts.Basis = "usage"
	}
	egress, util, count := defaultEgressQuery, defaultGPUUtilQuery, defaultGPUCountQuery
	if opts.PrometheusPreset == PresetVClusterPlatform {
		egress, util, count = platformQueries.egress, platformQueries.util, platformQueries.count
	}
	if opts.EgressQuery == "" {
		opts.EgressQuery = egress
	}
	// A custom utilization query usually means a different exporter, so the
	// DCGM device count only rides along with the default query.
	if opts.GPUUtilQuery == "" {
		opts.GPUUtilQuery = util
		if opts.GPUCountQuery == "" {
			opts.GPUCountQuery = count
		}
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
	c := &Collector{kube: kube, metrics: mc, opts: opts, excludedTenantNS: map[string]bool{}}
	for _, ns := range opts.TenantExcludeNamespaces {
		c.excludedTenantNS[ns] = true
	}
	if opts.PrometheusURL != "" {
		c.prom = newPrometheusClient(opts.PrometheusURL, opts.PromHeaders, opts.PromTokenFile)
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
}

// Stats summarize one collection run.
type Stats struct {
	Pods         int
	GPUPods      int
	NodesDown    int
	Dedicated    int
	PromFailures int
	PrivateNodes int
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
	for i := range nodeList.Items {
		ni := c.parseNode(&nodeList.Items[i])
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
	for _, t := range targets {
		// External tenant clusters have nothing on this control plane cluster.
		if !onlyTenantAPI && !t.Cluster.External {
			if err := c.collectTarget(ctx, t, nodes, dra, windows, accs, &st); err != nil {
				return nil, st, fmt.Errorf("meter %s: %w", t.Cluster.ExternalID(), err)
			}
		}
		if c.tenants != nil {
			c.collectTenant(ctx, t, nodes, windows, accs, &st)
		}
	}
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

func (c *Collector) collectTarget(ctx context.Context, t Target, nodes map[string]*nodeInfo, dra *draIndex, windows []Window, accs map[time.Time]*accumulator, st *Stats) error {
	cl := t.Cluster
	ns := cl.Namespace
	extID := cl.ExternalID()

	base := c.baseFor(t)

	podList, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}
	c.graveyard.SeenPods(podList.Items, c.opts.Now())
	pods := withDeleted(podList.Items, c.graveyard.Pods(ns))
	owned := ownedFilter(entryLabels(pods), cl.Name, c.opts.MeterControlPlane)

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
	}

	virtualNS := func(p *corev1.Pod) string { return p.Labels[LabelVirtualNamespace] }
	c.meterPods(pods, func(p *corev1.Pod) bool { return owned(p.Labels) }, virtualNS,
		nodes, dedicated, dra, windows, accs, st, base, usageByPod, "shared")

	// Dedicated nodes: full capacity, per node.
	for name, n := range dedicated {
		if n.labels[LabelBillable] == "false" {
			continue // the tenant's own hardware: neither the node nor its pods are billed
		}
		c.meterWholeNode(name, n, windows, accs, base, "dedicated_node")
	}

	// Storage: bound PVCs synced from the tenant cluster, per storage class.
	pvcs, err := c.kube.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list pvcs: %w", err)
	}
	meterVolumes(pvcs.Items, ownedFilter(pvcLabels(pvcs.Items), cl.Name, c.opts.MeterControlPlane), windows, accs, base)

	// Load balancers with an assigned address.
	svcs, err := c.kube.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	meterLoadBalancers(svcs.Items, func(svc *corev1.Service) bool {
		return !isControlPlane(svc.Labels, cl.Name) || c.opts.MeterControlPlane
	}, windows, accs, base)

	// Control plane hours, only while ready (no charge while provisioning or asleep).
	if cl.Ready {
		for _, w := range windows {
			ov := usage.Overlap(cl.CreatedAt, time.Time{}, w.Start, w.End)
			accs[w.End].add(base(usage.MetricInstanceHours, nil), ov.Hours(), nil)
		}
	}

	if c.prom != nil && latest.Latest {
		c.collectPrometheus(ctx, t, latest, accs[latest.End], base, st)
	}
	return nil
}

func (c *Collector) collectPrometheus(ctx context.Context, t Target, w Window, acc *accumulator, base baseFn, st *Stats) {
	size := w.End.Sub(w.Start)
	vars := varsFor(t, size)
	id := t.Cluster.ExternalID()
	res, err := c.prom.Query(ctx, expand(c.opts.EgressQuery, vars), w.End)
	if err != nil {
		st.PromFailures++
		log.Printf("[metrics] %s: egress query failed: %v", id, err)
	} else {
		var bytes float64
		for _, r := range res {
			bytes += r.Value
		}
		acc.add(base(usage.MetricNetworkEgressGB, nil), bytes/gib, nil)
	}

	if c.opts.GPUUtilQuery != "none" {
		c.collectGPUUtilization(ctx, vars, id, w, acc, base, st)
	}

	for _, m := range c.opts.PromMetrics {
		res, err := c.prom.Query(ctx, expand(m.Query, vars), w.End)
		if err != nil {
			st.PromFailures++
			log.Printf("[metrics] %s: %s query failed: %v", id, m.Code, err)
			continue
		}
		for _, r := range res {
			ev := base(m.Code, nil)
			for _, l := range m.Dimensions {
				if v := r.Labels[l]; v != "" {
					ev.Dimensions[dimKey(l)] = v
				}
			}
			if m.SKU != "" {
				ev.SKU = r.Labels[m.SKU]
			}
			acc.add(ev, r.Value, nil)
		}
	}
}

func (c *Collector) collectGPUUtilization(ctx context.Context, vars queryVars, id string, w Window, acc *accumulator, base baseFn, st *Stats) {
	util, err := c.prom.Query(ctx, expand(c.opts.GPUUtilQuery, vars), w.End)
	if err != nil {
		st.PromFailures++
		log.Printf("[metrics] %s: GPU utilization query failed: %v", id, err)
		return
	}
	countBy := map[string]float64{}
	if c.opts.GPUCountQuery != "" && len(util) > 0 {
		counts, _ := c.prom.Query(ctx, expand(c.opts.GPUCountQuery, vars), w.End)
		for _, r := range counts {
			countBy[promGPUModel(r.Labels)] = r.Value
		}
	}
	for _, r := range util {
		model := promGPUModel(r.Labels)
		ev := base(usage.MetricGPUUtilization, nil)
		ev.Dimensions[usage.DimGPUType] = model
		props := map[string]any{"avg_utilization_pct": r.Value}
		if n, ok := countBy[model]; ok {
			props["gpu_devices"] = n
		}
		acc.add(ev, r.Value*w.End.Sub(w.Start).Hours(), props)
	}
}

// promGPUModel reads the GPU model from a utilization result: DCGM's
// modelName, or gpu_type/model from a custom GPU_UTIL_QUERY.
func promGPUModel(labels map[string]string) string {
	if v := firstLabel(labels, "modelName", "gpu_type", "model"); v != "" {
		return normalizeGPUType(v)
	}
	return "unknown"
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
		if isControlPlane(l, clusterName) {
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
	base baseFn, usageByPod map[string][2]float64, mode string) {
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
		for _, w := range windows {
			acc := accs[w.End]
			billable, down := n.split(start, end, w.Start, w.End)
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
			switch c.opts.Basis {
			case "requests":
				cpu, mem = reqCPU, reqMem
			case "usage", "max":
				if !w.Latest {
					if c.opts.Basis == "max" {
						cpu, mem = reqCPU, reqMem // usage is unknown for past windows
					}
					break
				}
				u := usageByPod[p.Namespace+"/"+p.Name]
				cpu, mem = u[0], u[1]
				if c.opts.Basis == "max" {
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
				acc.add(ev, m.qty*billable.Hours(), map[string]any{"basis": c.opts.Basis})
			}
		}
	}
}

// meterWholeNode bills a node's full capacity (node-hours, whole GPUs, CPU
// and memory) to one tenant cluster: dedicated nodes of the control plane
// cluster and private nodes of the tenant cluster itself.
func (c *Collector) meterWholeNode(name string, n *nodeInfo, windows []Window, accs map[time.Time]*accumulator, base baseFn, mode string) {
	for _, w := range windows {
		acc := accs[w.End]
		billable, down := n.split(n.created, n.deletedAt, w.Start, w.End)
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
func meterVolumes(pvcs []corev1.PersistentVolumeClaim, include func(map[string]string) bool, windows []Window, accs map[time.Time]*accumulator, base baseFn) {
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
			ev := base(usage.MetricStorageGBHours, nil)
			ev.SKU = class
			accs[w.End].add(ev, float64(size.Value())/gib*ov.Hours(), map[string]any{"volumes": 1})
		}
	}
}

// meterLoadBalancers bills LoadBalancer services that have an address.
func meterLoadBalancers(svcs []corev1.Service, include func(*corev1.Service) bool, windows []Window, accs map[time.Time]*accumulator, base baseFn) {
	for i := range svcs {
		svc := &svcs[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || len(svc.Status.LoadBalancer.Ingress) == 0 || !include(svc) {
			continue
		}
		for _, w := range windows {
			ov := usage.Overlap(svc.CreationTimestamp.Time, time.Time{}, w.Start, w.End)
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
