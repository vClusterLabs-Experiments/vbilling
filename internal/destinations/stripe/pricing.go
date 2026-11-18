package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage reads meters and prices; it never creates or changes a
// price. Endpoints: GET /v1/billing/meters, GET /v1/prices
// (active=true, type=recurring, expand[]=data.tiers).

// ListPricesWithTiers lists active recurring prices with their tiers.
func (c *Client) ListPricesWithTiers(ctx context.Context) ([]Price, error) {
	params := url.Values{}
	params.Set("active", "true")
	params.Set("type", "recurring")
	params.Set("expand[]", "data.tiers")
	return listAll[Price](ctx, c, "/v1/prices", params)
}

// PricePlan is the vbilling_plan tag a tenant's subscription prices carry
// when vBilling manages subscriptions (STRIPE_AUTO_SUBSCRIBE), "" otherwise.
// Subscriptions only take prices in the tenant's currency, so a tenant whose
// currency is not billing.currency is on its own series: "standard (EUR)".
func (a *Adapter) PricePlan(t usage.Tenant) string {
	if !a.cfg.StripeAutoSubscribe {
		return ""
	}
	plan := t.Plan
	if plan == "" {
		plan = a.cfg.DefaultPlanCode
	}
	if t.Currency != "" && !strings.EqualFold(t.Currency, a.cfg.BillingCurrency) {
		return plan + " (" + strings.ToUpper(t.Currency) + ")"
	}
	return plan
}

var planCurrencyRe = regexp.MustCompile(`^(.*) \(([A-Z]{3})\)$`)

// planCurrency splits a PricePlan value into the vbilling_plan tag and the
// currency a subscription on it is in.
func (a *Adapter) planCurrency(s string) (plan, currency string) {
	if m := planCurrencyRe.FindStringSubmatch(s); m != nil {
		return m[1], strings.ToLower(m[2])
	}
	return s, strings.ToLower(a.cfg.BillingCurrency)
}

// PriceDims are the meter split keys besides the SKU: each combination of
// their values is its own meter, so its own price.
func (a *Adapter) PriceDims(metric string) []string {
	var out []string
	for _, k := range a.cfg.StripeSplitMetersBy {
		if k != "sku" {
			out = append(out, k)
		}
	}
	return out
}

// RefreshTenantsOnPriceChange asks vBilling to ensure tenants again when a
// series becomes priced: with STRIPE_AUTO_SUBSCRIBE, EnsureTenant adds the
// new price to existing subscriptions, without which Stripe bills nothing.
func (a *Adapter) RefreshTenantsOnPriceChange() bool { return a.cfg.StripeAutoSubscribe }

// PrepareSeries creates the meter of each new series, so Finance can attach
// a price to it before its first event is delivered.
func (a *Adapter) PrepareSeries(ctx context.Context, series []destinations.PriceSeries) error {
	var errs []error
	for _, s := range series {
		if s.Catalog || !destinations.BillableOnly(s.Metric) {
			continue
		}
		ev := s.EventFor()
		name := a.MeterName(&ev)
		if _, err := a.ensureMeter(ctx, name, meterDisplayName(&ev, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CheckPrices reports, for each series, whether its meter exists and has an
// active metered price (tagged with the tenant's plan under
// STRIPE_AUTO_SUBSCRIBE).
func (a *Adapter) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	meters, err := a.c.ListMeters(ctx)
	if err != nil {
		return nil, fmt.Errorf("list meters: %w", err)
	}
	prices, err := a.c.ListPricesWithTiers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list prices: %w", err)
	}
	// Fresh lists also refresh what subscriptions are built from.
	a.meterMu.Lock()
	for _, m := range meters {
		a.meters[m.EventName] = m
	}
	a.metersLoaded = true
	a.meterMu.Unlock()
	a.priceMu.Lock()
	a.prices, a.pricesTime = prices, a.now()
	a.priceMu.Unlock()

	byName := map[string]Meter{}
	for _, m := range meters {
		byName[m.EventName] = m
	}
	byMeter := map[string][]Price{}
	for _, p := range prices {
		if p.Active && p.Recurring != nil && p.Recurring.UsageType == "metered" && p.Recurring.Meter != "" {
			byMeter[p.Recurring.Meter] = append(byMeter[p.Recurring.Meter], p)
		}
	}

	out := make([]destinations.PriceCoverage, 0, len(series))
	for _, s := range series {
		var cov destinations.PriceCoverage
		if s.Catalog {
			cov = a.catalogCoverage(s, meters, byMeter)
		} else {
			cov = a.seriesCoverage(s, byName, byMeter)
		}
		cov.Series = s
		out = append(out, cov)
	}
	return out, nil
}

func (a *Adapter) seriesCoverage(s destinations.PriceSeries, byName map[string]Meter, byMeter map[string][]Price) destinations.PriceCoverage {
	ev := s.EventFor()
	name := a.MeterName(&ev)
	m, ok := byName[name]
	if !ok {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("no Stripe meter %q yet", name),
			Hint:   fmt.Sprintf("vBilling creates meter %q when the series first appears; then create a metered price on it", name),
		}
	}
	if m.Status == "inactive" {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("meter %q (%s) is deactivated", name, m.ID),
			Hint:   fmt.Sprintf("reactivate meter %q and give it an active metered price", name),
		}
	}
	return a.pricesCoverage(s, fmt.Sprintf("meter %q (%s)", name, m.ID), byMeter[m.ID])
}

