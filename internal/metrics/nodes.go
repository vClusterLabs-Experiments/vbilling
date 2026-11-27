package metrics

import (
	"context"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// nodeInfo is everything metering needs to know about a node.
type nodeInfo struct {
	name         string
	uid          string
	created      time.Time
	systemUUID   string // status.nodeInfo: which machine the node reports it is
	machineID    string
	gpuType      string
	zone         string
	region       string
	capacityType string
	// capacityLabeled: a label gave the capacity type (without one it is
	// on-demand).
	capacityLabeled bool
	instanceType    string
	sku             string // value of SKU_LABEL, if configured and present
	gpuCapacity     int64  // devices of GPU_RESOURCES (nvidia.com/gpu, amd.com/gpu)
	cpuCapacity     float64
	memCapacity     float64 // bytes
	diskCapacity    float64 // bytes of the node's own disk (ephemeral-storage capacity); 0 = not reported
	gpuReplicas     int     // time-slicing replicas per physical GPU (1 = not shared)
	labels          map[string]string
	deletedAt       time.Time // set for nodes seen deleted (Auto Nodes scale-down); zero = present
	draGPUs         float64   // whole GPUs the node offers through DRA ResourceSlices
	externalIP      bool      // the node has an ExternalIP address

	// MIG and sharing. migProfile is set when every advertised GPU device is
	// a MIG slice (NVIDIA "single" strategy, GKE GPU partitions). Under the
	// "mixed" strategy slices are their own resources, summed in migMixed as
	// whole-GPU equivalents. sharedCapacity counts nvidia.com/gpu.shared.
	migProfile     string
	migMixed       float64
	migDevices     int64
	sharedCapacity int64

	// gpuFloor is the fewest physical GPUs the node is billed for: for a
	// cluster's own node, the most it had while it lived (privateNodeHistory).
	gpuFloor float64

	// Fair metering: a node that is not Ready, or carries an unhealthy
	// taint, is down. downSince is when that started (zero = unknown, so the
	// whole window counts as down: the tenant gets the benefit of the doubt).
	down      bool
	downSince time.Time
	// downs are the times the node was not ready that the watch saw end: a
	// node that is ready again shows nothing of them itself.
	downs []pause.Span
}

var gpuProductLabels = []string{
	"nvidia.com/gpu.product",
	"nvidia.com/gpu.machine",
	"amd.com/gpu.product-name",
	"cloud.google.com/gke-accelerator",
	"k8s.amazonaws.com/accelerator",
	"accelerator",
}

func (c *Collector) parseNode(n *corev1.Node) *nodeInfo { return c.parseNodeLease(n, time.Time{}) }

// parseNodeLease parses a node of the control plane cluster; renewed is its
// Lease's last renewal, read only for a node whose kubelet went silent
// (kubeletSilent).
func (c *Collector) parseNodeLease(n *corev1.Node, renewed time.Time) *nodeInfo {
	return c.parseNodeAs(n, renewed, false)
}

// parsePrivateNode parses a node read through a cluster's own API. Its
// labels, taints and status are written from inside the cluster, so they
// are trusted less than on the operator's control plane cluster:
//   - Only its Ready condition marks it down. Taints never do: anyone who
//     can write the node can add one with an effect the node controller
//     never reconciles, which evicts nothing.
//   - Time-slicing replicas divide its advertised GPUs only when GPU Feature
//     Discovery's count corroborates them (advertised devices = physical
//     GPUs x replicas). GKE's time-sharing labels never do: GKE does not
//     manage a cluster's own nodes.
//
// What it had when vBilling first saw it and the most GPUs it had are kept
// separately (privateNodeHistory): what changes later never lowers its bill.
func (c *Collector) parsePrivateNode(n *corev1.Node, renewed time.Time) *nodeInfo {
	return c.parseNodeAs(n, renewed, true)
}

func (c *Collector) parseNodeAs(n *corev1.Node, renewed time.Time, private bool) *nodeInfo {
	ni := &nodeInfo{
		name:         n.Name,
		uid:          string(n.UID),
		systemUUID:   strings.TrimSpace(n.Status.NodeInfo.SystemUUID),
		machineID:    strings.TrimSpace(n.Status.NodeInfo.MachineID),
		created:      n.CreationTimestamp.Time,
		gpuType:      "unknown",
		zone:         firstLabel(n.Labels, "topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
		region:       firstLabel(n.Labels, "topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"),
		instanceType: firstLabel(n.Labels, "node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
		gpuReplicas:  1,
		labels:       n.Labels,
	}
	ni.capacityType, ni.capacityLabeled = capacityTypeOf(n.Labels, c.opts.CapacityTypeLabel)
	if v := firstLabel(n.Labels, gpuProductLabels...); v != "" {
		ni.gpuType = normalizeGPUType(v)
	}
	// GPU Feature Discovery suffixes the product for sharing ("-SHARED") and
	// for MIG single strategy ("-MIG-1g.10gb"); bill against the base GPU.
	ni.gpuType = strings.TrimSuffix(ni.gpuType, "-SHARED")
	if gpu, profile, ok := strings.Cut(ni.gpuType, "-MIG-"); ok && gpu != "" && profile != "" {
		ni.gpuType, ni.migProfile = gpu, profile
	} else if p := n.Labels["cloud.google.com/gke-gpu-partition-size"]; p != "" {
		ni.migProfile = usage.SafeValue(p)
	}
	if c.opts.SKULabel != "" {
		ni.sku = usage.SafeValue(n.Labels[c.opts.SKULabel])
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
	if q, ok := n.Status.Capacity[corev1.ResourceEphemeralStorage]; ok {
		ni.diskCapacity = float64(q.Value())
	}
	for _, addr := range n.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && addr.Address != "" {
			ni.externalIP = true
		}
	}
	if r, err := strconv.Atoi(n.Labels["nvidia.com/gpu.replicas"]); err == nil && r > 1 &&
		n.Labels["nvidia.com/gpu.sharing-strategy"] != "none" && (!private || ni.replicasCorroborated(n.Labels, r)) {
		ni.gpuReplicas = r
	}
	// GKE shares GPUs natively (time-sharing or MPS) and labels the node itself.
	if s := n.Labels["cloud.google.com/gke-gpu-sharing-strategy"]; !private && (s == "time-sharing" || s == "mps") {
		if r, err := strconv.Atoi(n.Labels["cloud.google.com/gke-max-shared-clients-per-gpu"]); err == nil && r > 1 {
			ni.gpuReplicas = r
		}
	}

	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			since := cond.LastTransitionTime.Time
			if cond.Status == corev1.ConditionUnknown {
				since = lastProofOfLife(cond.LastHeartbeatTime.Time, renewed, since)
			}
			ni.markDown(since)
		}
	}
	if private {
		return ni
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

// replicasCorroborated reports whether GPU Feature Discovery's physical GPU
// count agrees with time-slicing replicas: a time-sliced node advertises
// every physical GPU replicas times (as nvidia.com/gpu, or renamed to
// nvidia.com/gpu.shared). Replicas a label claims on a node whose devices
// do not add up are not believed.
func (n *nodeInfo) replicasCorroborated(labels map[string]string, replicas int) bool {
	count, err := strconv.ParseInt(labels["nvidia.com/gpu.count"], 10, 64)
	if err != nil || count <= 0 {
		return false
	}
	want := count * int64(replicas)
	return n.gpuCapacity == want || n.sharedCapacity == want
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
// advertised, so a partly carved GPU never counts as more than one. It is
// never below gpuFloor.
func (n *nodeInfo) physicalGPUs() float64 {
	return max(n.advertisedGPUs(), n.gpuFloor)
}

// instanceGPUs is the GPU count of the instance a node is: the GPUs it
// bills (physicalGPUs), or the GPUs GPU Feature Discovery counts installed
// (nvidia.com/gpu.count) when that is more, as while the driver or device
// plugin starts after a boot and the node advertises none yet: an A100
// instance is an A100 instance from its first minute, not a CPU node until
// its driver is ready. Its SKU and gpu_count follow it. It is never below
// what the node bills, so no label written inside the cluster lowers them;
// the GPUs not advertised are GPU downtime, not GPU hours.
func (n *nodeInfo) instanceGPUs() float64 {
	return max(n.physicalGPUs(), n.expectedGPUs())
}

// advertisedGPUs is physicalGPUs from what the node advertises now alone.
func (n *nodeInfo) advertisedGPUs() float64 {
	shared := 0.0
	if n.sharedCapacity > 0 {
		shared = float64(n.sharedCapacity) / float64(max(n.gpuReplicas, 1))
	}
	return usage.Round(float64(n.gpuCapacity)*n.deviceShare() + shared + n.migMixed + n.draGPUs)
}

// instanceAttributes describe a whole node on its node-hour events, the way
// instance-level SKUs are defined: vCPUs and memory from its capacity,
// physical GPUs, and whether it has a public address.
func (n *nodeInfo) instanceAttributes() map[string]string {
	out := map[string]string{
		usage.DimVCPUs:     strconv.FormatFloat(n.cpuCapacity, 'f', -1, 64),
		usage.DimMemoryGiB: strings.TrimSuffix(strconv.FormatFloat(n.memCapacity/gib, 'f', 1, 64), ".0"),
		usage.DimGPUCount:  strconv.FormatFloat(n.instanceGPUs(), 'f', -1, 64),
		usage.DimPublicIP:  strconv.FormatBool(n.externalIP),
	}
	// The node's own disk, where its kubelet reports one.
	if n.diskCapacity > 0 {
		out[usage.DimDiskGiB] = strings.TrimSuffix(strconv.FormatFloat(n.diskCapacity/gib, 'f', 1, 64), ".0")
	}
	return out
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

// downSpans are the times the node was not ready: those the watch saw end
// (downs), and the one it is in now, from when it began (unknown: always).
func (n *nodeInfo) downSpans() []pause.Span {
	if n == nil {
		return nil
	}
	if !n.down {
		return n.downs
	}
	return pause.Union(n.downs, []pause.Span{{From: n.downSince}})
}

// split divides the part of [start, end) inside [ws, we) into billable and
// downtime durations for this node.
func (n *nodeInfo) split(start, end, ws, we time.Time) (billable, down time.Duration) {
	total := usage.Overlap(start, end, ws, we)
	if total == 0 || n == nil {
		return total, 0
	}
	spans := n.downSpans()
	if len(spans) == 0 {
		return total, 0
	}
	down = min(pause.Overlap(spans, start, end, ws, we), total)
	return total - down, down
}

func (n *nodeInfo) regionOr(fallback string, fromNode bool) string {
	if fromNode && n != nil && n.region != "" {
		return n.region
	}
	return fallback
}

// capacityTypeOf normalizes the many spot/preemptible label conventions,
// and says whether a label gave the capacity type (on-demand without one).
func capacityTypeOf(labels map[string]string, customKey string) (string, bool) {
	if v := labels[customKey]; v != "" && customKey != "" {
		return normalizeCapacity(v), true
	}
	for _, key := range []string{
		"karpenter.sh/capacity-type",
		"eks.amazonaws.com/capacityType",
		"kubernetes.io/lifecycle",
		"node.kubernetes.io/lifecycle",
	} {
		if v := labels[key]; v != "" {
			return normalizeCapacity(v), true
		}
	}
	if labels["cloud.google.com/gke-spot"] == "true" {
		return usage.CapacitySpot, true
	}
	if labels["cloud.google.com/gke-preemptible"] == "true" {
		return usage.CapacityPreemptible, true
	}
	return usage.CapacityOnDemand, false
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
	return usage.SafeValue(strings.ReplaceAll(strings.TrimSpace(raw), " ", "-"))
}

// firstLabel returns the first label set, made safe to use as a SKU or
// dimension value (usage.SafeValue): a cluster with its own nodes labels
// them itself.
func firstLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return usage.SafeValue(v)
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
	case n.instanceGPUs() > 0:
		return strconv.FormatFloat(n.instanceGPUs(), 'f', -1, 64) + "x" + n.gpuType
	default:
		return "cpu-node"
	}
}

// maxSilence bounds how long before the node controller's Unknown mark a
// proof of life may be. The controller marks a node Unknown after its grace
// period (40s, 50s from Kubernetes 1.32) without a heartbeat, checked every
// 5s, and kubelets renew their Lease every 10s: about a minute. An older
// status heartbeat says nothing about when the node died, because kubelets
// post status only on change or every 5 minutes.
const maxSilence = 2 * time.Minute

// lastProofOfLife is when a node whose kubelet went silent was last known
// alive: its latest status heartbeat or Lease renewal. The Unknown mark
// comes a grace period after that, so dating the outage from the mark bills
// that long after the node died (seen live: a preempted A100 spot VM, last
// heard from at 11:08:29, billed until the mark at 11:09:28). Without a
// recent proof of life the mark is used.
func lastProofOfLife(heartbeat, renewed, marked time.Time) time.Time {
	last := heartbeat
	if renewed.After(last) {
		last = renewed
	}
	if last.IsZero() || !last.Before(marked) || marked.Sub(last) > maxSilence {
		return marked
	}
	return last
}

// kubeletSilent reports whether the node controller marked the node Unknown
// because its kubelet stopped reporting.
func kubeletSilent(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionUnknown
		}
	}
	return false
}

// leaseRenewals reads the Lease of each node whose kubelet went silent (its
// last renewal is the last proof the node was alive). A Lease that cannot be
// read is skipped: the node's status heartbeat is used alone.
func leaseRenewals(ctx context.Context, kube kubernetes.Interface, nodes []corev1.Node) map[string]time.Time {
	var out map[string]time.Time
	for i := range nodes {
		if !kubeletSilent(&nodes[i]) {
			continue
		}
		l, err := kube.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, nodes[i].Name, metav1.GetOptions{})
		if err != nil || l.Spec.RenewTime == nil {
			continue
		}
		if out == nil {
			out = map[string]time.Time{}
		}
		out[nodes[i].Name] = l.Spec.RenewTime.Time
	}
	return out
}
