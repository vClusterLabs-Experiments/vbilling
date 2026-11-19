package metronome

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Price coverage reads Metronome's catalog, customers and contracts; it never
// writes them. Endpoints (docs.metronome.com/api-reference):
//   - GET  /v1/billable-metrics                        (billable-metrics/list-all-billable-metrics)
//   - POST /v1/contract-pricing/products/list          (products/list-products)
//   - POST /v1/contract-pricing/rate-cards/list        (rate-cards/list-rate-cards)
//   - POST /v1/contract-pricing/rate-cards/getRates    (rate-cards/get-rates)
//
// Without METRONOME_PROVISION it also reads, per tenant:
//   - GET  /v1/customers?ingest_alias=<tenant>         (customers/list-customers)
//   - POST /v2/contracts/list with covering_date       (contracts/list-customer-contracts-v2)
//
// The rate cards are then the ones of the tenant's own customer's active
// contracts, and a metric's billable metrics are found by their event type
// filter and their SUM of value, whatever Finance named them.

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

// Plans without METRONOME_PROVISION. A tenant's usage is rated on the rate
// cards of the active contracts of the customer that holds the tenant ID as
// ingest alias. Until a check has read them (or while there is no such
// customer or contract) the tenant's plan is aliasPlan + tenant ID; then it
// is the rate card IDs, joined with "+", so tenants on the same rate card
// share their series.
const (
	aliasPlan   = "alias:"
	planRefresh = 5 * time.Minute // a tenant's contracts are read again after this
	planForget  = 25 * time.Hour  // a tenant no check asked about for this long is dropped
	planWorkers = 8               // concurrent customer and contract reads
)

// tenantPlan is what a check found for one tenant.
type tenantPlan struct {
	customer string   // the customer holding the tenant's alias, "" if none
	cards    []string // rate card IDs of its active contracts, sorted
	plan     string   // PricePlan's answer
	why      string   // without cards: why (no customer, no active contract)
	checked  time.Time
	used     time.Time // last PricePlan call
}

// PricePlan is the plan a tenant's usage is rated on. With
// METRONOME_PROVISION it is the rate card of the contract vBilling creates:
// the tenant's plan, or METRONOME_RATE_CARD. Without it, it is the rate cards
// of the tenant's own customer's active contracts, as the last check found
// them (aliasPlan + tenant ID until one has).
func (a *Adapter) PricePlan(t usage.Tenant) string {
	if a.provision() {
		if t.Plan != "" {
			return t.Plan
		}
		return a.cfg.MetronomeRateCard
	}
	if t.ID == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.plans[t.ID]
	if p == nil {
		p = &tenantPlan{plan: aliasPlan + t.ID}
		a.plans[t.ID] = p
	}
	p.used = a.now()
	return p.plan
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
	metricID  map[string]string           // provisioning: metric code -> billable metric ID
	byType    map[string][]BillableMetric // default: metric code -> billable metrics summing value on its event type
	other     map[string][]BillableMetric // default: billable metrics on the event type that do not sum value
	products  map[string][]Product        // billable metric ID -> usage products
	cards     []RateCard
	cardsRead bool
	rates     map[string][]RateSchedule // rate card ID -> rates of the candidate products
	tenants   map[string]tenantPlan     // default: tenants resolved by this check
}

// target is where a series is rated.
type target struct {
	cards   []string // rate card IDs
	missing bool     // provisioning: the configured rate card does not exist
	why     string   // default: why the tenant has no rate card yet
}

