package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// VClusterMetrics contains all billable metrics for a single vCluster namespace.
type VClusterMetrics struct {
	Namespace string
	Timestamp time.Time

	// Core resources (from metrics-server)
	CPUCores    float64 // current CPU usage in cores
	MemoryBytes float64 // current memory usage in bytes

	// Storage (from PVCs)
	StorageBytes float64 // total requested PVC storage in bytes

	// GPU allocation (from pod resource requests)
	GPUs []GPUAllocation

	// GPU utilization (from DCGM via Prometheus, optional)
	GPUUtilization []GPUUtilizationMetric

	// Network (from Prometheus or kubelet, optional)
	NetworkTxBytes float64 // egress bytes since last collection
	NetworkRxBytes float64 // ingress bytes since last collection

	// LoadBalancer services
	LoadBalancerCount int

	// Node cost attribution
	NodeCostMultiplier float64 // weighted average: 1.0 = on-demand, <1.0 = has spot nodes

	// Private/dedicated nodes allocated to this vCluster
	PrivateNodes []PrivateNode
}

// GPUAllocation tracks GPU resources requested by pods.
type GPUAllocation struct {
	GPUType  string // e.g. "NVIDIA-A100-SXM4-80GB" from node label
	Count    int64  // number of GPUs allocated
	NodeName string
}

// GPUUtilizationMetric tracks actual GPU utilization from DCGM exporter.
type GPUUtilizationMetric struct {
	GPUType       string
	GPUUUID       string
	Utilization   float64 // 0-100 percent
	MemoryUsedMB  float64
	MemoryTotalMB float64
	PodName       string
	PodNamespace  string
}

// PrivateNode represents a dedicated node allocated to a vCluster tenant.
// In private node mode, the entire node is billed to the tenant.
type PrivateNode struct {
	NodeName     string
	CPUCores     int64  // total CPU capacity
	MemoryBytes  int64  // total memory capacity
	GPUCount     int64  // total GPUs on this node
	GPUType      string // GPU model from labels
	StorageBytes int64  // total ephemeral storage capacity
	IsSpot       bool   // spot/preemptible node
	InstanceType string // cloud instance type (e.g. p4d.24xlarge)
}

// Collector gathers resource metrics from the Kubernetes cluster.
type Collector struct {
	kubeClient    kubernetes.Interface
	metricsClient metricsclient.Interface
	promClient    *prometheusClient // nil if Prometheus not configured
	spotDiscount  float64           // percentage discount for spot nodes
}

func NewCollector(kubeClient kubernetes.Interface, metricsClient metricsclient.Interface, prometheusURL string, spotDiscount float64) *Collector {
	var prom *prometheusClient
	if prometheusURL != "" {
		prom = newPrometheusClient(prometheusURL)
		log.Printf("[metrics] Prometheus integration enabled: %s", prometheusURL)
	} else {
		log.Printf("[metrics] Prometheus not configured - DCGM and network metrics unavailable")
	}

	return &Collector{
		kubeClient:    kubeClient,
		metricsClient: metricsClient,
		promClient:    prom,
		spotDiscount:  spotDiscount,
	}
}

// Collect gathers all billable metrics for a vCluster namespace.
func (c *Collector) Collect(ctx context.Context, namespace string) (*VClusterMetrics, error) {
	m := &VClusterMetrics{
		Namespace:          namespace,
		Timestamp:          time.Now(),
		NodeCostMultiplier: 1.0,
	}

	// Collect all metrics concurrently using goroutines would be nice,
	// but for clarity and debuggability we do them sequentially.
	// Each method logs its own errors and returns partial results.

	c.collectCPUMemory(ctx, namespace, m)
	c.collectStorage(ctx, namespace, m)
	c.collectGPUAllocation(ctx, namespace, m)
	c.collectLoadBalancers(ctx, namespace, m)
	c.collectNodeCostAttribution(ctx, namespace, m)

	// Optional Prometheus-based metrics
	if c.promClient != nil {
		c.collectDCGMMetrics(ctx, namespace, m)
		c.collectNetworkMetrics(ctx, namespace, m)
	}

	// Collect private/dedicated nodes for this vCluster
	c.collectPrivateNodes(ctx, namespace, m)

	// If private nodes were found, also fetch their actual CPU/memory usage
	// from metrics-server so both shared-namespace pods AND private node
	// usage are captured in the billing totals.
	if m.HasPrivateNodes() {
		c.collectPrivateNodeUsage(ctx, namespace, m)
	}

	return m, nil
}

