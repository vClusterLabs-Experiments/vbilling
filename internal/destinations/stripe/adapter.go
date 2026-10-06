package stripe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func init() {
	destinations.Register("stripe", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.StripeAPIKey == "" {
			return nil, errors.New("STRIPE_API_KEY is required when the stripe adapter is enabled")
		}
		state, err := kv.Open(filepath.Join(cfg.DataDir, "state-stripe.json"))
		if err != nil {
			return nil, fmt.Errorf("open stripe state: %w", err)
		}
		return New(NewClient(cfg.StripeAPIBase, cfg.StripeAPIKey, cfg.StripeAPIVersion, cfg.StripeMaxRPS), cfg, state), nil
	})
}

// Stripe accepts meter events up to 35 days old; keep an hour of margin.
const maxEventAge = 35*24*time.Hour - time.Hour

// Metadata keys vBilling owns on Stripe objects.
const (
	metaTenant = "vbilling_tenant_id"
	metaPlan   = "vbilling_plan"
)

// Adapter ships usage to Stripe Billing Meters.
//
// Stripe meters cannot price on dimensions, so events are routed to one
// meter per (metric, split values): with the default split on "sku", H100
// and L40S GPU-hours land on separate meters and can carry separate prices.
type Adapter struct {
	c     *Client
	cfg   *config.Config
	state *kv.Store
	sent  *destinations.SentCache
	now   func() time.Time

	meterMu      sync.Mutex
	meters       map[string]Meter // event_name -> meter
	metersLoaded bool

	cusMu sync.Mutex // serializes customer creation

	priceMu    sync.Mutex
	prices     []Price
	pricesTime time.Time

	workers int
}

// New builds the adapter around a client (exported for tests).
func New(c *Client, cfg *config.Config, state *kv.Store) *Adapter {
	return &Adapter{
		c: c, cfg: cfg, state: state,
		sent:    destinations.NewSentCache(200_000),
		now:     time.Now,
		meters:  map[string]Meter{},
		workers: 4,
	}
}

func (a *Adapter) Name() string { return "stripe" }

// Accepts skips informational metrics; Stripe would bill anything it meters.
func (a *Adapter) Accepts(metric string) bool { return destinations.BillableOnly(metric) }

// --- meters ---

func (a *Adapter) loadMetersLocked(ctx context.Context) error {
	if a.metersLoaded {
		return nil
	}
	ms, err := a.c.ListMeters(ctx)
	if err != nil {
		return fmt.Errorf("list meters: %w", err)
	}
	for _, m := range ms {
		a.meters[m.EventName] = m
	}
	a.metersLoaded = true
	return nil
}

// ensureMeter returns the meter for an event name, creating or reactivating it.
func (a *Adapter) ensureMeter(ctx context.Context, eventName, displayName string) (Meter, error) {
	a.meterMu.Lock()
	defer a.meterMu.Unlock()
	if err := a.loadMetersLocked(ctx); err != nil {
		return Meter{}, err
	}
	if m, ok := a.meters[eventName]; ok {
		if m.Status == "inactive" {
			m, err := a.c.ReactivateMeter(ctx, m.ID)
			if err != nil {
				return Meter{}, fmt.Errorf("reactivate meter %s: %w", eventName, err)
			}
			a.meters[eventName] = m
			return m, nil
		}
		return m, nil
	}
	m, err := a.c.CreateMeter(ctx, eventName, displayName)
	if err != nil {
		return Meter{}, fmt.Errorf("create meter %s: %w", eventName, err)
	}
	log.Printf("[stripe] created meter %s (%s)", eventName, m.ID)
	a.meters[eventName] = m
	_ = a.state.Set("meter/"+eventName, m.ID)
	return m, nil
}

// MeterName maps an event to its Stripe meter event_name.
func (a *Adapter) MeterName(e *usage.Event) string {
	name := e.Metric
	for _, key := range a.cfg.StripeSplitMetersBy {
		if v := fieldValue(e, key); v != "" {
			name += "__" + slug(v)
		}
	}
	if len(name) > 100 {
		sum := sha256.Sum256([]byte(name))
		name = name[:91] + "_" + hex.EncodeToString(sum[:4])
	}
	return name
}

func meterDisplayName(e *usage.Event, eventName string) string {
	def, ok := usage.Lookup(e.Metric)
	if !ok {
		return eventName
	}
	if suffix := strings.TrimPrefix(eventName, e.Metric); suffix != "" {
		return def.Name + " (" + strings.ReplaceAll(strings.TrimPrefix(suffix, "__"), "__", ", ") + ")"
	}
	return def.Name
}

