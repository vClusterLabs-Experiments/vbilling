package lago

import (
	"encoding/json"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A charge filter's predicates are conjunctive: a price for H100/us-east
// does not cover H100/us-west. The zero default remains a pricing gap.
func TestReviewLagoCoverageChecksEveryFilterPredicate(t *testing.T) {
	a := New(nil, &config.Config{DefaultPlanCode: "standard"}, nil, nil)
	bm := &BillableMetric{LagoID: "bm_gpu", Code: usage.MetricGPUHours, Filters: []MetricFilter{
		{Key: "sku", Values: []string{"H100"}}, {Key: "region", Values: []string{"us-east", "us-west"}},
	}}
	plan := &PlanDetail{Code: "standard", Charges: []PlanCharge{{
		LagoBillableMetricID: "bm_gpu", BillableMetricCode: usage.MetricGPUHours,
		ChargeModel: "standard", Properties: json.RawMessage(`{"amount":"0"}`),
		Filters: []ChargeFilter{{Properties: json.RawMessage(`{"amount":"2.49"}`),
			Values: map[string][]string{"sku": {"H100"}, "region": {"us-east"}}}},
	}}}
	ser := destinations.PriceSeries{Plan: "standard", Metric: usage.MetricGPUHours, SKU: "H100", Dims: map[string]string{"region": "us-west"}}
	cov := a.coverage(ser, bm, plan)
	if cov.Status != destinations.PriceMissing {
		t.Fatalf("coverage=%s (%s), but the sole paid filter is us-east and this series is us-west", cov.Status, cov.Detail)
	}
}

// Lago applies the matching filter with the most keys; a series seen before
// the region filter existed (no region key) keeps v0.3.0's sku-only match;
// PriceDims reports the filter keys once the billable metric is read.
func TestLagoCoverageMostSpecificFilter(t *testing.T) {
	a := New(nil, &config.Config{DefaultPlanCode: "standard"}, nil, nil)
	bm := &BillableMetric{LagoID: "bm_gpu", Code: usage.MetricGPUHours, Filters: []MetricFilter{
		{Key: "sku", Values: []string{"H100"}}, {Key: "region", Values: []string{"us-east", "us-west"}},
	}}
	plan := &PlanDetail{Code: "standard", Charges: []PlanCharge{{
		LagoBillableMetricID: "bm_gpu", BillableMetricCode: usage.MetricGPUHours,
		ChargeModel: "standard", Properties: json.RawMessage(`{"amount":"0"}`),
		Filters: []ChargeFilter{
			{Properties: json.RawMessage(`{"amount":"2.49"}`), Values: map[string][]string{"sku": {"H100"}}},
			{Properties: json.RawMessage(`{"amount":"0"}`), Values: map[string][]string{"sku": {"H100"}, "region": {"us-west"}}},
		},
	}}}
	cov := func(dims map[string]string) string {
		return a.coverage(destinations.PriceSeries{Plan: "standard", Metric: usage.MetricGPUHours, SKU: "H100", Dims: dims}, bm, plan).Status
	}
	if got := cov(map[string]string{"region": "us-east"}); got != destinations.PriceCovered {
		t.Errorf("us-east = %s, want priced by the sku filter", got)
	}
	if got := cov(map[string]string{"region": "us-west"}); got != destinations.PriceZero {
		t.Errorf("us-west = %s, want zero: its own filter prices it at 0", got)
	}
	if got := cov(nil); got != destinations.PriceCovered {
		t.Errorf("series without a region = %s, want the sku-only match", got)
	}
	a.noteFilterDims(bm)
	if got := a.PriceDims(usage.MetricGPUHours); len(got) != 1 || got[0] != "region" {
		t.Errorf("PriceDims = %v, want [region]", got)
	}
}
