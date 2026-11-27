package metrics

import (
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pricingKeys sets PRICING_KEYS for one test and clears it after.
func pricingKeys(t *testing.T, spec string) {
	t.Helper()
	keys, err := usage.ParsePricingKeys(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := usage.SetPricingKeys(keys); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { usage.SetPricingKeys(nil) })
}

// addresses sets a node's addresses: ExternalIP for each address given.
func addresses(external ...string) func(*corev1.Node) {
	return func(n *corev1.Node) {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}, {Type: corev1.NodeHostName, Address: n.Name}}
		for _, a := range external {
			n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: a})
		}
	}
}

// A public IP is a unicast address outside the private, shared, loopback,
// link-local and IPv6 unique-local ranges.
func TestPublicIPAddresses(t *testing.T) {
	for addr, public := range map[string]bool{
		"203.0.113.7": true, "198.51.100.20": true, "8.8.8.8": true, "100.128.0.1": true, "100.63.255.255": true,
		"2600:1f18:abcd::10": true, "::ffff:203.0.113.7": true, " 203.0.113.8 ": true,
		"10.1.2.3": false, "172.16.0.1": false, "172.31.255.254": false, "192.168.1.10": false, // RFC 1918
		"100.64.0.1": false, "100.127.255.255": false, // shared (RFC 6598)
		"127.0.0.1": false, "::1": false, // loopback
		"169.254.10.10": false, "fe80::1": false, "fe80::1%eth0": false, // link-local
		"fd12:3456::1": false, "fc00::1": false, // IPv6 unique-local
		"::ffff:10.0.0.1": false, "0.0.0.0": false, "::": false, "224.0.0.1": false, "ff02::1": false, "255.255.255.255": false,
		"": false, "node-1.example.com": false, "203.0.113.7/32": false,
	} {
		if _, ok := publicIP(addr); ok != public {
			t.Errorf("publicIP(%q) = %v, want %v", addr, ok, public)
		}
	}
	if ip, _ := publicIP("::ffff:203.0.113.7"); !ip.Is4() {
		t.Error("an IPv4-mapped address is an IPv4 address")
	}
}

// Public IP hours come from the lifetimes node-hours use: every public
// ExternalIP of a node billed whole, for the time its node-hours bill. A node
// with two public addresses counts two; private, shared, link-local and
// unique-local addresses count none; a load balancer's address is load
// balancer hours, never public IP hours; a shared node bills no addresses.
func TestPublicIPHoursFromNodeLifetimes(t *testing.T) {
	dedicated := map[string]string{"vcluster.loft.sh/managed-by": ns, "topology.kubernetes.io/zone": "syd-1a"}
	objs := []runtime.Object{
		// Created 15s into the window: billed for 45s.
		node("bm-01", dedicated, 0, true, createdAt(ws.Add(15*time.Second)), addresses("203.0.113.7", "198.51.100.20", "203.0.113.7",
			"10.1.2.3", "100.64.3.4", "169.254.10.10", "fd00::5", "2600:1f18:abcd::10")),
		node("bm-02", map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, true, addresses("192.168.1.10")),
		// Not ready from 30s into the window: neither its node-hours nor its address bill after that.
		node("bm-03", map[string]string{"vcluster.loft.sh/managed-by": ns}, 0, false, addresses("203.0.113.30"), func(n *corev1.Node) {
			n.Status.Conditions[0].LastTransitionTime = metav1.NewTime(ws.Add(30 * time.Second))
		}),
		node("shared-1", nil, 0, true, addresses("203.0.113.40")),
		pod("web", "shared-1", withRequests("1", "1Gi")),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"},
			CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.50"}}}}},
	}
	evs := collect(t, Options{}, objs, latest())[we]

	if got, n := find(evs, usage.MetricPublicIPHours, "resource_id", "bm-01", usage.DimIPFamily, "ipv4"); n != 1 || !near(got, usage.Round(2*45/3600.0)) {
		t.Errorf("bm-01 IPv4 = %v (%d events), want two addresses for 45s", got, n)
	}
	if got, n := find(evs, usage.MetricPublicIPHours, "resource_id", "bm-01", usage.DimIPFamily, "ipv6"); n != 1 || !near(got, usage.Round(45/3600.0)) {
		t.Errorf("bm-01 IPv6 = %v (%d events), want one address for 45s", got, n)
	}
	if nodeH, _ := find(evs, usage.MetricPrivateNodeHours, "resource_id", "bm-01"); !near(nodeH, usage.Round(45/3600.0)) {
		t.Errorf("bm-01 node hours = %v", nodeH)
	}
	if _, n := find(evs, usage.MetricPublicIPHours, "resource_id", "bm-02"); n != 0 {
		t.Error("a node whose ExternalIP is private bills a public IP")
	}
	if got, _ := find(evs, usage.MetricPublicIPHours, "resource_id", "bm-03"); !near(got, usage.Round(30/3600.0)) {
		t.Errorf("bm-03 = %v, want the 30s its node-hours bill", got)
	}
	if _, n := find(evs, usage.MetricPublicIPHours, "resource_id", "shared-1"); n != 0 {
		t.Error("a shared node's address was billed to a tenant")
	}
	all, _ := find(evs, usage.MetricPublicIPHours)
	if !near(all, usage.Round(2*45/3600.0)+usage.Round(45/3600.0)+usage.Round(30/3600.0)) {
		t.Errorf("all public IP hours = %v: the load balancer's address must not be among them", all)
	}
	if lb, n := find(evs, usage.MetricLBHours); n != 1 || !near(lb, usage.Round(1.0/60)) {
		t.Errorf("load balancer hours = %v (%d)", lb, n)
	}

	for _, e := range evs {
		switch {
		case e.Metric == usage.MetricPrivateNodeHours:
			want := map[string]string{"bm-01": "true", "bm-02": "false", "bm-03": "true"}[e.ResourceID]
			if e.Dim(usage.DimPublicIP) != want {
				t.Errorf("%s: public_ip %q, want %q", e.ResourceID, e.Dim(usage.DimPublicIP), want)
			}
		case e.Metric == usage.MetricPublicIPHours && e.ResourceID == "bm-01":
			if e.Region != "ap-southeast-2" || e.Dim(usage.DimZone) != "syd-1a" || e.Dim(usage.DimBillingMode) != "dedicated_node" ||
				e.Dim(usage.DimCapacityType) != usage.CapacityOnDemand || e.Dim(usage.DimNode) != "bm-01" || e.Tenant != "acme" || e.SKU != "" {
				t.Errorf("public IP event attribution: %+v", e)
			}
			if want := map[string]int{"ipv4": 2, "ipv6": 1}[e.Dim(usage.DimIPFamily)]; e.Properties["public_ips"] != want {
				t.Errorf("%s: public_ips %v, want %d", e.Dim(usage.DimIPFamily), e.Properties["public_ips"], want)
			}
		}
	}
}

