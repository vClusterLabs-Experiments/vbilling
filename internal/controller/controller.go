package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
)

// Controller is the main billing reconciliation loop.
// It discovers Tenant Clusters, ensures they exist in the configured billing
// destination, collects resource metrics, and ships usage events.
type Controller struct {
	cfg        *config.Config
	dest       destinations.Destination
	discoverer *discovery.Discoverer
	collector  *metrics.Collector

	mu    sync.Mutex
	known map[string]*tracked // key = ExternalID
}

type tracked struct {
	VCluster       discovery.VCluster
	Subscribed     bool
	LastCollection time.Time
}

func New(cfg *config.Config, dest destinations.Destination, disc *discovery.Discoverer, coll *metrics.Collector) *Controller {
	return &Controller{
		cfg:        cfg,
		dest:       dest,
		discoverer: disc,
		collector:  coll,
		known:      make(map[string]*tracked),
	}
}

// Run starts the controller with two loops:
// 1. Reconcile loop: discovers Tenant Clusters and manages billing entities
// 2. Collection loop: scrapes metrics and ships events to the destination
func (c *Controller) Run(ctx context.Context) error {
	log.Printf("[controller] starting (adapter=%s, reconcile=%s, collection=%s)",
		c.dest.Name(), c.cfg.ReconcileInterval, c.cfg.CollectionInterval)

	c.reconcile(ctx)

	reconcileTicker := time.NewTicker(c.cfg.ReconcileInterval)
	collectionTicker := time.NewTicker(c.cfg.CollectionInterval)
	defer reconcileTicker.Stop()
	defer collectionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[controller] shutting down")
			return ctx.Err()
		case <-reconcileTicker.C:
			c.reconcile(ctx)
		case <-collectionTicker.C:
			c.collectAndSend(ctx)
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) {
	vclusters, err := c.discoverer.Discover(ctx)
	if err != nil {
		log.Printf("[controller] discovery error: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	alive := make(map[string]bool)

	for _, vc := range vclusters {
		extID := vc.ExternalID()
		alive[extID] = true

		if _, exists := c.known[extID]; exists {
			continue
		}

		log.Printf("[controller] new Tenant Cluster discovered: %s/%s", vc.Namespace, vc.Name)

		if err := c.dest.EnsureTenant(ctx, tenantFor(vc, c.cfg.BillingCurrency)); err != nil {
			log.Printf("[controller] ensure tenant %s: %v", extID, err)
			continue
		}

		c.known[extID] = &tracked{VCluster: vc, Subscribed: true}
	}

	for extID, t := range c.known {
		if alive[extID] {
			continue
		}
		log.Printf("[controller] Tenant Cluster removed: %s/%s", t.VCluster.Namespace, t.VCluster.Name)
		if t.Subscribed {
			if err := c.dest.RemoveTenant(ctx, extID); err != nil {
				log.Printf("[controller] remove tenant %s: %v", extID, err)
			}
		}
		delete(c.known, extID)
	}
}

func tenantFor(vc discovery.VCluster, currency string) destinations.Tenant {
	return destinations.Tenant{
		ExternalID:  vc.ExternalID(),
		DisplayName: vc.DisplayName(),
		Currency:    currency,
		Metadata: map[string]string{
			"vcluster_name":      vc.Name,
			"vcluster_namespace": vc.Namespace,
			"vcluster_uid":       vc.UID,
			"created_at":         vc.CreatedAt.Format(time.RFC3339),
		},
		CreatedAt: vc.CreatedAt,
	}
}

func (c *Controller) collectAndSend(ctx context.Context) {
	c.mu.Lock()
	all := make([]*tracked, 0, len(c.known))
	for _, t := range c.known {
		all = append(all, t)
	}
	c.mu.Unlock()

	if len(all) == 0 {
		return
	}

	intervalHours := c.cfg.CollectionInterval.Hours()
	now := time.Now()
	var events []destinations.UsageEvent

	for _, t := range all {
		m, err := c.collector.Collect(ctx, t.VCluster.Namespace)
		if err != nil {
			log.Printf("[controller] metrics collection failed for %s: %v", t.VCluster.Namespace, err)
			continue
		}
		events = append(events, eventsForTenant(t.VCluster, m, intervalHours, now)...)

		c.mu.Lock()
		if existing, ok := c.known[t.VCluster.ExternalID()]; ok {
			existing.LastCollection = now
		}
		c.mu.Unlock()
	}

	if len(events) == 0 {
		return
	}
	if err := c.dest.SendEvents(ctx, events); err != nil {
		log.Printf("[controller] send events: %v", err)
	} else {
		log.Printf("[controller] sent %d billing events to %s", len(events), c.dest.Name())
	}
}

// eventsForTenant builds the canonical usage events for one Tenant Cluster.
func eventsForTenant(vc discovery.VCluster, m *metrics.VClusterMetrics, intervalHours float64, now time.Time) []destinations.UsageEvent {
	extID := vc.ExternalID()
	uid := vc.UID
	ts := now.Unix()
	costMul := m.NodeCostMultiplier

	commonProps := func(extra map[string]any) map[string]any {
		props := map[string]any{
			"vcluster_name":      vc.Name,
			"vcluster_namespace": vc.Namespace,
		}
		for k, v := range extra {
			props[k] = v
		}
		return props
	}

	var events []destinations.UsageEvent
	add := func(metric, suffix string, value float64, props map[string]any) {
		events = append(events, destinations.UsageEvent{
			TenantExternalID: extID,
			MetricCode:       metric,
			Value:            roundFloat(value),
			Unit:             unitFor(metric),
			Timestamp:        now,
			TransactionID:    fmt.Sprintf("%s-%s-%d", uid, suffix, ts),
			Properties:       props,
		})
	}

	if m.CPUCores > 0 {
		add(destinations.MetricCPUCoreHours, "cpu", m.CPUCores*intervalHours*costMul, commonProps(map[string]any{
			"raw_cpu_cores":   roundFloat(m.CPUCores),
			"cost_multiplier": roundFloat(costMul),
		}))
	}
	if m.MemoryGB() > 0 {
		add(destinations.MetricMemoryGBHours, "mem", m.MemoryGB()*intervalHours*costMul, commonProps(map[string]any{
			"raw_memory_gb": roundFloat(m.MemoryGB()),
		}))
	}
	if m.StorageGB() > 0 {
		add(destinations.MetricStorageGBHours, "stor", m.StorageGB()*intervalHours, commonProps(map[string]any{
			"raw_storage_gb": roundFloat(m.StorageGB()),
		}))
	}

	add(destinations.MetricInstanceHours, "inst", intervalHours, commonProps(nil))

	for gpuType, count := range m.GPUCountByType() {
		add(destinations.MetricGPUHours, "gpu-"+sanitize(gpuType), float64(count)*intervalHours, commonProps(map[string]any{
			"gpu_count": count,
			"gpu_type":  gpuType,
		}))
	}

	if len(m.GPUUtilization) > 0 {
		var sum float64
		for _, g := range m.GPUUtilization {
			sum += g.Utilization
		}
		avg := sum / float64(len(m.GPUUtilization))
		add(destinations.MetricGPUUtilization, "gpuutil", avg*intervalHours, commonProps(map[string]any{
			"avg_utilization_pct": roundFloat(avg),
			"gpu_count":           len(m.GPUUtilization),
		}))
	}

	if m.NetworkEgressGB() > 0 {
		add(destinations.MetricNetworkEgressGB, "net", m.NetworkEgressGB(), commonProps(nil))
	}

	if m.LoadBalancerCount > 0 {
		add(destinations.MetricLBHours, "lb", float64(m.LoadBalancerCount)*intervalHours, commonProps(map[string]any{
			"lb_count": m.LoadBalancerCount,
		}))
	}

	if m.HasPrivateNodes() {
		for gpuType, count := range m.PrivateNodeGPUsByType() {
			add(destinations.MetricGPUHours, "pn-gpu-"+sanitize(gpuType), float64(count)*intervalHours, commonProps(map[string]any{
				"gpu_count":    count,
				"gpu_type":     gpuType,
				"billing_mode": "private_node",
			}))
		}
		if m.PrivateNodeTotalCPU() > 0 {
			add(destinations.MetricCPUCoreHours, "pn-cpu", float64(m.PrivateNodeTotalCPU())*intervalHours, commonProps(map[string]any{
				"raw_cpu_cores": roundFloat(float64(m.PrivateNodeTotalCPU())),
				"billing_mode":  "private_node",
			}))
		}
		if m.PrivateNodeTotalMemoryGB() > 0 {
			add(destinations.MetricMemoryGBHours, "pn-mem", m.PrivateNodeTotalMemoryGB()*intervalHours, commonProps(map[string]any{
				"raw_memory_gb": roundFloat(m.PrivateNodeTotalMemoryGB()),
				"billing_mode":  "private_node",
			}))
		}
		add(destinations.MetricPrivateNodeHours, "pn-hours", float64(m.PrivateNodeCount())*intervalHours, commonProps(map[string]any{
			"node_count":   m.PrivateNodeCount(),
			"billing_mode": "private_node",
		}))
	}

	return events
}

func unitFor(metric string) string {
	switch metric {
	case destinations.MetricCPUCoreHours:
		return "core-hours"
	case destinations.MetricMemoryGBHours, destinations.MetricStorageGBHours:
		return "gb-hours"
	case destinations.MetricInstanceHours, destinations.MetricGPUHours,
		destinations.MetricLBHours, destinations.MetricPrivateNodeHours:
		return "hours"
	case destinations.MetricGPUUtilization:
		return "util-hours"
	case destinations.MetricNetworkEgressGB:
		return "gb"
	default:
		return ""
	}
}

func roundFloat(f float64) float64 {
	return float64(int64(f*1000000)) / 1000000
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	return string(out)
}
