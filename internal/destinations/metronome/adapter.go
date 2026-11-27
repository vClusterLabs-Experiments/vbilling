package metronome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func init() {
	destinations.Register("metronome", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.MetronomeAPIToken == "" {
			return nil, errors.New("METRONOME_API_TOKEN is required when the metronome adapter is enabled")
		}
		state, err := kv.Open(filepath.Join(cfg.DataDir, "state-metronome.json"))
		if err != nil {
			return nil, fmt.Errorf("open metronome state: %w", err)
		}
		var link *stripe.Adapter
		if cfg.MetronomeStripeLink && cfg.MetronomeProvision {
			if cfg.StripeAPIKey == "" {
				return nil, errors.New("METRONOME_STRIPE_LINK needs STRIPE_API_KEY to create Stripe customers")
			}
			sstate, err := kv.Open(filepath.Join(cfg.DataDir, "state-metronome-stripe.json"))
			if err != nil {
				return nil, err
			}
			link = stripe.New(stripe.NewClient(cfg.StripeAPIBase, cfg.StripeAPIKey, cfg.StripeAPIVersion, cfg.StripeMaxRPS), cfg, sstate)
		}
		if cfg.MetronomeProvision {
			log.Printf("[metronome] METRONOME_PROVISION=true: vBilling creates the billable metrics, customers, contracts and Stripe links it needs")
		} else {
			log.Printf("[metronome] usage is sent under each tenant's ID as ingest alias; vBilling only reads Metronome " +
				"(customers, contracts and the catalog stay with whoever owns the account)")
		}
		return New(NewClient(cfg.MetronomeAPIBase, cfg.MetronomeAPIToken), cfg, state, link), nil
	})
}

// Metronome accepts events backdated up to 34 days; keep an hour of margin.
const maxEventAge = 34*24*time.Hour - time.Hour

// placeholder fills group-key properties an event does not carry: Metronome
// metrics require every group-key property to exist.
const placeholder = "none"

// aliasTTL is how long CustomerAliases keeps a customer's aliases.
const aliasTTL = time.Minute

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Adapter ships usage to Metronome. Events carry the vBilling tenant ID as
// customer_id, an ingest alias, so Metronome attributes them to the customer
// that holds the alias, even when the customer is created after the usage
// arrives. Without METRONOME_PROVISION the adapter never creates or changes
// anything in Metronome; with it, it creates billable metrics, customers,
// contracts and the Stripe link.
type Adapter struct {
	c      *Client
	cfg    *config.Config
	state  *kv.Store
	stripe *stripe.Adapter // nil unless provisioning with a Stripe key
	now    func() time.Time

	mu      sync.Mutex
	metrics map[string]string      // metric code -> billable metric ID vBilling created or adopted (provisioning)
	aliases map[string]aliasEntry  // customer ID -> its ingest aliases, cached
	plans   map[string]*tenantPlan // tenant -> where its usage is rated, as the last price check read it
	cards   map[string]string      // provisioning: rate card setting -> the rate card ID it pointed to at the last price check
	failed  map[string]string      // provisioning: tenant -> why its customer or contract could not be created (a permanent error)
	known   map[string]bool        // tenants recorded as vBilling ingest aliases
	warned  map[string]bool        // tenants told that their pinned customer is not used

	reservedSeen map[string]bool // dimension names logged for colliding with a reserved property
}

type aliasEntry struct {
	aliases []string
	at      time.Time
}

// New builds the adapter (exported for tests). link may be nil.
func New(c *Client, cfg *config.Config, state *kv.Store, link *stripe.Adapter) *Adapter {
	return &Adapter{c: c, cfg: cfg, state: state, stripe: link, now: time.Now, metrics: map[string]string{},
		aliases: map[string]aliasEntry{}, plans: map[string]*tenantPlan{}, cards: map[string]string{}, failed: map[string]string{},
		known: map[string]bool{}, warned: map[string]bool{}, reservedSeen: map[string]bool{}}
}

func (a *Adapter) Name() string { return "metronome" }

// provision reports whether vBilling may create objects in Metronome.
func (a *Adapter) provision() bool { return a.cfg.MetronomeProvision }

// StripeLink returns the Stripe customer manager used for invoicing (nil
// unless METRONOME_PROVISION with a Stripe key). It resolves Stripe webhooks
// to tenants.
func (a *Adapter) StripeLink() *stripe.Adapter { return a.stripe }

func (a *Adapter) Accepts(metric string) bool { return destinations.BillableOnly(metric) }

// OldestUsage is the oldest usage SendEvents sends to Metronome: it
// dead-letters anything older (destinations.AgeLimiter), so ingest refuses
// it first.
func (a *Adapter) OldestUsage() time.Duration { return maxEventAge }

