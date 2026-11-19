package metronome

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage reads Metronome's products and rate cards; it never writes
// them. Endpoints (docs.metronome.com/api-reference):
//   - GET  /v1/billable-metrics                        (billable-metrics/list-all-billable-metrics)
//   - POST /v1/contract-pricing/products/list          (products/list-products)
//   - POST /v1/contract-pricing/rate-cards/list        (rate-cards/list-rate-cards)
//   - POST /v1/contract-pricing/rate-cards/getRates    (rate-cards/get-rates)

// Product is the part of a contract-pricing product the check reads.
type Product struct {
	ID         string       `json:"id"`
	Type       string       `json:"type"` // USAGE, SUBSCRIPTION, COMPOSITE, FIXED, PRO_SERVICE
	ArchivedAt string       `json:"archived_at"`
	Current    ProductState `json:"current"`
}

// ProductState is a product's current definition.
type ProductState struct {
	Name                 string   `json:"name"`
	StartingAt           string   `json:"starting_at"`
	BillableMetricID     string   `json:"billable_metric_id"`
	PricingGroupKey      []string `json:"pricing_group_key"`
	PresentationGroupKey []string `json:"presentation_group_key"`
}

// RateCard is the part of a rate card the check reads.
type RateCard struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Aliases []RateCardAlias `json:"aliases"`
}

// RateCardAlias is a (possibly scheduled) alias of a rate card.
type RateCardAlias struct {
	Name         string `json:"name"`
	StartingAt   string `json:"starting_at"`
	EndingBefore string `json:"ending_before"`
}

// RateSchedule is one rate of a rate card, as getRates returns it.
type RateSchedule struct {
	ProductID          string            `json:"product_id"`
	ProductName        string            `json:"product_name"`
	PricingGroupValues map[string]string `json:"pricing_group_values"`
	StartingAt         string            `json:"starting_at"`
	EndingBefore       string            `json:"ending_before"`
	Entitled           bool              `json:"entitled"`
	Rate               Rate              `json:"rate"`
}

// Rate is a rate's price.
type Rate struct {
	RateType string   `json:"rate_type"` // FLAT, PERCENTAGE, SUBSCRIPTION, CUSTOM, TIERED, TIERED_PERCENTAGE
	Price    *float64 `json:"price"`
	Tiers    []Tier   `json:"tiers"`
}

// Tier is one tier of a tiered rate.
type Tier struct {
	Size  *float64 `json:"size,omitempty"`
	Price float64  `json:"price"`
}

// RateSelector narrows getRates. vBilling selects by product only.
type RateSelector struct {
	ProductID string `json:"product_id,omitempty"`
}

type getRatesRequest struct {
	RateCardID string         `json:"rate_card_id"`
	At         string         `json:"at"`
	Selectors  []RateSelector `json:"selectors,omitempty"`
}

type listProductsRequest struct {
	ArchiveFilter string `json:"archive_filter"`
}

// ListProducts pages through the products that are not archived.
func (c *Client) ListProducts(ctx context.Context) ([]Product, error) {
	var out []Product
	q := url.Values{}
	q.Set("limit", "100")
	for page := 0; page < 100; page++ {
		var resp struct {
			Data     []Product `json:"data"`
			NextPage *string   `json:"next_page"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/products/list", q, listProductsRequest{ArchiveFilter: "NOT_ARCHIVED"}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.NextPage == nil || *resp.NextPage == "" {
			break
		}
		q.Set("next_page", *resp.NextPage)
	}
	return out, nil
}

// ListRateCards pages through the rate cards.
func (c *Client) ListRateCards(ctx context.Context) ([]RateCard, error) {
	var out []RateCard
	q := url.Values{}
	q.Set("limit", "100")
	for page := 0; page < 100; page++ {
		var resp struct {
			Data     []RateCard `json:"data"`
			NextPage *string    `json:"next_page"`
		}
		// The body is an empty object: the endpoint takes no filters.
		if err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/list", q, struct{}{}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.NextPage == nil || *resp.NextPage == "" {
			break
		}
		q.Set("next_page", *resp.NextPage)
	}
	return out, nil
}

// GetRates pages through the rates of a rate card in effect at at, for the
// given products (all products when none are given).
func (c *Client) GetRates(ctx context.Context, rateCardID string, at time.Time, products []string) ([]RateSchedule, error) {
	req := getRatesRequest{RateCardID: rateCardID, At: rfc3339(at)}
	for _, p := range products {
		req.Selectors = append(req.Selectors, RateSelector{ProductID: p})
	}
	var out []RateSchedule
	q := url.Values{}
	q.Set("limit", "100")
	for page := 0; page < 500; page++ {
		var resp struct {
			Data     []RateSchedule `json:"data"`
			NextPage *string        `json:"next_page"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/contract-pricing/rate-cards/getRates", q, req, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.NextPage == nil || *resp.NextPage == "" {
			break
		}
		q.Set("next_page", *resp.NextPage)
	}
	return out, nil
}

