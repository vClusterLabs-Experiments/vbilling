package lago

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Lago's charge filter value for "every value of this key".
const allFilterValues = "__ALL_FILTER_VALUES__"

const (
	skuRetryMin    = time.Minute
	skuRetryMax    = 30 * time.Minute
	skuSyncTimeout = 30 * time.Second // per metric
)

// retry is the backoff of a metric whose SKU filters could not be set up.
type retry struct {
	at    time.Time
	delay time.Duration
}

func skuKey(metric, sku string) string { return "sku/" + metric + "/" + sku }

// syncSKUs gives each new SKU of a metric its own line in Lago. Lago prices
// event properties only through filters: the SKU joins the billable metric's
// "sku" filter, then the metric's charge on every managed plan gets a charge
// filter for it. Finished SKUs are remembered in the adapter state, so this
// runs once per SKU. It is best effort: failures are logged and retried on a
// later batch, and never fail event delivery.
func (a *Adapter) syncSKUs(ctx context.Context, skus map[string][]string) {
	if len(skus) == 0 {
		return
	}
	a.planMu.Lock()
	defer a.planMu.Unlock()

	plans := a.managedPlans()
	metrics := make([]string, 0, len(skus))
	for m := range skus {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, metric := range metrics {
		pending := a.pendingSKUs(metric, skus[metric], plans)
		if len(pending) == 0 {
			continue
		}
		if r, ok := a.skuRetry[metric]; ok && a.now().Before(r.at) {
			continue
		}
		if err := a.addSKUs(ctx, metric, pending, plans); err != nil {
			r := a.skuRetry[metric]
			r.delay = min(max(2*r.delay, skuRetryMin), skuRetryMax)
			r.at = a.now().Add(r.delay)
			a.skuRetry[metric] = r
			log.Printf("[lago] sku filters for %s (%s) not set up, retrying in %s (usage is still delivered): %v",
				metric, strings.Join(pending, ", "), r.delay, err)
			continue
		}
		delete(a.skuRetry, metric)
		for _, sku := range pending {
			if err := a.state.Set(skuKey(metric, sku), strings.Join(plans, ",")); err != nil {
				log.Printf("[lago] save state: %v", err)
			}
		}
	}
}

// pendingSKUs returns the distinct SKUs of a metric that are not set up on
// every managed plan yet (a plan added to LAGO_MANAGED_PLANS gets them too).
func (a *Adapter) pendingSKUs(metric string, skus, plans []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, sku := range skus {
		if seen[sku] {
			continue
		}
		seen[sku] = true
		done := map[string]bool{}
		if v, ok := a.state.Get(skuKey(metric, sku)); ok {
			for _, p := range strings.Split(v, ",") {
				done[p] = true
			}
		}
		for _, p := range plans {
			if !done[p] {
				out = append(out, sku)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// addSKUs sets up the SKUs of one metric. The billable metric comes first:
// Lago rejects charge filter values its billable metric does not list.
func (a *Adapter) addSKUs(ctx context.Context, metric string, skus, plans []string) error {
	ctx, cancel := context.WithTimeout(ctx, skuSyncTimeout)
	defer cancel()

	bm, err := a.client.GetBillableMetric(ctx, metric)
	if err != nil {
		return fmt.Errorf("get billable metric: %w", err)
	}
	a.noteFilterDims(bm)
	if filters, added := withFilterValues(bm.Filters, "sku", skus); len(added) > 0 {
		if _, err := a.client.UpdateBillableMetricFilters(ctx, metric, filters); err != nil {
			return err
		}
		bm.Filters = filters
		log.Printf("[lago] billable metric %s: added sku filter values %s", metric, strings.Join(added, ", "))
	}
	for _, code := range plans {
		if err := a.addSKUFilters(ctx, code, bm, skus); err != nil {
			return err
		}
	}
	return nil
}

// withFilterValues returns a copy of filters with values added under key,
// keeping every other key and value (Lago discards the ones a billable
// metric update omits), and the values that were new.
func withFilterValues(filters []MetricFilter, key string, values []string) ([]MetricFilter, []string) {
	out := make([]MetricFilter, 0, len(filters)+1)
	at := -1
	for _, f := range filters {
		out = append(out, MetricFilter{Key: f.Key, Values: append([]string(nil), f.Values...)})
		if f.Key == key {
			at = len(out) - 1
		}
	}
	if at < 0 {
		out = append(out, MetricFilter{Key: key})
		at = len(out) - 1
	}
	have := map[string]bool{}
	for _, v := range out[at].Values {
		have[v] = true
	}
	var added []string
	for _, v := range values {
		if !have[v] {
			have[v] = true
			out[at].Values = append(out[at].Values, v)
			added = append(added, v)
		}
	}
	return out, added
}

// addSKUFilters adds a charge filter per SKU to the metric's charge on a
// plan, and sends every charge and filter of the plan back unchanged. It
// leaves alone SKUs that a filter in Lago already covers, and charges that
// are not standard (filters would change how graduated or package pricing
// applies) or that the metric has more than one of.
func (a *Adapter) addSKUFilters(ctx context.Context, planCode string, bm *BillableMetric, skus []string) error {
	plan, err := a.client.GetPlan(ctx, planCode)
	if isNotFound(err) && planCode != a.cfg.DefaultPlanCode {
		return nil // Bootstrap reports missing managed plans
	}
	if err != nil {
		return fmt.Errorf("get plan %s: %w", planCode, err)
	}
	var charge *PlanCharge
	n := 0
	for i := range plan.Charges {
		if plan.Charges[i].LagoBillableMetricID == bm.LagoID || plan.Charges[i].BillableMetricCode == bm.Code {
			charge = &plan.Charges[i]
			n++
		}
	}
	switch {
	case n == 0:
		log.Printf("[lago] plan %s has no charge for %s: no sku filters added", planCode, bm.Code)
		return nil
	case n > 1:
		log.Printf("[lago] plan %s has %d charges for %s: add its sku filters in Lago", planCode, n, bm.Code)
		return nil
	case charge.ChargeModel != "standard":
		log.Printf("[lago] plan %s prices %s with the %s charge model: add its sku filters in Lago", planCode, bm.Code, charge.ChargeModel)
		return nil
	}

	var added []string
	inherited := map[string]string{} // sku -> amount copied from the charge
	for _, sku := range skus {
		if coversSKU(charge.Filters, sku) {
			continue
		}
		props, err := a.filterProperties(bm.Code, sku, charge.Properties)
		if err != nil {
			return fmt.Errorf("plan %s, charge for %s: %w", planCode, bm.Code, err)
		}
		if _, carded := a.priceCard.skuAmount(bm.Code, sku); !carded {
			inherited[sku], _ = amountOf(props)
		}
		name, _ := json.Marshal(bm.Name + " - " + sku)
		charge.Filters = append(charge.Filters, ChargeFilter{InvoiceDisplayName: name, Properties: props, Values: map[string][]string{"sku": {sku}}})
		added = append(added, sku)
	}
	if len(added) == 0 {
		return nil
	}
	if _, err := a.client.UpdatePlan(ctx, plan); err != nil {
		return err
	}
	// Remember which filters only copy the charge's price: the price check
	// treats them as the charge's default price, not as a price for the SKU.
	for sku, amount := range inherited {
		if err := a.state.Set(inheritKey(planCode, bm.Code, sku), amount); err != nil {
			log.Printf("[lago] save state: %v", err)
		}
	}
	log.Printf("[lago] plan %s: added %s charge filters for sku %s", planCode, bm.Code, strings.Join(added, ", "))
	return nil
}

// keyValueKey is the adapter state of a value vBilling added to the filter
// of a key PRICING_KEYS prices a metric on.
func keyValueKey(metric, key, value string) string {
	return "keyvalue/" + metric + "/" + key + "/" + value
}

// keyValues adds to values (metric -> key -> values) the values one event
// or series carries for the keys PRICING_KEYS prices its metric on, besides
// sku (syncSKUs). A key without a value is left out: Lago prices such usage
// with the filters that do not name the key.
func keyValues(values map[string]map[string][]string, metric string, value func(key string) string) {
	for _, k := range usage.ExtraPricingKeys(metric) {
		v := value(k)
		if k == "sku" || v == "" {
			continue
		}
		if values[metric] == nil {
			values[metric] = map[string][]string{}
		}
		if !slices.Contains(values[metric][k], v) {
			values[metric][k] = append(values[metric][k], v)
		}
	}
}

// syncKeyValues adds the values of the keys PRICING_KEYS prices metrics on
// to the filters of their billable metrics in Lago, as syncSKUs does for
// SKUs: Lago accepts a charge filter only for a value its billable metric
// lists. It adds no charge filter and no price: whoever prices the usage
// adds charge filters for the combinations that cost something else, and
// the price check reads them. Values added once are remembered in the
// adapter state. With backoff (after a delivery), a metric whose values
// could not be added waits before it is tried again.
func (a *Adapter) syncKeyValues(ctx context.Context, values map[string]map[string][]string, backoff bool) error {
	if len(values) == 0 {
		return nil
	}
	a.planMu.Lock()
	defer a.planMu.Unlock()
	metrics := make([]string, 0, len(values))
	for m := range values {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	var errs []error
	for _, metric := range metrics {
		pending := map[string][]string{}
		var keys []string
		for k, vs := range values[metric] {
			for _, v := range vs {
				if _, done := a.state.Get(keyValueKey(metric, k, v)); !done {
					if pending[k] == nil {
						keys = append(keys, k)
					}
					pending[k] = append(pending[k], v)
				}
			}
		}
		if len(pending) == 0 {
			continue
		}
		retryKey := "values/" + metric
		if r, ok := a.skuRetry[retryKey]; backoff && ok && a.now().Before(r.at) {
			continue
		}
		sort.Strings(keys)
		found, err := a.addKeyValues(ctx, metric, keys, pending)
		if err != nil {
			if backoff {
				r := a.skuRetry[retryKey]
				r.delay = min(max(2*r.delay, skuRetryMin), skuRetryMax)
				r.at = a.now().Add(r.delay)
				a.skuRetry[retryKey] = r
				log.Printf("[lago] filter values for %s not added, retrying in %s (usage is still delivered): %v", metric, r.delay, err)
			}
			errs = append(errs, err)
			continue
		}
		delete(a.skuRetry, retryKey)
		if !found {
			continue // added once the billable metric exists
		}
		for k, vs := range pending {
			for _, v := range vs {
				if err := a.state.Set(keyValueKey(metric, k, v), "1"); err != nil {
					log.Printf("[lago] save state: %v", err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// addKeyValues adds values to the filters of one billable metric, keeping
// every other key and value it has. found is false when Lago has no such
// billable metric (the price check reports it). a.planMu held.
func (a *Adapter) addKeyValues(ctx context.Context, metric string, keys []string, values map[string][]string) (found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, skuSyncTimeout)
	defer cancel()
	bm, err := a.client.GetBillableMetric(ctx, metric)
	if isNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get billable metric %s: %w", metric, err)
	}
	filters := bm.Filters
	var added []string
	for _, k := range keys {
		var more []string
		filters, more = withFilterValues(filters, k, values[k])
		for _, v := range more {
			added = append(added, k+"="+v)
		}
	}
	if len(added) == 0 {
		a.noteFilterDims(bm)
		return true, nil
	}
	if _, err := a.client.UpdateBillableMetricFilters(ctx, metric, filters); err != nil {
		return false, fmt.Errorf("billable metric %s: add filter values %s: %w", metric, strings.Join(added, ", "), err)
	}
	bm.Filters = filters
	a.noteFilterDims(bm)
	log.Printf("[lago] billable metric %s: added filter values %s (no price set)", metric, strings.Join(added, ", "))
	return true, nil
}

// coversSKU reports whether an existing filter already prices sku, alone or
// with other values or keys. vBilling never adds a filter that overlaps one.
func coversSKU(filters []ChargeFilter, sku string) bool {
	for _, f := range filters {
		for _, v := range f.Values["sku"] {
			if v == sku || v == allFilterValues {
				return true
			}
		}
	}
	return false
}

// filterProperties returns a new SKU filter's properties: the charge's own
// (amount and grouping), with the price card's amount for the SKU if it has
// one. Without one the filter keeps the charge's amount, so splitting a SKU
// out never changes what its usage costs.
func (a *Adapter) filterProperties(metric, sku string, charge json.RawMessage) (json.RawMessage, error) {
	props := map[string]json.RawMessage{}
	if len(charge) > 0 && string(charge) != "null" {
		if err := json.Unmarshal(charge, &props); err != nil {
			return nil, fmt.Errorf("read charge properties: %w", err)
		}
	}
	if amount, ok := a.priceCard.skuAmount(metric, sku); ok {
		props["amount"], _ = json.Marshal(amount)
	}
	if _, ok := props["amount"]; !ok {
		props["amount"] = json.RawMessage(`"0"`)
	}
	return json.Marshal(props)
}
