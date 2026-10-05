package metrics

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Labels vCluster's syncer puts on objects it creates in the host namespace.
const (
	LabelManagedBy        = "vcluster.loft.sh/managed-by"
	LabelVirtualNamespace = "vcluster.loft.sh/namespace"
)

// gpuAlloc is one container's accelerator request.
type gpuAlloc struct {
	count    float64 // devices requested
	fraction float64 // GPU-equivalent per device (1, 1/7 for a 1g MIG slice, 1/replicas when time-sliced)
	profile  string  // full | mig-<profile> | timeslice-<n>
}

// podLifetime returns the billable interval of a pod: from the moment its
// first container started (image pulls and scheduling are not billed) to
// the moment its last container finished. A zero end means still running;
// started=false means nothing has run yet.
func podLifetime(p *corev1.Pod) (start, end time.Time, started bool) {
	running := false
	for _, cs := range p.Status.ContainerStatuses {
		var s time.Time
		switch {
		case cs.State.Running != nil:
			s = cs.State.Running.StartedAt.Time
			running = true
		case cs.State.Terminated != nil:
			s = cs.State.Terminated.StartedAt.Time
			if f := cs.State.Terminated.FinishedAt.Time; f.After(end) {
				end = f
			}
		}
		// A restarted container reports its latest start; the previous run
		// is in LastTerminationState.
		if lt := cs.LastTerminationState.Terminated; lt != nil && !lt.StartedAt.IsZero() && (s.IsZero() || lt.StartedAt.Time.Before(s)) {
			s = lt.StartedAt.Time
		}
		if !s.IsZero() && (start.IsZero() || s.Before(start)) {
			start = s
		}
	}
	if start.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	if running || (p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed) {
		end = time.Time{}
	}
	return start, end, true
}

// gpuAllocs extracts accelerator requests from a pod. Device plugins
// require requests == limits, so whichever is set counts.
func (c *Collector) gpuAllocs(p *corev1.Pod, n *nodeInfo) []gpuAlloc {
	var out []gpuAlloc
	full := map[string]bool{}
	for _, r := range c.opts.GPUResources {
		full[r] = true
	}
	for _, ctr := range p.Spec.Containers {
		seen := map[corev1.ResourceName]bool{}
		for _, list := range []corev1.ResourceList{ctr.Resources.Requests, ctr.Resources.Limits} {
			for name, q := range list {
				if seen[name] {
					continue
				}
				seen[name] = true
				count := float64(q.Value())
				if count <= 0 {
					continue
				}
				res := string(name)
				switch {
				case full[res]:
					// On MIG single-strategy or time-sliced nodes one device
					// is a slice or replica of a GPU, not a whole GPU.
					a := gpuAlloc{count: count, fraction: 1, profile: "full"}
					if n != nil {
						a.fraction, a.profile = n.deviceShare(), n.deviceProfile()
					}
					out = append(out, a)
				case res == "nvidia.com/gpu.shared":
					replicas := 1
					if n != nil && n.gpuReplicas > 1 {
						replicas = n.gpuReplicas
					}
					out = append(out, gpuAlloc{count: count, fraction: 1 / float64(replicas), profile: "timeslice-" + strconv.Itoa(replicas)})
				case strings.HasPrefix(res, "nvidia.com/mig-"):
					profile := strings.TrimPrefix(res, "nvidia.com/mig-")
					gpuType := ""
					if n != nil {
						gpuType = n.gpuType
					}
					out = append(out, gpuAlloc{count: count, fraction: migFraction(profile, gpuType), profile: "mig-" + profile})
				}
			}
		}
	}
	return out
}

// migFraction converts a MIG profile ("1g.10gb", "3g.40gb", "1g.10gb+me")
// to the share of a physical GPU's compute slices. A30 GPUs have 4 slices;
// A100/H100/H200/B200-class GPUs have 7.
func migFraction(profile, gpuType string) float64 {
	g := strings.IndexByte(profile, 'g')
	if g <= 0 {
		return 1
	}
	slices, err := strconv.Atoi(profile[:g])
	if err != nil || slices <= 0 {
		return 1
	}
	total := 7.0
	if strings.Contains(strings.ToUpper(gpuType), "A30") {
		total = 4
	}
	if f := float64(slices) / total; f < 1 {
		return f
	}
	return 1
}

// podRequests sums CPU (cores) and memory (bytes) requests of a pod's
// regular containers.
func podRequests(p *corev1.Pod) (cpu, mem float64) {
	for _, ctr := range p.Spec.Containers {
		if q, ok := ctr.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += float64(q.MilliValue()) / 1000
		}
		if q, ok := ctr.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += float64(q.Value())
		}
	}
	return cpu, mem
}

// isControlPlane reports whether a host object belongs to the tenant
// cluster's own control plane (its StatefulSet pods, data PVCs, API service).
func isControlPlane(labels map[string]string, clusterName string) bool {
	return labels["app"] == "vcluster" && (labels["release"] == clusterName || labels["release"] == "")
}