// PricePlan is the rate card a tenant's contract uses: the tenant's plan, or
// METRONOME_RATE_CARD (the same choice EnsureTenant makes).
func (a *Adapter) PricePlan(t usage.Tenant) string {
	if t.Plan != "" {
		return t.Plan
	}
	return a.cfg.MetronomeRateCard
}

// PriceDims are the group keys of the metric's billable metric, except the
// SKU (always checked) and tenant_cluster (presentation only). A product can
// price on any of them through its pricing_group_key.
func (a *Adapter) PriceDims(metric string) []string {
	def, ok := usage.Lookup(metric)
	if !ok {
		return nil
	}
	var out []string
	for _, k := range GroupKeys(def) {
		if k != "sku" && k != usage.DimTenantCluster {
			out = append(out, k)
		}
	}
	return out
}

// pricingView is what one check read from Metronome.
type pricingView struct {
	now       time.Time
	metricID  map[string]string    // metric code -> active billable metric ID
	metricErr error                // listing billable metrics failed
	products  map[string][]Product // billable metric ID -> usage products
	cards     []RateCard
	cardsRead bool
	rates     map[string][]RateSchedule // rate card ID -> rates of the candidate products
}

// CheckPrices reports, for each series, whether a product on the rate card
// prices it: the metric's billable metric exists, a usage product meters it,
// and the rate card has an entitled rate for that product whose pricing
// group values match the series (SKU and any other pricing group key).
func (a *Adapter) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	v := &pricingView{now: a.now().UTC(), metricID: map[string]string{}, products: map[string][]Product{}, rates: map[string][]RateSchedule{}}

	bms, err := a.c.ListBillableMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("list billable metrics: %w", err)
	}
	active, byName := map[string]bool{}, map[string]string{}
	for _, m := range bms {
		if m.ArchivedAt == "" {
			active[m.ID] = true
			byName[m.Name] = m.ID
		}
	}
	for _, s := range series {
		if _, done := v.metricID[s.Metric]; done {
			continue
		}
		def, ok := usage.Lookup(s.Metric)
		if !ok {
			continue
		}
		if id, ok := a.state.Get("bm/" + s.Metric); ok && active[id] {
			v.metricID[s.Metric] = id
		} else if id, ok := byName[metricName(def)]; ok {
			v.metricID[s.Metric] = id
		}
	}

	products, err := a.c.ListProducts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	for _, p := range products {
		if p.ArchivedAt != "" || !strings.EqualFold(p.Type, "USAGE") || p.Current.BillableMetricID == "" {
			continue
		}
		v.products[p.Current.BillableMetricID] = append(v.products[p.Current.BillableMetricID], p)
	}

	// Rates, once per rate card, for the products of the metrics checked.
	cardProducts := map[string]map[string]bool{} // plan -> product IDs
	for _, s := range series {
		if s.Plan == "" {
			continue
		}
		if cardProducts[s.Plan] == nil {
			cardProducts[s.Plan] = map[string]bool{}
		}
		for _, p := range v.products[v.metricID[s.Metric]] {
			cardProducts[s.Plan][p.ID] = true
		}
	}
	cardID := map[string]string{} // plan -> rate card ID ("" if not found)
	plans := make([]string, 0, len(cardProducts))
	for p := range cardProducts {
		plans = append(plans, p)
	}
	sort.Strings(plans)
	for _, plan := range plans {
		id, err := a.resolveRateCard(ctx, v, plan)
		if err != nil {
			return nil, err
		}
		cardID[plan] = id
		if id == "" || len(cardProducts[plan]) == 0 {
			continue
		}
		if _, done := v.rates[id]; done {
			continue
		}
		ids := make([]string, 0, len(cardProducts[plan]))
		for p := range cardProducts[plan] {
			ids = append(ids, p)
		}
		sort.Strings(ids)
		rates, err := a.c.GetRates(ctx, id, v.now, ids)
		if err != nil {
			return nil, fmt.Errorf("rates of rate card %s: %w", plan, err)
		}
		v.rates[id] = rates
	}

	out := make([]destinations.PriceCoverage, 0, len(series))
	for _, s := range series {
		cov := a.coverage(v, s, cardID[s.Plan])
		cov.Series = s
		out = append(out, cov)
	}
	return out, nil
}