// TotalGPUCount returns the total number of GPUs allocated across all types.
func (m *VClusterMetrics) TotalGPUCount() int64 {
	var total int64
	for _, g := range m.GPUs {
		total += g.Count
	}
	return total
}

// GPUCountByType returns GPU counts grouped by GPU model.
func (m *VClusterMetrics) GPUCountByType() map[string]int64 {
	result := make(map[string]int64)
	for _, g := range m.GPUs {
		result[g.GPUType] += g.Count
	}
	return result
}

// MemoryGB returns memory usage in GB.
func (m *VClusterMetrics) MemoryGB() float64 {
	return m.MemoryBytes / (1024 * 1024 * 1024)
}

// StorageGB returns storage in GB.
func (m *VClusterMetrics) StorageGB() float64 {
	return m.StorageBytes / (1024 * 1024 * 1024)
}

// NetworkEgressGB returns egress traffic in GB.
func (m *VClusterMetrics) NetworkEgressGB() float64 {
	return m.NetworkTxBytes / (1024 * 1024 * 1024)
}

// HasPrivateNodes returns true if this vCluster has dedicated nodes allocated.
func (m *VClusterMetrics) HasPrivateNodes() bool {
	return len(m.PrivateNodes) > 0
}

// PrivateNodeCount returns the number of private/dedicated nodes.
func (m *VClusterMetrics) PrivateNodeCount() int {
	return len(m.PrivateNodes)
}

// PrivateNodeGPUsByType returns GPU counts on private nodes grouped by GPU model.
func (m *VClusterMetrics) PrivateNodeGPUsByType() map[string]int64 {
	result := make(map[string]int64)
	for _, n := range m.PrivateNodes {
		if n.GPUCount > 0 && n.GPUType != "" {
			result[n.GPUType] += n.GPUCount
		}
	}
	return result
}

// PrivateNodeTotalCPU returns the total CPU cores across all private nodes.
func (m *VClusterMetrics) PrivateNodeTotalCPU() int64 {
	var total int64
	for _, n := range m.PrivateNodes {
		total += n.CPUCores
	}
	return total
}

// PrivateNodeTotalMemoryGB returns total memory in GB across all private nodes.
func (m *VClusterMetrics) PrivateNodeTotalMemoryGB() float64 {
	var total int64
	for _, n := range m.PrivateNodes {
		total += n.MemoryBytes
	}
	return float64(total) / (1024 * 1024 * 1024)
}

// --- CPU and Memory from metrics-server ---

func (c *Collector) collectCPUMemory(ctx context.Context, namespace string, m *VClusterMetrics) {
	podMetrics, err := c.metricsClient.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[metrics] warning: cannot get pod metrics for %s: %v", namespace, err)
		return
	}

	for _, pod := range podMetrics.Items {
		for _, container := range pod.Containers {
			cpu := container.Usage.Cpu()
			mem := container.Usage.Memory()
			if cpu != nil {
				m.CPUCores += float64(cpu.MilliValue()) / 1000.0
			}
			if mem != nil {
				m.MemoryBytes += float64(mem.Value())
			}
		}
	}
	log.Printf("[metrics] %s: CPU=%.3f cores, Memory=%.2f GB", namespace, m.CPUCores, m.MemoryGB())
}

// --- Storage from PVCs ---

func (c *Collector) collectStorage(ctx context.Context, namespace string, m *VClusterMetrics) {
	pvcs, err := c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[metrics] warning: cannot list PVCs in %s: %v", namespace, err)
		return
	}

	for _, pvc := range pvcs.Items {
		if pvc.Status.Phase != corev1.ClaimBound {
			continue
		}
		storage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		m.StorageBytes += float64(storage.Value())
	}
	log.Printf("[metrics] %s: Storage=%.2f GB (%d PVCs)", namespace, m.StorageGB(), len(pvcs.Items))
}

// --- GPU allocation from pod resource requests ---