func fieldValue(e *usage.Event, key string) string {
	switch key {
	case "sku":
		return e.SKU
	case "region":
		return e.Region
	case "project":
		return e.Project
	default:
		return e.Dim(key)
	}
}

func slug(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

// Bootstrap creates (or reactivates) one meter per billable metric. Split
// meters (per SKU, per region, ...) are created on first use.
func (a *Adapter) Bootstrap(ctx context.Context, metrics []usage.MetricDef) error {
	var errs []error
	for _, def := range metrics {
		if !def.Billable {
			continue
		}
		if _, err := a.ensureMeter(ctx, def.Code, def.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- customers ---

func (a *Adapter) customerFor(ctx context.Context, t usage.Tenant) (string, error) {
	if id := t.ProviderIDs["stripe"]; id != "" {
		a.remember(t.ID, id)
		return id, nil
	}
	if id, ok := a.state.Get("cus/" + t.ID); ok {
		return id, nil
	}
	a.cusMu.Lock()
	defer a.cusMu.Unlock()
	if id, ok := a.state.Get("cus/" + t.ID); ok { // created while we waited
		return id, nil
	}
	cus, err := a.c.FindCustomer(ctx, metaTenant, t.ID, t.Email)
	if err != nil {
		return "", fmt.Errorf("search customer %s: %w", t.ID, err)
	}
	if cus == nil {
		f := customerForm(t)
		// Hour-bucketed key: retries within the hour replay, and a cached
		// 5xx cannot block creation for Stripe's full 24h key retention.
		key := fmt.Sprintf("vbilling-cus-%s-%s", shortHash(t.ID), a.now().UTC().Format("2006010215"))
		if cus, err = a.c.CreateCustomer(ctx, f, key); err != nil {
			return "", fmt.Errorf("create customer %s: %w", t.ID, err)
		}
		log.Printf("[stripe] created customer %s for tenant %s", cus.ID, t.ID)
	}
	a.remember(t.ID, cus.ID)
	return cus.ID, nil
}

func (a *Adapter) remember(tenant, cus string) {
	_ = a.state.Set("cus/"+tenant, cus)
	_ = a.state.Set("tenant/"+cus, tenant)
}

func customerForm(t usage.Tenant) url.Values {
	f := url.Values{}
	name := t.DisplayName
	if name == "" {
		name = t.ID
	}
	f.Set("name", truncate(name, 256))
	if t.Email != "" {
		f.Set("email", t.Email)
	}
	f.Set("metadata["+metaTenant+"]", t.ID)
	set := func(k, v string) {
		if v != "" {
			f.Set("metadata["+k+"]", truncate(v, 500))
		}
	}
	set("vbilling_region", t.Region)
	set("vbilling_project", t.Project)
	set("vbilling_tenant_class", t.Class)
	set(metaPlan, t.Plan)
	set("vbilling_clusters", strings.Join(t.Clusters, ","))
	return f
}

// EnsureTenant creates or updates the Stripe customer and, when
// STRIPE_AUTO_SUBSCRIBE is on, subscribes it to the tenant plan's prices.
func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	cusID, err := a.EnsureCustomer(ctx, t)
	if err != nil {
		return err
	}
	if a.cfg.StripeAutoSubscribe {
		return a.ensureSubscription(ctx, t, cusID)
	}
	return nil
}

// EnsureCustomer creates or updates the tenant's Stripe customer and returns
// its ID. The Metronome adapter uses it to link customers for invoicing.
func (a *Adapter) EnsureCustomer(ctx context.Context, t usage.Tenant) (string, error) {
	cusID, err := a.customerFor(ctx, t)
	if err != nil {
		return "", err
	}
	f := customerForm(t)
	fp := shortHash(f.Encode())
	if prev, _ := a.state.Get("cusfp/" + t.ID); prev != fp {
		if err := a.c.UpdateCustomer(ctx, cusID, f); err != nil {
			if IsCode(err, "resource_missing") {
				// Deleted in Stripe: forget it so the next pass recreates it.
				_ = a.state.Delete("cus/" + t.ID)
				_ = a.state.Delete("tenant/" + cusID)
			}
			return "", fmt.Errorf("update customer %s: %w", cusID, err)
		}
		_ = a.state.Set("cusfp/"+t.ID, fp)
	}
	return cusID, nil
}

// planPrices returns active metered prices on vBilling-managed meters tagged
// metadata[vbilling_plan]=plan in the tenant's currency.
func (a *Adapter) planPrices(ctx context.Context, plan, currency string) ([]Price, error) {
	a.priceMu.Lock()
	if a.prices == nil || a.now().Sub(a.pricesTime) > 5*time.Minute {
		ps, err := a.c.ListMeteredPrices(ctx)
		if err != nil {
			a.priceMu.Unlock()
			return nil, fmt.Errorf("list prices: %w", err)
		}
		a.prices, a.pricesTime = ps, a.now()
	}
	all := a.prices
	a.priceMu.Unlock()

	a.meterMu.Lock()
	managed := map[string]bool{}
	for _, m := range a.meters {
		managed[m.ID] = true
	}
	a.meterMu.Unlock()

	var out []Price
	for _, p := range all {
		if !p.Active || p.Recurring == nil || p.Recurring.UsageType != "metered" || !managed[p.Recurring.Meter] {
			continue
		}
		if p.Metadata[metaPlan] != plan || !strings.EqualFold(p.Currency, currency) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (a *Adapter) ensureSubscription(ctx context.Context, t usage.Tenant, cusID string) error {
	plan := t.Plan
	if plan == "" {
		plan = a.cfg.DefaultPlanCode
	}
	currency := t.Currency
	if currency == "" {
		currency = a.cfg.BillingCurrency
	}
	prices, err := a.planPrices(ctx, plan, currency)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		log.Printf("[stripe] no active metered prices tagged %s=%s in %s; tenant %s not subscribed", metaPlan, plan, currency, t.ID)
		return nil
	}
	if len(prices) > 20 {
		// Permanent: retrying cannot help, and a retryable error would hold up
		// every tenant's usage. The customer exists, so usage keeps flowing.
		return destinations.Permanent(fmt.Errorf("plan %s has %d prices; Stripe allows 20 items per subscription", plan, len(prices)))
	}
	subs, err := a.c.ListSubscriptions(ctx, cusID)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	for _, s := range subs {
		if s.Metadata[metaTenant] != t.ID || s.Status == "canceled" || s.Status == "incomplete_expired" {
			continue
		}
		have := map[string]bool{}
		for _, it := range s.Items.Data {
			have[it.Price.ID] = true
		}
		for _, p := range prices {
			if !have[p.ID] {
				if err := a.c.AddSubscriptionItem(ctx, s.ID, p.ID); err != nil {
					return fmt.Errorf("add price %s to %s: %w", p.ID, s.ID, err)
				}
				log.Printf("[stripe] added price %s to subscription %s", p.ID, s.ID)
			}
		}
		_ = a.state.Set("sub/"+t.ID, s.ID)
		return nil
	}
	ids := make([]string, len(prices))
	for i, p := range prices {
		ids[i] = p.ID
	}
	key := fmt.Sprintf("vbilling-sub-%s-%s", shortHash(t.ID+"|"+plan), a.now().UTC().Format("2006010215"))
	sub, err := a.c.CreateSubscription(ctx, cusID, ids, map[string]string{metaTenant: t.ID, metaPlan: plan}, key)
	if err != nil {
		return fmt.Errorf("create subscription: %w", err)
	}
	log.Printf("[stripe] subscribed tenant %s to plan %s (%s, %d prices)", t.ID, plan, sub.ID, len(ids))
	return a.state.Set("sub/"+t.ID, sub.ID)
}

// RemoveTenant optionally cancels the subscription at period end so final
// usage is still invoiced. Customers are never deleted.
func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	if !a.cfg.StripeCancelOnRemove {
		log.Printf("[stripe] tenant %s offboarded; customer left in place (STRIPE_CANCEL_ON_REMOVE=false)", t.ID)
		return nil
	}
	subID, ok := a.state.Get("sub/" + t.ID)
	if !ok {
		return nil
	}
	return a.c.CancelAtPeriodEnd(ctx, subID)
}

// TenantForCustomer resolves an inbound webhook's customer to a tenant.
func (a *Adapter) TenantForCustomer(ctx context.Context, cusID string) (string, bool) {
	if t, ok := a.state.Get("tenant/" + cusID); ok {
		return t, true
	}
	cus, err := a.c.GetCustomer(ctx, cusID)
	if err != nil || cus.Metadata[metaTenant] == "" {
		return "", false
	}
	a.remember(cus.Metadata[metaTenant], cusID)
	return cus.Metadata[metaTenant], true
}

// --- events ---

type job struct {
	ev    usage.Event
	meter Meter
	cus   string
}

// SendEvents posts each event to its meter. Work is partitioned by
// (customer, meter) because Stripe allows one concurrent meter-event call
// per customer per meter.
func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	rejected := map[string]string{}
	var jobs []job
	for _, ev := range events {
		if a.sent.Has(ev.ID) || !a.Accepts(ev.Metric) {
			continue
		}
		if a.now().Sub(ev.WindowStart) > maxEventAge {
			rejected[ev.ID] = "older than Stripe's 35-day meter event window"
			continue
		}
		cus, err := a.customerFor(ctx, usage.Tenant{ID: ev.Tenant})
		if err != nil {
			return err
		}
		name := a.MeterName(&ev)
		m, err := a.ensureMeter(ctx, name, meterDisplayName(&ev, name))
		if err != nil {
			if destinations.IsPermanent(err) {
				rejected[ev.ID] = err.Error()
				continue
			}
			return err
		}
		jobs = append(jobs, job{ev: ev, meter: m, cus: cus})
	}

	parts := make([][]job, a.workers)
	for _, j := range jobs {
		h := fnv.New32a()
		h.Write([]byte(j.cus + "|" + j.meter.EventName))
		parts[h.Sum32()%uint32(a.workers)] = append(parts[h.Sum32()%uint32(a.workers)], j)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		retryErr error
	)
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		wg.Add(1)
		go func(part []job) {
			defer wg.Done()
			for _, j := range part {
				err := a.c.CreateMeterEvent(ctx, j.meter, j.cus, FormatValue(j.ev.Quantity), j.ev.ID, j.ev.WindowStart)
				switch {
				case err == nil || isDuplicate(err):
					a.sent.Add(j.ev.ID)
				case destinations.IsPermanent(err):
					mu.Lock()
					rejected[j.ev.ID] = err.Error()
					mu.Unlock()
				default:
					mu.Lock()
					if retryErr == nil {
						retryErr = err
					}
					mu.Unlock()
					return // stop this partition; the batch is retried
				}
			}
		}(part)
	}
	wg.Wait()
	if retryErr != nil {
		return retryErr
	}
	if len(rejected) > 0 {
		return &destinations.PartialError{Rejected: rejected}
	}
	return nil
}

// isDuplicate treats Stripe's duplicate-identifier rejection as delivered.
func isDuplicate(err error) bool {
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return false
	}
	return strings.Contains(apiErr.Code, "duplicate") ||
		strings.Contains(strings.ToLower(apiErr.Message), "already exists with identifier")
}

