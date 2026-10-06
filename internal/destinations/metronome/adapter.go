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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/stripe"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
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
		if cfg.MetronomeStripeLink {
			if cfg.StripeAPIKey == "" {
				return nil, errors.New("METRONOME_STRIPE_LINK needs STRIPE_API_KEY to create Stripe customers")
			}
			sstate, err := kv.Open(filepath.Join(cfg.DataDir, "state-metronome-stripe.json"))
			if err != nil {
				return nil, err
			}
			link = stripe.New(stripe.NewClient(cfg.StripeAPIBase, cfg.StripeAPIKey, cfg.StripeAPIVersion, cfg.StripeMaxRPS), cfg, sstate)
		}
		return New(NewClient(cfg.MetronomeAPIBase, cfg.MetronomeAPIToken), cfg, state, link), nil
	})
}

// Metronome accepts events backdated up to 34 days; keep an hour of margin.
const maxEventAge = 34*24*time.Hour - time.Hour

// placeholder fills group-key properties an event does not carry: Metronome
// metrics require every group-key property to exist.
const placeholder = "none"

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Adapter ships usage to Metronome. Tenants become customers whose ingest
// alias is the vBilling tenant ID, so events are attributed even if they
// arrive before the customer exists.
type Adapter struct {
	c      *Client
	cfg    *config.Config
	state  *kv.Store
	stripe *stripe.Adapter // nil unless METRONOME_STRIPE_LINK
	now    func() time.Time

	mu      sync.Mutex
	metrics map[string]string // metric code -> billable metric ID
}

// New builds the adapter (exported for tests). link may be nil.
func New(c *Client, cfg *config.Config, state *kv.Store, link *stripe.Adapter) *Adapter {
	return &Adapter{c: c, cfg: cfg, state: state, stripe: link, now: time.Now, metrics: map[string]string{}}
}

func (a *Adapter) Name() string { return "metronome" }

// StripeLink returns the Stripe customer manager used for invoicing (nil
// unless METRONOME_STRIPE_LINK). It resolves Stripe webhooks to tenants.
func (a *Adapter) StripeLink() *stripe.Adapter { return a.stripe }

func (a *Adapter) Accepts(metric string) bool { return destinations.BillableOnly(metric) }

// GroupKeys is the single compound group key vBilling creates per metric:
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

// Bootstrap creates one SUM billable metric per billable metric code,
// filtered on event_type and grouped on GroupKeys. Existing metrics are
// matched by stored ID or by name, never duplicated.
func (a *Adapter) Bootstrap(ctx context.Context, metrics []usage.MetricDef) error {
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
		id, err := a.c.CreateBillableMetric(ctx, bm)
		if err != nil {
			errs = append(errs, fmt.Errorf("create billable metric %s: %w", def.Code, err))
			continue
		}
		log.Printf("[metronome] created billable metric %s (%s)", def.Code, id)
		a.setMetric(def.Code, id)
	}
	return errors.Join(errs...)
}

func (a *Adapter) setMetric(code, id string) {
	a.mu.Lock()
	a.metrics[code] = id
	a.mu.Unlock()
	_ = a.state.Set("bm/"+code, id)
	_ = a.state.Set("bmcode/"+id, code)
}

// EnsureTenant creates the Metronome customer (and the linked Stripe
// customer when configured) and, with a rate card configured, a contract.
func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
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

