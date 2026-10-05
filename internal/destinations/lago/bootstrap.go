package lago

import (
	"context"
	"log"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Re-exports of canonical metric codes for in-package use.
const (
	MetricCPUCoreHours     = usage.MetricCPUCoreHours
	MetricMemoryGBHours    = usage.MetricMemoryGBHours
	MetricStorageGBHours   = usage.MetricStorageGBHours
	MetricInstanceHours    = usage.MetricInstanceHours
	MetricGPUHours         = usage.MetricGPUHours
	MetricGPUUtilization   = usage.MetricGPUUtilization
	MetricNetworkEgressGB  = usage.MetricNetworkEgressGB
	MetricLBHours          = usage.MetricLBHours
	MetricPrivateNodeHours = usage.MetricPrivateNodeHours
)

// Bootstrap creates all billable metrics and a default plan in Lago.
// It is idempotent: safe to call on every startup. Custom metrics from the
// catalog get a billable metric whose field name is their code.
func Bootstrap(ctx context.Context, client *Client, cfg *config.Config, catalog []usage.MetricDef) error {
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

	for _, def := range catalog {
		if !def.Custom {
			continue
		}
		metrics = append(metrics, BillableMetric{
			Name: def.Name, Code: def.Code, Description: def.Description,
			AggregationType: "sum_agg", FieldName: fieldNameFor(def.Code),
		})
	}

	metricIDs := make(map[string]string) // code -> lago_id
	for _, m := range metrics {
		existing, err := client.GetBillableMetric(ctx, m.Code)
		if err == nil && existing.LagoID != "" {
			metricIDs[m.Code] = existing.LagoID
			log.Printf("[bootstrap] metric %q already exists (id=%s)", m.Code, existing.LagoID)
			continue
		}

		created, err := client.CreateBillableMetric(ctx, m)
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
	_, err := client.GetPlan(ctx, cfg.DefaultPlanCode)
	if err == nil {
		log.Printf("[bootstrap] plan %q already exists", cfg.DefaultPlanCode)
		return nil
	}

	log.Println("[bootstrap] Configure your pricing in the Lago UI or API — all charges default to $0")

	var charges []Charge
	for _, m := range metrics {
		charges = append(charges, Charge{
			BillableMetricID: metricIDs[m.Code],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": "0"},
		})
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

	created, err := client.CreatePlan(ctx, plan)
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