// A cluster's own nodes bill their public addresses as private nodes; with
// PRIVATE_NODE_BILLING=usage they are not billed whole, and neither are
// their addresses.
func TestPublicIPHoursOfPrivateNodes(t *testing.T) {
	own := node("gpu-1", map[string]string{"node.kubernetes.io/instance-type": "bm.gpu.h100.8"}, 8, true, addresses("203.0.113.70"))
	for _, billing := range []string{"node", "usage"} {
		out, _ := collectWith(t, Options{PrivateNodeBilling: billing}, nil, latest(), func(c *Collector) {
			c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenantConn(own.DeepCopy())}})
		})
		got, n := find(out[we], usage.MetricPublicIPHours, "resource_id", "gpu-1", usage.DimBillingMode, "private_node", usage.DimIPFamily, "ipv4")
		switch {
		case billing == "node" && (n != 1 || !near(got, usage.Round(1.0/60))):
			t.Errorf("private node's address = %v (%d events)", got, n)
		case billing == "usage" && n != 0:
			t.Errorf("PRIVATE_NODE_BILLING=usage billed the node's address: %v", got)
		}
	}
}

// With sku among their pricing keys, CPU core-hours and memory GiB-hours
// carry the SKU of the node they ran on, the one its node-hours carry: whole
// nodes (dedicated and private) and pods on shared nodes alike, so profiles
// of one cluster are priced apart. Without it they carry none, as before.
func TestCPUAndMemoryCarryTheNodeSKUWhenPricedOnIt(t *testing.T) {
	const skuLabel = "vbilling.vcluster.com/sku"
	objs := []runtime.Object{
		node("bm-01", map[string]string{"vcluster.loft.sh/managed-by": ns, skuLabel: "gpu-h100-8"}, 8, true),
		node("cpu-a", map[string]string{skuLabel: "cpu-standard"}, 0, true),
		node("cpu-b", map[string]string{"node.kubernetes.io/instance-type": "m7i.2xlarge"}, 0, true),
		pod("on-a", "cpu-a", withRequests("2", "4Gi")),
		pod("on-a-too", "cpu-a", withRequests("1", "2Gi")),
		pod("on-b", "cpu-b", withRequests("4", "8Gi")),
		pod("node-gone", "deleted-node", withRequests("1", "1Gi")),
	}
	own := node("own-1", map[string]string{skuLabel: "gpu-a100-8"}, 8, true)
	run := func() []usage.Event {
		out, _ := collectWith(t, Options{SKULabel: skuLabel}, objs, latest(), func(c *Collector) {
			c.UseTenantAPI(fakeTenantAPI{conns: map[string]*TenantConn{"gpu": tenantConn(own.DeepCopy())}})
		})
		return out[we]
	}

	for _, e := range run() {
		if (e.Metric == usage.MetricCPUCoreHours || e.Metric == usage.MetricMemoryGBHours) && e.SKU != "" {
			t.Errorf("without PRICING_KEYS %s carries SKU %q", e.Metric, e.SKU)
		}
	}

	pricingKeys(t, "vcluster_cpu_core_hours:sku")
	evs := run()
	for _, c := range []struct {
		sku, mode string
		cores     float64
	}{
		{"gpu-h100-8", "dedicated_node", 64}, {"gpu-a100-8", "private_node", 64}, // whole nodes: their capacity
		{"cpu-standard", "shared", 3}, {"m7i.2xlarge", "shared", 4}, // pods on shared nodes: their requests
		{"", "shared", 1}, // the node is gone: no SKU (a placeholder at Metronome)
	} {
		if got, n := find(evs, usage.MetricCPUCoreHours, "sku", c.sku, usage.DimBillingMode, c.mode); n != 1 || !near(got, usage.Round(c.cores/60)) {
			t.Errorf("CPU of sku %q (%s) = %v (%d events), want %v cores for a minute", c.sku, c.mode, got, n, c.cores)
		}
	}
	for _, name := range []string{"bm-01", "own-1"} {
		var nodeSKU string
		for _, e := range evs {
			if e.Metric == usage.MetricPrivateNodeHours && e.ResourceID == name {
				nodeSKU = e.SKU
			}
		}
		if _, n := find(evs, usage.MetricCPUCoreHours, "resource_id", name, "sku", nodeSKU); nodeSKU == "" || n != 1 {
			t.Errorf("%s: CPU hours do not carry its node-hours' SKU %q", name, nodeSKU)
		}
	}
	for _, e := range evs {
		if e.Metric == usage.MetricMemoryGBHours && e.SKU != "" {
			t.Errorf("memory is not priced on sku, yet carries %q", e.SKU)
		}
	}
}