// FormatValue renders a quantity as a plain decimal with at most 15
// significant digits (Stripe's precision guard) and 9 decimals.
func FormatValue(q float64) string {
	if q == 0 || math.IsNaN(q) || math.IsInf(q, 0) {
		return "0"
	}
	intDigits := 1
	if a := math.Abs(q); a >= 1 {
		intDigits = int(math.Floor(math.Log10(a))) + 1
	}
	decimals := 15 - intDigits
	if decimals > 9 {
		decimals = 9
	}
	if decimals < 0 {
		decimals = 0
	}
	s := strconv.FormatFloat(q, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// --- reconciliation ---

// RecordedTotals sums Stripe's meter event summaries per tenant and metric,
// across every split meter of that metric. Bounds are truncated to minutes.
func (a *Adapter) RecordedTotals(ctx context.Context, q destinations.TotalsQuery) ([]destinations.Total, error) {
	from, to := q.From.UTC().Truncate(time.Minute), q.To.UTC().Truncate(time.Minute)
	a.meterMu.Lock()
	if err := a.loadMetersLocked(ctx); err != nil {
		a.meterMu.Unlock()
		return nil, err
	}
	byMetric := map[string][]Meter{}
	for name, m := range a.meters {
		for _, metric := range q.Metrics {
			if name == metric || strings.HasPrefix(name, metric+"__") {
				byMetric[metric] = append(byMetric[metric], m)
			}
		}
	}
	a.meterMu.Unlock()

	var out []destinations.Total
	for _, t := range q.Tenants {
		cus, ok := a.state.Get("cus/" + t.ID)
		if !ok {
			continue
		}
		for _, metric := range q.Metrics {
			var sum float64
			for _, m := range byMetric[metric] {
				v, err := a.c.MeterTotal(ctx, m.ID, cus, from, to)
				if err != nil {
					return nil, fmt.Errorf("event summaries %s/%s: %w", t.ID, m.EventName, err)
				}
				sum += v
			}
			out = append(out, destinations.Total{Tenant: t.ID, Metric: metric, Quantity: usage.Round(sum)})
		}
	}
	return out, nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

var (
	_ destinations.Destination      = (*Adapter)(nil)
	_ destinations.EventFilter      = (*Adapter)(nil)
	_ destinations.Reconciler       = (*Adapter)(nil)
	_ destinations.CustomerResolver = (*Adapter)(nil)
)
