package lago

import (
	"log"
	"strings"

	"github.com/loft-sh/vbilling/internal/config"
)

// MetricCodes for all billable metrics.
const (
	MetricCPUCoreHours      = "vcluster_cpu_core_hours"
	MetricMemoryGBHours     = "vcluster_memory_gb_hours"
	MetricStorageGBHours    = "vcluster_storage_gb_hours"
	MetricInstanceHours     = "vcluster_instance_hours"
	MetricGPUHours          = "vcluster_gpu_hours"
	MetricGPUUtilization    = "vcluster_gpu_utilization"
	MetricNetworkEgressGB   = "vcluster_network_egress_gb"
	MetricLBHours           = "vcluster_lb_hours"
	MetricPrivateNodeHours  = "vcluster_private_node_hours"
)

// Bootstrap creates all billable metrics and a default plan in Lago.
// It is idempotent — safe to call on every startup.
func Bootstrap(client *Client, cfg *config.Config) error {
	log.Println("[bootstrap] setting up Lago billing configuration...")

	// Step 1: Create billable metrics
	metrics := []BillableMetric{
		{
			Name:            "vCluster CPU Core-Hours",
			Code:            MetricCPUCoreHours,
			Description:     "CPU core-hours consumed by vCluster workloads",
			AggregationType: "sum_agg",
			FieldName:       "cpu_core_hours",
		},
		{
			Name:            "vCluster Memory GB-Hours",
			Code:            MetricMemoryGBHours,
			Description:     "Memory GB-hours consumed by vCluster workloads",
			AggregationType: "sum_agg",
			FieldName:       "memory_gb_hours",
		},
		{
			Name:            "vCluster Storage GB-Hours",
			Code:            MetricStorageGBHours,
			Description:     "Persistent storage GB-hours consumed by vCluster workloads",
			AggregationType: "sum_agg",
			FieldName:       "storage_gb_hours",
		},
		{
			Name:            "vCluster Instance Hours",
			Code:            MetricInstanceHours,
			Description:     "Per-vCluster flat hourly charge for running an instance",
			AggregationType: "sum_agg",
			FieldName:       "instance_hours",
		},
		{
			Name:            "vCluster GPU Hours",
			Code:            MetricGPUHours,
			Description:     "GPU-hours allocated to vCluster workloads (by GPU type)",
			AggregationType: "sum_agg",
			FieldName:       "gpu_hours",
		},
		{
			Name:            "vCluster GPU Utilization Score",
			Code:            MetricGPUUtilization,
			Description:     "GPU utilization percentage points (from DCGM) for charge adjustments",
			AggregationType: "sum_agg",
			FieldName:       "gpu_util_score",
		},
		{
			Name:            "vCluster Network Egress GB",
			Code:            MetricNetworkEgressGB,
			Description:     "Network egress traffic in GB from vCluster workloads",
			AggregationType: "sum_agg",
			FieldName:       "egress_gb",
		},
		{
			Name:            "vCluster LoadBalancer Hours",
			Code:            MetricLBHours,
			Description:     "Hourly charge per LoadBalancer service in a vCluster",
			AggregationType: "sum_agg",
			FieldName:       "lb_hours",
		},
		{
			Name:            "vCluster Private Node Hours",
			Code:            MetricPrivateNodeHours,
			Description:     "Dedicated node hours allocated to a vCluster tenant (private node mode)",
			AggregationType: "sum_agg",
			FieldName:       "private_node_hours",
		},
	}

	metricIDs := make(map[string]string) // code -> lago_id
	for _, m := range metrics {
		existing, err := client.GetBillableMetric(m.Code)
		if err == nil && existing.LagoID != "" {
			metricIDs[m.Code] = existing.LagoID
			log.Printf("[bootstrap] metric %q already exists (id=%s)", m.Code, existing.LagoID)
			continue
		}

		created, err := client.CreateBillableMetric(m)
		if err != nil {
			// If it already exists (409), try to get it again
			if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "already") {
				log.Printf("[bootstrap] metric %q already exists, skipping", m.Code)
				continue
			}
			return err
		}
		metricIDs[m.Code] = created.LagoID
		log.Printf("[bootstrap] created metric %q (id=%s)", m.Code, created.LagoID)
	}

	// Step 2: Create default plan with charges
	_, err := client.GetPlan(cfg.DefaultPlanCode)
	if err == nil {
		log.Printf("[bootstrap] plan %q already exists", cfg.DefaultPlanCode)
		return nil
	}

	log.Println("[bootstrap] Configure your pricing in the Lago UI or API — all charges default to $0")

	charges := []Charge{
		{
			BillableMetricID: metricIDs[MetricCPUCoreHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricMemoryGBHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricStorageGBHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricInstanceHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricGPUHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricGPUUtilization],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricNetworkEgressGB],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricLBHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
		{
			BillableMetricID: metricIDs[MetricPrivateNodeHours],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		},
	}

	// Filter out charges with empty metric IDs (metric creation may have failed)
	var validCharges []Charge
	for _, ch := range charges {
		if ch.BillableMetricID != "" {
			validCharges = append(validCharges, ch)
		}
	}

	plan := Plan{
		Name:           "vCluster Standard",
		Code:           cfg.DefaultPlanCode,
		Interval:       "monthly",
		AmountCents:    0, // no base price, pure usage-based
		AmountCurrency: cfg.BillingCurrency,
		PayInAdvance:   false,
		Charges:        validCharges,
	}

	created, err := client.CreatePlan(plan)
	if err != nil {
		if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "already") {
			log.Printf("[bootstrap] plan %q already exists, skipping", cfg.DefaultPlanCode)
			return nil
		}
		return err
	}
	log.Printf("[bootstrap] created plan %q (id=%s) with %d charges", created.Code, created.LagoID, len(validCharges))

	log.Println("[bootstrap] Lago billing configuration complete")
	return nil
}
