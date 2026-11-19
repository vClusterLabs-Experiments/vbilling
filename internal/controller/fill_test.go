package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// flakyTenantAPI is a cluster's own API that is down until up is set.
type flakyTenantAPI struct {
	mu   sync.Mutex
	up   bool
	conn *metrics.TenantConn
}

func (f *flakyTenantAPI) Conn(context.Context, discovery.TenantCluster) (*metrics.TenantConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return nil, errors.New("connection refused")
	}
	return f.conn, nil
}

// Events filled after a tenant API outage go to the ledger next to the
// events already committed for the window from what the control plane
// cluster showed. With METER_CONTROL_PLANE=true the control plane's volume
// and LoadBalancer and the cluster's own volume and LoadBalancer are built
// from the same template (finding 4 of the second review): every filled
// event must still carry an ID no committed event has, or billing
// platforms drop it as a duplicate.
func TestFilledEventsNeverReuseCommittedIDs(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MeterControlPlane = true })
	class := "fast-nvme"
	created := metav1.NewTime(t0.Add(-time.Hour))
	readyNode := func(name string, gpus int64) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: created},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), corev1.ResourceMemory: resource.MustParse("512Gi")},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		if gpus > 0 {
			n.Status.Capacity["nvidia.com/gpu"] = *resource.NewQuantity(gpus, resource.DecimalSI)
			n.Labels = map[string]string{"nvidia.com/gpu.product": "NVIDIA-A100-SXM4-80GB"}
		}
		return n
	}
	pvc := func(name, ns, size string, labels map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, CreationTimestamp: created},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}
	}
	lb := func(name, ns string, labels map[string]string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, CreationTimestamp: created},
			Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}}}}
	}
	cp := map[string]string{"app": "vcluster", "release": "gpu"}
	host := fake.NewSimpleClientset(readyNode("cp-worker-1", 0), pvc("data-gpu-0", "team-a", "5Gi", cp), lb("gpu", "team-a", cp))
	api := &flakyTenantAPI{conn: &metrics.TenantConn{Kube: fake.NewSimpleClientset(
		readyNode("gpu-a100-1", 8), pvc("ckpt", "training", "100Gi", nil), lb("serve", "inference", nil))}}
	coll := metrics.NewCollector(host, nil, metrics.Options{GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Region: "ap-southeast-2", MeterControlPlane: true, Now: func() time.Time { return h.now }})
	coll.UseTenantAPI(api)
	h.c.coll = coll
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "gpu", nil)}
	h.c.Reconcile(ctx)

	h.c.CollectDue(ctx) // the window ending 10:00, collected while the cluster's API is down
	api.mu.Lock()
	api.up = true
	api.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.c.CollectDue(ctx) // the next window, and the fill of the first

	recs, err := h.sp.Read(0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]usage.Event{}
	filled := map[string]int{}
	for _, r := range recs {
		if r.Event == nil {
			continue
		}
		e := *r.Event
		if prev, dup := seen[e.ID]; dup {
			t.Errorf("event %s (%s %v) has the ID of an event already in the ledger (%s %v): billing platforms drop it",
				e.ID, e.Metric, e.Quantity, prev.Metric, prev.Quantity)
		}
		seen[e.ID] = e
		if e.Properties["filled"] == true {
			filled[e.Metric]++
		}
	}
	for _, m := range []string{usage.MetricStorageGBHours, usage.MetricLBHours, usage.MetricPrivateNodeHours} {
		if filled[m] == 0 {
			t.Errorf("no %s filled for the window the cluster's API missed (filled: %v)", m, filled)
		}
	}
}
