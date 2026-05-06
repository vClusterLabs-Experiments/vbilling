// Package noop is a destination adapter that logs events without sending them anywhere.
// Useful for dry-run validation and integration testing without a billing backend.
package noop

import (
	"context"
	"log"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
)

func init() {
	destinations.Register("noop", func(cfg *config.Config) (destinations.Destination, error) {
		return &Adapter{}, nil
	})
}

type Adapter struct{}

func (a *Adapter) Name() string { return "noop" }

func (a *Adapter) Bootstrap(ctx context.Context) error {
	log.Println("[noop] bootstrap (no-op)")
	return nil
}

func (a *Adapter) EnsureTenant(ctx context.Context, t destinations.Tenant) error {
	log.Printf("[noop] ensure tenant %s (%s)", t.ExternalID, t.DisplayName)
	return nil
}

func (a *Adapter) RemoveTenant(ctx context.Context, externalID string) error {
	log.Printf("[noop] remove tenant %s", externalID)
	return nil
}

func (a *Adapter) SendEvents(ctx context.Context, events []destinations.UsageEvent) error {
	for _, ev := range events {
		log.Printf("[noop] event tenant=%s metric=%s value=%v ts=%s",
			ev.TenantExternalID, ev.MetricCode, ev.Value, ev.Timestamp.Format("15:04:05"))
	}
	return nil
}

var _ destinations.Destination = (*Adapter)(nil)