func (c *Collector) collectGPUAllocation(ctx context.Context, namespace string, m *VClusterMetrics) {
	pods, err := c.kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		log.Printf("[metrics] warning: cannot list pods in %s: %v", namespace, err)
		return
	}

	// Cache node GPU types
	nodeGPUType := make(map[string]string)

	for _, pod := range pods.Items {
		gpuCount := podGPUCount(&pod)
		if gpuCount == 0 {
			continue
		}

		// Determine GPU type from the node this pod runs on
		nodeName := pod.Spec.NodeName
		gpuType, ok := nodeGPUType[nodeName]
		if !ok {
			gpuType = c.getNodeGPUType(ctx, nodeName)
			nodeGPUType[nodeName] = gpuType
		}

		m.GPUs = append(m.GPUs, GPUAllocation{
			GPUType:  gpuType,
			Count:    gpuCount,
			NodeName: nodeName,
		})
	}

	if total := m.TotalGPUCount(); total > 0 {
		log.Printf("[metrics] %s: GPUs=%d allocated (%v)", namespace, total, m.GPUCountByType())
	}
}

// podGPUCount returns the total nvidia.com/gpu requests across all containers.
func podGPUCount(pod *corev1.Pod) int64 {
	var total int64
	gpuResource := corev1.ResourceName("nvidia.com/gpu")

	for _, c := range pod.Spec.Containers {
		if qty, ok := c.Resources.Requests[gpuResource]; ok {
			total += qty.Value()
		}
		if qty, ok := c.Resources.Limits[gpuResource]; ok && total == 0 {
			total += qty.Value()
		}
	}
	return total
}

// getNodeGPUType reads the GPU model from node labels.
func (c *Collector) getNodeGPUType(ctx context.Context, nodeName string) string {
	if nodeName == "" {
		return "unknown"
	}

	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		log.Printf("[metrics] warning: cannot get node %s: %v", nodeName, err)
		return "unknown"
	}

	// Check common GPU label conventions
	gpuLabels := []string{
		"nvidia.com/gpu.product",           // NVIDIA GPU Operator
		"nvidia.com/gpu.machine",           // alternative
		"accelerator",                      // GKE
		"cloud.google.com/gke-accelerator", // GKE specific
		"k8s.amazonaws.com/accelerator",    // EKS
		"node.kubernetes.io/instance-type", // fallback: instance type
	}

	for _, label := range gpuLabels {
		if v, ok := node.Labels[label]; ok && v != "" {
			return sanitizeGPUType(v)
		}
	}

	return "unknown"
}

// sanitizeGPUType normalizes GPU type strings for consistent billing.
func sanitizeGPUType(raw string) string {
	// Remove spaces and convert to uppercase for consistency
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// --- LoadBalancer services ---

func (c *Collector) collectLoadBalancers(ctx context.Context, namespace string, m *VClusterMetrics) {
	services, err := c.kubeClient.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("[metrics] warning: cannot list services in %s: %v", namespace, err)
		return
	}

	for _, svc := range services.Items {
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			m.LoadBalancerCount++
		}
	}

	if m.LoadBalancerCount > 0 {
		log.Printf("[metrics] %s: LoadBalancers=%d", namespace, m.LoadBalancerCount)
	}
}

// --- Node cost attribution (spot vs on-demand) ---

func (c *Collector) collectNodeCostAttribution(ctx context.Context, namespace string, m *VClusterMetrics) {
	pods, err := c.kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return
	}

	if len(pods.Items) == 0 {
		return
	}

	// Cache node spot status
	nodeSpot := make(map[string]bool)
	var spotPods, totalPods int

	for _, pod := range pods.Items {
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			continue
		}

		isSpot, ok := nodeSpot[nodeName]
		if !ok {
			isSpot = c.isSpotNode(ctx, nodeName)
			nodeSpot[nodeName] = isSpot
		}

		totalPods++
		if isSpot {
			spotPods++
		}
	}

	if totalPods > 0 {
		spotFraction := float64(spotPods) / float64(totalPods)
		// Cost multiplier: spot pods get discounted, on-demand pods stay at 1.0
		discount := (c.spotDiscount / 100.0) * spotFraction
		m.NodeCostMultiplier = 1.0 - discount
		if spotPods > 0 {
			log.Printf("[metrics] %s: %d/%d pods on spot nodes, cost multiplier=%.2f",
				namespace, spotPods, totalPods, m.NodeCostMultiplier)
		}
	}
}

