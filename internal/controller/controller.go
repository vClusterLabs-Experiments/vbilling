package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/loft-sh/vbilling/internal/config"
	"github.com/loft-sh/vbilling/internal/discovery"
	"github.com/loft-sh/vbilling/internal/lago"
	"github.com/loft-sh/vbilling/internal/metrics"
)

// Controller is the main billing reconciliation loop.
// It discovers vClusters, ensures they have billing entities in Lago,
// collects resource metrics, and sends usage events.
type Controller struct {
	cfg        *config.Config
	lago       *lago.Client
	discoverer *discovery.Discoverer
	collector  *metrics.Collector

	// Track known vClusters and their subscription state
	mu       sync.Mutex
	known    map[string]*trackedVCluster // key = ExternalID
}

type trackedVCluster struct {
	VCluster       discovery.VCluster
	Subscribed     bool
	LastCollection time.Time
}

func New(cfg *config.Config, lagoClient *lago.Client, disc *discovery.Discoverer, coll *metrics.Collector) *Controller {
	return &Controller{
		cfg:        cfg,
		lago:       lagoClient,
		discoverer: disc,
		collector:  coll,
		known:      make(map[string]*trackedVCluster),
	}
}

// Run starts the controller with two loops:
// 1. Reconcile loop: discovers vClusters and manages billing entities
// 2. Collection loop: scrapes metrics and sends events to Lago
func (c *Controller) Run(ctx context.Context) error {
	log.Println("[controller] starting billing controller")
	log.Printf("[controller] reconcile interval: %s, collection interval: %s",
		c.cfg.ReconcileInterval, c.cfg.CollectionInterval)

	// Do an initial reconcile immediately
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

// reconcile discovers vClusters and ensures billing entities exist.
func (c *Controller) reconcile(ctx context.Context) {
	vclusters, err := c.discoverer.Discover(ctx)
	if err != nil {
		log.Printf("[controller] discovery error: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Track which vClusters are still alive
	alive := make(map[string]bool)

	for _, vc := range vclusters {
		extID := vc.ExternalID()
		alive[extID] = true

		if _, exists := c.known[extID]; exists {
			continue // already tracked
		}

		// New vCluster found — create billing entities
		log.Printf("[controller] new vCluster discovered: %s/%s", vc.Namespace, vc.Name)

		if err := c.ensureBillingEntities(vc); err != nil {
			log.Printf("[controller] error creating billing entities for %s: %v", extID, err)
			continue
		}

		c.known[extID] = &trackedVCluster{
			VCluster:   vc,
			Subscribed: true,
		}
	}

	// Handle removed vClusters
	for extID, tracked := range c.known {
		if alive[extID] {
			continue
		}

		log.Printf("[controller] vCluster removed: %s/%s", tracked.VCluster.Namespace, tracked.VCluster.Name)

		if tracked.Subscribed {
			subID := tracked.VCluster.SubscriptionID()
			if err := c.lago.TerminateSubscription(subID); err != nil {
				log.Printf("[controller] error terminating subscription %s: %v", subID, err)
			} else {
				log.Printf("[controller] terminated subscription %s", subID)
			}
		}

		delete(c.known, extID)
	}
}

// ensureBillingEntities creates or updates the Lago customer and subscription.
func (c *Controller) ensureBillingEntities(vc discovery.VCluster) error {
	// Upsert customer — try with metadata first, fall back without if it already exists
	customer := lago.Customer{
		ExternalID: vc.ExternalID(),
		Name:       vc.DisplayName(),
		Currency:   c.cfg.BillingCurrency,
		Metadata: []lago.CustomerMeta{
			{Key: "vcluster_name", Value: vc.Name},
			{Key: "vcluster_namespace", Value: vc.Namespace},
			{Key: "vcluster_uid", Value: vc.UID},
			{Key: "created_at", Value: vc.CreatedAt.Format(time.RFC3339)},
		},
	}

	_, err := c.lago.UpsertCustomer(customer)
	if err != nil {
		// Metadata keys may already exist on the customer — retry without metadata
		customer.Metadata = nil
		_, err = c.lago.UpsertCustomer(customer)
		if err != nil {
			return fmt.Errorf("upsert customer: %w", err)
		}
	}
	log.Printf("[controller] ensured customer %s", vc.ExternalID())

	// Check if subscription already exists by probing current usage
	subID := vc.SubscriptionID()
	_, usageErr := c.lago.GetCurrentUsage(vc.ExternalID(), subID)
	if usageErr == nil {
		// Usage returned successfully — subscription already exists
		log.Printf("[controller] subscription %s already exists, reusing", subID)
		return nil
	}

	// Subscription does not exist — create it
	sub := lago.Subscription{
		ExternalCustomerID: vc.ExternalID(),
		PlanCode:           c.cfg.DefaultPlanCode,
		ExternalID:         subID,
	}

	_, err = c.lago.CreateSubscription(sub)
	if err != nil {
		log.Printf("[controller] warning: could not create subscription %s: %v", subID, err)
	} else {
		log.Printf("[controller] created subscription %s -> plan %s", subID, c.cfg.DefaultPlanCode)
	}

	return nil
}

// collectAndSend scrapes metrics for all known vClusters and sends events to Lago.
func (c *Controller) collectAndSend(ctx context.Context) {
	c.mu.Lock()
	tracked := make([]*trackedVCluster, 0, len(c.known))
	for _, t := range c.known {
		tracked = append(tracked, t)
	}
	c.mu.Unlock()

	if len(tracked) == 0 {
		return
	}

	// Calculate the time window for this collection (in hours)
	intervalHours := c.cfg.CollectionInterval.Hours()

	var allEvents []lago.Event
	now := time.Now()

	for _, t := range tracked {
		m, err := c.collector.Collect(ctx, t.VCluster.Namespace)
		if err != nil {
			log.Printf("[controller] metrics collection failed for %s: %v", t.VCluster.Namespace, err)
			continue
		}

		subID := t.VCluster.SubscriptionID()
		ts := now.Unix()
		uid := t.VCluster.UID

		// Apply node cost multiplier (spot discount)
		costMultiplier := m.NodeCostMultiplier

		// CPU core-hours: current_cores * interval_hours * cost_multiplier
		if m.CPUCores > 0 {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-cpu-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricCPUCoreHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"cpu_core_hours":      roundFloat(m.CPUCores * intervalHours * costMultiplier),
					"raw_cpu_cores":       roundFloat(m.CPUCores),
					"cost_multiplier":     roundFloat(costMultiplier),
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Memory GB-hours
		if m.MemoryGB() > 0 {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-mem-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricMemoryGBHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"memory_gb_hours":     roundFloat(m.MemoryGB() * intervalHours * costMultiplier),
					"raw_memory_gb":       roundFloat(m.MemoryGB()),
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Storage GB-hours (storage isn't affected by spot/on-demand)
		if m.StorageGB() > 0 {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-stor-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricStorageGBHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"storage_gb_hours":    roundFloat(m.StorageGB() * intervalHours),
					"raw_storage_gb":      roundFloat(m.StorageGB()),
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Instance hours (flat per-vCluster charge)
		allEvents = append(allEvents, lago.Event{
			TransactionID:          fmt.Sprintf("%s-inst-%d", uid, ts),
			ExternalSubscriptionID: subID,
			Code:                   lago.MetricInstanceHours,
			Timestamp:              ts,
			Properties: map[string]interface{}{
				"instance_hours":      roundFloat(intervalHours),
				"vcluster_name":       t.VCluster.Name,
				"vcluster_namespace":  t.VCluster.Namespace,
			},
		})

		// GPU hours (per GPU type for accurate pricing)
		for gpuType, count := range m.GPUCountByType() {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-gpu-%s-%d", uid, sanitize(gpuType), ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricGPUHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"gpu_hours":           roundFloat(float64(count) * intervalHours),
					"gpu_count":           count,
					"gpu_type":            gpuType,
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// GPU utilization (from DCGM, informational + can be used for billing adjustments)
		if len(m.GPUUtilization) > 0 {
			avgUtil := 0.0
			for _, g := range m.GPUUtilization {
				avgUtil += g.Utilization
			}
			avgUtil /= float64(len(m.GPUUtilization))

			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-gpuutil-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricGPUUtilization,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"gpu_util_score":      roundFloat(avgUtil * intervalHours),
					"avg_utilization_pct": roundFloat(avgUtil),
					"gpu_count":           len(m.GPUUtilization),
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Network egress
		if m.NetworkEgressGB() > 0 {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-net-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricNetworkEgressGB,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"egress_gb":           roundFloat(m.NetworkEgressGB()),
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// LoadBalancer hours
		if m.LoadBalancerCount > 0 {
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-lb-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricLBHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"lb_hours":            roundFloat(float64(m.LoadBalancerCount) * intervalHours),
					"lb_count":            m.LoadBalancerCount,
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Private Node events — dedicated node billing for tenants using private nodes
		if m.HasPrivateNodes() {
			// GPU hours for private nodes (per GPU type)
			for gpuType, count := range m.PrivateNodeGPUsByType() {
				allEvents = append(allEvents, lago.Event{
					TransactionID:          fmt.Sprintf("%s-pn-gpu-%s-%d", uid, sanitize(gpuType), ts),
					ExternalSubscriptionID: subID,
					Code:                   lago.MetricGPUHours,
					Timestamp:              ts,
					Properties: map[string]interface{}{
						"gpu_hours":           roundFloat(float64(count) * intervalHours),
						"gpu_count":           count,
						"gpu_type":            gpuType,
						"billing_mode":        "private_node",
						"vcluster_name":       t.VCluster.Name,
						"vcluster_namespace":  t.VCluster.Namespace,
					},
				})
			}

			// CPU core-hours for private nodes
			if m.PrivateNodeTotalCPU() > 0 {
				allEvents = append(allEvents, lago.Event{
					TransactionID:          fmt.Sprintf("%s-pn-cpu-%d", uid, ts),
					ExternalSubscriptionID: subID,
					Code:                   lago.MetricCPUCoreHours,
					Timestamp:              ts,
					Properties: map[string]interface{}{
						"cpu_core_hours":      roundFloat(float64(m.PrivateNodeTotalCPU()) * intervalHours),
						"raw_cpu_cores":       roundFloat(float64(m.PrivateNodeTotalCPU())),
						"billing_mode":        "private_node",
						"vcluster_name":       t.VCluster.Name,
						"vcluster_namespace":  t.VCluster.Namespace,
					},
				})
			}

			// Memory GB-hours for private nodes
			if m.PrivateNodeTotalMemoryGB() > 0 {
				allEvents = append(allEvents, lago.Event{
					TransactionID:          fmt.Sprintf("%s-pn-mem-%d", uid, ts),
					ExternalSubscriptionID: subID,
					Code:                   lago.MetricMemoryGBHours,
					Timestamp:              ts,
					Properties: map[string]interface{}{
						"memory_gb_hours":     roundFloat(m.PrivateNodeTotalMemoryGB() * intervalHours),
						"raw_memory_gb":       roundFloat(m.PrivateNodeTotalMemoryGB()),
						"billing_mode":        "private_node",
						"vcluster_name":       t.VCluster.Name,
						"vcluster_namespace":  t.VCluster.Namespace,
					},
				})
			}

			// Private node hours — count of dedicated nodes * interval
			allEvents = append(allEvents, lago.Event{
				TransactionID:          fmt.Sprintf("%s-pn-hours-%d", uid, ts),
				ExternalSubscriptionID: subID,
				Code:                   lago.MetricPrivateNodeHours,
				Timestamp:              ts,
				Properties: map[string]interface{}{
					"private_node_hours":  roundFloat(float64(m.PrivateNodeCount()) * intervalHours),
					"node_count":          m.PrivateNodeCount(),
					"billing_mode":        "private_node",
					"vcluster_name":       t.VCluster.Name,
					"vcluster_namespace":  t.VCluster.Namespace,
				},
			})
		}

		// Update last collection time
		c.mu.Lock()
		if existing, ok := c.known[t.VCluster.ExternalID()]; ok {
			existing.LastCollection = now
		}
		c.mu.Unlock()
	}

	// Send all events in batch
	if len(allEvents) > 0 {
		// Lago batch API has a limit, send in chunks of 100
		for i := 0; i < len(allEvents); i += 100 {
			end := i + 100
			if end > len(allEvents) {
				end = len(allEvents)
			}
			batch := allEvents[i:end]
			if err := c.lago.SendEvents(batch); err != nil {
				log.Printf("[controller] error sending %d events: %v", len(batch), err)
			} else {
				log.Printf("[controller] sent %d billing events to Lago", len(batch))
			}
		}
	}
}

// roundFloat rounds to 6 decimal places for billing precision.
func roundFloat(f float64) float64 {
	return float64(int64(f*1000000)) / 1000000
}

// sanitize removes characters that aren't safe for transaction IDs.
func sanitize(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result = append(result, c)
		}
	}
	return string(result)
}