// GroupKeys is the single compound group key of a metric's billable metric:
// the metric's pricing dimensions plus tenant_cluster for presentation.
// Metronome requires pricing + presentation keys to be one compound key.
func GroupKeys(def usage.MetricDef) []string {
	keys := append([]string(nil), def.GroupKeys...)
	if !def.Custom {
		keys = append(keys, usage.DimTenantCluster)
	}
	seen := map[string]bool{}
	out := keys[:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func metricName(def usage.MetricDef) string { return def.Name + " [" + def.Code + "]" }

// BillableMetricFor is the billable metric a metric's events need: the SUM
// of the value property, filtered on the event type (the metric code),
// requiring value and every group-key property, with one compound group key.
// With METRONOME_PROVISION=true vBilling creates it; otherwise
// `vbilling metronome billable-metrics` prints it for Finance's configuration.
func BillableMetricFor(def usage.MetricDef) BillableMetric {
	yes := true
	filters := []PropertyFilter{{Name: "value", Exists: &yes}}
	keys := GroupKeys(def)
	for _, k := range keys {
		filters = append(filters, PropertyFilter{Name: k, Exists: &yes})
	}
	bm := BillableMetric{
		Name:            metricName(def),
		AggregationType: "SUM",
		AggregationKey:  "value",
		EventTypeFilter: &EventTypeFilter{InValues: []string{def.Code}},
		PropertyFilters: filters,
	}
	if len(keys) > 0 {
		bm.GroupKeys = [][]string{keys}
	}
	return bm
}

// Bootstrap, with METRONOME_PROVISION, creates one billable metric per
// billable metric code (BillableMetricFor), matching existing ones by stored
// ID or by name, never duplicating them. Without it nothing is created: it
// logs the metrics no billable metric in Metronome counts yet. Metronome
// never bills usage sent before its billable metric exists, so price
// coverage reports such usage as uncounted (see Uncounted), and the pricing
// gate holds it until a billable metric counts it.
func (a *Adapter) Bootstrap(ctx context.Context, metrics []usage.MetricDef) error {
	if !a.provision() {
		a.reportCatalog(ctx, metrics)
		return nil
	}
	existing, err := a.c.ListBillableMetrics(ctx)
	if err != nil {
		return fmt.Errorf("list billable metrics: %w", err)
	}
	byID, byName := map[string]bool{}, map[string]string{}
	for _, m := range existing {
		if m.ArchivedAt != "" {
			continue
		}
		byID[m.ID] = true
		byName[m.Name] = m.ID
	}
	var errs []error
	for _, def := range metrics {
		if !def.Billable {
			continue
		}
		if id, ok := a.state.Get("bm/" + def.Code); ok && byID[id] {
			a.setMetric(def.Code, id)
			continue
		}
		if id, ok := byName[metricName(def)]; ok {
			a.setMetric(def.Code, id)
			continue
		}
		id, err := a.c.CreateBillableMetric(ctx, BillableMetricFor(def))
		if err != nil {
			errs = append(errs, fmt.Errorf("create billable metric %s: %w", def.Code, err))
			continue
		}
		log.Printf("[metronome] created billable metric %s (%s)", def.Code, id)
		a.setMetric(def.Code, id)
	}
	return errors.Join(errs...)
}

// reportCatalog logs, read-only, the billable metrics of the catalog that no
// billable metric in Metronome sums.
func (a *Adapter) reportCatalog(ctx context.Context, defs []usage.MetricDef) {
	bms, err := a.c.ListBillableMetrics(ctx)
	if err != nil {
		log.Printf("[metronome] could not list billable metrics (%v); price coverage reads them again", err)
		return
	}
	var missing []string
	for _, def := range defs {
		if def.Billable && len(summing(bms, def.Code)) == 0 {
			missing = append(missing, def.Code)
		}
	}
	if len(missing) == 0 {
		return
	}
	const prints = "`vbilling metronome billable-metrics` prints the definitions vBilling expects"
	hold := pipeline.MaxUnpricedHold
	if mh := a.cfg.PriceCheckMaxHold; mh > 0 && mh < hold {
		hold = mh
	}
	switch sql := sqlMetrics(bms); {
	case len(sql) > 0:
		log.Printf("[metronome] no billable metric sums the value of event type(s) %s, but SQL billable metric %q may count them, which vBilling cannot check: "+
			"it sends that usage (%s)", strings.Join(missing, ", "), sql[0].Name, prints)
	case a.cfg.PriceCheck == "off":
		log.Printf("[metronome] WARNING: no billable metric sums the value of event type(s) %s yet: with PRICE_CHECK=off vBilling sends that usage anyway, "+
			"and Metronome never bills usage sent before its billable metric exists (%s)", strings.Join(missing, ", "), prints)
	default:
		log.Printf("[metronome] no billable metric sums the value of event type(s) %s yet: vBilling holds that usage until one does, for at most %s, "+
			"because Metronome never bills usage sent before its billable metric exists (%s)", strings.Join(missing, ", "), hold, prints)
	}
}

func (a *Adapter) setMetric(code, id string) {
	a.mu.Lock()
	a.metrics[code] = id
	a.mu.Unlock()
	_ = a.state.Set("bm/"+code, id)
	_ = a.state.Set("bmcode/"+id, code)
}

// EnsureTenant, with METRONOME_PROVISION, creates the Metronome customer (and
// the linked Stripe customer when configured) and, with a rate card, a
// contract. Without it nothing is written to Metronome: the tenant ID is the
// ingest alias its usage is sent under, and the portal that owns the account
// creates the customer with that alias whenever it does.
func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	if !a.provision() {
		a.noteTenant(t)
		return nil
	}
	err := a.ensureTenant(ctx, t)
	a.noteEnsured(t.ID, err)
	return err
}

// noteEnsured remembers, for the price check, a tenant whose customer or
// contract vBilling could not create for good. The dispatcher delivers such
// a tenant's usage anyway, and nothing rates it: price coverage reports it
// unpriced until EnsureTenant succeeds. An error that is retried holds up
// delivery instead, so there is nothing to report.
func (a *Adapter) noteEnsured(tenant string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.plans[tenant]
	switch {
	case err == nil:
		delete(a.failed, tenant)
		// The next price check reads the contract Metronome holds now.
		if p != nil && p.pending {
			p.checked = time.Time{}
		}
	case destinations.IsPermanent(err):
		a.failed[tenant] = err.Error()
		// The check assumed a contract that was not created: the tenant's
		// next usage is checked again before it is delivered.
		if p != nil && p.pending {
			*p = tenantPlan{plan: aliasPlan + tenant, setting: p.setting, pinned: p.pinned, used: p.used, sig: p.sig, state: p.state, movedLog: p.movedLog}
		}
	}
}

// ensureTenant creates what a tenant needs in Metronome (METRONOME_PROVISION).
func (a *Adapter) ensureTenant(ctx context.Context, t usage.Tenant) error {
	cusID, err := a.ensureCustomer(ctx, t)
	if err != nil {
		return err
	}
	if err := a.ensureStripeLink(ctx, t, cusID); err != nil {
		return err
	}
	rateCard := t.Plan
	if rateCard == "" {
		rateCard = a.cfg.MetronomeRateCard
	}
	if rateCard == "" {
		return nil
	}
	return a.ensureContract(ctx, t, cusID, rateCard)
}

// noteTenant records a tenant ID as one of vBilling's ingest aliases, so a
// webhook for the customer holding it resolves to the tenant.
func (a *Adapter) noteTenant(t usage.Tenant) {
	if id := t.ProviderIDs["metronome"]; id != "" {
		a.mu.Lock()
		first := !a.warned[t.ID]
		a.warned[t.ID] = true
		a.mu.Unlock()
		if first {
			log.Printf("[metronome] WARNING: tenant %s pins customer %s, which only METRONOME_PROVISION=true uses: usage is sent under the ingest alias %q, "+
				"so add that alias to the customer", t.ID, id, t.ID)
		}
	}
	a.markKnown(t.ID)
}

// markKnown remembers that vBilling sends usage under alias.
func (a *Adapter) markKnown(alias string) {
	a.mu.Lock()
	seen := a.known[alias]
	a.known[alias] = true
	a.mu.Unlock()
	if !seen {
		if err := a.state.Set("alias/"+alias, "tenant"); err != nil {
			log.Printf("[metronome] remember tenant %s: %v", alias, err)
		}
	}
}

// isTenant reports whether vBilling knows alias as one of its tenants: it
// sent usage under it, or found or created the customer for it.
func (a *Adapter) isTenant(alias string) bool {
	a.mu.Lock()
	known := a.known[alias]
	a.mu.Unlock()
	if known {
		return true
	}
	if _, ok := a.state.Get("alias/" + alias); ok {
		return true
	}
	_, ok := a.state.Get("cus/" + alias)
	return ok
}

func (a *Adapter) ensureCustomer(ctx context.Context, t usage.Tenant) (string, error) {
	if id := t.ProviderIDs["metronome"]; id != "" {
		a.remember(t.ID, id)
		return id, nil
	}
	if id, ok := a.state.Get("cus/" + t.ID); ok {
		a.keepName(ctx, t, id)
		return id, nil
	}
	if cus, err := a.c.FindCustomerByAlias(ctx, t.ID); err != nil {
		return "", fmt.Errorf("find customer %s: %w", t.ID, err)
	} else if cus != nil {
		a.remember(t.ID, cus.ID)
		return cus.ID, nil
	}

	req := CreateCustomerRequest{Name: customerName(t), IngestAliases: []string{t.ID}}
	var stripeID string
	if a.stripe != nil {
		id, err := a.stripe.EnsureCustomer(ctx, t)
		if err != nil {
			return "", fmt.Errorf("link stripe customer for %s: %w", t.ID, err)
		}
		stripeID = id
		req.BillingProviderConfigs = []BillingProviderConfig{a.stripeConfig(stripeID)}
	}
	cus, err := a.c.CreateCustomer(ctx, req)
	if IsConflict(err) {
		// The alias is taken. vBilling never creates a second customer for
		// it: it adopts the customer that holds it (created meanwhile by the
		// portal, an earlier attempt or another replica).
		found, ferr := a.c.FindCustomerByAlias(ctx, t.ID)
		if ferr != nil {
			return "", fmt.Errorf("find customer %s after a conflict: %w", t.ID, ferr)
		}
		if found != nil {
			a.remember(t.ID, found.ID)
			return found.ID, nil
		}
		// Metronome lists only active customers by default; an archived one
		// keeps its aliases until they are removed from it.
		if archived, aerr := a.c.FindArchivedCustomerByAlias(ctx, t.ID); aerr == nil && archived != nil {
			return "", destinations.Permanent(fmt.Errorf("ingest alias %q belongs to archived customer %s: remove the alias from it in Metronome, "+
				"or pin the tenant to a customer with vbilling.vcluster.com/metronome-customer-id", t.ID, archived.ID))
		}
	}
	if err != nil {
		return "", fmt.Errorf("create customer %s: %w", t.ID, err)
	}
	log.Printf("[metronome] created customer %s for tenant %s", cus.ID, t.ID)
	a.remember(t.ID, cus.ID)
	// vBilling created this customer, so it keeps its name in step with the
	// tenant's display name (keepName).
	_ = a.state.Set("named/"+cus.ID, req.Name)
	if stripeID != "" {
		_ = a.state.Set("link/"+cus.ID, stripeID)
	}
	return cus.ID, nil
}

// customerName is the name vBilling gives a tenant's customer: its display
// name, or its ID when it has none.
func customerName(t usage.Tenant) string {
	if t.DisplayName != "" {
		return t.DisplayName
	}
	return t.ID
}

// keepName gives a customer vBilling created the tenant's display name
// after the tenant was renamed, so invoices show the current name. Only
// customers vBilling created are renamed (state key named/<customer>,
// written at creation since v0.3.3): a customer vBilling found by its alias
// or was given (vbilling.vcluster.com/metronome-customer-id) belongs to
// whoever made it and keeps its name. A rename never holds up usage: a
// failed one is logged and tried again the next time the tenant is set up
// (when it changes again, or after a restart).
func (a *Adapter) keepName(ctx context.Context, t usage.Tenant, cusID string) {
	set, ok := a.state.Get("named/" + cusID)
	if !ok {
		return
	}
	want := customerName(t)
	if set == want {
		return
	}
	if err := a.c.SetCustomerName(ctx, cusID, want); err != nil {
		log.Printf("[metronome] WARNING: could not rename customer %s of tenant %s from %q to %q: %v; tried again the next time the tenant is set up",
			cusID, t.ID, set, want, err)
		return
	}
	_ = a.state.Set("named/"+cusID, want)
	log.Printf("[metronome] customer %s of tenant %s renamed from %q to %q", cusID, t.ID, set, want)
}

func (a *Adapter) remember(tenant, cus string) {
	_ = a.state.Set("cus/"+tenant, cus)
	_ = a.state.Set("tenant/"+cus, tenant)
}

func (a *Adapter) stripeConfig(stripeID string) BillingProviderConfig {
	return BillingProviderConfig{
		BillingProvider: "stripe",
		DeliveryMethod:  "direct_to_billing_provider",
		Configuration: map[string]string{
			"stripe_customer_id":       stripeID,
			"stripe_collection_method": a.cfg.MetronomeStripeCollect,
		},
	}
}

// ensureStripeLink gives a customer vBilling did not create (found by its
// alias, or pinned by ID) the Stripe configuration it creates customers with,
// unless the customer already has one.
func (a *Adapter) ensureStripeLink(ctx context.Context, t usage.Tenant, cusID string) error {
	if a.stripe == nil {
		return nil
	}
	if _, ok := a.state.Get("link/" + cusID); ok {
		return nil
	}
	cfgs, err := a.c.BillingProviderConfigs(ctx, cusID)
	if err != nil {
		return fmt.Errorf("billing provider configurations of %s: %w", cusID, err)
	}
	for _, c := range cfgs {
		if c.BillingProvider == "stripe" && c.ArchivedAt == "" {
			return a.state.Set("link/"+cusID, fmt.Sprint(c.Configuration["stripe_customer_id"]))
		}
	}
	stripeID, err := a.stripe.EnsureCustomer(ctx, t)
	if err != nil {
		return fmt.Errorf("link stripe customer for %s: %w", t.ID, err)
	}
	if err := a.c.SetBillingProviderConfig(ctx, cusID, a.stripeConfig(stripeID)); err != nil {
		return fmt.Errorf("link customer %s to stripe: %w", cusID, err)
	}
	log.Printf("[metronome] linked customer %s of tenant %s to Stripe customer %s", cusID, t.ID, stripeID)
	return a.state.Set("link/"+cusID, stripeID)
}

// Contract uniqueness keys. v0.2.0 used "vbilling-<tenant>-<rate card>", which
// cannot be reused once that contract ended, so newer contracts carry their
// start date (and a counter for a second contract on the same day):
// "vbilling:<rate card>@<YYYY-MM-DD>[#n]:<tenant hash>".
const maxUniquenessKey = 128

func rateCardToken(rateCard string) string {
	if len(rateCard) <= 64 {
		return rateCard
	}
	return "h" + shortHash(rateCard)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// contractRateCard returns the rate card token a vBilling contract key of
// this tenant was created for, or "" for any other contract.
func contractRateCard(key, tenant string) string {
	if body, ok := strings.CutPrefix(key, "vbilling:"); ok {
		i := strings.LastIndex(body, ":")
		if i < 0 || body[i+1:] != shortHash(tenant) {
			return ""
		}
		if j := strings.LastIndex(body[:i], "@"); j > 0 {
			return body[:j]
		}
		return ""
	}
	if rc, ok := strings.CutPrefix(key, "vbilling-"+tenant+"-"); ok {
		return rateCardToken(rc)
	}
	return ""
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// openAt reports whether a contract is in force at t (or starts later).
func openAt(c Contract, t time.Time) bool {
	return c.ArchivedAt == "" && (c.EndingBefore == "" || parseTime(c.EndingBefore).After(t))
}

// ensureContract keeps one open vBilling contract per tenant, on the tenant's
// rate card. When the rate card changes, a new contract starts at UTC
// midnight and every other open vBilling contract of the tenant ends there,
// so no usage is rated twice. Contracts vBilling did not create are left alone.
func (a *Adapter) ensureContract(ctx context.Context, t usage.Tenant, cusID, rateCard string) error {
	if cur, _ := a.state.Get("contract-current/" + t.ID); cur == rateCard {
		return nil
	}
	all, err := a.c.ListContracts(ctx, cusID)
	if err != nil {
		return fmt.Errorf("list contracts of %s: %w", t.ID, err)
	}
	today := a.now().UTC().Truncate(24 * time.Hour)
	token := rateCardToken(rateCard)
	keys := map[string]bool{}
	var ours []Contract
	var keep *Contract
	for _, c := range all {
		keys[c.UniquenessKey] = true
		rc := contractRateCard(c.UniquenessKey, t.ID)
		if rc == "" {
			continue
		}
		ours = append(ours, c)
		if rc == token && openAt(c, today) && (keep == nil || parseTime(c.StartingAt).Before(parseTime(keep.StartingAt))) {
			c := c
			keep = &c
		}
	}
	if keep == nil {
		if keep, err = a.createContract(ctx, t, cusID, rateCard, contractStart(ctx, today, ours), keys); err != nil {
			return err
		}
	} else {
		// A contract vBilling created earlier: its state was lost, another
		// replica created it, or the tenant is back on a rate card whose
		// contract is still open.
		log.Printf("[metronome] tenant %s (customer %s) keeps contract %s on rate card %s, which started at %s",
			t.ID, cusID, keep.ID, rateCardRef(rateCard, keep.RateCardID), keep.StartingAt)
	}
	if err := a.endOtherContracts(ctx, t, cusID, ours, *keep, today); err != nil {
		return err
	}
	return a.state.Set("contract-current/"+t.ID, rateCard)
}

// rateCardRef names, in a log line, the rate card a contract is on: the ID
// Metronome holds for the contract and, when the rate card was configured as
// an alias, that alias. Metronome resolves an alias once, when the contract
// is created, and the contract keeps that rate card wherever the alias
// points later. id is "" when Metronome's answer did not name the rate card.
func rateCardRef(setting, id string) string {
	switch {
	case id == setting || (id == "" && uuidRe.MatchString(setting)):
		return setting
	case id == "":
		return fmt.Sprintf("alias %q (Metronome did not say which rate card it resolved to)", setting)
	case uuidRe.MatchString(setting):
		return fmt.Sprintf("%s (configured as %s)", id, setting)
	default:
		return fmt.Sprintf("%s (alias %q)", id, setting)
	}
}

// contractStart is when a new contract starts: at UTC midnight of the day the
// usage it is created for starts (destinations.UsageStart: a backlog after an
// outage, a backfill, the last window of a day delivered after midnight), so
// Metronome rates that usage under it. It starts today when there is no such
// usage, or when one of the tenant's vBilling contracts is still in force at
// that day: then the new one takes over today and no day is rated twice.
func contractStart(ctx context.Context, today time.Time, ours []Contract) time.Time {
	us, ok := destinations.UsageStart(ctx)
	if !ok {
		return today
	}
	day := us.UTC().Truncate(24 * time.Hour)
	if !day.Before(today) {
		return today
	}
	for _, c := range ours {
		if openAt(c, day) {
			return today
		}
	}
	return day
}

func (a *Adapter) createContract(ctx context.Context, t usage.Tenant, cusID, rateCard string, start time.Time, used map[string]bool) (*Contract, error) {
	base := "vbilling:" + rateCardToken(rateCard) + "@" + start.Format("2006-01-02")
	for n := 1; n <= 9; n++ {
		key := base
		if n > 1 {
			key += "#" + strconv.Itoa(n)
		}
		key += ":" + shortHash(t.ID)
		if used[key] {
			continue // ended or archived earlier today: keys cannot be reused
		}
		if len(key) > maxUniquenessKey {
			return nil, destinations.Permanent(fmt.Errorf("contract key for %s on %s exceeds %d characters", t.ID, rateCard, maxUniquenessKey))
		}
		req := CreateContractRequest{
			CustomerID:             cusID,
			StartingAt:             rfc3339(start),
			UniquenessKey:          key,
			UsageStatementSchedule: &UsageStatementSchedule{Frequency: "MONTHLY", Day: "FIRST_OF_MONTH"},
		}
		if uuidRe.MatchString(rateCard) {
			req.RateCardID = rateCard
		} else {
			req.RateCardAlias = rateCard
		}
		if a.stripe != nil {
			req.BillingProviderConfig = &BillingProviderConfig{BillingProvider: "stripe", DeliveryMethod: "direct_to_billing_provider"}
		}
		id, err := a.c.CreateContract(ctx, req)
		if IsConflict(err) {
			// Created meanwhile, by an earlier attempt or another replica.
			all, lerr := a.c.ListContracts(ctx, cusID)
			if lerr != nil {
				return nil, fmt.Errorf("list contracts of %s: %w", t.ID, lerr)
			}
			for _, c := range all {
				if c.UniquenessKey == key && openAt(c, start) {
					log.Printf("[metronome] tenant %s (customer %s) keeps contract %s on rate card %s, which another attempt created from %s",
						t.ID, cusID, c.ID, rateCardRef(rateCard, c.RateCardID), start.Format("2006-01-02"))
					return &c, nil
				}
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create contract for %s on %s: %w", t.ID, rateCard, err)
		}
		c := &Contract{ID: id, UniquenessKey: key, StartingAt: rfc3339(start), RateCardID: req.RateCardID}
		if c.RateCardID == "" {
			// Metronome answers with the contract's ID only. Read the contract
			// back for the rate card the alias resolved to, so the log says
			// which rate card the tenant is on. A failed read changes nothing:
			// the price check reads the contract again.
			if all, lerr := a.c.ListContracts(ctx, cusID); lerr == nil {
				for _, x := range all {
					if x.ID == id {
						c.RateCardID = x.RateCardID
					}
				}
			}
		}
		log.Printf("[metronome] contract %s for tenant %s (customer %s) on rate card %s from %s",
			id, t.ID, cusID, rateCardRef(rateCard, c.RateCardID), start.Format("2006-01-02"))
		return c, nil
	}
	return nil, destinations.Permanent(fmt.Errorf("no free contract key for %s on %s today", t.ID, rateCard))
}

// endOtherContracts ends every other open vBilling contract of the tenant
// where keep takes over: at keep's start, or today if keep started earlier
// (two contracts that overlapped before this version). One that would not
// have started by then is archived instead.
func (a *Adapter) endOtherContracts(ctx context.Context, t usage.Tenant, cusID string, ours []Contract, keep Contract, today time.Time) error {
	at := parseTime(keep.StartingAt)
	if at.Before(today) {
		at = today
	}
	for _, c := range ours {
		if c.ID == keep.ID || !openAt(c, at) {
			continue
		}
		start := parseTime(c.StartingAt)
		archive := !start.Before(at)
		var err error
		if archive {
			err = a.c.ArchiveContract(ctx, cusID, c.ID)
		} else {
			err = a.c.EndContract(ctx, cusID, c.ID, at)
		}
		on := ""
		if c.RateCardID != "" {
			on = ", on rate card " + c.RateCardID
		}
		switch {
		case err == nil && archive:
			// Archived, not ended: the log says what Metronome holds.
			log.Printf("[metronome] contract %s of tenant %s (customer %s%s) archived: it started at %s, and contract %s takes over from %s, so it rates no usage",
				c.ID, t.ID, cusID, on, rfc3339(start), keep.ID, rfc3339(at))
		case err == nil:
			log.Printf("[metronome] contract %s of tenant %s (customer %s%s) ended at %s, where contract %s takes over", c.ID, t.ID, cusID, on, rfc3339(at), keep.ID)
			if from := laterOf(start, parseTime(keep.StartingAt)); from.Before(at) {
				log.Printf("[metronome] WARNING: contracts %s and %s of tenant %s both ran from %s to %s: check their invoices for that period",
					c.ID, keep.ID, t.ID, rfc3339(from), rfc3339(at))
			}
		case destinations.IsPermanent(err):
			// Retrying cannot help: say so once and move on.
			log.Printf("[metronome] WARNING: could not end contract %s of tenant %s (%v): end it in Metronome", c.ID, t.ID, err)
		default:
			return fmt.Errorf("end contract %s of %s: %w", c.ID, t.ID, err)
		}
	}
	return nil
}

// RemoveTenant leaves the customer and contract in place: final usage still
// needs to invoice, and ending contracts is a commercial decision.
func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	log.Printf("[metronome] tenant %s offboarded; customer and contract left in place", t.ID)
	return nil
}

// CustomerAliases returns every ingest alias of a Metronome customer, cached
// for a minute. A customer Metronome does not know has none (nil, no error);
// any other failure is returned, so the webhook that asked can be retried.
func (a *Adapter) CustomerAliases(ctx context.Context, customerID string) ([]string, error) {
	if customerID == "" {
		return nil, nil
	}
	now := a.now()
	a.mu.Lock()
	if e, ok := a.aliases[customerID]; ok && now.Sub(e.at) < aliasTTL {
		a.mu.Unlock()
		return slices.Clone(e.aliases), nil
	}
	a.mu.Unlock()
	var aliases []string
	cus, err := a.c.GetCustomer(ctx, customerID)
	switch {
	case IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("customer %s: %w", customerID, err)
	default:
		aliases = slices.Clone(cus.IngestAliases)
	}
	a.mu.Lock()
	if len(a.aliases) >= 10000 {
		for id, e := range a.aliases {
			if now.Sub(e.at) >= aliasTTL {
				delete(a.aliases, id)
			}
		}
	}
	a.aliases[customerID] = aliasEntry{aliases: aliases, at: now}
	a.mu.Unlock()
	return slices.Clone(aliases), nil
}

// KnowsTenant reports whether vBilling knows an ingest alias as one of its
// tenants: it sent usage under it, or found or created the customer for
// it. The enforcer applies a customer's webhooks to every alias it knows.
func (a *Adapter) KnowsTenant(alias string) bool { return a.isTenant(alias) }

// TenantForCustomer resolves a webhook's customer to a tenant: the alias of
// the customer that vBilling knows as a tenant. A customer with an identity
// alias and a compute alias (the tenant ID) resolves to the compute alias,
// whatever their order; a customer with none of vBilling's tenants resolves
// to nothing. With METRONOME_PROVISION, a customer vBilling created, found or
// was pinned to resolves to that tenant directly. A customer that holds the
// aliases of several tenants (one commit funding them all) resolves to the
// first here; the enforcer reads every alias (CustomerAliases, KnowsTenant)
// and applies the webhook to each tenant.
func (a *Adapter) TenantForCustomer(ctx context.Context, customerID string) (string, bool) {
	stored, hasStored := a.state.Get("tenant/" + customerID)
	if hasStored && a.provision() {
		return stored, true
	}
	aliases, err := a.CustomerAliases(ctx, customerID)
	if err != nil {
		// Metronome cannot be read now: use what vBilling mapped itself.
		log.Printf("[metronome] resolve customer %s: %v", customerID, err)
		return stored, hasStored
	}
	var mapped []string
	for _, al := range aliases {
		if a.isTenant(al) {
			mapped = append(mapped, al)
		}
	}
	switch {
	case len(mapped) == 0:
		return "", false
	case hasStored && slices.Contains(mapped, stored):
		return stored, true
	}
	return mapped[0], true
}

// reservedProps are the properties ToIngest sets from the event itself.
// Billable metrics sum value and price on sku: a dimension of the same name
// (an emitter chooses dimension keys) must never replace one, or Metronome
// would bill another quantity or SKU than the ledger holds.
var reservedProps = map[string]bool{"value": true, "unit": true, "window_start": true, "window_end": true, "schema_version": true,
	"region": true, "project": true, "sku": true, "resource_id": true, "source": true, "accuracy": true}

// ToIngest converts a canonical event to Metronome's wire format. The
// dimensions go first, then the reserved properties, which a dimension
// never replaces (one that collides is dropped and logged).
func (a *Adapter) ToIngest(e *usage.Event) IngestEvent {
	props := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			props[k] = v
		}
	}
	for k, v := range e.Dimensions {
		if reservedProps[k] {
			a.warnReserved(e.Metric, k)
			continue
		}
		set(k, v)
	}
	props["value"] = strconv.FormatFloat(e.Quantity, 'f', -1, 64)
	props["unit"] = e.Unit
	props["window_start"] = rfc3339(e.WindowStart)
	props["window_end"] = rfc3339(e.WindowEnd)
	props["schema_version"] = e.SchemaVersion
	set("region", e.Region)
	set("project", e.Project)
	set("sku", e.SKU)
	set("resource_id", e.ResourceID)
	set("source", e.Source)
	if def, ok := usage.Lookup(e.Metric); ok {
		for _, k := range GroupKeys(def) {
			if props[k] == "" {
				props[k] = placeholder
			}
		}
	}
	if e.Estimate() {
		props["accuracy"] = usage.AccuracyEstimate
	}
	customer := e.Tenant // the ingest alias
	if a.provision() {
		if id, ok := a.state.Get("cus/" + e.Tenant); ok {
			customer = id // the customer vBilling created, found or was pinned to
		}
	}
	return IngestEvent{
		TransactionID: e.ID,
		CustomerID:    customer,
		EventType:     e.Metric,
		Timestamp:     rfc3339(e.WindowStart),
		Properties:    props,
	}
}

// warnReserved logs, once per key, a dimension named like a property
// vBilling sets itself.
func (a *Adapter) warnReserved(metric, key string) {
	a.mu.Lock()
	first := !a.reservedSeen[key]
	a.reservedSeen[key] = true
	a.mu.Unlock()
	if first {
		log.Printf("[metronome] WARNING: events of %s carry a dimension %q, the name of a property vBilling sets from the event itself: "+
			"the dimension is not sent (logged once per name)", metric, key)
	}
}

// SendEvents ingests in batches of 100. Retrying a batch is safe: Metronome
// ignores repeated transaction IDs for 34 days. Usage no billable metric
// counts yet reaches it only with PRICE_CHECK=off: otherwise the pricing gate
// holds it (see Uncounted).
func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	rejected := map[string]string{}
	var batch []IngestEvent
	for i := range events {
		e := &events[i]
		if !destinations.AcceptsEvent(a, e) {
			continue
		}
		if a.now().Sub(e.WindowStart) > maxEventAge {
			rejected[e.ID] = "older than Metronome's 34-day ingest window"
			continue
		}
		a.markKnown(e.Tenant)
		batch = append(batch, a.ToIngest(e))
	}
	for i := 0; i < len(batch); i += 100 {
		end := i + 100
		if end > len(batch) {
			end = len(batch)
		}
		// A permanent error here makes the dispatcher bisect the batch, so
		// the age rejections above resurface on their own.
		if err := a.c.Ingest(ctx, batch[i:end]); err != nil {
			return err
		}
	}
	if len(rejected) > 0 {
		return &destinations.PartialError{Rejected: rejected}
	}
	return nil
}

