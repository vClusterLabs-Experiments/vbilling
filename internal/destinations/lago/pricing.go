package lago

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
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

// PriceDims lists the keys besides "sku" that the metric's billable metric
// filters on (region, capacity_type, ...), as last read from Lago, and the
// keys PRICING_KEYS prices the metric on: charge filters can price each
// combination of their values differently. The v0.1 property "vcluster" is
// the tenant_cluster dimension.
func (a *Adapter) PriceDims(metric string) []string {
	a.dimsMu.Lock()
	keys := slices.Clone(a.filterDims[metric])
	a.dimsMu.Unlock()
	added := false
	for _, k := range usage.ExtraPricingKeys(metric) {
		if k != "sku" && !slices.Contains(keys, k) {
			keys, added = append(keys, k), true
		}
	}
	if added {
		sort.Strings(keys)
	}
	return keys
}

// noteFilterDims remembers a billable metric's filter keys for PriceDims.
func (a *Adapter) noteFilterDims(bm *BillableMetric) {
	if bm == nil {
		return
	}
	var keys []string
	seen := map[string]bool{}
	for _, f := range bm.Filters {
		k := dimOfFilterKey(f.Key)
		if k != "sku" && k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	a.dimsMu.Lock()
	defer a.dimsMu.Unlock()
	if a.filterDims == nil {
		a.filterDims = map[string][]string{}
	}
	a.filterDims[bm.Code] = keys
}

// dimOfFilterKey maps a Lago event property to the series dimension that
// carries its value (ToEvent's mapping, reversed).
func dimOfFilterKey(k string) string {
	if k == "vcluster" {
		return usage.DimTenantCluster
	}
	return k
}

// PrepareSeries adds new SKUs to their billable metric's "sku" filter
// values, and the values of the keys PRICING_KEYS prices a metric on to
// those keys' filters. Lago only accepts a charge filter for a value its
// billable metric lists, so without this nobody could price a held SKU (or
// value) in Lago. It adds no charge filter and no price.
func (a *Adapter) PrepareSeries(ctx context.Context, series []destinations.PriceSeries) error {
	skus := map[string][]string{}
	values := map[string]map[string][]string{}
	for _, s := range series {
		if s.Catalog || !a.Accepts(s.Metric) {
			continue
		}
		if s.SKU != "" {
			skus[s.Metric] = append(skus[s.Metric], s.SKU)
		}
		keyValues(values, s.Metric, func(k string) string { return s.Dims[k] })
	}
	if err := a.syncKeyValues(ctx, values, false); err != nil {
		return err
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
		a.noteFilterDims(bm)
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
				a.noteFilterDims(bm)
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

	if s.Catalog || (s.SKU == "" && destinations.DimsLabel(s.Dims) == "") {
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
	// A series with a SKU, or with values of other keys a charge filter can
	// price on (a key PRICING_KEYS adds to a metric without SKUs), is priced
	// by the charge filter Lago applies to it, else by the charge.

	f, exact := filterFor(ch.Filters, s)
	inherited := false
	if f != nil {
		label := "the charge filter for " + describeFilter(f)
		if !exact {
			label = "the charge filter for " + describeFilter(f) + " (matching every value)"
		}
		if p, found := pricesSomething(f.Properties); p || !found {
			return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: fmt.Sprintf("%s: %s", where, label)}
		}
		if copied, ok := a.state.Get(inheritKey(plan.Code, bm.Code, s.SKU)); exact && len(f.Values) == 1 && ok {
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
			Detail: fmt.Sprintf("%s: billed at the charge's default price (no charge filter matches %s)", where, describeSeries(s))}
	}
	if sibling := pricedSibling(ch.Filters); sibling != nil {
		how := "no charge filter prices " + describeSeries(s) + " and the charge's default price is 0"
		if inherited {
			how = "the charge filter vBilling split out for sku=" + s.SKU + " copied the default price 0"
		}
		with := "a charge filter in Lago, or LAGO_PRICE_CARD before the SKU's first event"
		if s.SKU == "" {
			with = "a charge filter in Lago"
		}
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("%s: %s, so Lago bills it at 0 while other usage has prices (%s)", where, how, describeFilter(sibling)),
			Hint:   fmt.Sprintf("price %s in the charge for %s on plan %q (%s)", describeSeries(s), bm.Code, plan.Code, with),
		}
	}
	return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: fmt.Sprintf("%s: priced at 0", where)}
}

// filterFor returns the charge filter Lago applies to a series' usage, as
// Lago matches them: every key of the filter must match (a listed value, or
// any value for __ALL_FILTER_VALUES__), and of the filters that match, the
// one with the most keys applies. exact is false when a key matched through
// __ALL_FILTER_VALUES__ or a key the series does not carry. A key the
// series does not carry at all (it was seen before the filter existed) is
// assumed to match, as v0.3.0 assumed for every key besides sku.
func filterFor(filters []ChargeFilter, s destinations.PriceSeries) (*ChargeFilter, bool) {
	var best *ChargeFilter
	bestKeys, bestExact, bestUnknown := -1, false, true
	for i := range filters {
		f := &filters[i]
		ok, exact, unknown := true, true, false
		for k, values := range f.Values {
			v, known := seriesValue(s, k)
			if !known {
				exact, unknown = false, true
				continue
			}
			match := false
			for _, want := range values {
				if want == v && v != "" {
					match = true
					break
				}
				if want == allFilterValues && v != "" {
					match, exact = true, false
					break
				}
			}
			if !match {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		// A filter that matches on keys the series carries beats one that
		// only matches by assumption; then the one with more keys wins.
		n := len(f.Values)
		better := best == nil ||
			!unknown && bestUnknown ||
			unknown == bestUnknown && (n > bestKeys || n == bestKeys && exact && !bestExact)
		if better {
			best, bestKeys, bestExact, bestUnknown = f, n, exact, unknown
		}
	}
	return best, bestExact
}

// seriesValue returns a series' value of a Lago filter key, and whether the
// series carries that key at all.
func seriesValue(s destinations.PriceSeries, key string) (string, bool) {
	if key == "sku" {
		return s.SKU, true
	}
	v, ok := s.Dims[dimOfFilterKey(key)]
	return v, ok
}

// describeFilter renders a charge filter's values, "sku=H100, region=us-east".
func describeFilter(f *ChargeFilter) string {
	keys := make([]string, 0, len(f.Values))
	for k := range f.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vals := make([]string, 0, len(f.Values[k]))
		for _, v := range f.Values[k] {
			if v == allFilterValues {
				v = "*"
			}
			vals = append(vals, v)
		}
		parts = append(parts, k+"="+strings.Join(vals, "|"))
	}
	return strings.Join(parts, ", ")
}

// describeSeries renders a series' SKU and dimensions.
func describeSeries(s destinations.PriceSeries) string {
	var parts []string
	if s.SKU != "" {
		parts = append(parts, "sku="+s.SKU)
	}
	keys := make([]string, 0, len(s.Dims))
	for k := range s.Dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+s.Dims[k])
	}
	return strings.Join(parts, ", ")
}

// pricedSibling returns a charge filter with a non-zero price for specific
// values (not only __ALL_FILTER_VALUES__).
func pricedSibling(filters []ChargeFilter) *ChargeFilter {
	for i := range filters {
		f := &filters[i]
		p, _ := pricesSomething(f.Properties)
		if !p {
			continue
		}
		for _, vals := range f.Values {
			for _, v := range vals {
				if v != allFilterValues {
					return f
				}
			}
		}
	}
	return nil
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
