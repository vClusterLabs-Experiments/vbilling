package lago

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage reads billable metrics and plans
// (GET /api/v1/billable_metrics/{code}, GET /api/v1/plans/{code}). It never
// changes a charge or a price.

func inheritKey(plan, metric, sku string) string {
	return "skuinherit/" + plan + "/" + metric + "/" + sku
}

// PricePlan is the plan a tenant is subscribed to (EnsureTenant's choice).
func (a *Adapter) PricePlan(t usage.Tenant) string {
	if t.Plan != "" {
		return t.Plan
	}
	return a.cfg.DefaultPlanCode
}

// PriceDims: vBilling prices SKUs through charge filters on "sku" only.
func (a *Adapter) PriceDims(string) []string { return nil }

// PrepareSeries adds new SKUs to their billable metric's "sku" filter
// values. Lago only accepts a charge filter for a value its billable metric
// lists, so without this nobody could price a held SKU in Lago. It adds no
// charge filter and no price.
func (a *Adapter) PrepareSeries(ctx context.Context, series []destinations.PriceSeries) error {
	skus := map[string][]string{}
	for _, s := range series {
		if !s.Catalog && s.SKU != "" && a.Accepts(s.Metric) {
			skus[s.Metric] = append(skus[s.Metric], s.SKU)
		}
	}
	if len(skus) == 0 {
		return nil
	}
	a.planMu.Lock()
	defer a.planMu.Unlock()
	metrics := make([]string, 0, len(skus))
	for m := range skus {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, metric := range metrics {
		bm, err := a.client.GetBillableMetric(ctx, metric)
		if isNotFound(err) {
			continue // the check reports it
		}
		if err != nil {
			return fmt.Errorf("get billable metric %s: %w", metric, err)
		}
		filters, added := withFilterValues(bm.Filters, "sku", skus[metric])
		if len(added) == 0 {
			continue
		}
		if _, err := a.client.UpdateBillableMetricFilters(ctx, metric, filters); err != nil {
			return err
		}
		log.Printf("[lago] billable metric %s: added sku filter values %s (no price set)", metric, strings.Join(added, ", "))
	}
	return nil
}

// CheckPrices reports, for each series, whether its plan has a charge for
// the metric and what price applies to the SKU: the charge filter for it, or
// the charge's default price. A SKU that falls to a default price of 0 while
// other SKUs of the metric have prices is a gap (the rate miss this check
// exists for); a filter or a whole charge priced at 0 is reported as zero.
func (a *Adapter) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	metrics := map[string]*BillableMetric{} // code -> nil when missing
	plans := map[string]*PlanDetail{}       // code -> nil when missing
	for _, s := range series {
		if _, done := metrics[s.Metric]; !done {
			bm, err := a.client.GetBillableMetric(ctx, s.Metric)
			switch {
			case isNotFound(err):
				metrics[s.Metric] = nil
			case err != nil:
				return nil, fmt.Errorf("get billable metric %s: %w", s.Metric, err)
			default:
				metrics[s.Metric] = bm
			}
		}
		if _, done := plans[s.Plan]; !done && s.Plan != "" {
			p, err := a.client.GetPlan(ctx, s.Plan)
			switch {
			case isNotFound(err):
				plans[s.Plan] = nil
			case err != nil:
				return nil, fmt.Errorf("get plan %s: %w", s.Plan, err)
			default:
				plans[s.Plan] = p
			}
		}
	}
	out := make([]destinations.PriceCoverage, 0, len(series))
	for _, s := range series {
		cov := a.coverage(s, metrics[s.Metric], plans[s.Plan])
		cov.Series = s
		out = append(out, cov)
	}
	return out, nil
}

