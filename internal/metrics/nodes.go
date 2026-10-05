package metrics

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// nodeInfo is everything metering needs to know about a node.
type nodeInfo struct {
	name         string
	created      time.Time
	gpuType      string
	zone         string
	region       string
	capacityType string
	instanceType string
	sku          string // value of SKU_LABEL, if configured and present
	gpuCapacity  int64  // devices of GPU_RESOURCES (nvidia.com/gpu, amd.com/gpu)
	cpuCapacity  float64
	memCapacity  float64 // bytes
	gpuReplicas  int     // time-slicing replicas per physical GPU (1 = not shared)
	labels       map[string]string
	deletedAt    time.Time // set for nodes seen deleted (Auto Nodes scale-down); zero = present
	draGPUs      float64   // whole GPUs the node offers through DRA ResourceSlices

	// MIG and sharing. migProfile is set when every advertised GPU device is
	// a MIG slice (NVIDIA "single" strategy, GKE GPU partitions). Under the
	// "mixed" strategy slices are their own resources, summed in migMixed as
	// whole-GPU equivalents. sharedCapacity counts nvidia.com/gpu.shared.
	migProfile     string
	migMixed       float64
	migDevices     int64
	sharedCapacity int64

	// Fair metering: a node that is not Ready, or carries an unhealthy
	// taint, is down. downSince is when that started (zero = unknown, so the
	// whole window counts as down: the tenant gets the benefit of the doubt).
	down      bool
	downSince time.Time
}

var gpuProductLabels = []string{
	"nvidia.com/gpu.product",
	"nvidia.com/gpu.machine",
	"amd.com/gpu.product-name",
	"cloud.google.com/gke-accelerator",
	"k8s.amazonaws.com/accelerator",
	"accelerator",
}

