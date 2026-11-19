package metrics

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Third review: a tenant is cluster admin inside its own cluster, so it
// writes the labels and taints of its private nodes (docs/security.html
// lists "labels on their own nodes" as tenant-controlled). With the default
// PRIVATE_NODE_BILLING=node, every input of a private node's bill is read
// from those objects.

// privateGPUNode is an 8-GPU private node of cluster "gpu", as the tenant
// API reports it, with whatever the tenant added.
func privateGPUNode(labels map[string]string, mods ...func(*corev1.Node)) *corev1.Node {
	l := map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "node.kubernetes.io/instance-type": "bm.gpu.h100.8"}
	for k, v := range labels {
		l[k] = v
	}
	return node("gpu-h100-1", l, 8, true, mods...)
}

func privateNodeBill(t *testing.T, n *corev1.Node) (nodeHours, gpuHours float64) {
	t.Helper()
	host := []runtime.Object{node("cp-worker-1", nil, 0, true)}
	tenant := tenantConn(n, pod("trainer", n.Name, withGPU("nvidia.com/gpu", 8)))
	// UNHEALTHY_NODE_TAINTS as config.Load defaults it.
	opts := Options{UnhealthyTaints: []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"}}
	out, _ := collectWith(t, opts, host, latest(), func(c *Collector) {
		c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenant}})
	})
	nodeHours, _ = find(out[we], usage.MetricPrivateNodeHours, "resource_id", n.Name)
	gpuHours, _ = find(out[we], usage.MetricGPUHours, usage.DimBillingMode, "private_node")
	return nodeHours, gpuHours
}

// The tenant labels its own private node vbilling.vcluster.com/billable=false
// (kubectl label node gpu-h100-1 vbilling.vcluster.com/billable=false). The
// node keeps running the tenant's 8-GPU job, and nothing of it is billed.
func TestReview3TenantLabelStopsBillingItsPrivateNode(t *testing.T) {
	wantNode, wantGPU := usage.Round(1.0/60), usage.Round(8.0/60)
	if n, g := privateNodeBill(t, privateGPUNode(nil)); !near(n, wantNode) || !near(g, wantGPU) {
		t.Fatalf("baseline: node-hours %v GPU-hours %v, want %v and %v", n, g, wantNode, wantGPU)
	}
	n, g := privateNodeBill(t, privateGPUNode(map[string]string{LabelBillable: "false"}))
	if !near(n, wantNode) || !near(g, wantGPU) {
		t.Fatalf("a label the tenant set on its own private node changed its bill: node-hours %v GPU-hours %v, want %v and %v "+
			"(an exemption must come from the operator, not from an object inside the tenant's cluster)", n, g, wantNode, wantGPU)
	}
}

// The tenant adds GPU Feature Discovery's time-slicing labels to a node
// whose device plugin still advertises its 8 whole GPUs. vBilling divides
// the advertised GPUs by the replica count and bills one GPU.
func TestReview3TenantLabelsDivideItsPrivateNodeGPUs(t *testing.T) {
	wantGPU := usage.Round(8.0 / 60)
	_, g := privateNodeBill(t, privateGPUNode(map[string]string{
		"nvidia.com/gpu.replicas":         "8",
		"nvidia.com/gpu.sharing-strategy": "time-slicing",
	}))
	if !near(g, wantGPU) {
		t.Fatalf("an 8-GPU private node running an 8-GPU job billed %v GPU-hours, want %v: "+
			"the tenant's own labels divided its GPU count", g, wantGPU)
	}
}