func (a *Adapter) coverage(s destinations.PriceSeries, bm *BillableMetric, plan *PlanDetail) destinations.PriceCoverage {
	if bm == nil {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("Lago has no billable metric %q", s.Metric),
			Hint:   "vBilling creates billable metrics when the lago destination bootstraps: check its last error in GET /api/v1/destinations",
		}
	}
	if plan == nil {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("Lago has no plan %q", s.Plan),
			Hint:   fmt.Sprintf("create plan %q in Lago, or fix DEFAULT_PLAN_CODE or the tenant's plan", s.Plan),
		}
	}
	var charges []PlanCharge
	for _, ch := range plan.Charges {
		if ch.LagoBillableMetricID == bm.LagoID || ch.BillableMetricCode == bm.Code {
			charges = append(charges, ch)
		}
	}
	if len(charges) == 0 {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("plan %q has no charge for %s", plan.Code, bm.Code),
			Hint:   fmt.Sprintf("add a charge for billable metric %s to plan %q", bm.Code, plan.Code),
		}
	}
	ch := charges[0]
	where := fmt.Sprintf("plan %q, charge for %s", plan.Code, bm.Code)
	if len(charges) > 1 {
		where += fmt.Sprintf(" (first of %d charges)", len(charges))
	}
	def, defFound := pricesSomething(ch.Properties)

	if s.Catalog || s.SKU == "" {
		priced := def
		for _, f := range ch.Filters {
			if p, _ := pricesSomething(f.Properties); p && s.Catalog {
				priced = true
			}
		}
		if priced || !defFound {
			return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: fmt.Sprintf("%s (%s model)", where, ch.ChargeModel)}
		}
		return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: where + ": priced at 0"}
	}

	f, exact := filterFor(ch.Filters, s.SKU)
	inherited := false
	if f != nil {
		label := "the charge filter for sku=" + s.SKU
		if !exact {
			label = "the charge filter for every sku"
		}
		if p, found := pricesSomething(f.Properties); p || !found {
			return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: fmt.Sprintf("%s: %s", where, label)}
		}
		if copied, ok := a.state.Get(inheritKey(plan.Code, bm.Code, s.SKU)); exact && ok {
			amount, _ := amountOf(f.Properties)
			inherited = amount == copied
		}
		if _, carded := a.priceCard.skuAmount(bm.Code, s.SKU); carded {
			inherited = false // the price card prices it, at 0 on purpose
		}
		if !inherited {
			return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: fmt.Sprintf("%s: %s is priced at 0", where, label)}
		}
		// A filter vBilling split out with the charge's price of 0 is not a
		// decision to bill this SKU at 0.
	} else if def || !defFound {
		return destinations.PriceCoverage{Status: destinations.PriceCovered,
			Detail: fmt.Sprintf("%s: billed at the charge's default price (no charge filter of its own for sku=%s)", where, s.SKU)}
	}
	if sibling := pricedSibling(ch.Filters); sibling != "" {
		how := "no charge filter prices sku=" + s.SKU + " and the charge's default price is 0"
		if inherited {
			how = "the charge filter vBilling split out for sku=" + s.SKU + " copied the default price 0"
		}
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("%s: %s, so Lago bills it at 0 while other SKUs have prices (sku=%s)", where, how, sibling),
			Hint:   fmt.Sprintf("price sku=%s in the charge for %s on plan %q (its charge filter in Lago, or LAGO_PRICE_CARD before the SKU's first event)", s.SKU, bm.Code, plan.Code),
		}
	}
	return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: fmt.Sprintf("%s: priced at 0", where)}
}

// filterFor returns the charge filter that prices sku: one listing it, else
// one for every value of "sku".
func filterFor(filters []ChargeFilter, sku string) (*ChargeFilter, bool) {
	var all *ChargeFilter
	for i := range filters {
		for _, v := range filters[i].Values["sku"] {
			if v == sku {
				return &filters[i], true
			}
			if v == allFilterValues && all == nil {
				all = &filters[i]
			}
		}
	}
	return all, false
}

// pricedSibling returns a SKU whose own charge filter has a non-zero price.
func pricedSibling(filters []ChargeFilter) string {
	for _, f := range filters {
		p, _ := pricesSomething(f.Properties)
		if !p {
			continue
		}
		for _, v := range f.Values["sku"] {
			if v == allFilterValues {
				continue
			}
			return v
		}
	}
	return ""
}

// amountOf returns a charge or filter's "amount" property.
func amountOf(props json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if len(props) == 0 || json.Unmarshal(props, &m) != nil {
		return "", false
	}
	raw, ok := m["amount"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	return s, true
}

// priceKeys are the properties of Lago's charge models that carry money:
// standard and package (amount), percentage (rate, fixed_amount), graduated
// and volume ranges (per_unit_amount, flat_amount).
var priceKeys = map[string]bool{"amount": true, "rate": true, "fixed_amount": true, "per_unit_amount": true, "flat_amount": true}

// pricesSomething reports whether any money-carrying property is non-zero,
// and whether there was any such property at all.
func pricesSomething(props json.RawMessage) (nonZero, found bool) {
	var v any
	if len(props) == 0 || json.Unmarshal(props, &v) != nil {
		return false, false
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if priceKeys[k] {
					var f float64
					var err error
					switch n := val.(type) {
					case string:
						f, err = strconv.ParseFloat(n, 64)
					case float64:
						f = n
					case nil:
						continue
					default:
						continue
					}
					if err == nil {
						found = true
						if f != 0 {
							nonZero = true
						}
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	return nonZero, found
}

var (
	_ destinations.PriceChecker   = (*Adapter)(nil)
	_ destinations.SeriesPreparer = (*Adapter)(nil)
)
