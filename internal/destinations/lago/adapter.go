package lago

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func init() {
	destinations.Register("lago", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.LagoAPIKey == "" {
			return nil, fmt.Errorf("LAGO_API_KEY is required when the lago adapter is enabled")
		}
		card, err := LoadPriceCard(cfg.LagoPriceCard)
		if err != nil {
			return nil, fmt.Errorf("LAGO_PRICE_CARD: %w", err)
		}
		state, err := kv.Open(filepath.Join(cfg.DataDir, "state-lago.json"))
		if err != nil {
			return nil, fmt.Errorf("open lago state: %w", err)
		}
		return New(NewClient(cfg.LagoAPIURL, cfg.LagoAPIKey), cfg, state, card), nil
	})
}

// Adapter ships usage events to Lago.
type Adapter struct {
	client    *Client
	cfg       *config.Config
	state     *kv.Store // charges and SKU filters already set up in Lago
	priceCard PriceCard
	now       func() time.Time

	// planMu serializes read-modify-write updates of plans and billable
	// metrics: Lago has no conditional update, and a plan update built
	// from a stale read deletes what another update just added.
	planMu   sync.Mutex
	skuRetry map[string]retry // metric -> when to retry its SKU filters
}

// New builds the adapter around a client (exported for tests). A nil state
// keeps adapter state in memory only.
func New(c *Client, cfg *config.Config, state *kv.Store, card PriceCard) *Adapter {
	if state == nil {
		state = kv.Memory()
	}
	return &Adapter{client: c, cfg: cfg, state: state, priceCard: card, now: time.Now, skuRetry: map[string]retry{}}
}

func (a *Adapter) Name() string { return "lago" }

// managedPlans lists the plans whose charges and SKU filters vBilling keeps
// up to date: the default plan, then LAGO_MANAGED_PLANS. Other plans are
// never changed.
func (a *Adapter) managedPlans() []string {
	seen := map[string]bool{}
	var out []string
	for _, code := range append([]string{a.cfg.DefaultPlanCode}, a.cfg.LagoManagedPlans...) {
		if code != "" && !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}

// Accepts billable metrics plus GPU utilization, which v0.1 created as a
// Lago billable metric and existing plans may still reference.
func (a *Adapter) Accepts(metric string) bool {
	return destinations.BillableOnly(metric) || metric == MetricGPUUtilization
}

func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	meta := make([]CustomerMeta, 0, len(t.Metadata))
	for k, v := range t.Metadata {
		meta = append(meta, CustomerMeta{Key: k, Value: v})
	}
	currency := t.Currency
	if currency == "" {
		currency = a.cfg.BillingCurrency
	}
	cust := Customer{
		ExternalID: t.ID,
		Name:       t.DisplayName,
		Email:      t.Email,
		Currency:   currency,
		Metadata:   meta,
	}
	if _, err := a.client.UpsertCustomer(ctx, cust); err != nil {
		// Metadata keys may already exist on the customer; retry without metadata.
		cust.Metadata = nil
		if _, err2 := a.client.UpsertCustomer(ctx, cust); err2 != nil {
			return fmt.Errorf("upsert customer: %w", err2)
		}
	}

	subID := subscriptionIDFor(t.ID)
	if _, err := a.client.GetCurrentUsage(ctx, t.ID, subID); err == nil {
		return nil // subscription already exists
	}
	plan := t.Plan
	if plan == "" {
		plan = a.cfg.DefaultPlanCode
	}
	sub := Subscription{ExternalCustomerID: t.ID, PlanCode: plan, ExternalID: subID}
	usageStart, _ := destinations.UsageStart(ctx)
	if at := subscriptionStart(t.CreatedAt, usageStart, a.now()); !at.IsZero() {
		sub.SubscriptionAt = at.UTC().Format(time.RFC3339)
	}
	if _, err := a.client.CreateSubscription(ctx, sub); err != nil {
		// Returned, not swallowed: without a subscription Lago drops the
		// tenant's events, so EnsureTenant must be retried.
		return fmt.Errorf("create subscription %s on plan %s: %w", subID, plan, err)
	}
	if sub.SubscriptionAt != "" {
		log.Printf("[lago] subscribed tenant %s to plan %s from %s", t.ID, plan, sub.SubscriptionAt)
	} else {
		log.Printf("[lago] subscribed tenant %s to plan %s", t.ID, plan)
	}
	return nil
}

