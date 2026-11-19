package metronome

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestReview2ContractCoversTheUsageItIsCreatedFor: the dispatcher creates a
// tenant in a billing platform with the start of the usage it is about to
// deliver (destinations.WithUsageStart). Stripe and Lago start the
// subscription there; Metronome's contract always starts at UTC midnight of
// the day vBilling creates it. A new tenant whose first usage is delivered
// after midnight (a backlog after a Metronome or vBilling outage, a
// backfill, the last window of the day) gets a contract that starts after
// that usage, and Metronome rates usage only under a contract that covers
// its timestamp.
func TestReview2ContractCoversTheUsageItIsCreatedFor(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg-aud" })
	// A tenant whose first window is 22:00 yesterday is delivered today at
	// 12:00 (now), after an outage.
	first := now.Truncate(24 * time.Hour).Add(-2 * time.Hour)
	ctx := destinations.WithUsageStart(context.Background(), first)
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	if len(f.contracts) != 1 {
		t.Fatalf("contracts = %d", len(f.contracts))
	}
	for _, c := range f.contracts {
		start, _ := time.Parse(time.RFC3339, c.StartingAt)
		if start.After(first) {
			t.Errorf("contract starts %s, after the usage it was created for (%s): Metronome never rates that usage",
				c.StartingAt, first.Format(time.RFC3339))
		}
	}
}