// isSpotNode checks if a node is a spot/preemptible instance.
func (c *Collector) isSpotNode(ctx context.Context, nodeName string) bool {
	node, err := c.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return false
	}

	spotLabels := []string{
		"kubernetes.io/lifecycle",           // common: "spot" or "preemptible"
		"node.kubernetes.io/lifecycle",      // alternative
		"cloud.google.com/gke-preemptible", // GKE preemptible
		"cloud.google.com/gke-spot",        // GKE spot
		"eks.amazonaws.com/capacityType",   // EKS: "SPOT" or "ON_DEMAND"
		"karpenter.sh/capacity-type",       // Karpenter: "spot" or "on-demand"
		"node.kubernetes.io/instance-type", // check below
	}

	spotValues := map[string]bool{
		"spot":        true,
		"preemptible": true,
		"SPOT":        true,
		"true":        true,
	}

	for _, label := range spotLabels {
		if v, ok := node.Labels[label]; ok {
			if spotValues[v] {
				return true
			}
		}
	}

	return false
}

// isSpotNodeFromObj checks if a node object is a spot/preemptible instance
// without fetching the node from the API (used when we already have the object).
func isSpotNodeFromObj(node *corev1.Node) bool {
	spotLabels := []string{
		"kubernetes.io/lifecycle",
		"node.kubernetes.io/lifecycle",
		"cloud.google.com/gke-preemptible",
		"cloud.google.com/gke-spot",
		"eks.amazonaws.com/capacityType",
		"karpenter.sh/capacity-type",
	}

	spotValues := map[string]bool{
		"spot":        true,
		"preemptible": true,
		"SPOT":        true,
		"true":        true,
	}

	for _, label := range spotLabels {
		if v, ok := node.Labels[label]; ok {
			if spotValues[v] {
				return true
			}
		}
	}

	return false
}

// --- Private/dedicated node collection ---

// collectPrivateNodes finds nodes dedicated to this vCluster and records their
// full capacity for billing. In private node mode, the entire node is billed
// to the tenant regardless of actual utilization.
func (c *Collector) collectPrivateNodes(ctx context.Context, namespace string, m *VClusterMetrics) {
	gpuResource := corev1.ResourceName("nvidia.com/gpu")
	ephemeralResource := corev1.ResourceName("ephemeral-storage")

	// Search for private nodes using multiple strategies.
	// The label value could be the vCluster name, the namespace, or a custom pattern.
	// We try all reasonable combinations.
	selectors := []string{
		fmt.Sprintf("vcluster.loft.sh/managed-by=%s", namespace),
		fmt.Sprintf("vcluster.loft.sh/cluster=%s", namespace),
	}

	// Also try with the vCluster name derived from the namespace
	// (strip common prefixes like "vcluster-", "vc-")
	vclusterName := namespace
	for _, prefix := range []string{"vcluster-", "vc-"} {
		if strings.HasPrefix(namespace, prefix) {
			vclusterName = strings.TrimPrefix(namespace, prefix)
			break
		}
	}
	if vclusterName != namespace {
		selectors = append(selectors,
			fmt.Sprintf("vcluster.loft.sh/managed-by=%s", vclusterName),
			fmt.Sprintf("vcluster.loft.sh/cluster=%s", vclusterName),
		)
	}

	// Strategy 3: custom label from environment variable.
	// Format: VCLUSTER_NODE_LABEL=<label-key>=<label-value-pattern>
	// The value pattern may contain %s which is replaced with the vCluster name.
	if customLabel := os.Getenv("VCLUSTER_NODE_LABEL"); customLabel != "" {
		parts := strings.SplitN(customLabel, "=", 2)
		if len(parts) == 2 {
			key := parts[0]
			valPattern := parts[1]
			val := strings.ReplaceAll(valPattern, "%s", vclusterName)
			selectors = append(selectors, fmt.Sprintf("%s=%s", key, val))
		}
	}

	seen := make(map[string]bool)

	for _, sel := range selectors {
		nodes, err := c.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: sel,
		})
		if err != nil {
			log.Printf("[metrics] warning: cannot list nodes with selector %q: %v", sel, err)
			continue
		}

		for _, node := range nodes.Items {
			if seen[node.Name] {
				continue
			}
			seen[node.Name] = true

			pn := PrivateNode{
				NodeName: node.Name,
			}

			// CPU capacity
			if cpu, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
				pn.CPUCores = cpu.Value()
			}

			// Memory capacity
			if mem, ok := node.Status.Capacity[corev1.ResourceMemory]; ok {
				pn.MemoryBytes = mem.Value()
			}

			// GPU count
			if gpu, ok := node.Status.Capacity[gpuResource]; ok {
				pn.GPUCount = gpu.Value()
			}

			// GPU type from label (check multiple label conventions)
			for _, gpuLabel := range []string{
				"nvidia.com/gpu.product",
				"nvidia.com/gpu.machine",
				"cloud.google.com/gke-accelerator",
				"k8s.amazonaws.com/accelerator",
			} {
				if gpuType, ok := node.Labels[gpuLabel]; ok && gpuType != "" {
					pn.GPUType = sanitizeGPUType(gpuType)
					break
				}
			}

			// Ephemeral storage capacity
			if stor, ok := node.Status.Capacity[ephemeralResource]; ok {
				pn.StorageBytes = stor.Value()
			}

			// Instance type from label
			if instType, ok := node.Labels["node.kubernetes.io/instance-type"]; ok {
				pn.InstanceType = instType
			}

			// Spot status
			pn.IsSpot = isSpotNodeFromObj(&node)

			m.PrivateNodes = append(m.PrivateNodes, pn)
		}
	}

	if len(m.PrivateNodes) > 0 {
		var totalCPU int64
		var totalGPU int64
		for _, pn := range m.PrivateNodes {
			totalCPU += pn.CPUCores
			totalGPU += pn.GPUCount
		}
		log.Printf("[metrics] %s: found %d private node(s): totalCPU=%d cores, totalMemory=%.2f GB, totalGPUs=%d",
			namespace, len(m.PrivateNodes), totalCPU, m.PrivateNodeTotalMemoryGB(), totalGPU)

		for _, pn := range m.PrivateNodes {
			spotStr := "on-demand"
			if pn.IsSpot {
				spotStr = "spot"
			}
			log.Printf("[metrics]   node=%s type=%s cpu=%d mem=%dMi gpus=%d(%s) %s",
				pn.NodeName, pn.InstanceType, pn.CPUCores,
				pn.MemoryBytes/(1024*1024), pn.GPUCount, pn.GPUType, spotStr)
		}
	}
}

