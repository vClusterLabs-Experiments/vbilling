// Package noop is a destination adapter that logs events without sending them anywhere.
// Useful for dry-run validation and integration testing without a billing backend.
package noop

import (
	"context"
	"log"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func init() {
	destinations.Register("noop", func(cfg *config.Config) (destinations.Destination, error) {
		return &Adapter{}, nil
	})
}

type Adapter struct{}

func (a *Adapter) Name() string { return "noop" }

func (a *Adapter) Bootstrap(ctx context.Context, metrics []usage.MetricDef) error {
	log.Printf("[noop] bootstrap: %d metrics in catalog", len(metrics))
	return nil
}

func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	log.Printf("[noop] ensure tenant %s (%s) clusters=%v", t.ID, t.DisplayName, t.Clusters)
	return nil
}

func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	log.Printf("[noop] remove tenant %s", t.ID)
	return nil
}

func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	for _, ev := range events {
		log.Printf("[noop] event tenant=%s metric=%s qty=%v %s sku=%s region=%s window=%s",
			ev.Tenant, ev.Metric, ev.Quantity, ev.Unit, ev.SKU, ev.Region, ev.WindowStart.Format("15:04:05"))
	}
	return nil
}

var _ destinations.Destination = (*Adapter)(nil)