// catalogCoverage answers for a metric with no usage yet: does any meter of
// the metric (the base meter or a split one) have a price?
func (a *Adapter) catalogCoverage(s destinations.PriceSeries, meters []Meter, byMeter map[string][]Price) destinations.PriceCoverage {
	var ps []Price
	for _, m := range meters {
		if m.Status != "inactive" && (m.EventName == s.Metric || strings.HasPrefix(m.EventName, s.Metric+"__")) {
			ps = append(ps, byMeter[m.ID]...)
		}
	}
	return a.pricesCoverage(s, fmt.Sprintf("the meters of %s", s.Metric), ps)
}

func (a *Adapter) pricesCoverage(s destinations.PriceSeries, where string, ps []Price) destinations.PriceCoverage {
	if len(ps) == 0 {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: "no active metered price on " + where,
			Hint:   "create a metered price on " + where + a.planTagHint(s.Plan),
		}
	}
	if a.cfg.StripeAutoSubscribe {
		// The same rules as planPrices, which builds the subscriptions.
		plan, cur := a.planCurrency(s.Plan)
		var tagged, inCur []Price
		for _, p := range ps {
			if p.Metadata[metaPlan] == plan {
				tagged = append(tagged, p)
				if strings.EqualFold(p.Currency, cur) {
					inCur = append(inCur, p)
				}
			}
		}
		if len(tagged) == 0 {
			return destinations.PriceCoverage{
				Status: destinations.PriceMissing,
				Detail: fmt.Sprintf("%s has %d active price(s), none tagged %s=%s, so subscriptions on plan %s do not include it", where, len(ps), metaPlan, plan, plan),
				Hint:   fmt.Sprintf("add metadata %s=%s to a metered price on %s", metaPlan, plan, where),
			}
		}
		if len(inCur) == 0 {
			return destinations.PriceCoverage{
				Status: destinations.PriceMissing,
				Detail: fmt.Sprintf("%s has %d price(s) tagged %s=%s, none in %s, so %s subscriptions on plan %s do not include it",
					where, len(tagged), metaPlan, plan, strings.ToUpper(cur), strings.ToUpper(cur), plan),
				Hint: fmt.Sprintf("create a metered price in %s on %s with metadata %s=%s", strings.ToUpper(cur), where, metaPlan, plan),
			}
		}
		ps = inCur
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	note := ""
	if !a.cfg.StripeAutoSubscribe {
		note = "; customers must be subscribed to it (STRIPE_AUTO_SUBSCRIBE is off)"
	}
	for _, p := range ps {
		if !priceIsZero(p) {
			return destinations.PriceCoverage{Status: destinations.PriceCovered,
				Detail: fmt.Sprintf("price %s (%s) on %s%s", p.ID, describePrice(p), where, note)}
		}
	}
	return destinations.PriceCoverage{Status: destinations.PriceZero,
		Detail: fmt.Sprintf("every active price on %s is 0 (%s)%s", where, ps[0].ID, note)}
}

func (a *Adapter) planTagHint(s string) string {
	if !a.cfg.StripeAutoSubscribe {
		return ""
	}
	plan, cur := a.planCurrency(s)
	return fmt.Sprintf(" in %s with metadata %s=%s", strings.ToUpper(cur), metaPlan, plan)
}

func amountIsZero(n *int64, dec *string) bool {
	if dec != nil && *dec != "" {
		f, err := strconv.ParseFloat(*dec, 64)
		return err == nil && f == 0
	}
	return n == nil || *n == 0
}

// priceIsZero reports a price that bills nothing: a per-unit amount of 0,
// or tiers whose unit and flat amounts are all 0.
func priceIsZero(p Price) bool {
	if p.BillingScheme == "tiered" {
		if len(p.Tiers) == 0 {
			return false // tiers not expanded: assume they price something
		}
		for _, t := range p.Tiers {
			if !amountIsZero(t.UnitAmount, t.UnitAmountDecimal) || !amountIsZero(t.FlatAmount, t.FlatAmountDecimal) {
				return false
			}
		}
		return true
	}
	return amountIsZero(p.UnitAmount, p.UnitAmountDecimal)
}

func describePrice(p Price) string {
	cur := strings.ToUpper(p.Currency)
	if p.BillingScheme == "tiered" {
		return fmt.Sprintf("tiered, %d tiers, %s", len(p.Tiers), cur)
	}
	switch {
	case p.UnitAmountDecimal != nil && *p.UnitAmountDecimal != "":
		return fmt.Sprintf("%s minor units of %s per unit", *p.UnitAmountDecimal, cur)
	case p.UnitAmount != nil:
		return fmt.Sprintf("%d minor units of %s per unit", *p.UnitAmount, cur)
	}
	return cur
}

var (
	_ destinations.PriceChecker   = (*Adapter)(nil)
	_ destinations.SeriesPreparer = (*Adapter)(nil)
)
