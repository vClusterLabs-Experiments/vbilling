package metronome

import (
	"context"
	"fmt"
	"log"
	"maps"
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
// It also reads, per tenant:
//   - GET  /v1/customers?ingest_alias=<tenant>         (customers/list-customers)
//   - POST /v2/contracts/list with covering_date       (contracts/list-customer-contracts-v2)
//
// The rate cards are the ones of the contracts in force of the customer that
// gets the tenant's usage (see "Plans" below). With METRONOME_PROVISION the
// customer is the one vBilling created, found or was pinned to, so only the
// contracts are read. Without it, a metric's billable metrics are found by
// their event type filter and their SUM of value, whatever Finance named
// them.
//
// Metronome does not attribute usage to a billable metric created after it:
// "When using streaming billable metrics, the billable metric must be
// defined in Metronome before any usage can be associated with it. If usage
// is sent before a streaming metric is defined, it isn't attributed to the
// metric by default" (docs.metronome.com, guides/implement-metronome/
// core-concepts/create-billable-metrics; a reflow needs Metronome support),
// and "billable metrics only match usage events sent after the billable
// metric is created" (guides/get-started/developer-sdks). Coverage of usage
// no billable metric counts yet is therefore uncounted (Uncounted), and the
// pricing gate holds it in every PRICE_CHECK mode but off. A SQL billable
// metric may count any event type, and vBilling cannot read what it counts:
// while one is active, no usage is uncounted.

// Hints of coverage that says no billable metric counts a series' usage yet;
// Uncounted recognizes them.
const (
	noMetricHint          = "no billable metric counts this event type yet: add it (vbilling metronome billable-metrics prints it)"
	provisionNoMetricHint = "no billable metric counts this event type yet: vBilling creates it when the metronome destination bootstraps; " +
		"check its last error in GET /api/v1/destinations"
)

// Uncounted reports whether a coverage answer says that no billable metric
// counts a series' usage yet, so Metronome would never bill it
// (pricing.UncountedReporter): the pricing gate holds such usage until one
// does, in every PRICE_CHECK mode but off. A billable metric whose property
// filters leave the series out is a price gap, not this.
func (a *Adapter) Uncounted(c destinations.PriceCoverage) bool {
	return c.Status == destinations.PriceMissing && (c.Hint == noMetricHint || c.Hint == provisionNoMetricHint)
}

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

// Plans. Metronome rates a tenant's usage on the rate cards of the contracts
// in force of the customer that gets the usage: without METRONOME_PROVISION
// the customer that holds the tenant ID as ingest alias, with it the customer
// vBilling created, found or was pinned to. A contract keeps the rate card
// it was created on: a rate card alias is resolved once, when the contract
// is created, and moving the alias later moves no contract. So the price
// check reads each tenant's contracts, in both modes, and a tenant's plan is
// the rate card IDs of those contracts, joined with "+": tenants on the same
// rate card share their series. Until a check has read them (or while
// nothing rates the tenant's usage) the plan is aliasPlan + tenant ID.
//
// With METRONOME_PROVISION vBilling creates the contract itself, on the
// tenant's rate card setting (its plan, else METRONOME_RATE_CARD), before
// the tenant's first usage is delivered or held, and creates a new one only
// when that setting changes (ensureContract). While it has yet to create the
// contract for the current setting, the tenant's usage is checked against
// the rate card the setting points to now, which is where the contract will
// be created.
const (
	aliasPlan   = "alias:"
	planRefresh = 5 * time.Minute // a tenant's contracts are read again after this
	planForget  = 25 * time.Hour  // a tenant no check asked about for this long is dropped
	planWorkers = 8               // concurrent customer and contract reads
)

// tenantPlan is where one tenant's usage is rated, as the last check read it.
type tenantPlan struct {
	// What vBilling was told about the tenant, with METRONOME_PROVISION: its
	// rate card setting (its plan, else METRONOME_RATE_CARD) and the customer
	// it is pinned to.
	setting, pinned string

	customer  string  // the customer that gets the tenant's usage, "" if none
	contracts []rated // its contracts in force that have a rate card
	cards     []string
	// pending: cards is the rate card vBilling's contract will be created
	// on, because it has yet to create the one for the tenant's setting.
	pending bool
	plan    string // PricePlan's answer
	// Without cards: why nothing rates the usage (in a few words for the
	// log, and in full for the series), and what to add.
	brief, why, hint string
	moved            *moved // the rate card setting no longer points to the contract's rate card

	// state is the above in a few words, as last logged, and sig the IDs it
	// was made of: a change of sig is logged, a rate card that only reads
	// differently (an alias moved) is not.
	state, sig string
	movedLog   string // moved as it was last logged
	checked    time.Time
	used       time.Time // last PricePlan call
}

// rated is a contract and the rate card it rates usage on.
type rated struct{ contract, card string }

// moved says that a tenant's rate card setting points to another rate card
// now than the one its vBilling contract is on.
type moved struct {
	contract, card string // the contract and the rate card it is on
	setting, now   string // the setting, and the rate card it points to now ("" if none)
}

// PricePlan is the plan a tenant's usage is rated on: the rate cards of the
// contracts in force of its customer, as the last check read them (aliasPlan
// + tenant ID until one has, and while nothing rates its usage). For no
// tenant it is the plan billable metrics without usage are checked under:
// with METRONOME_PROVISION the rate card METRONOME_RATE_CARD points to, the
// one new tenants get a contract on.
func (a *Adapter) PricePlan(t usage.Tenant) string {
	if t.ID == "" {
		return a.defaultPlan()
	}
	var setting, pinned string
	if a.provision() {
		setting, pinned = t.Plan, t.ProviderIDs["metronome"]
		if setting == "" {
			setting = a.cfg.MetronomeRateCard
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.plans[t.ID]
	switch {
	case p == nil:
		p = &tenantPlan{plan: aliasPlan + t.ID, setting: setting, pinned: pinned}
		a.plans[t.ID] = p
	case p.setting != setting || p.pinned != pinned:
		// The tenant's rate card setting or customer changed (or discovery
		// only now told vBilling about them): what the last check read no
		// longer says where its usage is rated. The next check reads it again.
		*p = tenantPlan{plan: aliasPlan + t.ID, setting: setting, pinned: pinned, sig: p.sig, state: p.state, movedLog: p.movedLog}
	}
	p.used = a.now()
	return p.plan
}

// defaultPlan is the plan of no tenant in particular: none without
// METRONOME_PROVISION, where only contracts say where usage is rated; with
// it METRONOME_RATE_CARD, as the rate card ID it pointed to at the last
// check once one has resolved it.
func (a *Adapter) defaultPlan() string {
	if !a.provision() {
		return ""
	}
	rc := a.cfg.MetronomeRateCard
	if rc == "" || uuidRe.MatchString(rc) {
		return rc
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id := a.cards[rc]; id != "" {
		return id
	}
	return rc
}

// ChecksCatalog reports whether the billable metrics without usage are
// checked under a plan (destinations.CatalogScoper). They are not for a
// tenant whose customer and contracts have not been read, or that has none:
// it has no rate card to check them against, and its series with usage say
// what is missing.
func (a *Adapter) ChecksCatalog(plan string) bool { return !strings.HasPrefix(plan, aliasPlan) }

// isCardPlan reports whether a plan is rate card IDs joined with "+", as
// PricePlan answers once a tenant's contracts were read.
func isCardPlan(plan string) bool {
	if plan == "" {
		return false
	}
	for _, id := range strings.Split(plan, "+") {
		if !uuidRe.MatchString(id) {
			return false
		}
	}
	return true
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
	byType    map[string][]BillableMetric // metric code -> billable metrics summing value on its event type
	other     map[string][]BillableMetric // default: billable metrics on the event type that do not sum value
	sql       []BillableMetric            // active SQL billable metrics: they may count any event type
	products  map[string][]Product        // billable metric ID -> usage products
	cards     []RateCard
	cardsRead bool
	rates     map[string][]RateSchedule // rate card ID -> rates of the candidate products
	tenants   map[string]tenantPlan     // tenants whose customer and contracts this check read
	settings  map[string]string         // provisioning: rate card setting -> the rate card ID it points to now ("" if none)
	notes     map[string]string         // plan -> note for its series with usage (see noteTenants)
}

// target is where a series is rated.
type target struct {
	cards []string // rate card IDs
	// Without cards: why nothing rates the usage and what to add (the series
	// is unpriced), or neither, for a series that names no tenant.
	why, hint string
}

// CheckPrices reports, for each series, whether Metronome prices it: a
// customer gets the tenant's usage and has a contract in force, a billable
// metric counts the usage, a usage product meters that billable metric, and
// the rate card of that contract has an entitled rate for the product whose
// pricing group values match the series (SKU and any other pricing group
// key).
func (a *Adapter) CheckPrices(ctx context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	v := &pricingView{now: a.now().UTC(), metricID: map[string]string{}, byType: map[string][]BillableMetric{},
		other: map[string][]BillableMetric{}, products: map[string][]Product{}, rates: map[string][]RateSchedule{},
		tenants: map[string]tenantPlan{}, settings: map[string]string{}, notes: map[string]string{}}

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
	v.sql = sqlMetrics(bms)
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
		v.byType[s.Metric] = summing(bms, s.Metric)
		if !a.provision() {
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

	// Where each series is rated: the rate card settings in use
	// (provisioning), then each tenant's customer and contracts.
	if err := a.resolveSettings(ctx, v, series); err != nil {
		return nil, err
	}
	if err := a.resolvePlans(ctx, v, series); err != nil {
		return nil, err
	}
	targets := make([]target, len(series))
	named := false
	for i, s := range series {
		targets[i] = a.target(v, s)
		named = named || len(targets[i].cards) > 0
	}
	if named && !v.cardsRead && !a.provision() {
		// For the rate cards' names. With METRONOME_PROVISION the list is
		// read only to resolve an alias: messages name the setting.
		if v.cards, err = a.c.ListRateCards(ctx); err != nil {
			return nil, fmt.Errorf("list rate cards: %w", err)
		}
		v.cardsRead = true
	}
	a.noteTenants(v)

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
			return nil, fmt.Errorf("rates of rate card %s: %w", a.cardRef(v, rc), err)
		}
		v.rates[rc] = rates
	}

	out := make([]destinations.PriceCoverage, 0, len(series))
	for i, s := range series {
		cov := a.coverage(v, s, targets[i])
		cov.Series = s
		if !s.Catalog {
			cov.Note = v.notes[a.ratedPlan(v, s)]
		}
		out = append(out, cov)
	}
	return out, nil
}

// ratedPlan is the plan a series' usage is rated on: its own, or for a
// series of a tenant this check read for the first time, that tenant's.
func (a *Adapter) ratedPlan(v *pricingView, s destinations.PriceSeries) string {
	if t, ok := strings.CutPrefix(s.Plan, aliasPlan); ok {
		if tp, ok := v.tenants[t]; ok {
			return tp.plan
		}
	}
	return s.Plan
}

// resolveSettings maps, with METRONOME_PROVISION, every rate card setting in
// use to the rate card it points to now: METRONOME_RATE_CARD, the tenants'
// plans, and the plans of series that name a setting. A rate card ID is
// itself; an alias takes the list of rate cards, read once per check.
func (a *Adapter) resolveSettings(ctx context.Context, v *pricingView, series []destinations.PriceSeries) error {
	if !a.provision() {
		return nil
	}
	want := map[string]bool{}
	if rc := a.cfg.MetronomeRateCard; rc != "" {
		want[rc] = true
	}
	a.mu.Lock()
	for _, p := range a.plans {
		if p.setting != "" {
			want[p.setting] = true
		}
	}
	a.mu.Unlock()
	for _, s := range series {
		if s.Plan != "" && !strings.HasPrefix(s.Plan, aliasPlan) && !isCardPlan(s.Plan) {
			want[s.Plan] = true
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		id, err := a.resolveRateCard(ctx, v, name)
		if err != nil {
			return err
		}
		v.settings[name] = id
	}
	a.mu.Lock()
	a.cards = maps.Clone(v.settings)
	a.mu.Unlock()
	return nil
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

// target returns where a series is rated.
func (a *Adapter) target(v *pricingView, s destinations.PriceSeries) target {
	switch {
	case s.Plan == "":
		return target{}
	case strings.HasPrefix(s.Plan, aliasPlan):
		// A tenant whose contracts this check read (resolvePlans answers for
		// every such series, or fails the check).
		tp, ok := v.tenants[strings.TrimPrefix(s.Plan, aliasPlan)]
		switch {
		case !ok:
			return target{why: "the tenant's customer and contracts have not been read yet", hint: "the next price check reads them"}
		case len(tp.cards) == 0:
			return target{why: tp.why, hint: tp.hint}
		}
		return target{cards: tp.cards}
	case a.provision() && !isCardPlan(s.Plan):
		// A rate card setting: the rate card it points to now.
		if id := v.settings[s.Plan]; id != "" {
			return target{cards: []string{id}}
		}
		return target{why: fmt.Sprintf("no Metronome rate card has the ID or alias %q", s.Plan),
			hint: fmt.Sprintf("create rate card %q in Metronome, or fix METRONOME_RATE_CARD or the tenant's plan", s.Plan)}
	default:
		return target{cards: strings.Split(s.Plan, "+")}
	}
}

// resolvePlans reads, for the tenants this check answers for (their series'
// plan is still aliasPlan) and for those last read planRefresh ago, the
// customer that gets the tenant's usage and its contracts in force. A failed
// read of a tenant this check answers for fails the check; a failed refresh
// keeps the last answer. Every tenant that was read is kept either way, so a
// check that fails on one tenant does not read the others again.
func (a *Adapter) resolvePlans(ctx context.Context, v *pricingView, series []destinations.PriceSeries) error {
	need := map[string]bool{}
	for _, s := range series {
		if t, ok := strings.CutPrefix(s.Plan, aliasPlan); ok {
			need[t] = true
		}
	}
	type job struct {
		tenant string
		known  tenantPlan // the tenant's rate card setting and pinned customer
	}
	var jobs []job
	a.mu.Lock()
	for t, p := range a.plans {
		if v.now.Sub(p.used) > planForget && !need[t] {
			delete(a.plans, t)
			continue
		}
		if need[t] || v.now.Sub(p.checked) >= planRefresh {
			jobs = append(jobs, job{t, tenantPlan{setting: p.setting, pinned: p.pinned}})
		}
	}
	for t := range need {
		if a.plans[t] == nil { // a series names a tenant PricePlan never answered for
			var known tenantPlan
			if a.provision() {
				known.setting = a.cfg.MetronomeRateCard
			}
			jobs = append(jobs, job{t, known})
		}
	}
	a.mu.Unlock()
	if len(jobs) == 0 {
		return nil
	}
	sort.Slice(jobs, func(i, j int) bool {
		if need[jobs[i].tenant] != need[jobs[j].tenant] {
			return need[jobs[i].tenant]
		}
		return jobs[i].tenant < jobs[j].tenant
	})
	// A tenant that appeared since resolveSettings may have a rate card
	// setting it did not see.
	for _, j := range jobs {
		name := j.known.setting
		if _, ok := v.settings[name]; ok || name == "" {
			continue
		}
		id, err := a.resolveRateCard(ctx, v, name)
		if err != nil {
			return err
		}
		v.settings[name] = id
		a.mu.Lock()
		a.cards[name] = id
		a.mu.Unlock()
	}

	results := make([]tenantPlan, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, planWorkers)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i], errs[i] = a.resolveTenant(ctx, v, j.tenant, j.known, v.now)
		}()
	}
	wg.Wait()

	var failed error
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, j := range jobs {
		t := j.tenant
		if errs[i] != nil {
			switch {
			case !need[t]:
				log.Printf("[metronome] could not read the contracts of tenant %s again (%v); keeping the last answer", t, errs[i])
			case failed == nil:
				failed = fmt.Errorf("customer and contracts of tenant %s: %w", t, errs[i])
			}
			continue
		}
		r := results[i]
		r.checked = v.now
		v.tenants[t] = r
		switch p := a.plans[t]; {
		case p == nil:
			r.used = v.now
			a.plans[t] = &r
		case p.setting == r.setting && p.pinned == r.pinned:
			r.used, r.sig, r.state, r.movedLog = p.used, p.sig, p.state, p.movedLog
			a.plans[t] = &r
		}
		// Otherwise the tenant's setting changed while it was read: this
		// check answers with what it read, and the next one reads it again.
	}
	return failed
}

// resolveTenant reads where a tenant's usage is rated at at: the customer
// that gets it and the rate cards of that customer's contracts in force.
func (a *Adapter) resolveTenant(ctx context.Context, v *pricingView, tenant string, known tenantPlan, at time.Time) (tenantPlan, error) {
	out := tenantPlan{setting: known.setting, pinned: known.pinned, plan: aliasPlan + tenant}
	if a.provision() && out.setting != "" {
		if cur, _ := a.state.Get("contract-current/" + tenant); cur != out.setting {
			// vBilling has yet to create the tenant's contract for this
			// setting (a new tenant, or the setting changed): EnsureTenant
			// does before the tenant's next usage is delivered or held.
			return a.pendingContract(v, tenant, out), nil
		}
	}
	cus, err := a.customerOf(ctx, tenant, out.pinned)
	if err != nil {
		return out, err
	}
	if out.customer = cus; cus == "" {
		out.brief, out.why, out.hint = a.noCustomer(tenant, out.setting)
		return out, nil
	}
	contracts, err := a.c.ContractsCovering(ctx, cus, at)
	if IsNotFound(err) {
		// Metronome does not know the customer vBilling sends the tenant's
		// usage to (any more). That is an answer about this tenant, not a
		// check that failed: one such tenant must not stop the others from
		// being checked.
		out.brief = fmt.Sprintf("Metronome has no customer %s, so nothing rates its usage", cus)
		out.why = fmt.Sprintf("Metronome has no customer %s, which gets the usage of tenant %q, so nothing rates this usage", cus, tenant)
		out.hint = fmt.Sprintf("give the tenant a customer that exists: add the ingest alias %q to it", tenant)
		if a.provision() {
			out.hint = "pin the tenant to a customer that exists with vbilling.vcluster.com/metronome-customer-id"
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	for _, c := range contracts {
		if c.ArchivedAt != "" || c.RateCardID == "" || parseTime(c.StartingAt).After(at) ||
			(c.EndingBefore != "" && !parseTime(c.EndingBefore).After(at)) {
			continue
		}
		out.contracts = append(out.contracts, rated{c.ID, c.RateCardID})
		if !slices.Contains(out.cards, c.RateCardID) {
			out.cards = append(out.cards, c.RateCardID)
		}
		// vBilling's own contract for the tenant's setting: is it on the
		// rate card the setting points to now?
		if a.provision() && out.setting != "" && out.moved == nil && contractRateCard(c.UniquenessKey, tenant) == rateCardToken(out.setting) {
			if now := v.settings[out.setting]; now != c.RateCardID {
				out.moved = &moved{contract: c.ID, card: c.RateCardID, setting: out.setting, now: now}
			}
		}
	}
	if len(out.cards) == 0 {
		out.brief, out.why, out.hint = a.noContract(tenant, cus, out.setting)
		return out, nil
	}
	sort.Strings(out.cards)
	sort.Slice(out.contracts, func(i, j int) bool { return out.contracts[i].contract < out.contracts[j].contract })
	out.plan = strings.Join(out.cards, "+")
	return out, nil
}

// customerOf returns the customer that gets a tenant's usage, "" when there
// is none: with METRONOME_PROVISION the customer vBilling is pinned to,
// created or found (ToIngest sends its usage there), else the customer that
// holds the tenant ID as ingest alias.
func (a *Adapter) customerOf(ctx context.Context, tenant, pinned string) (string, error) {
	if a.provision() {
		if pinned != "" {
			return pinned, nil
		}
		if id, ok := a.state.Get("cus/" + tenant); ok {
			return id, nil
		}
	}
	cus, err := a.c.FindCustomerByAlias(ctx, tenant)
	if err != nil || cus == nil {
		return "", err
	}
	a.remember(tenant, cus.ID)
	return cus.ID, nil
}

// pendingContract answers for a tenant whose contract vBilling has yet to
// create: its usage will be rated on the rate card its setting points to
// now, unless that rate card does not exist or the contract could not be
// created.
func (a *Adapter) pendingContract(v *pricingView, tenant string, out tenantPlan) tenantPlan {
	a.mu.Lock()
	failed := a.failed[tenant]
	a.mu.Unlock()
	card := v.settings[out.setting]
	switch {
	case failed != "":
		out.brief = fmt.Sprintf("vBilling could not create its customer or contract on rate card %q, so nothing rates its usage: %s", out.setting, failed)
		out.why = fmt.Sprintf("vBilling could not create the customer or contract of tenant %q on rate card %q, so nothing rates this usage: %s", tenant, out.setting, failed)
		out.hint = "fix what the error names: vBilling tries again when the tenant's next usage is delivered"
	case card == "":
		out.brief = fmt.Sprintf("no Metronome rate card has the ID or alias %q, so vBilling cannot create its contract and nothing rates its usage", out.setting)
		out.why = fmt.Sprintf("no Metronome rate card has the ID or alias %q", out.setting)
		out.hint = fmt.Sprintf("create rate card %q in Metronome, or fix METRONOME_RATE_CARD or the tenant's plan", out.setting)
	default:
		out.cards, out.pending, out.plan = []string{card}, true, card
	}
	return out
}

const provisionContractHint = "set METRONOME_RATE_CARD or the tenant's vbilling.vcluster.com/plan so vBilling creates a contract, or add one for the tenant's customer in Metronome"

// noCustomer says why nothing rates the usage of a tenant no customer gets
// (in a few words about the tenant, and in full about its usage), and what
// to add.
func (a *Adapter) noCustomer(tenant, setting string) (brief, why, hint string) {
	switch {
	case !a.provision():
		return fmt.Sprintf("no Metronome customer has the ingest alias %q, so nothing rates its usage", tenant),
			fmt.Sprintf("no customer for this alias yet: no Metronome customer has the ingest alias %q, so nothing rates this usage", tenant),
			fmt.Sprintf("create a Metronome customer with the ingest alias %q (or add the alias to the tenant's customer), with a contract on a rate card that prices this usage", tenant)
	case setting == "":
		return "no rate card is configured for it and it has no Metronome customer yet, so nothing rates its usage",
			fmt.Sprintf("no rate card is configured for tenant %q (METRONOME_RATE_CARD or the tenant's plan), so vBilling creates no contract and nothing rates this usage "+
				"(vBilling creates the tenant's customer when its first usage is delivered)", tenant),
			provisionContractHint
	}
	return "it has no Metronome customer, so nothing rates its usage",
		fmt.Sprintf("tenant %q has no Metronome customer, so nothing rates this usage", tenant),
		"vBilling creates the customer and its contract when the tenant's next usage is delivered: check the metronome destination's last error in GET /api/v1/destinations"
}

// noContract says why nothing rates the usage of a customer without a
// contract in force that has a rate card, and what to add.
func (a *Adapter) noContract(tenant, customer, setting string) (brief, why, hint string) {
	switch {
	case !a.provision():
		return fmt.Sprintf("customer %s has no active contract with a rate card, so nothing rates its usage", customer),
			fmt.Sprintf("customer %s (ingest alias %q) has no active contract with a rate card, so nothing rates this usage", customer, tenant),
			fmt.Sprintf("add a contract for customer %s on a rate card that prices this usage, starting no later than the usage", customer)
	case setting == "":
		return fmt.Sprintf("customer %s has no active contract with a rate card and no rate card is configured for the tenant, so nothing rates its usage", customer),
			fmt.Sprintf("customer %s of tenant %q has no active contract with a rate card, and no rate card is configured for the tenant "+
				"(METRONOME_RATE_CARD or the tenant's plan), so vBilling creates none and nothing rates this usage", customer, tenant),
			provisionContractHint
	}
	return fmt.Sprintf("customer %s has no active contract with a rate card (the one vBilling created on rate card %q is not in force any more), so nothing rates its usage", customer, setting),
		fmt.Sprintf("customer %s of tenant %q has no active contract with a rate card: the contract vBilling created on rate card %q is not in force any more, "+
			"so nothing rates this usage", customer, tenant, setting),
		fmt.Sprintf("add a contract for customer %s in Metronome, or change the tenant's rate card setting (vbilling.vcluster.com/plan, else METRONOME_RATE_CARD): "+
			"vBilling starts a new contract only when that setting changes", customer)
}

// noteTenants logs, once per change, where each tenant this check read is
// rated (the customer, its contracts and their rate cards, by ID), and that a
// rate card setting moved away from a tenant's contract. It also writes the
// notes of the plans that have such tenants.
func (a *Adapter) noteTenants(v *pricingView) {
	var lines []string
	a.mu.Lock()
	tenants := make([]string, 0, len(v.tenants))
	for t := range v.tenants {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		p := a.plans[t]
		if p == nil || !p.checked.Equal(v.now) {
			continue // forgotten, or reset since it was read
		}
		if state, sig := a.describe(v, p); sig != p.sig {
			line := fmt.Sprintf("[metronome] tenant %s: %s", t, state)
			if p.state != "" {
				line += " (before: " + p.state + ")"
			}
			lines = append(lines, line)
			p.state, p.sig = state, sig
		}
		was := p.movedLog
		p.movedLog = ""
		if m := p.moved; m != nil {
			p.movedLog = strings.Join([]string{m.contract, m.card, m.setting, m.now}, "|")
		}
		switch {
		case p.movedLog != "" && p.movedLog != was:
			lines = append(lines, fmt.Sprintf("[metronome] WARNING: tenant %s: %s", t, a.movedText(v, []string{t}, p.moved)))
		case p.movedLog == "" && was != "" && len(p.cards) > 0 && !p.pending:
			lines = append(lines, fmt.Sprintf("[metronome] tenant %s: its rate card setting %q points to the rate card of its contract again", t, p.setting))
		}
	}
	// Notes are for every tenant whose setting moved, read by this check or not.
	type group struct {
		m       moved
		tenants []string
	}
	groups := map[string]map[string]*group{} // plan -> setting and rate cards -> tenants
	for t, p := range a.plans {
		if p.moved == nil || len(p.cards) == 0 {
			continue
		}
		k := strings.Join([]string{p.moved.card, p.moved.setting, p.moved.now}, "|")
		if groups[p.plan] == nil {
			groups[p.plan] = map[string]*group{}
		}
		g := groups[p.plan][k]
		if g == nil {
			g = &group{m: *p.moved}
			groups[p.plan][k] = g
		}
		g.tenants = append(g.tenants, t)
	}
	for plan, byKey := range groups {
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			g := byKey[k]
			sort.Strings(g.tenants)
			m := g.m
			if len(g.tenants) > 1 {
				m.contract = "" // several contracts: the tenants name them
			}
			parts = append(parts, a.movedText(v, g.tenants, &m))
		}
		v.notes[plan] = strings.Join(parts, " ")
	}
	a.mu.Unlock()
	for _, l := range lines {
		log.Print(l)
	}
}

// describe says in a few words where a tenant's usage is rated, with the IDs
// Metronome holds, and returns those IDs as a signature. a.mu held.
func (a *Adapter) describe(v *pricingView, p *tenantPlan) (state, sig string) {
	switch {
	case p.pending:
		return fmt.Sprintf("no contract yet: vBilling creates one on rate card %s before the tenant's next usage is delivered", a.cardRef(v, p.cards[0])),
			"pending|" + p.cards[0]
	case len(p.cards) == 0:
		return p.brief, "none|" + p.brief
	}
	parts := make([]string, len(p.contracts))
	ids := make([]string, len(p.contracts))
	for i, c := range p.contracts {
		parts[i] = fmt.Sprintf("contract %s on rate card %s", c.contract, a.cardRef(v, c.card))
		ids[i] = c.contract + "@" + c.card
	}
	return fmt.Sprintf("customer %s, usage rated on %s", p.customer, strings.Join(parts, " and ")),
		"rated|" + p.customer + "|" + strings.Join(ids, ",")
}

// movedText says that a rate card setting points to another rate card than
// the contracts of some tenants are on, which rate card rates their usage,
// and when vBilling moves a tenant (ensureContract's rules).
func (a *Adapter) movedText(v *pricingView, tenants []string, m *moved) string {
	now := "no rate card"
	if m.now != "" {
		now = "rate card " + a.cardRef(v, m.now)
	}
	who := "the contract of tenant " + tenants[0]
	switch {
	case len(tenants) == 1 && m.contract != "":
		who = fmt.Sprintf("contract %s of tenant %s", m.contract, tenants[0])
	case len(tenants) > 1:
		who = "the contracts of tenants " + strings.Join(firstNames(tenants, 3), ", ")
	}
	verb := "is"
	if len(tenants) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("rate card setting %q points to %s now, but %s %s on rate card %s: Metronome keeps a contract on the rate card it was created on, "+
		"so the usage is still rated on that rate card, and that is the one the price check reads. vBilling starts a new contract for a tenant only when the tenant's "+
		"rate card setting changes (vbilling.vcluster.com/plan, else METRONOME_RATE_CARD): the new contract starts at 00:00 UTC of the day of the change, and the old one ends there",
		m.setting, now, who, verb, a.cardRef(v, m.card))
}

func firstNames(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string(nil), l[:n]...), fmt.Sprintf("and %d more", len(l)-n))
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

// sqlMetrics returns the active SQL billable metrics. vBilling cannot tell
// which event types one counts.
func sqlMetrics(bms []BillableMetric) []BillableMetric {
	var out []BillableMetric
	for _, m := range bms {
		if m.ArchivedAt == "" && m.SQL != "" {
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
		if id != "" {
			return []string{id}, nil
		}
		detail := fmt.Sprintf("Metronome has no billable metric %q", metricName(def))
		if len(v.byType[s.Metric]) == 0 {
			return nil, uncounted(v, detail, provisionNoMetricHint)
		}
		return nil, &destinations.PriceCoverage{ // another billable metric counts the usage
			Status: destinations.PriceMissing,
			Detail: detail,
			Hint:   "vBilling creates billable metrics when the metronome destination bootstraps: check its last error in GET /api/v1/destinations",
		}
	}
	typed := v.byType[s.Metric]
	if len(typed) == 0 {
		detail := fmt.Sprintf("no billable metric sums the value of event type %q", s.Metric)
		if others := v.other[s.Metric]; len(others) > 0 {
			detail += fmt.Sprintf(" (%s aggregates it with %s of %q, but vBilling's quantities must be summed)",
				strconv.Quote(others[0].Name), strings.ToUpper(others[0].AggregationType), others[0].AggregationKey)
		}
		return nil, uncounted(v, detail, noMetricHint)
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
		detail := fmt.Sprintf("the property filters of billable metric %s exclude %s, so no billable metric counts it", strings.Join(names, ", "), destinations.DescribeSeries(s))
		if cov := sqlMayCount(v, detail); cov != nil {
			return nil, cov
		}
		// A price gap like any other (held only with PRICE_CHECK=hold): the
		// event type has billable metrics, and a filter is how Finance chose
		// what each one bills.
		return nil, &destinations.PriceCoverage{Status: destinations.PriceMissing, Detail: detail,
			Hint: fmt.Sprintf("widen the property filters of %s, or add a billable metric for this usage", names[0])}
	}
	return ids, nil
}

// uncounted answers for usage of an event type no billable metric sums: it
// is uncounted, unless a SQL billable metric may count it.
func uncounted(v *pricingView, detail, hint string) *destinations.PriceCoverage {
	if cov := sqlMayCount(v, detail); cov != nil {
		return cov
	}
	return &destinations.PriceCoverage{Status: destinations.PriceMissing, Hint: hint,
		Detail: detail + ": vBilling holds this usage until one does, because Metronome never bills usage sent before its billable metric exists"}
}

// sqlMayCount answers unknown, never held, for usage no other billable
// metric counts while a SQL billable metric is active: it may count the
// usage, and vBilling cannot tell.
func sqlMayCount(v *pricingView, detail string) *destinations.PriceCoverage {
	if len(v.sql) == 0 {
		return nil
	}
	return &destinations.PriceCoverage{Status: destinations.PriceUnknown,
		Detail: fmt.Sprintf("%s, but SQL billable metric %q may count it: vBilling cannot check SQL billable metrics, so it never holds this usage", detail, v.sql[0].Name),
		Hint: fmt.Sprintf("check in Metronome that SQL billable metric %q bills this usage at a rate, or add a billable metric that sums value for this event type "+
			"(vbilling metronome billable-metrics prints it), which vBilling can check", v.sql[0].Name)}
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

// cardLabel names a rate card in details and hints the way the operator knows
// it: with METRONOME_PROVISION the rate card setting that points to it now
// (METRONOME_RATE_CARD first), else its name, else its ID.
func (a *Adapter) cardLabel(v *pricingView, rcID string) string {
	if a.provision() {
		var names []string
		for name, id := range v.settings {
			if id == rcID && name != rcID {
				names = append(names, name)
			}
		}
		sort.Slice(names, func(i, j int) bool {
			if def := a.cfg.MetronomeRateCard; (names[i] == def) != (names[j] == def) {
				return names[i] == def
			}
			return names[i] < names[j]
		})
		if len(names) > 0 {
			return strconv.Quote(names[0])
		}
	}
	for _, rc := range v.cards {
		if rc.ID == rcID && rc.Name != "" {
			return strconv.Quote(rc.Name)
		}
	}
	return rcID
}

// cardRef names a rate card in log lines and notes, always with its ID.
func (a *Adapter) cardRef(v *pricingView, rcID string) string {
	if label := a.cardLabel(v, rcID); label != rcID {
		return label + " (" + rcID + ")"
	}
	return rcID
}

var coverageRank = map[string]int{destinations.PriceMissing: 0, destinations.PriceUnknown: 1, destinations.PriceZero: 2, destinations.PriceCovered: 3}

func (a *Adapter) coverage(v *pricingView, s destinations.PriceSeries, tg target) destinations.PriceCoverage {
	def, ok := usage.Lookup(s.Metric)
	if !ok {
		return destinations.PriceCoverage{Status: destinations.PriceUnknown, Detail: "metric is not in vBilling's catalog"}
	}
	bmIDs, miss := a.billableMetrics(v, s, def)
	if miss != nil && a.Uncounted(*miss) {
		// No billable metric counts the event type: its usage is held in
		// every mode, for every tenant, customer or not.
		return *miss
	}
	if len(tg.cards) == 0 && tg.why != "" {
		// No customer gets the tenant's usage, or no contract of that
		// customer is in force: nothing rates the usage, whatever the
		// catalog holds. Unpriced, so PRICE_CHECK=hold holds it until the
		// customer and contract exist.
		return destinations.PriceCoverage{Status: destinations.PriceMissing, Detail: tg.why, Hint: tg.hint}
	}
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
		where := "the rate card of the tenant's contract"
		if len(tg.cards) == 1 {
			where = "rate card " + a.cardLabel(v, tg.cards[0])
		}
		return destinations.PriceCoverage{
			Status: destinations.PriceMissing,
			Detail: fmt.Sprintf("no usage product meters billable metric %s", strings.Join(labels, " or ")),
			Hint:   fmt.Sprintf("create a usage product on billable metric %s and add its rate to %s", names[0], where),
		}
	}
	names := productNames(prods)
	if len(tg.cards) == 0 {
		// The series names no tenant, so there is no contract to read a rate
		// card from: the billable metrics without usage, where no default
		// rate card exists.
		detail := fmt.Sprintf("product %s meters it; rates are checked per tenant, on the rate cards of its customer's active contracts", names)
		if a.provision() {
			detail = fmt.Sprintf("product %s meters it, but no rate card is configured (METRONOME_RATE_CARD), so there is no default rate card to check: "+
				"rates are checked per tenant, on the rate card of its contract", names)
		}
		return destinations.PriceCoverage{Status: destinations.PriceUnknown, Detail: detail}
	}
	var best destinations.PriceCoverage
	for i, rc := range tg.cards {
		c := a.cardCoverage(v, s, prods, names, rc, a.cardLabel(v, rc))
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
		keys := append([]string{"sku"}, a.PriceDims(s.Metric)...)
		return destinations.PriceCoverage{
			Status: destinations.PriceUnknown,
			Detail: fmt.Sprintf("%s, which vBilling does not track per series: check its rates on rate card %s", strings.Join(untracked, "; "), label),
			Hint: fmt.Sprintf("check by hand that rate card %s has a rate for every value this usage carries, or price the product only on keys vBilling checks for this metric (%s)",
				label, strings.Join(keys, ", ")),
		}
	}
	return destinations.PriceCoverage{
		Status: destinations.PriceMissing,
		Detail: fmt.Sprintf("rate card %s has no rate for %s", label, strings.Join(wanted, " or ")),
		Hint:   fmt.Sprintf("add a rate for %s to rate card %s", wanted[0], label),
	}
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
