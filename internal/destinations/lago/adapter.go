package lago

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/loft-sh/vbilling/internal/config"
	"github.com/loft-sh/vbilling/internal/destinations"
)

func init() {
	destinations.Register("lago", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.LagoAPIKey == "" {
			return nil, fmt.Errorf("LAGO_API_KEY is required when ADAPTER=lago")
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

func (a *Adapter) Bootstrap(ctx context.Context) error {
	return Bootstrap(a.client, a.cfg)
}

func (a *Adapter) EnsureTenant(ctx context.Context, t destinations.Tenant) error {
	meta := make([]CustomerMeta, 0, len(t.Metadata))
	for k, v := range t.Metadata {
		meta = append(meta, CustomerMeta{Key: k, Value: v})
	}
	cust := Customer{
		ExternalID: t.ExternalID,
		Name:       t.DisplayName,
		Currency:   t.Currency,
		Metadata:   meta,
	}
	if _, err := a.client.UpsertCustomer(cust); err != nil {
		// Metadata keys may already exist on the customer — retry without metadata.
		cust.Metadata = nil
		if _, err2 := a.client.UpsertCustomer(cust); err2 != nil {
			return fmt.Errorf("upsert customer: %w", err2)
		}
	}

	subID := subscriptionIDFor(t.ExternalID)
	if _, err := a.client.GetCurrentUsage(t.ExternalID, subID); err == nil {
		// Subscription already exists.
		return nil
	}

	sub := Subscription{
		ExternalCustomerID: t.ExternalID,
		PlanCode:           a.cfg.DefaultPlanCode,
		ExternalID:         subID,
	}
	if _, err := a.client.CreateSubscription(sub); err != nil {
		log.Printf("[lago] warning: could not create subscription %s: %v", subID, err)
	}
	return nil
}

func (a *Adapter) RemoveTenant(ctx context.Context, externalID string) error {
	return a.client.TerminateSubscription(subscriptionIDFor(externalID))
}

func (a *Adapter) SendEvents(ctx context.Context, events []destinations.UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	lagoEvents := make([]Event, 0, len(events))
	for _, ev := range events {
		props := map[string]interface{}{}
		for k, v := range ev.Properties {
			props[k] = v
		}
		// The metric's primary value is what Lago aggregates on. Lago's
		// billable metrics use a sum_agg over a named property field, so we
		// repeat the value under the metric's field name. Existing properties
		// (gpu_type, vcluster_name, etc.) flow through untouched.
		props[fieldNameFor(ev.MetricCode)] = ev.Value
		lagoEvents = append(lagoEvents, Event{
			TransactionID:          ev.TransactionID,
			ExternalSubscriptionID: subscriptionIDFor(ev.TenantExternalID),
			Code:                   ev.MetricCode,
			Timestamp:              ev.Timestamp.Unix(),
			Properties:             props,
		})
	}

	// Lago's batch endpoint caps at 100 events per call.
	for i := 0; i < len(lagoEvents); i += 100 {
		end := i + 100
		if end > len(lagoEvents) {
			end = len(lagoEvents)
		}
		batch := lagoEvents[i:end]
		if err := a.client.SendEvents(batch); err != nil {
			return fmt.Errorf("send %d events: %w", len(batch), err)
		}
		log.Printf("[lago] sent %d billing events", len(batch))
	}
	return nil
}

// subscriptionIDFor derives a stable Lago subscription ID from a tenant external ID.
// Tenants are 1:1 with subscriptions in vBilling's Lago model.
func subscriptionIDFor(externalID string) string {
	return "sub-" + externalID
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

// Compile-time check.
var _ destinations.Destination = (*Adapter)(nil)

// silence unused import in some build configurations
var _ = time.Second