// Every key PRICING_KEYS may add to a metric is on the metric's events, so
// a billing platform that requires every group key (Metronome) counts them.
// The scenario gives the collector everything it can describe: zone,
// instance type, SKU, disk, public addresses of both families, a tenant
// class and a project.
func TestEveryPricingKeyChoiceIsOnTheEvents(t *testing.T) {
	pricingKeys(t, "vcluster_cpu_core_hours:sku,vcluster_memory_gb_hours:sku")
	labels := map[string]string{"vcluster.loft.sh/managed-by": ns, "topology.kubernetes.io/zone": "syd-1a",
		"node.kubernetes.io/instance-type": "bm.gpu.h100.8", "nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3", "vbilling.vcluster.com/sku": "gpu-h100-8"}
	class := "fast-nvme"
	objs := []runtime.Object{
		node("bm-01", labels, 8, true, addresses("203.0.113.7", "2600:1f18:abcd::10"), func(n *corev1.Node) {
			n.Status.Capacity[corev1.ResourceEphemeralStorage] = resource.MustParse("960Gi")
		}),
		node("shared-1", map[string]string{"topology.kubernetes.io/zone": "syd-1b", "nvidia.com/gpu.product": "NVIDIA-L4"}, 4, true),
		pod("infer", "shared-1", withGPU("nvidia.com/gpu", 1), withRequests("2", "4Gi")),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}},
			Spec:   corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status: corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.50"}}}}},
	}
	tgt := target("gpu", true)
	tgt.TenantClass = "enterprise"
	prom, _ := promServer(t, nil, func(q string) (string, map[string]string) {
		if strings.Contains(q, "container_network_transmit_bytes_total") {
			return strconv.Itoa(gib), map[string]string{"pod": "infer"}
		}
		return "", nil
	})
	evs := collect(t, Options{SKULabel: "vbilling.vcluster.com/sku", PrometheusURL: prom.URL}, objs, latest(), tgt)[we]

	value := func(e usage.Event, k string) string {
		switch k {
		case "sku":
			return e.SKU
		case "region":
			return e.Region
		case "project":
			return e.Project
		}
		return e.Dim(k)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		choices := usage.PricingKeyChoices(e.Metric)
		if choices == nil {
			continue // informational
		}
		seen[e.Metric] = true
		for _, k := range choices {
			if value(e, k) == "" {
				t.Errorf("%s event (%s %s) has no %s, a key PRICING_KEYS may price it on", e.Metric, e.ResourceID, e.Dim(usage.DimBillingMode), k)
			}
		}
	}
	for _, d := range usage.Billable() {
		if !seen[d.Code] && !d.Custom {
			t.Errorf("the scenario has no %s event", d.Code)
		}
	}
}
