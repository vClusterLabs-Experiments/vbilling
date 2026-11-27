package lago

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
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

// lagoMetrics lists the billable metrics vBilling creates in Lago. Other
// billable metrics of the catalog (custom ones, the storage overage) get a
// billable metric whose field name is their code.
func lagoMetrics(catalog []usage.MetricDef) []BillableMetric {
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
	listed := map[string]bool{}
	for _, m := range metrics {
		listed[m.Code] = true
	}
	for _, def := range catalog {
		// Informational metrics (billable: false) never reach Lago. A
		// billable metric not listed above (a custom one, or the storage
		// overage of ALLOWANCES_FILE) gets a billable metric of its own.
		if listed[def.Code] || !def.Billable {
			continue
		}
		metrics = append(metrics, BillableMetric{
			Name: def.Name, Code: def.Code, Description: def.Description,
			AggregationType: "sum_agg", FieldName: fieldNameFor(def.Code),
		})
	}
	return metrics
}

// Bootstrap creates the billable metrics and the default plan in Lago, and
// gives every managed plan a charge for each metric it has never had. It is
// idempotent: safe to call on every startup.
func (a *Adapter) Bootstrap(ctx context.Context, catalog []usage.MetricDef) error {
	log.Println("[bootstrap] setting up Lago billing configuration...")
	metrics := lagoMetrics(catalog)
	metricIDs, err := a.ensureMetrics(ctx, metrics)
	if err != nil {
		return err
	}
	if err := a.ensurePlans(ctx, metrics, metricIDs); err != nil {
		return err
	}

	// SKUs the price card prices get their charge filters now, so their
	// prices are in Lago before the first event. Best effort, like on events.
	known := map[string]bool{}
	for _, m := range metrics {
		known[m.Code] = true
	}
	skus := a.priceCard.skus()
	for metric := range a.priceCard {
		if !known[metric] {
			log.Printf("[bootstrap] price card: %q is not a vBilling metric, ignoring it", metric)
			delete(skus, metric)
		}
	}
	a.syncSKUs(ctx, skus)

	log.Println("[bootstrap] Lago billing configuration complete")
	return nil
}

// ensureMetrics creates the billable metrics that do not exist yet and
// returns the Lago ID of each.
func (a *Adapter) ensureMetrics(ctx context.Context, metrics []BillableMetric) (map[string]string, error) {
	ids := make(map[string]string) // code -> lago_id
	for _, m := range metrics {
		existing, err := a.client.GetBillableMetric(ctx, m.Code)
		if err == nil && existing.LagoID != "" {
			ids[m.Code] = existing.LagoID
			a.noteFilterDims(existing)
			log.Printf("[bootstrap] metric %q already exists (id=%s)", m.Code, existing.LagoID)
			continue
		}

		created, err := a.client.CreateBillableMetric(ctx, m)
		if err != nil {
			if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "already") {
				// Created concurrently, or the lookup failed: read it again.
				if existing, gerr := a.client.GetBillableMetric(ctx, m.Code); gerr == nil && existing.LagoID != "" {
					ids[m.Code] = existing.LagoID
				}
				log.Printf("[bootstrap] metric %q already exists, skipping", m.Code)
				continue
			}
			return nil, err
		}
		ids[m.Code] = created.LagoID
		log.Printf("[bootstrap] created metric %q (id=%s)", m.Code, created.LagoID)
	}
	return ids, nil
}