// collectPrivateNodeUsage queries metrics-server for actual CPU/memory usage
// on private nodes and adds that usage to the billing totals. This ensures
// that workloads running directly on dedicated nodes (outside the vCluster
// namespace) are also captured.
func (c *Collector) collectPrivateNodeUsage(ctx context.Context, namespace string, m *VClusterMetrics) {
	var addedCPU float64
	var addedMem float64

	for _, pn := range m.PrivateNodes {
		nodeMetrics, err := c.metricsClient.MetricsV1beta1().NodeMetricses().Get(ctx, pn.NodeName, metav1.GetOptions{})
		if err != nil {
			log.Printf("[metrics] warning: cannot get node metrics for private node %s: %v", pn.NodeName, err)
			continue
		}

		cpu := nodeMetrics.Usage.Cpu()
		mem := nodeMetrics.Usage.Memory()

		if cpu != nil {
			cpuVal := float64(cpu.MilliValue()) / 1000.0
			m.CPUCores += cpuVal
			addedCPU += cpuVal
		}
		if mem != nil {
			memVal := float64(mem.Value())
			m.MemoryBytes += memVal
			addedMem += memVal
		}
	}

	if addedCPU > 0 || addedMem > 0 {
		log.Printf("[metrics] %s: added private node usage: CPU=+%.3f cores, Memory=+%.2f GB",
			namespace, addedCPU, addedMem/(1024*1024*1024))
	}
}

// --- DCGM GPU utilization from Prometheus ---

