package metrics

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Whole-node events describe the instance the way instance-level SKUs do:
// vCPUs and memory from the node's capacity, physical GPUs and whether it
// has a public IP. The attributes are not group keys.
func TestNodeHourEventsCarryInstanceAttributes(t *testing.T) {
	dedicated := map[string]string{"vcluster.loft.sh/managed-by": ns, "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}
	gpuNode := node("bm-01", dedicated, 8, true, func(n *corev1.Node) {
		n.Status.Capacity[corev1.ResourceMemory] = resource.MustParse("2063200Mi")
		n.Status.Capacity[corev1.ResourceEphemeralStorage] = resource.MustParse("960Gi")
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}, {Type: corev1.NodeExternalIP, Address: "203.0.113.7"}}
	})
	cpuNode := node("cpu-02", map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, true, func(n *corev1.Node) {
		n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("7500m")
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.6"}}
	})
	evs := collect(t, Options{}, []runtime.Object{gpuNode, cpuNode}, latest())[we]

	want := map[string]map[string]string{
		"bm-01": {usage.DimVCPUs: "64", usage.DimMemoryGiB: "2014.8", usage.DimGPUCount: "8", usage.DimPublicIP: "true", usage.DimDiskGiB: "960"},
		// A node that reports no disk of its own carries no disk_gib.
		"cpu-02": {usage.DimVCPUs: "7.5", usage.DimMemoryGiB: "512", usage.DimGPUCount: "0", usage.DimPublicIP: "false", usage.DimDiskGiB: ""},
	}
	for _, e := range evs {
		attrs, isNode := want[e.ResourceID]
		if e.Metric != usage.MetricPrivateNodeHours {
			if e.Dim(usage.DimVCPUs) != "" || e.Dim(usage.DimPublicIP) != "" {
				t.Errorf("%s event carries instance attributes: %v", e.Metric, e.Dimensions)
			}
			continue
		}
		if !isNode {
			t.Fatalf("unexpected node event %+v", e)
		}
		for k, v := range attrs {
			if got := e.Dim(k); got != v {
				t.Errorf("%s: %s = %q, want %q", e.ResourceID, k, got, v)
			}
		}
		delete(want, e.ResourceID)
	}
	if len(want) != 0 {
		t.Fatalf("no node-hour event for %v", want)
	}
	def, _ := usage.Lookup(usage.MetricPrivateNodeHours)
	for _, k := range []string{usage.DimVCPUs, usage.DimMemoryGiB, usage.DimGPUCount, usage.DimPublicIP, usage.DimDiskGiB} {
		if slices.Contains(def.GroupKeys, k) {
			t.Errorf("%s must not be a group key", k)
		}
	}
}