// The tenant taints its own Ready node with one of the default
// UNHEALTHY_NODE_TAINTS keys, with the PreferNoSchedule effect that the node
// lifecycle controller never reconciles and that evicts nothing. vBilling
// matches the key only, so the node counts as down for the whole window.
func TestReview3TenantTaintMarksItsRunningPrivateNodeDown(t *testing.T) {
	wantNode, wantGPU := usage.Round(1.0/60), usage.Round(8.0/60)
	n, g := privateNodeBill(t, privateGPUNode(nil, func(n *corev1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectPreferNoSchedule})
	}))
	if !near(n, wantNode) || !near(g, wantGPU) {
		t.Fatalf("a Ready private node with a tenant-added PreferNoSchedule taint billed node-hours %v GPU-hours %v, want %v and %v",
			n, g, wantNode, wantGPU)
	}
}

// vCluster's own placeholder label: the tenant labels its private node
// vcluster.loft.sh/fake-node=true. Metering skips it as a placeholder, and
// so does the pause drain (it is not "own"), so it is neither billed nor
// stopped.
func TestReview3TenantMarksItsPrivateNodeAsAPlaceholder(t *testing.T) {
	wantNode, wantGPU := usage.Round(1.0/60), usage.Round(8.0/60)
	n, g := privateNodeBill(t, privateGPUNode(map[string]string{LabelFakeNode: "true"}))
	if !near(n, wantNode) || !near(g, wantGPU) {
		t.Fatalf("a private node the tenant labeled %s=true billed node-hours %v GPU-hours %v, want %v and %v",
			LabelFakeNode, n, g, wantNode, wantGPU)
	}
}

// Whoever writes vcluster.yaml can turn on vCluster's namespace syncing
// (sync.toHost.namespaces), which creates the cluster's pods in host
// namespaces of their own instead of its control plane namespace. The
// collector lists pods only in the control plane namespace, so a GPU pod
// synced elsewhere is never billed (and a suspension label or a pause, which
// act on that one namespace too, never reach it).
func TestReview3PodsSyncedToAnotherHostNamespaceAreNeverBilled(t *testing.T) {
	gpuNode := node("h100-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, 8, true)
	inCP := pod("trainer", "h100-1", withGPU("nvidia.com/gpu", 4)) // in team-a, the control plane namespace
	elsewhere := pod("trainer-2", "h100-1", withGPU("nvidia.com/gpu", 4))
	elsewhere.Namespace = "gpu-x-ml" // a host namespace the cluster's namespace syncing created
	out := collect(t, Options{}, []runtime.Object{gpuNode, inCP, elsewhere}, latest())
	if got, _ := find(out[we], usage.MetricGPUHours); !near(got, usage.Round(8.0/60)) {
		t.Fatalf("cluster gpu runs 8 GPUs (4 in its control plane namespace, 4 synced to gpu-x-ml) and is billed %v GPU-hours, want %v",
			got, usage.Round(8.0/60))
	}
}

// A Prometheus named tenant-service://NS/NAME:PORT is a Service inside the
// tenant's cluster: the tenant writes its answers. vBilling reads them
// through the cluster's API proxy with client-go's Result.Raw, which keeps
// the whole body in memory, with no limit (the plain HTTP transport stops at
// 64 MiB, itself more than the chart's 256Mi memory limit once decoded). An
// answer that never ends exhausts vBilling's memory, and every collection
// queries it again: metering stops for every tenant.
func TestReview3TenantPrometheusAnswerIsReadWithoutBound(t *testing.T) {
	const limit = 64 << 20 // what the HTTP transport accepts
	var served atomic.Int64
	chunk := bytes.Repeat([]byte(" "), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < 80; i++ { // 80 MiB of JSON whitespace, then stop
			n, err := w.Write(chunk)
			served.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	tr := &kubeProxyTransport{kube: kube, namespace: "monitoring", service: "prometheus:9090"}
	_, _, _ = tr.get(context.Background(), "/api/v1/query", url.Values{"query": {"up"}})
	if got := served.Load(); got > limit+(1<<20) {
		t.Fatalf("vBilling read %d MiB of a tenant Service's answer into memory; bound it as the HTTP transport does (and lower both)", got>>20)
	}
}