// resolveRateCard maps a plan (rate card ID or alias) to a rate card ID, ""
// when no rate card has that alias now.
func (a *Adapter) resolveRateCard(ctx context.Context, v *pricingView, plan string) (string, error) {
	if uuidRe.MatchString(plan) {
		return plan, nil
	}
	if !v.cardsRead {
		cards, err := a.c.ListRateCards(ctx)
		if err != nil {
			return "", fmt.Errorf("list rate cards: %w", err)
		}
		v.cards, v.cardsRead = cards, true
	}
	for _, rc := range v.cards {
		for _, al := range rc.Aliases {
			if al.Name != plan {
				continue
			}
			if al.StartingAt != "" && parseTime(al.StartingAt).After(v.now) {
				continue
			}
			if al.EndingBefore != "" && !parseTime(al.EndingBefore).After(v.now) {
				continue
			}
			return rc.ID, nil
		}
	}
	return "", nil
}

func (a *Adapter) coverage(v *pricingView, s destinations.PriceSeries, rcID string) destinations.PriceCoverage {
	def, ok := usage.Lookup(s.Metric)
	if !ok {
		return destinations.PriceCoverage{Status: destinations.PriceUnknown, Detail: "metric is not in vBilling's catalog"}
	}
	bmID := v.metricID[s.Metric]
	if bmID == "" {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("Metronome has no billable metric %q", metricName(def)),
			Hint:   "vBilling creates billable metrics when the metronome destination bootstraps: check its last error in GET /api/v1/destinations",
		}
	}
	prods := v.products[bmID]
	if len(prods) == 0 {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("no usage product meters billable metric %q (%s)", metricName(def), bmID),
			Hint:   fmt.Sprintf("create a usage product on billable metric %q and add its rate to rate card %s", metricName(def), planLabel(s.Plan)),
		}
	}
	names := productNames(prods)
	if s.Plan == "" {
		return destinations.PriceCoverage{
			Status: destinations.PriceUnknown,
			Detail: fmt.Sprintf("product %s meters it, but no rate card is configured (METRONOME_RATE_CARD or the tenant's plan), so vBilling cannot tell which rates apply", names),
		}
	}
	if rcID == "" {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("no Metronome rate card has the ID or alias %q", s.Plan),
			Hint:   fmt.Sprintf("create rate card %q in Metronome, or fix METRONOME_RATE_CARD or the tenant's plan", s.Plan),
		}
	}
	rates := v.rates[rcID]

	if s.Catalog {
		var found []RateSchedule
		for _, p := range prods {
			for _, r := range rates {
				if r.ProductID == p.ID && r.Entitled && rateActive(r, v.now) {
					found = append(found, r)
				}
			}
		}
		if len(found) == 0 {
			return destinations.PriceCoverage{
				Status: destinations.PriceMissing,
				Detail: fmt.Sprintf("rate card %s has no entitled rate for product %s", planLabel(s.Plan), names),
				Hint:   fmt.Sprintf("add a rate for product %s to rate card %s", names, planLabel(s.Plan)),
			}
		}
		for _, r := range found {
			if !rateIsZero(r.Rate) {
				return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: fmt.Sprintf("product %q has %d rate(s) on rate card %s", r.ProductName, len(found), planLabel(s.Plan))}
			}
		}
		return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: fmt.Sprintf("every rate for product %s on rate card %s is 0", names, planLabel(s.Plan))}
	}

	var notEntitled string
	var wanted []string
	for _, p := range prods {
		need := map[string]string{}
		for _, k := range p.Current.PricingGroupKey {
			if k == usage.DimTenantCluster {
				continue // per-cluster pricing cannot be checked per series
			}
			val := s.SKU
			if k != "sku" {
				val = s.Dims[k]
			}
			if val == "" {
				val = placeholder // what ToIngest sends for a missing group key
			}
			need[k] = val
		}
		for _, r := range rates {
			if r.ProductID != p.ID || !rateActive(r, v.now) || !matchesGroup(r.PricingGroupValues, need) {
				continue
			}
			if !r.Entitled {
				notEntitled = p.Current.Name
				continue
			}
			label := describeRate(r.Rate)
			if rateIsZero(r.Rate) {
				return destinations.PriceCoverage{Status: destinations.PriceZero,
					Detail: fmt.Sprintf("product %q is priced at 0 on rate card %s%s", p.Current.Name, planLabel(s.Plan), groupLabel(need))}
			}
			return destinations.PriceCoverage{Status: destinations.PriceCovered,
				Detail: fmt.Sprintf("product %q: %s on rate card %s%s", p.Current.Name, label, planLabel(s.Plan), groupLabel(need))}
		}
		wanted = append(wanted, fmt.Sprintf("product %q%s", p.Current.Name, groupLabel(need)))
	}
	if notEntitled != "" {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("the rate for product %q on rate card %s is not entitled", notEntitled, planLabel(s.Plan)),
			Hint:   fmt.Sprintf("set entitled on the rate for product %q on rate card %s", notEntitled, planLabel(s.Plan)),
		}
	}
	return destinations.PriceCoverage{
		Status: destinations.PriceMissing,
		Detail: fmt.Sprintf("rate card %s has no rate for %s", planLabel(s.Plan), strings.Join(wanted, " or ")),
		Hint:   fmt.Sprintf("add a rate for %s to rate card %s", wanted[0], planLabel(s.Plan)),
	}
}