func (a *Adapter) ensureCustomer(ctx context.Context, t usage.Tenant) (string, error) {
	if id := t.ProviderIDs["metronome"]; id != "" {
		a.remember(t.ID, id)
		return id, nil
	}
	if id, ok := a.state.Get("cus/" + t.ID); ok {
		return id, nil
	}
	if cus, err := a.c.FindCustomerByAlias(ctx, t.ID); err != nil {
		return "", fmt.Errorf("find customer %s: %w", t.ID, err)
	} else if cus != nil {
		a.remember(t.ID, cus.ID)
		return cus.ID, nil
	}

	req := CreateCustomerRequest{Name: t.DisplayName, IngestAliases: []string{t.ID}}
	if req.Name == "" {
		req.Name = t.ID
	}
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
		// Alias taken: someone (or an earlier attempt) created it already.
		if cus, ferr := a.c.FindCustomerByAlias(ctx, t.ID); ferr == nil && cus != nil {
			a.remember(t.ID, cus.ID)
			return cus.ID, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("create customer %s: %w", t.ID, err)
	}
	log.Printf("[metronome] created customer %s for tenant %s", cus.ID, t.ID)
	a.remember(t.ID, cus.ID)
	if stripeID != "" {
		_ = a.state.Set("link/"+cus.ID, stripeID)
	}
	return cus.ID, nil
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
		if keep, err = a.createContract(ctx, t, cusID, rateCard, today, keys); err != nil {
			return err
		}
	}
	if err := a.endOtherContracts(ctx, t, cusID, ours, *keep, today); err != nil {
		return err
	}
	return a.state.Set("contract-current/"+t.ID, rateCard)
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
					return &c, nil
				}
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("create contract for %s on %s: %w", t.ID, rateCard, err)
		}
		log.Printf("[metronome] contract %s for tenant %s on rate card %s from %s", id, t.ID, rateCard, start.Format("2006-01-02"))
		return &Contract{ID: id, UniquenessKey: key, StartingAt: rfc3339(start)}, nil
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
		var err error
		if start.Before(at) {
			err = a.c.EndContract(ctx, cusID, c.ID, at)
		} else {
			err = a.c.ArchiveContract(ctx, cusID, c.ID)
		}
		switch {
		case err == nil:
			log.Printf("[metronome] contract %s of tenant %s ended at %s, where contract %s takes over", c.ID, t.ID, rfc3339(at), keep.ID)
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

// TenantForCustomer resolves a webhook's customer_id to a tenant.
func (a *Adapter) TenantForCustomer(ctx context.Context, customerID string) (string, bool) {
	if t, ok := a.state.Get("tenant/" + customerID); ok {
		return t, true
	}
	cus, err := a.c.GetCustomer(ctx, customerID)
	if err != nil || len(cus.IngestAliases) == 0 {
		return "", false
	}
	a.remember(cus.IngestAliases[0], customerID)
	return cus.IngestAliases[0], true
}

// ToIngest converts a canonical event to Metronome's wire format.
func (a *Adapter) ToIngest(e *usage.Event) IngestEvent {
	props := map[string]string{
		"value":          strconv.FormatFloat(e.Quantity, 'f', -1, 64),
		"unit":           e.Unit,
		"window_start":   rfc3339(e.WindowStart),
		"window_end":     rfc3339(e.WindowEnd),
		"schema_version": e.SchemaVersion,
	}
	set := func(k, v string) {
		if v != "" {
			props[k] = v
		}
	}
	set("region", e.Region)
	set("project", e.Project)
	set("sku", e.SKU)
	set("resource_id", e.ResourceID)
	set("source", e.Source)
	for k, v := range e.Dimensions {
		set(k, v)
	}
	if def, ok := usage.Lookup(e.Metric); ok {
		for _, k := range GroupKeys(def) {
			if props[k] == "" {
				props[k] = placeholder
			}
		}
	}
	customer := e.Tenant
	if id, ok := a.state.Get("cus/" + e.Tenant); ok {
		customer = id // pinned or created customer UUID
	}
	return IngestEvent{
		TransactionID: e.ID,
		CustomerID:    customer,
		EventType:     e.Metric,
		Timestamp:     rfc3339(e.WindowStart),
		Properties:    props,
	}
}

// SendEvents ingests in batches of 100. Retrying a batch is safe: Metronome
// ignores repeated transaction IDs for 34 days.
func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	rejected := map[string]string{}
	var batch []IngestEvent
	for i := range events {
		e := &events[i]
		if !a.Accepts(e.Metric) {
			continue
		}
		if a.now().Sub(e.WindowStart) > maxEventAge {
			rejected[e.ID] = "older than Metronome's 34-day ingest window"
			continue
		}
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
// whole UTC days, so both bounds must be UTC midnights.
func (a *Adapter) RecordedTotals(ctx context.Context, q destinations.TotalsQuery) ([]destinations.Total, error) {
	from, to := q.From.UTC(), q.To.UTC()
	if !from.Equal(from.Truncate(24*time.Hour)) || !to.Equal(to.Truncate(24*time.Hour)) || !to.After(from) {
		return nil, fmt.Errorf("metronome reconciliation needs whole UTC days (got %s to %s)", rfc3339(from), rfc3339(to))
	}
	req := UsageRequest{StartingOn: rfc3339(from), EndingBefore: rfc3339(to), WindowSize: "day"}
	tenantOf := map[string]string{}
	for _, t := range q.Tenants {
		if id, ok := a.state.Get("cus/" + t.ID); ok {
			req.CustomerIDs = append(req.CustomerIDs, id)
			tenantOf[id] = t.ID
		}
	}
	codeOf := map[string]string{}
	for _, m := range q.Metrics {
		if id, ok := a.state.Get("bm/" + m); ok {
			req.BillableMetrics = append(req.BillableMetrics, map[string]any{"id": id})
			codeOf[id] = m
		}
	}
	if len(req.CustomerIDs) == 0 || len(req.BillableMetrics) == 0 {
		return nil, nil
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
	var out []destinations.Total
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

var (
	_ destinations.Destination      = (*Adapter)(nil)
	_ destinations.EventFilter      = (*Adapter)(nil)
	_ destinations.Reconciler       = (*Adapter)(nil)
	_ destinations.CustomerResolver = (*Adapter)(nil)
)