// subscriptionStart is when a new subscription starts. Lago counts usage from
// a subscription's start, and a subscription created when the first usage is
// delivered starts after that usage's window: the first window would never
// be billed. So it starts at the earliest of the usage being delivered and
// the tenant's creation, both in the past. The creation time alone never
// moves the start to an earlier UTC day than today: Lago v1.17 then skips the
// in-advance invoice it issues when a subscription starts today, and bills
// an arrears base fee from the earlier date, with no usage to show for it.
// Usage from earlier days (backfill, held usage) does move it: that usage
// must count.
func subscriptionStart(created, usageStart, now time.Time) time.Time {
	var start time.Time
	if !created.IsZero() && created.Before(now) {
		start = created
		if today := now.UTC().Truncate(24 * time.Hour); start.Before(today) {
			start = today
		}
	}
	if !usageStart.IsZero() && usageStart.Before(now) && (start.IsZero() || usageStart.Before(start)) {
		start = usageStart
	}
	return start
}

// RemoveTenant terminates the subscription, which makes Lago issue the final
// invoice. vBilling only calls it after the offboarding grace period.
func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	err := a.client.TerminateSubscription(ctx, subscriptionIDFor(t.ID))
	var httpErr *destinations.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound {
		return nil // already terminated, or never created: nothing to end
	}
	return err
}

// ToEvent converts a canonical event to Lago's wire format. Dimensions and
// top-level attributes become string properties, so Lago charge filters can
// price on gpu_type, sku, region or capacity_type.
func ToEvent(ev *usage.Event) Event {
	props := map[string]interface{}{}
	for k, v := range ev.Properties {
		props[k] = v
	}
	for k, v := range ev.Dimensions {
		props[k] = v
	}
	set := func(k, v string) {
		if v != "" {
			props[k] = v
		}
	}
	set("region", ev.Region)
	set("project", ev.Project)
	set("sku", ev.SKU)
	set("resource_id", ev.ResourceID)
	if tc := ev.Dim(usage.DimTenantCluster); tc != "" {
		// v0.1 property names, kept for existing Lago filters.
		set("vcluster", tc)
	}
	// Lago's sum_agg aggregates a named property: repeat the quantity under
	// the field name Bootstrap registered for this metric.
	props[fieldNameFor(ev.Metric)] = ev.Quantity // a JSON number, as v0.1 sent it
	return Event{
		TransactionID:          ev.ID,
		ExternalSubscriptionID: subscriptionIDFor(ev.Tenant),
		Code:                   ev.Metric,
		Timestamp:              ev.WindowStart.Unix(),
		Properties:             props,
	}
}

func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	lagoEvents := make([]Event, 0, len(events))
	skus := map[string][]string{} // metric -> distinct SKUs in this batch
	seen := map[[2]string]bool{}
	for i := range events {
		if a.Accepts(events[i].Metric) {
			lagoEvents = append(lagoEvents, ToEvent(&events[i]))
			if key := [2]string{events[i].Metric, events[i].SKU}; key[1] != "" && !seen[key] {
				seen[key] = true
				skus[key[0]] = append(skus[key[0]], key[1])
			}
		}
	}
	// Lago's batch endpoint caps at 100 events per call. Transaction IDs are
	// deterministic, so a resent window is deduplicated by Lago.
	for i := 0; i < len(lagoEvents); i += 100 {
		end := i + 100
		if end > len(lagoEvents) {
			end = len(lagoEvents)
		}
		batch := lagoEvents[i:end]
		if err := a.client.SendEvents(ctx, batch); err != nil {
			if isDuplicate(err) && len(batch) == 1 {
				continue // already recorded
			}
			return fmt.Errorf("send %d events: %w", len(batch), err)
		}
	}
	// After delivery, never before: SKU filters are best effort. Lago
	// matches filters when it aggregates, so the events just sent are
	// priced by the filters added here.
	a.syncSKUs(ctx, skus)
	return nil
}

// isDuplicate recognizes Lago's "transaction_id already exists" rejection.
func isDuplicate(err error) bool {
	var httpErr *destinations.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == 422 && strings.Contains(httpErr.Body, "value_already_exist")
}

// subscriptionIDFor derives a stable Lago subscription ID from a tenant ID.
// Tenants are 1:1 with subscriptions in vBilling's Lago model.
func subscriptionIDFor(tenantID string) string {
	return "sub-" + tenantID
}

// fieldNameFor maps a canonical metric code to the Lago BillableMetric.field_name
// that Bootstrap registered. Keep this in sync with bootstrap.go.
func fieldNameFor(code string) string {
	switch code {
	case MetricCPUCoreHours:
		return "cpu_core_hours"
	case MetricMemoryGBHours:
		return "memory_gb_hours"
	case MetricStorageGBHours:
		return "storage_gb_hours"
	case MetricInstanceHours:
		return "instance_hours"
	case MetricGPUHours:
		return "gpu_hours"
	case MetricGPUUtilization:
		return "gpu_util_score"
	case MetricNetworkEgressGB:
		return "egress_gb"
	case MetricLBHours:
		return "lb_hours"
	case MetricPrivateNodeHours:
		return "private_node_hours"
	default:
		return code
	}
}

var (
	_ destinations.Destination = (*Adapter)(nil)
	_ destinations.EventFilter = (*Adapter)(nil)
)
