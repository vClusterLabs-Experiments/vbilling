package metrics

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Fourth review: what a cluster's own node bills as (privateNodeHistory).

// The history that keeps a node billing as first seen, and for no fewer
// GPUs than it had, is keyed by the node object's UID. The tenant is
// cluster admin inside its cluster: it deletes its node object and creates
// it again under the same name, with the labels it likes. The kubelet keeps
// running its pods and updates the new object's status (it reads its node
// by name), so nothing stops; with the device plugin (a DaemonSet inside
// the tenant's cluster) removed, the node advertises no GPUs, while the
// running pods keep the devices they were given. vBilling sees a node it
// never saw: a spot T4 node with no GPUs, instead of 8 on-demand H100s. No
// root on the node is needed, and the private-nodes source does not report
// a divergence.
func TestReview4RecreatedNodeObjectStartsItsBillingOver(t *testing.T) {
	now := we
	conn := tenantConn(h100Node())
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	readPrivate(t, c, &now, we)
	readPrivate(t, c, &now, we.Add(time.Minute)) // seen twice: 8 H100s on demand

	ctx := context.Background()
	if err := conn.Kube.CoreV1().Nodes().Delete(ctx, "gpu-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	again := h100Node(withUID("uid-gpu-1-recreated"), func(n *corev1.Node) {
		n.Labels["nvidia.com/gpu.product"] = "Tesla-T4"
		n.Labels["karpenter.sh/capacity-type"] = "spot"
		n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(0)
	})
	if _, err := conn.Kube.CoreV1().Nodes().Create(ctx, again, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	readPrivate(t, c, &now, we.Add(2*time.Minute))
	evs := readPrivate(t, c, &now, we.Add(3*time.Minute))
	if got, n := find(evs, usage.MetricGPUHours, "sku", "NVIDIA-H100-80GB-HBM3", usage.DimCapacityType, usage.CapacityOnDemand); n != 1 || !near(got, usage.Round(8.0/60)) {
		all, _ := find(evs, usage.MetricGPUHours)
		t.Fatalf("after the tenant recreated its node object gpu-1, it bills %v GPU-hours of H100 on demand (%v GPU-hours in all), want 8 GPUs "+
			"as vBilling first saw the node: the tenant reset what the node bills with two kubectl calls: %+v", got, all, evs)
	}
}

// The high-water mark rises to a count two reads 30 seconds apart both
// saw. When an operator turns on MIG (single strategy) or time-slicing on a
// node, the device plugin advertises the new devices within seconds, while
// the labels that convert them back to physical GPUs come from GPU Feature
// Discovery through Node Feature Discovery, whose worker relabels every 60
// seconds by default. Two vBilling reads in that gap set a high-water mark
// of every advertised device (56 GPUs for 8 H100s cut into 1g.10gb slices,
// 64 for 8 time-sliced 8 ways), billed for as long as the node object lives,
// although the labels add up from the next read on. (So does any transient
// over-count, such as a device plugin and a DRA driver both advertising the
// node's GPUs during a migration.)
func TestReview4GPUCountSeenBeforeItsLabelsBillsForever(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devices int64
		labels  map[string]string
	}{
		{"MIG single strategy", 56, map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3-MIG-1g.10gb", "nvidia.com/mig.strategy": "single"}},
		{"time-slicing", 64, map[string]string{"nvidia.com/gpu.count": "8", "nvidia.com/gpu.replicas": "8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := we
			conn := tenantConn(h100Node())
			c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
				Now: func() time.Time { return now }})
			c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
			readPrivate(t, c, &now, we)
			readPrivate(t, c, &now, we.Add(time.Minute)) // 8 whole GPUs

			// Reconfigured: the devices are advertised, the labels follow a
			// minute later.
			setNode(t, conn, h100Node(func(n *corev1.Node) { n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(tc.devices) }))
			readPrivate(t, c, &now, we.Add(2*time.Minute))
			readPrivate(t, c, &now, we.Add(3*time.Minute))
			setNode(t, conn, h100Node(func(n *corev1.Node) {
				n.Status.Capacity["nvidia.com/gpu"] = *resourceQty(tc.devices)
				for k, v := range tc.labels {
					n.Labels[k] = v
				}
			}))
			for i := 4; i <= 6; i++ {
				if got, _ := find(readPrivate(t, c, &now, we.Add(time.Duration(i)*time.Minute)), usage.MetricGPUHours); !near(got, usage.Round(8.0/60)) {
					t.Fatalf("read %d, labels in place since read 5: %v GPU-hours, want 8 physical GPUs (%d devices): two reads before the "+
						"labels caught up set a high-water mark of %v GPUs for as long as the node object lives", i+1, got, tc.devices, got*60)
				}
			}
		})
	}
}

// Labels that controllers put on a node after its kubelet registered it
// never count: the cloud controller manager sets the instance type, zone
// and region once it initializes the node, and Karpenter syncs its
// NodeClaim's labels (capacity type among them) onto the node after it
// registers. If vBilling's first read falls in that gap (it reads every
// minute; the gap is seconds), the node bills for as long as its object
// lives as it looked then: no instance type (its SKU falls back to
// "8xNVIDIA-H100-80GB-HBM3", which no price keyed on the instance type
// matches), no zone, and on-demand, the default for a missing capacity type,
// even for a reserved node. Only the GPU model is filled in from the first
// label that names one.
func TestReview4LabelsSetRightAfterRegistrationNeverCount(t *testing.T) {
	now := we
	registering := h100Node(func(n *corev1.Node) {
		delete(n.Labels, "node.kubernetes.io/instance-type")
		delete(n.Labels, "topology.kubernetes.io/zone")
		delete(n.Labels, "karpenter.sh/capacity-type")
		n.CreationTimestamp = metav1.NewTime(we.Add(-20 * time.Second)) // the kubelet registered it just before the first read
	})
	conn := tenantConn(registering)
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r", GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		Now: func() time.Time { return now }})
	c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": conn}})
	readPrivate(t, c, &now, we) // the first read lands between registration and labeling

	setNode(t, conn, h100Node(func(n *corev1.Node) { n.Labels["karpenter.sh/capacity-type"] = "reserved" }))
	for i := 1; i <= 3; i++ {
		evs := readPrivate(t, c, &now, we.Add(time.Duration(i)*time.Minute))
		if got, n := find(evs, usage.MetricPrivateNodeHours, "sku", "bm.gpu.h100.8", usage.DimCapacityType, usage.CapacityReserved,
			usage.DimZone, "syd-1a"); n != 1 || !near(got, usage.Round(1.0/60)) {
			all, _ := find(evs, usage.MetricPrivateNodeHours)
			t.Fatalf("read %d, labels in place since read 2: %v node-hours as bm.gpu.h100.8, reserved, in syd-1a (%v node-hours in all): "+
				"the labels the node got seconds after it registered are ignored for as long as it lives: %+v", i+1, got, all, evs)
		}
	}
}