func (c *Collector) collectDCGMMetrics(ctx context.Context, namespace string, m *VClusterMetrics) {
	if c.promClient == nil {
		return
	}

	// Query DCGM GPU utilization for pods in this namespace
	// DCGM_FI_DEV_GPU_UTIL{namespace="<ns>"} gives GPU utilization 0-100
	query := fmt.Sprintf(`DCGM_FI_DEV_GPU_UTIL{namespace="%s"}`, namespace)
	results, err := c.promClient.Query(ctx, query)
	if err != nil {
		log.Printf("[metrics] warning: DCGM query failed for %s: %v", namespace, err)
		return
	}

	for _, r := range results {
		util, _ := strconv.ParseFloat(r.Value, 64)

		// Also get memory usage for this GPU
		var memUsed, memTotal float64
		memQuery := fmt.Sprintf(`DCGM_FI_DEV_FB_USED{namespace="%s",gpu="%s"}`, namespace, r.Labels["gpu"])
		memResults, err := c.promClient.Query(ctx, memQuery)
		if err == nil && len(memResults) > 0 {
			memUsed, _ = strconv.ParseFloat(memResults[0].Value, 64)
		}
		memTotalQuery := fmt.Sprintf(`DCGM_FI_DEV_FB_FREE{namespace="%s",gpu="%s"} + DCGM_FI_DEV_FB_USED{namespace="%s",gpu="%s"}`,
			namespace, r.Labels["gpu"], namespace, r.Labels["gpu"])
		memTotalResults, err := c.promClient.Query(ctx, memTotalQuery)
		if err == nil && len(memTotalResults) > 0 {
			memTotal, _ = strconv.ParseFloat(memTotalResults[0].Value, 64)
		}

		m.GPUUtilization = append(m.GPUUtilization, GPUUtilizationMetric{
			GPUType:       r.Labels["modelName"],
			GPUUUID:       r.Labels["UUID"],
			Utilization:   util,
			MemoryUsedMB:  memUsed,
			MemoryTotalMB: memTotal,
			PodName:       r.Labels["pod"],
			PodNamespace:  r.Labels["namespace"],
		})
	}

	if len(m.GPUUtilization) > 0 {
		avgUtil := 0.0
		for _, g := range m.GPUUtilization {
			avgUtil += g.Utilization
		}
		avgUtil /= float64(len(m.GPUUtilization))
		log.Printf("[metrics] %s: DCGM GPU utilization: %d GPUs, avg=%.1f%%",
			namespace, len(m.GPUUtilization), avgUtil)
	}
}

// --- Network egress from Prometheus ---

func (c *Collector) collectNetworkMetrics(ctx context.Context, namespace string, m *VClusterMetrics) {
	if c.promClient == nil {
		return
	}

	// Query total network transmit bytes (egress) over the last collection interval
	// Using rate over 5m to get bytes/sec, then we'll multiply by our interval
	txQuery := fmt.Sprintf(
		`sum(rate(container_network_transmit_bytes_total{namespace="%s"}[5m])) * 300`,
		namespace,
	)
	txResults, err := c.promClient.Query(ctx, txQuery)
	if err != nil {
		log.Printf("[metrics] warning: network tx query failed for %s: %v", namespace, err)
		return
	}
	if len(txResults) > 0 {
		m.NetworkTxBytes, _ = strconv.ParseFloat(txResults[0].Value, 64)
	}

	// Also get rx for completeness (not billed by default but tracked)
	rxQuery := fmt.Sprintf(
		`sum(rate(container_network_receive_bytes_total{namespace="%s"}[5m])) * 300`,
		namespace,
	)
	rxResults, err := c.promClient.Query(ctx, rxQuery)
	if err == nil && len(rxResults) > 0 {
		m.NetworkRxBytes, _ = strconv.ParseFloat(rxResults[0].Value, 64)
	}

	if m.NetworkTxBytes > 0 {
		log.Printf("[metrics] %s: Network egress=%.2f MB",
			namespace, m.NetworkTxBytes/(1024*1024))
	}
}

// --- Prometheus client for DCGM and network metrics ---

type prometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

func newPrometheusClient(baseURL string) *prometheusClient {
	return &prometheusClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type promQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type promResult struct {
	Labels map[string]string
	Value  string
}

func (p *prometheusClient) Query(ctx context.Context, query string) ([]promResult, error) {
	url := fmt.Sprintf("%s/api/v1/query?query=%s", p.baseURL, query)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var promResp promQueryResponse
	if err := json.Unmarshal(body, &promResp); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}

	if promResp.Status != "success" {
		return nil, fmt.Errorf("prometheus query failed: %s", string(body))
	}

	var results []promResult
	for _, r := range promResp.Data.Result {
		val := ""
		if len(r.Value) >= 2 {
			val = fmt.Sprintf("%v", r.Value[1])
		}
		results = append(results, promResult{
			Labels: r.Metric,
			Value:  val,
		})
	}

	return results, nil
}

// --- Helpers ---

// round rounds a float to n decimal places.
func round(val float64, precision int) float64 {
	ratio := math.Pow(10, float64(precision))
	return math.Round(val*ratio) / ratio
}

// parseQuantity safely parses a Kubernetes resource quantity.
func parseQuantity(s string) float64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return float64(q.Value())
}

// Ensure metricsv1beta1 is used (compile check)
var _ *metricsv1beta1.PodMetrics
