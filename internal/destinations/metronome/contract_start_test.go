package metronome

import (
	"context"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A new contract starts at UTC midnight of the usage it is created for
// (TestReview2ContractCoversTheUsageItIsCreatedFor), but never where another
// vBilling contract of the tenant still runs: a rate card change with a
// backlog of older usage hands over today, so no day is rated twice.
func TestContractStartNeverOverlapsAnOpenContract(t *testing.T) {
	f := newFakeMetronome(t)
	a := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	yesterday := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	ctx := destinations.WithUsageStart(context.Background(), yesterday.Add(3*time.Hour))

	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	payg := f.contract(t, "payg", "2026-10-04") // the usage's day, not today
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme", Plan: "committed"}); err != nil {
		t.Fatal(err)
	}
	committed := f.contract(t, "committed", "2026-10-05")
	if f.ended[payg] != "2026-10-05T00:00:00Z" || f.ended[committed] != "" {
		t.Fatalf("handover: payg ends %q, committed ends %q", f.ended[payg], f.ended[committed])
	}

	// Usage dated after today (a clock ahead) never starts a contract later.
	b := newAdapter(t, f, nil, func(c *config.Config) { c.MetronomeRateCard = "payg" })
	if err := b.EnsureTenant(destinations.WithUsageStart(context.Background(), now.Add(48*time.Hour)), usage.Tenant{ID: "globex"}); err != nil {
		t.Fatal(err)
	}
	f.contract(t, "payg", "2026-10-05")
}