func planLabel(plan string) string {
	if plan == "" {
		return "(none configured)"
	}
	return strconv.Quote(plan)
}

func productNames(ps []Product) string {
	names := make([]string, 0, len(ps))
	for _, p := range ps {
		names = append(names, strconv.Quote(p.Current.Name))
	}
	return strings.Join(names, ", ")
}

func groupLabel(need map[string]string) string {
	if len(need) == 0 {
		return ""
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + need[k]
	}
	return " (pricing group " + strings.Join(parts, ", ") + ")"
}

func rateActive(r RateSchedule, at time.Time) bool {
	if r.StartingAt != "" && parseTime(r.StartingAt).After(at) {
		return false
	}
	return r.EndingBefore == "" || parseTime(r.EndingBefore).After(at)
}

// matchesGroup reports whether a rate's pricing group values carry every
// needed key with the needed value.
func matchesGroup(have, need map[string]string) bool {
	for k, v := range need {
		if have[k] != v {
			return false
		}
	}
	return true
}

func rateIsZero(r Rate) bool {
	switch strings.ToUpper(r.RateType) {
	case "FLAT", "PERCENTAGE":
		return r.Price == nil || *r.Price == 0
	case "TIERED", "TIERED_PERCENTAGE":
		for _, t := range r.Tiers {
			if t.Price != 0 {
				return false
			}
		}
		return true
	default: // SUBSCRIPTION, CUSTOM: priced by other means
		return false
	}
}

func describeRate(r Rate) string {
	t := strings.ToUpper(r.RateType)
	switch {
	case (t == "FLAT" || t == "PERCENTAGE") && r.Price != nil:
		return fmt.Sprintf("%s %s", strings.ToLower(t), strconv.FormatFloat(*r.Price, 'f', -1, 64))
	case t == "TIERED" || t == "TIERED_PERCENTAGE":
		return fmt.Sprintf("%s (%d tiers)", strings.ToLower(t), len(r.Tiers))
	default:
		return strings.ToLower(t)
	}
}

// summing returns the active billable metrics that sum the value property of
// an event type: the ones that bill vBilling's usage of a metric.
func summing(bms []BillableMetric, code string) []BillableMetric {
	var out []BillableMetric
	for _, m := range bms {
		if m.ArchivedAt == "" && m.SQL == "" && strings.EqualFold(m.AggregationType, "SUM") && m.AggregationKey == "value" && countsType(m, code) {
			out = append(out, m)
		}
	}
	return out
}

func countsType(m BillableMetric, code string) bool {
	f := m.EventTypeFilter
	return f != nil && slices.Contains(f.InValues, code) && !slices.Contains(f.NotInValues, code)
}

// completeMetric returns a billable metric that sums the value of every
// event of a metric and nothing else: its event type filter names only the
// metric, and its property filters require no more than the properties every
// such event carries. Reconciliation compares its total with the ledger.
// vBilling's own definition (BillableMetricFor) is one, and preferred.
func completeMetric(bms []BillableMetric, def usage.MetricDef) *BillableMetric {
	always := map[string]bool{"value": true, "unit": true, "window_start": true, "window_end": true, "schema_version": true}
	for _, k := range GroupKeys(def) {
		always[k] = true
	}
	var found []BillableMetric
	for _, m := range summing(bms, def.Code) {
		if len(m.EventTypeFilter.InValues) != 1 {
			continue
		}
		ok := true
		for _, f := range m.PropertyFilters {
			if len(f.InValues) > 0 || len(f.NotInValues) > 0 || (f.Exists != nil && !(*f.Exists && always[f.Name])) {
				ok = false
				break
			}
		}
		if ok {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool {
		if (found[i].Name == metricName(def)) != (found[j].Name == metricName(def)) {
			return found[i].Name == metricName(def)
		}
		return found[i].ID < found[j].ID
	})
	return &found[0]
}

var (
	_ destinations.PriceChecker = (*Adapter)(nil)
)