// CheckPrices reports, for each series, whether Metronome prices it: a
// billable metric counts it, a usage product meters that billable metric, and
// the rate card the usage is rated on has an entitled rate for the product
// whose pricing group values match the series (SKU and any other pricing
// group key).
func (a *Adapter) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	v := &pricingView{now: a.now().UTC(), metricID: map[string]string{}, byType: map[string][]BillableMetric{},
		other: map[string][]BillableMetric{}, products: map[string][]Product{}, rates: map[string][]RateSchedule{},
		tenants: map[string]tenantPlan{}}

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
	seen := map[string]bool{}
	for _, s := range series {
		if seen[s.Metric] {
			continue
		}
		seen[s.Metric] = true
		def, ok := usage.Lookup(s.Metric)
		if !ok {
			continue
		}
		if !a.provision() {
			v.byType[s.Metric] = summing(bms, s.Metric)
			v.other[s.Metric] = notSumming(bms, s.Metric)
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

	// Where each series is rated.
	targets := make([]target, len(series))
	if a.provision() {
		cardID := map[string]string{} // plan -> rate card ID ("" if not found)
		var plans []string
		for _, s := range series {
			if _, ok := cardID[s.Plan]; !ok && s.Plan != "" {
				cardID[s.Plan] = ""
				plans = append(plans, s.Plan)
			}
		}
		sort.Strings(plans)
		for _, plan := range plans {
			if cardID[plan], err = a.resolveRateCard(ctx, v, plan); err != nil {
				return nil, err
			}
		}
		for i, s := range series {
			switch id := cardID[s.Plan]; {
			case s.Plan == "":
			case id == "":
				targets[i].missing = true
			default:
				targets[i].cards = []string{id}
			}
		}
	} else {
		if err := a.resolvePlans(ctx, v, series); err != nil {
			return nil, err
		}
		named := false
		for i, s := range series {
			targets[i] = v.target(s)
			named = named || len(targets[i].cards) > 0
		}
		if named && !v.cardsRead {
			if v.cards, err = a.c.ListRateCards(ctx); err != nil {
				return nil, fmt.Errorf("list rate cards: %w", err)
			}
			v.cardsRead = true
		}
	}

	// Rates, once per rate card, for the products of the series rated on it.
	cardProducts := map[string]map[string]bool{}
	for i, s := range series {
		for _, rc := range targets[i].cards {
			if cardProducts[rc] == nil {
				cardProducts[rc] = map[string]bool{}
			}
			for _, p := range a.productsFor(v, s) {
				cardProducts[rc][p.ID] = true
			}
		}
	}
	cards := make([]string, 0, len(cardProducts))
	for rc := range cardProducts {
		cards = append(cards, rc)
	}
	sort.Strings(cards)
	for _, rc := range cards {
		if len(cardProducts[rc]) == 0 {
			continue
		}
		ids := make([]string, 0, len(cardProducts[rc]))
		for p := range cardProducts[rc] {
			ids = append(ids, p)
		}
		sort.Strings(ids)
		rates, err := a.c.GetRates(ctx, rc, v.now, ids)
		if err != nil {
			return nil, fmt.Errorf("rates of rate card %s: %w", a.cardLabel(v, rc, rc), err)
		}
		v.rates[rc] = rates
	}

	out := make([]destinations.PriceCoverage, 0, len(series))
	for i, s := range series {
		cov := a.coverage(v, s, targets[i])
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

// target returns where a series is rated, without METRONOME_PROVISION.
func (v *pricingView) target(s destinations.PriceSeries) target {
	switch {
	case s.Plan == "":
		return target{}
	case strings.HasPrefix(s.Plan, aliasPlan):
		tp := v.tenants[strings.TrimPrefix(s.Plan, aliasPlan)]
		if len(tp.cards) == 0 {
			return target{why: tp.why}
		}
		return target{cards: tp.cards}
	default:
		return target{cards: strings.Split(s.Plan, "+")}
	}
}

// resolvePlans reads, for the tenants this check answers for (their series'
// plan is still aliasPlan) and for those last read planRefresh ago, the
// customer that holds the tenant's alias and its active contracts. A failed
// read of a tenant this check answers for fails the check; a failed refresh
// keeps the last answer.
func (a *Adapter) resolvePlans(ctx context.Context, v *pricingView, series []destinations.PriceSeries) error {
	need := map[string]bool{}
	for _, s := range series {
		if t, ok := strings.CutPrefix(s.Plan, aliasPlan); ok {
			need[t] = true
		}
	}
	var refresh []string
	a.mu.Lock()
	for t, p := range a.plans {
		if v.now.Sub(p.used) > planForget {
			delete(a.plans, t)
			continue
		}
		if !need[t] && v.now.Sub(p.checked) >= planRefresh {
			refresh = append(refresh, t)
		}
	}
	a.mu.Unlock()
	tenants := make([]string, 0, len(need)+len(refresh))
	for t := range need {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	sort.Strings(refresh)
	tenants = append(tenants, refresh...)
	if len(tenants) == 0 {
		return nil
	}

	results := make([]tenantPlan, len(tenants))
	errs := make([]error, len(tenants))
	sem := make(chan struct{}, planWorkers)
	var wg sync.WaitGroup
	for i, t := range tenants {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], errs[i] = a.resolveTenant(ctx, t, v.now)
		}()
	}
	wg.Wait()

	a.mu.Lock()
	defer a.mu.Unlock()
	for i, t := range tenants {
		if errs[i] != nil {
			if need[t] {
				return fmt.Errorf("customer and contracts of tenant %s: %w", t, errs[i])
			}
			log.Printf("[metronome] could not read the contracts of tenant %s again (%v); keeping the last answer", t, errs[i])
			continue
		}
		r := results[i]
		r.checked = v.now
		if p := a.plans[t]; p != nil {
			r.used = p.used
		} else {
			r.used = v.now
		}
		a.plans[t] = &r
		v.tenants[t] = r
	}
	return nil
}

// resolveTenant finds the customer holding a tenant's alias and the rate
// cards of its contracts in force at at.
func (a *Adapter) resolveTenant(ctx context.Context, tenant string, at time.Time) (tenantPlan, error) {
	cus, err := a.c.FindCustomerByAlias(ctx, tenant)
	if err != nil {
		return tenantPlan{}, err
	}
	if cus == nil {
		return tenantPlan{plan: aliasPlan + tenant, why: fmt.Sprintf("no customer for this alias yet: no Metronome customer has the ingest alias %q. "+
			"Metronome keeps the usage and attributes it to the customer that gets the alias", tenant)}, nil
	}
	a.remember(tenant, cus.ID)
	contracts, err := a.c.ContractsCovering(ctx, cus.ID, at)
	if err != nil {
		return tenantPlan{}, err
	}
	var cards []string
	for _, c := range contracts {
		if c.ArchivedAt != "" || c.RateCardID == "" || parseTime(c.StartingAt).After(at) ||
			(c.EndingBefore != "" && !parseTime(c.EndingBefore).After(at)) || slices.Contains(cards, c.RateCardID) {
			continue
		}
		cards = append(cards, c.RateCardID)
	}
	sort.Strings(cards)
	if len(cards) == 0 {
		return tenantPlan{customer: cus.ID, plan: aliasPlan + tenant, why: fmt.Sprintf("customer %s (ingest alias %q) has no active contract with a rate card: "+
			"Metronome rates its usage once a contract covers it", cus.ID, tenant)}, nil
	}
	return tenantPlan{customer: cus.ID, cards: cards, plan: strings.Join(cards, "+")}, nil
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

// notSumming returns the active billable metrics on an event type that do
// not sum its value (a COUNT, MAX or LATEST): they cannot bill quantities.
func notSumming(bms []BillableMetric, code string) []BillableMetric {
	var out []BillableMetric
	for _, m := range bms {
		if m.ArchivedAt == "" && countsType(m, code) && !(strings.EqualFold(m.AggregationType, "SUM") && m.AggregationKey == "value") {
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

// prop is what vBilling sends for one property on every event of a series.
type prop struct {
	present, absent bool // known to be on, or missing from, every event
	value           string
	valueKnown      bool
}

// seriesProp returns what the events of a series carry for a property.
// Properties that vary within a series (resource_id, tenant_cluster, project,
// dimensions such as public_ip) are unknown.
func (a *Adapter) seriesProp(s destinations.PriceSeries, def usage.MetricDef, name string) prop {
	keys := GroupKeys(def)
	switch name {
	case "value", "unit", "window_start", "window_end":
		return prop{present: true}
	case "schema_version":
		return prop{present: true, value: usage.SchemaVersion, valueKnown: true}
	case "accuracy":
		if def.Accuracy == usage.AccuracyEstimate {
			return prop{present: true, value: usage.AccuracyEstimate, valueKnown: true}
		}
		return prop{}
	}
	if !s.Catalog && (name == "sku" || slices.Contains(a.PriceDims(s.Metric), name)) {
		v := s.SKU
		if name != "sku" {
			v = s.Dims[name]
		}
		switch {
		case v != "":
			return prop{present: true, value: v, valueKnown: true}
		case slices.Contains(keys, name):
			return prop{present: true, value: placeholder, valueKnown: true}
		default:
			return prop{absent: true}
		}
	}
	if slices.Contains(keys, name) {
		return prop{present: true} // vBilling sends every group key, with a placeholder if need be
	}
	return prop{}
}

// accepts reports whether a property filter can pass the events of a series.
// A filter on a property the series does not determine passes: some of its
// events may match.
func accepts(f PropertyFilter, p prop) bool {
	if f.Exists != nil && ((*f.Exists && p.absent) || (!*f.Exists && p.present)) {
		return false
	}
	if len(f.InValues) > 0 && (p.absent || (p.valueKnown && !slices.Contains(f.InValues, p.value))) {
		return false
	}
	if len(f.NotInValues) > 0 && p.valueKnown && slices.Contains(f.NotInValues, p.value) {
		return false
	}
	return true
}

// billableMetrics returns the IDs of the billable metrics that count a
// series' usage, or why there are none.
func (a *Adapter) billableMetrics(v *pricingView, s destinations.PriceSeries, def usage.MetricDef) ([]string, *destinations.PriceCoverage) {
	if a.provision() {
		id := v.metricID[s.Metric]
		if id == "" {
			return nil, &destinations.PriceCoverage{
				Status: destinations.PriceMissing,
				Detail: fmt.Sprintf("Metronome has no billable metric %q", metricName(def)),
				Hint:   "vBilling creates billable metrics when the metronome destination bootstraps: check its last error in GET /api/v1/destinations",
			}
		}
		return []string{id}, nil
	}
	typed := v.byType[s.Metric]
	if len(typed) == 0 {
		detail := fmt.Sprintf("no billable metric sums the value of event type %q", s.Metric)
		if others := v.other[s.Metric]; len(others) > 0 {
			detail += fmt.Sprintf(" (%s aggregates it with %s of %q, but vBilling's quantities must be summed)",
				strconv.Quote(others[0].Name), strings.ToUpper(others[0].AggregationType), others[0].AggregationKey)
		}
		detail += ": vBilling holds this usage until one does, because Metronome never bills usage sent before its billable metric exists"
		return nil, &destinations.PriceCoverage{Status: destinations.PriceMissing, Detail: detail, Hint: noMetricHint}
	}
	var ids, names []string
	for _, m := range typed {
		names = append(names, strconv.Quote(m.Name))
		ok := true
		for _, f := range m.PropertyFilters {
			if !accepts(f, a.seriesProp(s, def, f.Name)) {
				ok = false
				break
			}
		}
		if ok {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return nil, &destinations.PriceCoverage{Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("the property filters of billable metric %s exclude %s, so no billable metric counts it", strings.Join(names, ", "), destinations.DescribeSeries(s)),
			Hint:   fmt.Sprintf("widen the property filters of %s, or add a billable metric for this usage", names[0])}
	}
	return ids, nil
}

// productsFor returns the usage products on the billable metrics that count a series.
func (a *Adapter) productsFor(v *pricingView, s destinations.PriceSeries) []Product {
	def, ok := usage.Lookup(s.Metric)
	if !ok {
		return nil
	}
	ids, miss := a.billableMetrics(v, s, def)
	if miss != nil {
		return nil
	}
	var out []Product
	for _, id := range ids {
		out = append(out, v.products[id]...)
	}
	return out
}

// cardLabel names a rate card in messages: the plan in provisioning mode,
// the rate card's name otherwise.
func (a *Adapter) cardLabel(v *pricingView, plan, rcID string) string {
	if a.provision() {
		return planLabel(plan)
	}
	for _, rc := range v.cards {
		if rc.ID == rcID && rc.Name != "" {
			return strconv.Quote(rc.Name)
		}
	}
	return rcID
}

var coverageRank = map[string]int{destinations.PriceMissing: 0, destinations.PriceUnknown: 1, destinations.PriceZero: 2, destinations.PriceCovered: 3}

func (a *Adapter) coverage(v *pricingView, s destinations.PriceSeries, tg target) destinations.PriceCoverage {
	def, ok := usage.Lookup(s.Metric)
	if !ok {
		return destinations.PriceCoverage{Status: destinations.PriceUnknown, Detail: "metric is not in vBilling's catalog"}
	}
	if !a.provision() && len(v.byType[s.Metric]) == 0 {
		// No billable metric counts the event type: its usage waits, for
		// every tenant, customer or not.
		_, miss := a.billableMetrics(v, s, def)
		return *miss
	}
	if !a.provision() && strings.HasPrefix(s.Plan, aliasPlan) && len(tg.cards) == 0 {
		// No customer or no contract yet: a warning, never a hold. Metronome
		// keeps the usage and rates it once a customer and contract cover it.
		why := tg.why
		if why == "" {
			why = "the tenant's customer and contracts have not been read yet"
		}
		return destinations.PriceCoverage{Status: destinations.PriceUnknown, Detail: why}
	}
	bmIDs, miss := a.billableMetrics(v, s, def)
	if miss != nil {
		return *miss
	}
	prods := a.productsFor(v, s)
	if len(prods) == 0 {
		var labels, names []string
		for _, id := range bmIDs {
			name := metricName(def)
			if !a.provision() {
				for _, m := range v.byType[s.Metric] {
					if m.ID == id {
						name = m.Name
					}
				}
			}
			labels = append(labels, fmt.Sprintf("%q (%s)", name, id))
			names = append(names, strconv.Quote(name))
		}
		hint := fmt.Sprintf("create a usage product on billable metric %s and add its rate to rate card %s", names[0], planLabel(s.Plan))
		if !a.provision() {
			hint = fmt.Sprintf("create a usage product on billable metric %s and add its rate to the rate card of the tenant's contract", names[0])
		}
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("no usage product meters billable metric %s", strings.Join(labels, " or ")),
			Hint:   hint,
		}
	}
	names := productNames(prods)
	if len(tg.cards) == 0 {
		switch {
		case tg.missing:
			return destinations.PriceCoverage{
				Status: destinations.PriceMissing,
				Detail: fmt.Sprintf("no Metronome rate card has the ID or alias %q", s.Plan),
				Hint:   fmt.Sprintf("create rate card %q in Metronome, or fix METRONOME_RATE_CARD or the tenant's plan", s.Plan),
			}
		case a.provision():
			return destinations.PriceCoverage{
				Status: destinations.PriceUnknown,
				Detail: fmt.Sprintf("product %s meters it, but no rate card is configured (METRONOME_RATE_CARD or the tenant's plan), so vBilling cannot tell which rates apply", names),
			}
		default:
			return destinations.PriceCoverage{
				Status: destinations.PriceUnknown,
				Detail: fmt.Sprintf("product %s meters it; rates are checked per tenant, on the rate cards of its customer's active contracts", names),
			}
		}
	}
	var best destinations.PriceCoverage
	for i, rc := range tg.cards {
		c := a.cardCoverage(v, s, prods, names, rc, a.cardLabel(v, s.Plan, rc))
		if i == 0 || coverageRank[c.Status] > coverageRank[best.Status] {
			best = c
		}
	}
	return best
}

// cardCoverage checks a series against one rate card.
func (a *Adapter) cardCoverage(v *pricingView, s destinations.PriceSeries, prods []Product, names, rcID, label string) destinations.PriceCoverage {
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
				Detail: fmt.Sprintf("rate card %s has no entitled rate for product %s", label, names),
				Hint:   fmt.Sprintf("add a rate for product %s to rate card %s", names, label),
			}
		}
		for _, r := range found {
			if !rateIsZero(r.Rate) {
				return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: fmt.Sprintf("product %q has %d rate(s) on rate card %s", r.ProductName, len(found), label)}
			}
		}
		return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: fmt.Sprintf("every rate for product %s on rate card %s is 0", names, label)}
	}

	tracked := map[string]bool{"sku": true}
	for _, k := range a.PriceDims(s.Metric) {
		tracked[k] = true
	}
	var notEntitled string
	var wanted, untracked []string
	for _, p := range prods {
		need := map[string]string{}
		var skipped []string
		for _, k := range p.Current.PricingGroupKey {
			if k == usage.DimTenantCluster {
				continue // per-cluster pricing cannot be checked per series
			}
			if !tracked[k] {
				skipped = append(skipped, k)
				continue
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
		if len(skipped) > 0 {
			untracked = append(untracked, fmt.Sprintf("product %q prices on %s", p.Current.Name, strings.Join(skipped, ", ")))
			continue
		}
		for _, r := range rates {
			if r.ProductID != p.ID || !rateActive(r, v.now) || !matchesGroup(r.PricingGroupValues, need) {
				continue
			}
			if !r.Entitled {
				notEntitled = p.Current.Name
				continue
			}
			if rateIsZero(r.Rate) {
				return destinations.PriceCoverage{Status: destinations.PriceZero,
					Detail: fmt.Sprintf("product %q is priced at 0 on rate card %s%s", p.Current.Name, label, groupLabel(need))}
			}
			return destinations.PriceCoverage{Status: destinations.PriceCovered,
				Detail: fmt.Sprintf("product %q: %s on rate card %s%s", p.Current.Name, describeRate(r.Rate), label, groupLabel(need))}
		}
		wanted = append(wanted, fmt.Sprintf("product %q%s", p.Current.Name, groupLabel(need)))
	}
	if notEntitled != "" {
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("the rate for product %q on rate card %s is not entitled", notEntitled, label),
			Hint:   fmt.Sprintf("set entitled on the rate for product %q on rate card %s", notEntitled, label),
		}
	}
	if len(wanted) == 0 {
		return destinations.PriceCoverage{
			Status: destinations.PriceUnknown,
			Detail: fmt.Sprintf("%s, which vBilling does not track per series: check its rates on rate card %s", strings.Join(untracked, "; "), label),
		}
	}
	return destinations.PriceCoverage{
		Status: destinations.PriceMissing,
		Detail: fmt.Sprintf("rate card %s has no rate for %s", label, strings.Join(wanted, " or ")),
		Hint:   fmt.Sprintf("add a rate for %s to rate card %s", wanted[0], label),
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

var (
	_ destinations.PriceChecker = (*Adapter)(nil)
)