// RecordedTotals reads Metronome's usage API. Metronome only answers for
// whole UTC days, so both bounds must be UTC midnights. Without
// METRONOME_PROVISION, customers are looked up by ingest alias and each
// metric's billable metric by event type: one that sums the value of every
// event of the type.
func (a *Adapter) RecordedTotals(ctx context.Context, q destinations.TotalsQuery) ([]destinations.Total, error) {
	from, to := q.From.UTC(), q.To.UTC()
	if !from.Equal(from.Truncate(24*time.Hour)) || !to.Equal(to.Truncate(24*time.Hour)) || !to.After(from) {
		return nil, fmt.Errorf("metronome reconciliation needs whole UTC days (got %s to %s)", rfc3339(from), rfc3339(to))
	}
	req := UsageRequest{StartingOn: rfc3339(from), EndingBefore: rfc3339(to), WindowSize: "day"}
	tenantOf := map[string]string{}
	codeOf := map[string]string{}
	notes := map[string]string{} // tenant -> why Metronome reports no total for it
	if a.provision() {
		for _, t := range q.Tenants {
			if id, ok := a.state.Get("cus/" + t.ID); ok {
				req.CustomerIDs = append(req.CustomerIDs, id)
				tenantOf[id] = t.ID
			}
		}
		for _, m := range q.Metrics {
			if id, ok := a.state.Get("bm/" + m); ok {
				req.BillableMetrics = append(req.BillableMetrics, map[string]any{"id": id})
				codeOf[id] = m
			}
		}
	} else if err := a.totalsTargets(ctx, q, &req, tenantOf, codeOf, notes); err != nil {
		return nil, err
	}
	var out []destinations.Total
	for _, t := range q.Tenants {
		if n, ok := notes[t.ID]; ok {
			out = append(out, destinations.Total{Tenant: t.ID, Note: n})
		}
	}
	if len(req.CustomerIDs) == 0 || len(req.BillableMetrics) == 0 {
		return out, nil
	}
	rows, err := a.c.Usage(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	sums := map[[2]string]float64{}
	for _, r := range rows {
		v, err := r.Value.Float64()
		if err != nil {
			continue
		}
		sums[[2]string{tenantOf[r.CustomerID], codeOf[r.BillableMetricID]}] += v
	}
	for k, v := range sums {
		if k[0] == "" || k[1] == "" {
			continue
		}
		out = append(out, destinations.Total{Tenant: k[0], Metric: k[1], Quantity: usage.Round(v)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Metric < out[j].Metric
	})
	return out, nil
}

// totalsTargets finds, read-only, the customers (by ingest alias) and the
// billable metrics (by event type) a reconciliation compares.
func (a *Adapter) totalsTargets(ctx context.Context, q destinations.TotalsQuery, req *UsageRequest, tenantOf, codeOf, notes map[string]string) error {
	shared := map[string]bool{}
	for _, t := range q.Tenants {
		cus, err := a.c.FindCustomerByAlias(ctx, t.ID)
		if err != nil {
			return fmt.Errorf("find customer %s: %w", t.ID, err)
		}
		if cus == nil {
			// No customer holds the alias yet: Metronome keeps the usage under
			// it, and counts it for the customer that gets the alias later.
			notes[t.ID] = fmt.Sprintf("no Metronome customer has the ingest alias %q yet: Metronome keeps the usage and bills it to the customer that gets the alias", t.ID)
			continue
		}
		a.remember(t.ID, cus.ID)
		if other, dup := tenantOf[cus.ID]; dup || shared[cus.ID] {
			if dup {
				log.Printf("[metronome] tenants %s and %s are aliases of one customer %s, which Metronome reports one total for: neither is reconciled", other, t.ID, cus.ID)
				notes[other] = fmt.Sprintf("an alias of customer %s with other tenants, which Metronome reports one total for: not reconciled per tenant", cus.ID)
			}
			notes[t.ID] = fmt.Sprintf("an alias of customer %s with other tenants, which Metronome reports one total for: not reconciled per tenant", cus.ID)
			shared[cus.ID] = true
			delete(tenantOf, cus.ID)
			continue
		}
		tenantOf[cus.ID] = t.ID
		req.CustomerIDs = append(req.CustomerIDs, cus.ID)
	}
	bms, err := a.c.ListBillableMetrics(ctx)
	if err != nil {
		return fmt.Errorf("list billable metrics: %w", err)
	}
	for _, m := range q.Metrics {
		def, ok := usage.Lookup(m)
		if !ok {
			continue
		}
		bm := completeMetric(bms, def)
		if bm == nil {
			log.Printf("[metronome] reconcile: no billable metric sums the value of every %s event (without filters on values), so Metronome's total for it reads 0", m)
			continue
		}
		req.BillableMetrics = append(req.BillableMetrics, map[string]any{"id": bm.ID})
		codeOf[bm.ID] = m
	}
	return nil
}

var (
	_ destinations.Destination      = (*Adapter)(nil)
	_ destinations.EventFilter      = (*Adapter)(nil)
	_ destinations.Reconciler       = (*Adapter)(nil)
	_ destinations.CustomerResolver = (*Adapter)(nil)
)