func (c *Collector) parseNode(n *corev1.Node) *nodeInfo {
	ni := &nodeInfo{
		name:         n.Name,
		created:      n.CreationTimestamp.Time,
		gpuType:      "unknown",
		zone:         firstLabel(n.Labels, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
		region:       firstLabel(n.Labels, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"),
		instanceType: firstLabel(n.Labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
		capacityType: capacityType(n.Labels, c.opts.CapacityTypeLabel),
		gpuReplicas:  1,
		labels:       n.Labels,
	}
	if v := firstLabel(n.Labels, gpuProductLabels...); v != "" {
		ni.gpuType = normalizeGPUType(v)
	}
	// GPU Feature Discovery suffixes the product for sharing ("-SHARED") and
	// for MIG single strategy ("-MIG-1g.10gb"); bill against the base GPU.
	ni.gpuType = strings.TrimSuffix(ni.gpuType, "-SHARED")
	if gpu, profile, ok := strings.Cut(ni.gpuType, "-MIG-"); ok && gpu != "" && profile != "" {
		ni.gpuType, ni.migProfile = gpu, profile
	} else if p := n.Labels["cloud.google.com/gke-gpu-partition-size"]; p != "" {
		ni.migProfile = p
	}
	if c.opts.SKULabel != "" {
		ni.sku = n.Labels[c.opts.SKULabel]
	}
	for _, res := range c.opts.GPUResources {
		if q, ok := n.Status.Capacity[corev1.ResourceName(res)]; ok {
			ni.gpuCapacity += q.Value()
		}
	}
	for name, q := range n.Status.Capacity {
		switch res := string(name); {
		case res == "nvidia.com/gpu.shared":
			ni.sharedCapacity += q.Value()
		case strings.HasPrefix(res, "nvidia.com/mig-"):
			ni.migDevices += q.Value()
			ni.migMixed += float64(q.Value()) * migFraction(strings.TrimPrefix(res, "nvidia.com/mig-"), ni.gpuType)
		}
	}
	if q, ok := n.Status.Capacity[corev1.ResourceCPU]; ok {
		ni.cpuCapacity = float64(q.MilliValue()) / 1000
	}
	if q, ok := n.Status.Capacity[corev1.ResourceMemory]; ok {
		ni.memCapacity = float64(q.Value())
	}
	if r, err := strconv.Atoi(n.Labels["nvidia.com/gpu.replicas"]); err == nil && r > 1 &&
		n.Labels["nvidia.com/gpu.sharing-strategy"] != "none" {
		ni.gpuReplicas = r
	}
	// GKE shares GPUs natively (time-sharing or MPS) and labels the node itself.
	if s := n.Labels["cloud.google.com/gke-gpu-sharing-strategy"]; s == "time-sharing" || s == "mps" {
		if r, err := strconv.Atoi(n.Labels["cloud.google.com/gke-max-shared-clients-per-gpu"]); err == nil && r > 1 {
			ni.gpuReplicas = r
		}
	}

	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			ni.markDown(cond.LastTransitionTime.Time)
		}
	}
	for _, t := range n.Spec.Taints {
		for _, key := range c.opts.UnhealthyTaints {
			if t.Key == key {
				var since time.Time
				if t.TimeAdded != nil {
					since = t.TimeAdded.Time
				}
				ni.markDown(since)
			}
		}
	}
	return ni
}

// deviceShare is the part of a physical GPU that one advertised GPU device
// is on this node: a MIG slice, a time-slicing replica, or both.
func (n *nodeInfo) deviceShare() float64 {
	share := 1.0
	if n.migProfile != "" {
		share = migFraction(n.migProfile, n.gpuType)
	}
	if n.gpuReplicas > 1 {
		share /= float64(n.gpuReplicas)
	}
	return share
}

// expectedGPUs is how many physical GPUs the node's GPU Feature Discovery
// labels say it has (0 without such labels). While a driver or device plugin
// restarts (after a reboot, a preemption or a MIG change) a node advertises
// no GPUs although they are installed.
func (n *nodeInfo) expectedGPUs() float64 {
	c, err := strconv.ParseFloat(n.labels["nvidia.com/gpu.count"], 64)
	if err != nil || c <= 0 {
		return 0
	}
	if n.migProfile != "" { // under the single strategy the count is MIG devices
		c *= migFraction(n.migProfile, n.gpuType)
	}
	return usage.Round(c)
}

// deviceProfile names how one advertised GPU device is carved up.
func (n *nodeInfo) deviceProfile() string {
	var parts []string
	if n.migProfile != "" {
		parts = append(parts, "mig-"+n.migProfile)
	}
	if n.gpuReplicas > 1 {
		parts = append(parts, "timeslice-"+strconv.Itoa(n.gpuReplicas))
	}
	if len(parts) == 0 {
		return "full"
	}
	return strings.Join(parts, "-")
}

// physicalGPUs converts everything the node advertises (whole GPUs, MIG
// slices, time-slicing replicas) to whole-GPU equivalents, the quantity a
// dedicated node is billed for. Slices that are not partitioned out are not
// advertised, so a partly carved GPU never counts as more than one.
func (n *nodeInfo) physicalGPUs() float64 {
	shared := 0.0
	if n.sharedCapacity > 0 {
		shared = float64(n.sharedCapacity) / float64(max(n.gpuReplicas, 1))
	}
	return usage.Round(float64(n.gpuCapacity)*n.deviceShare() + shared + n.migMixed + n.draGPUs)
}

// applyDRANodes adds the GPUs nodes offer through DRA, which no extended
// resource reports.
func applyDRANodes(nodes map[string]*nodeInfo, dra *draIndex) {
	if dra == nil {
		return
	}
	for name, n := range nodes {
		n.draGPUs = dra.nodeGPUs[name]
		if n.gpuType == "unknown" && dra.nodeType[name] != "" {
			n.gpuType = dra.nodeType[name]
		}
	}
}

// gpuDevices is the number of GPU devices the node advertises.
func (n *nodeInfo) gpuDevices() int64 { return n.gpuCapacity + n.sharedCapacity + n.migDevices }

func (n *nodeInfo) markDown(since time.Time) {
	if !n.down {
		n.down, n.downSince = true, since
		return
	}
	// Keep the earliest known start; zero (unknown) dominates.
	if since.IsZero() || (!n.downSince.IsZero() && since.Before(n.downSince)) {
		n.downSince = since
	}
}

// split divides the part of [start, end) inside [ws, we) into billable and
// downtime durations for this node.
func (n *nodeInfo) split(start, end, ws, we time.Time) (billable, down time.Duration) {
	total := usage.Overlap(start, end, ws, we)
	if total == 0 || n == nil || !n.down {
		return total, 0
	}
	from := start
	if !n.downSince.IsZero() && n.downSince.After(from) {
		from = n.downSince
	}
	down = usage.Overlap(from, end, ws, we)
	return total - down, down
}

func (n *nodeInfo) regionOr(fallback string, fromNode bool) string {
	if fromNode && n != nil && n.region != "" {
		return n.region
	}
	return fallback
}

// capacityType normalizes the many spot/preemptible label conventions.
func capacityType(labels map[string]string, customKey string) string {
	if v := labels[customKey]; v != "" && customKey != "" {
		return normalizeCapacity(v)
	}
	for _, key := range []string{
		"karpenter.sh/capacity-type",
		"eks.amazonaws.com/capacityType",
		"kubernetes.io/lifecycle",
		"node.kubernetes.io/lifecycle",
	} {
		if v := labels[key]; v != "" {
			return normalizeCapacity(v)
		}
	}
	if labels["cloud.google.com/gke-spot"] == "true" {
		return usage.CapacitySpot
	}
	if labels["cloud.google.com/gke-preemptible"] == "true" {
		return usage.CapacityPreemptible
	}
	return usage.CapacityOnDemand
}

func normalizeCapacity(v string) string {
	switch strings.ToLower(strings.ReplaceAll(v, "_", "-")) {
	case "spot":
		return usage.CapacitySpot
	case "preemptible", "preemptable", "reclaimable":
		return usage.CapacityPreemptible
	case "reserved", "committed", "dedicated":
		return usage.CapacityReserved
	default:
		return usage.CapacityOnDemand
	}
}

func normalizeGPUType(raw string) string {
	return strings.ReplaceAll(strings.TrimSpace(raw), " ", "-")
}

func firstLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// dedicatedTo reports whether a node is dedicated to the tenant cluster.
// Conventions (any match): vbilling.vcluster.com/tenant-cluster=<external
// ID or name>, vcluster.loft.sh/managed-by or vcluster.loft.sh/cluster =
// <namespace or name>, or the VCLUSTER_NODE_LABEL pattern ("key=%s").
func (c *Collector) dedicatedTo(n *nodeInfo, name, namespace, externalID string) bool {
	l := n.labels
	if v := l["vbilling.vcluster.com/tenant-cluster"]; v != "" && (v == externalID || v == name) {
		return true
	}
	candidates := []string{namespace, name}
	for _, prefix := range []string{"vcluster-", "vc-"} { // v0.1 compatibility
		if strings.HasPrefix(namespace, prefix) {
			candidates = append(candidates, strings.TrimPrefix(namespace, prefix))
		}
	}
	for _, key := range []string{"vcluster.loft.sh/managed-by", "vcluster.loft.sh/cluster"} {
		v := l[key]
		for _, cand := range candidates {
			if v != "" && v == cand {
				return true
			}
		}
	}
	if c.opts.DedicatedNodeLabel != "" {
		if key, pattern, ok := strings.Cut(c.opts.DedicatedNodeLabel, "="); ok {
			for _, cand := range candidates {
				if l[key] != "" && l[key] == strings.ReplaceAll(pattern, "%s", cand) {
					return true
				}
			}
		}
	}
	return false
}

// nodeSKU is the SKU for whole-node billing.
func nodeSKU(n *nodeInfo) string {
	switch {
	case n.sku != "":
		return n.sku
	case n.instanceType != "":
		return n.instanceType
	case n.physicalGPUs() > 0:
		return strconv.FormatFloat(n.physicalGPUs(), 'f', -1, 64) + "x" + n.gpuType
	default:
		return "cpu-node"
	}
}
