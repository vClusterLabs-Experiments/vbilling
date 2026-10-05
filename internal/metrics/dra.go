package metrics

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Dynamic Resource Allocation (resource.k8s.io): GPUs allocated through
// ResourceClaims never appear in a container's resource limits. A claim's
// status names the allocated devices and reservedFor names the pods using
// them; ResourceSlices describe each device (model, MIG profile) and which
// node it is on. Read through the dynamic client so every API version from
// v1beta1 (Kubernetes 1.32) to v1 works.

var draVersions = []string{"v1", "v1beta2", "v1beta1"}

// DefaultDRAGPUDrivers are the DRA drivers whose devices are GPUs.
var DefaultDRAGPUDrivers = []string{"gpu.nvidia.com", "gpu.amd.com"}

type draDevice struct {
	gpuType string
	profile string // MIG profile, "" for a whole GPU
	node    string
}

// draIndex is one collection's view of DRA GPU allocations.
type draIndex struct {
	byPod    map[types.UID][]gpuAlloc
	gpuType  map[types.UID]string
	nodeGPUs map[string]float64 // whole GPUs a node offers through DRA
	nodeType map[string]string
}

func (x *draIndex) allocs(uid types.UID) []gpuAlloc {
	if x == nil {
		return nil
	}
	return x.byPod[uid]
}

// draFor builds the index for a namespace ("" = all), or nil when the
// cluster has no DRA API or no client is configured.
func (c *Collector) draFor(ctx context.Context, dyn dynamic.Interface, namespace string) *draIndex {
	if dyn == nil {
		return nil
	}
	slices, version := listDRA(ctx, dyn, "resourceslices", "")
	if version == "" {
		return nil
	}
	gpuDrivers := map[string]bool{}
	for _, d := range c.opts.DRAGPUDrivers {
		gpuDrivers[d] = true
	}
	devices := map[string]draDevice{}
	x := &draIndex{byPod: map[types.UID][]gpuAlloc{}, gpuType: map[types.UID]string{}, nodeGPUs: map[string]float64{}, nodeType: map[string]string{}}
	for _, s := range slices {
		driver, _, _ := unstructured.NestedString(s.Object, "spec", "driver")
		if !gpuDrivers[driver] {
			continue
		}
		pool, _, _ := unstructured.NestedString(s.Object, "spec", "pool", "name")
		node, _, _ := unstructured.NestedString(s.Object, "spec", "nodeName")
		list, _, _ := unstructured.NestedSlice(s.Object, "spec", "devices")
		for _, raw := range list {
			dev, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := dev["name"].(string)
			attrs := deviceAttributes(dev)
			d := draDevice{gpuType: normalizeGPUType(attrs["productName"]), node: node}
			if strings.EqualFold(attrs["type"], "mig") {
				d.profile = attrs["profile"]
			} else if node != "" {
				x.nodeGPUs[node]++
				if d.gpuType != "" {
					x.nodeType[node] = d.gpuType
				}
			}
			devices[driver+"/"+pool+"/"+name] = d
		}
	}
	claims, _ := listDRAVersion(ctx, dyn, "resourceclaims", namespace, version)
	for _, cl := range claims {
		results, _, _ := unstructured.NestedSlice(cl.Object, "status", "allocation", "devices", "results")
		consumers, _, _ := unstructured.NestedSlice(cl.Object, "status", "reservedFor")
		var pods []types.UID
		for _, raw := range consumers {
			if r, ok := raw.(map[string]any); ok && r["resource"] == "pods" {
				if uid, ok := r["uid"].(string); ok {
					pods = append(pods, types.UID(uid))
				}
			}
		}
		if len(pods) == 0 {
			continue
		}
		share := 1 / float64(len(pods)) // a claim shared by several pods splits its devices
		for _, raw := range results {
			r, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			driver, _ := r["driver"].(string)
			if !gpuDrivers[driver] {
				continue
			}
			pool, _ := r["pool"].(string)
			name, _ := r["device"].(string)
			d := devices[driver+"/"+pool+"/"+name]
			a := gpuAlloc{count: share, fraction: 1, profile: "full"}
			if d.profile != "" {
				a.fraction, a.profile = migFraction(d.profile, d.gpuType), "mig-"+d.profile
			}
			for _, uid := range pods {
				x.byPod[uid] = append(x.byPod[uid], a)
				if d.gpuType != "" {
					x.gpuType[uid] = d.gpuType
				}
			}
		}
	}
	return x
}

// deviceAttributes flattens a device's attributes (v1beta1 nests them under
// "basic"; later versions do not) to name -> value, dropping the domain
// prefix of qualified names ("gpu.nvidia.com/productName").
func deviceAttributes(dev map[string]any) map[string]string {
	attrs, ok := dev["attributes"].(map[string]any)
	if !ok {
		if basic, ok := dev["basic"].(map[string]any); ok {
			attrs, _ = basic["attributes"].(map[string]any)
		}
	}
	out := map[string]string{}
	for k, raw := range attrs {
		if i := strings.LastIndex(k, "/"); i >= 0 {
			k = k[i+1:]
		}
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"string", "version"} {
			if s, ok := v[field].(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func listDRA(ctx context.Context, dyn dynamic.Interface, resource, namespace string) ([]unstructured.Unstructured, string) {
	for _, v := range draVersions {
		if items, ok := listDRAVersion(ctx, dyn, resource, namespace, v); ok {
			return items, v
		}
	}
	return nil, ""
}

func listDRAVersion(ctx context.Context, dyn dynamic.Interface, resource, namespace, version string) ([]unstructured.Unstructured, bool) {
	gvr := schema.GroupVersionResource{Group: "resource.k8s.io", Version: version, Resource: resource}
	var (
		list *unstructured.UnstructuredList
		err  error
	)
	if namespace == "" {
		list, err = dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	} else {
		list, err = dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return nil, false
	}
	return list.Items, true
}