func (a *Adapter) ensurePlans(ctx context.Context, metrics []BillableMetric, metricIDs map[string]string) error {
	a.planMu.Lock()
	defer a.planMu.Unlock()
	for _, code := range a.managedPlans() {
		err := a.ensurePlan(ctx, code, metrics, metricIDs)
		if destinations.IsPermanent(err) || isNotFound(err) {
			// Lago rejects the change itself, so retrying cannot help, and
			// delivery waits for Bootstrap. Lago keeps the events and prices
			// them once the charge exists: report it and carry on.
			log.Printf("[bootstrap] %v (continuing; fix it in Lago, vBilling retries on its next start)", err)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ensurePlan creates the default plan if it is missing, and adds missing
// charges to a plan that exists. A missing managed plan other than the
// default one is left to the operator.
func (a *Adapter) ensurePlan(ctx context.Context, code string, metrics []BillableMetric, metricIDs map[string]string) error {
	plan, err := a.client.GetPlan(ctx, code)
	switch {
	case isNotFound(err) && code == a.cfg.DefaultPlanCode:
		return a.createDefaultPlan(ctx, metrics, metricIDs)
	case isNotFound(err):
		log.Printf("[bootstrap] managed plan %q does not exist in Lago; create it there for vBilling to manage its charges", code)
		return nil
	case err != nil:
		return fmt.Errorf("get plan %q: %w", code, err)
	}
	return a.addMissingCharges(ctx, plan, metrics, metricIDs)
}

func (a *Adapter) createDefaultPlan(ctx context.Context, metrics []BillableMetric, metricIDs map[string]string) error {
	if len(a.priceCard) == 0 {
		log.Println("[bootstrap] Configure your pricing in the Lago UI or API: all charges default to $0")
	}
	var charges []Charge
	for _, m := range metrics {
		if metricIDs[m.Code] == "" {
			continue // metric creation failed
		}
		charges = append(charges, Charge{
			BillableMetricID: metricIDs[m.Code],
			ChargeModel:      "standard",
			Properties:       map[string]string{"amount": a.priceCard.chargeAmount(m.Code)},
		})
	}

	plan := Plan{
		Name:           "vCluster Standard",
		Code:           a.cfg.DefaultPlanCode,
		Interval:       "monthly",
		AmountCents:    0, // no base price, pure usage-based
		AmountCurrency: a.cfg.BillingCurrency,
		PayInAdvance:   false,
		Charges:        charges,
	}

	created, err := a.client.CreatePlan(ctx, plan)
	if err != nil {
		if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "already") {
			if existing, gerr := a.client.GetPlan(ctx, plan.Code); gerr == nil {
				return a.addMissingCharges(ctx, existing, metrics, metricIDs) // created concurrently
			}
			log.Printf("[bootstrap] could not create plan %q, skipping: %v", plan.Code, err)
			return nil
		}
		return err
	}
	for _, m := range metrics {
		if metricIDs[m.Code] != "" {
			a.markCharge(plan.Code, m.Code)
		}
	}
	log.Printf("[bootstrap] created plan %q (id=%s) with %d charges", created.Code, created.LagoID, len(charges))
	return nil
}

// addMissingCharges gives plan a standard charge for every metric it has
// never had a charge for, so metrics declared after the first start are
// billed. The update resends every existing charge unchanged: Lago deletes
// the charges a plan update omits. A charge someone deleted in Lago after
// vBilling saw it is not added back.
func (a *Adapter) addMissingCharges(ctx context.Context, plan *PlanDetail, metrics []BillableMetric, metricIDs map[string]string) error {
	charged := map[string]bool{}
	for _, ch := range plan.Charges {
		charged[ch.BillableMetricCode] = true
		charged[ch.LagoBillableMetricID] = true
	}
	var added []string
	for _, m := range metrics {
		id := metricIDs[m.Code]
		if id == "" {
			continue // metric creation failed
		}
		if charged[m.Code] || charged[id] {
			a.markCharge(plan.Code, m.Code)
			continue
		}
		if _, seen := a.state.Get(chargeKey(plan.Code, m.Code)); seen {
			continue // removed in Lago on purpose
		}
		props, _ := json.Marshal(map[string]string{"amount": a.priceCard.chargeAmount(m.Code)})
		plan.Charges = append(plan.Charges, PlanCharge{LagoBillableMetricID: id, BillableMetricCode: m.Code, ChargeModel: "standard", Properties: props})
		added = append(added, m.Code)
	}
	if len(added) == 0 {
		log.Printf("[bootstrap] plan %q already exists with its charges", plan.Code)
		return nil
	}
	if _, err := a.client.UpdatePlan(ctx, plan); err != nil {
		return fmt.Errorf("add charges for %s to plan %q: %w", strings.Join(added, ", "), plan.Code, err)
	}
	for _, code := range added {
		a.markCharge(plan.Code, code)
	}
	sort.Strings(added)
	log.Printf("[bootstrap] plan %q: added charges for %s", plan.Code, strings.Join(added, ", "))
	return nil
}

func chargeKey(plan, metric string) string { return "charge/" + plan + "/" + metric }

func (a *Adapter) markCharge(plan, metric string) {
	if err := a.state.Set(chargeKey(plan, metric), "1"); err != nil {
		log.Printf("[lago] save state: %v", err)
	}
}
