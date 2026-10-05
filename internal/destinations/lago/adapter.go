package lago

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func init() {
	destinations.Register("lago", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.LagoAPIKey == "" {
			return nil, fmt.Errorf("LAGO_API_KEY is required when the lago adapter is enabled")
		}
		return &Adapter{
			client: NewClient(cfg.LagoAPIURL, cfg.LagoAPIKey),
			cfg:    cfg,
		}, nil
	})
}

// Adapter ships usage events to Lago.
type Adapter struct {
	client *Client
	cfg    *config.Config
}

func (a *Adapter) Name() string { return "lago" }

// Accepts billable metrics plus GPU utilization, which v0.1 created as a
// Lago billable metric and existing plans may still reference.
func (a *Adapter) Accepts(metric string) bool {
	return destinations.BillableOnly(metric) || metric == MetricGPUUtilization
}

func (a *Adapter) Bootstrap(ctx context.Context, catalog []usage.MetricDef) error {
	return Bootstrap(ctx, a.client, a.cfg, catalog)
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
	if _, err := a.client.CreateSubscription(ctx, sub); err != nil {
		// Returned, not swallowed: without a subscription Lago drops the
		// tenant's events, so EnsureTenant must be retried.
		return fmt.Errorf("create subscription %s on plan %s: %w", subID, plan, err)
	}
	log.Printf("[lago] subscribed tenant %s to plan %s", t.ID, plan)
	return nil
}

// RemoveTenant terminates the subscription, which makes Lago issue the final
// invoice. vBilling only calls it after the offboarding grace period.
func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	return a.client.TerminateSubscription(ctx, subscriptionIDFor(t.ID))
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
	for i := range events {
		if a.Accepts(events[i].Metric) {
			lagoEvents = append(lagoEvents, ToEvent(&events[i]))
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
