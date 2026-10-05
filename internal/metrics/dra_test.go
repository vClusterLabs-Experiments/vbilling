package metrics

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func draObj(kind, ns, name string, spec, status map[string]any) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "resource.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"name": name}, "spec": spec}}
	if ns != "" {
		o.SetNamespace(ns)
	}
	if status != nil {
		o.Object["status"] = status
	}
	return o
}

func gpuDevice(name, typ, profile string) map[string]any {
	attrs := map[string]any{"type": map[string]any{"string": typ}, "productName": map[string]any{"string": "NVIDIA H200"}}
	if profile != "" {
		attrs["gpu.nvidia.com/profile"] = map[string]any{"string": profile} // qualified name
	}
	return map[string]any{"name": name, "attributes": attrs}
}

func claim(name, device, podUID string) *unstructured.Unstructured {
	return draObj("ResourceClaim", ns, name, map[string]any{}, map[string]any{
		"allocation":  map[string]any{"devices": map[string]any{"results": []any{map[string]any{"request": "gpu", "driver": "gpu.nvidia.com", "pool": "dra-node-1", "device": device}}}},
		"reservedFor": []any{map[string]any{"resource": "pods", "name": name, "uid": podUID}},
	})
}

func TestDRAClaimedGPUs(t *testing.T) {
	slice := draObj("ResourceSlice", "", "dra-node-1-gpus", map[string]any{"driver": "gpu.nvidia.com", "nodeName": "dra-node-1",
		"pool":    map[string]any{"name": "dra-node-1", "generation": int64(1), "resourceSliceCount": int64(1)},
		"devices": []any{gpuDevice("gpu-0", "gpu", ""), gpuDevice("gpu-1", "gpu", ""), gpuDevice("mig-1g", "mig", "1g.18gb")}}, nil)
	dedicatedSlice := draObj("ResourceSlice", "", "dra-ded-gpus", map[string]any{"driver": "gpu.nvidia.com", "nodeName": "dra-ded",
		"pool":    map[string]any{"name": "dra-ded", "generation": int64(1), "resourceSliceCount": int64(1)},
		"devices": []any{gpuDevice("gpu-0", "gpu", ""), gpuDevice("gpu-1", "gpu", "")}}, nil)
	gvrs := map[schema.GroupVersionResource]string{
		{Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims"}: "ResourceClaimList",
		{Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices"}: "ResourceSliceList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), gvrs,
		slice, dedicatedSlice, claim("trainer", "gpu-0", "uid-trainer"), claim("small", "mig-1g", "uid-small"))

	trainer := pod("trainer", "dra-node-1")
	trainer.UID = types.UID("uid-trainer")
	small := pod("small", "dra-node-1")
	small.UID = types.UID("uid-small")
	host := []runtime.Object{node("dra-node-1", nil, 0, true), node("dra-ded", map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, true), trainer, small}
	opts := Options{GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests", Region: "r", Now: func() time.Time { return we }}
	c := NewCollector(fake.NewSimpleClientset(host...), nil, opts)
	c.UseDynamic(dyn)
	out, _, err := c.Collect(context.Background(), []Target{target("gpu", true)}, latest())
	if err != nil {
		t.Fatal(err)
	}
	evs := out[we]
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H200", usage.DimBillingMode, "shared"); !near(got, usage.Round(1.0/60)) {
		t.Errorf("DRA whole-GPU hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H200-mig-1g.18gb"); !near(got, usage.Round((1.0/7)/60)) {
		t.Errorf("DRA MIG hours = %v", got)
	}
	if got, _ := find(evs, usage.MetricGPUHours, usage.DimBillingMode, "dedicated_node", "sku", "NVIDIA-H200"); !near(got, usage.Round(2.0/60)) {
		t.Errorf("dedicated node with DRA-only GPUs: GPU hours = %v, want its 2 GPUs", got)
	}
}
