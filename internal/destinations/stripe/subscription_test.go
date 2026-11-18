package stripe

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// subscribedAdapter returns an adapter with STRIPE_AUTO_SUBSCRIBE and one
// monthly GPU-hour price on the "standard" plan.
func subscribedAdapter(t *testing.T, f *fakeStripe, statePath string, interval string) *Adapter {
	t.Helper()
	a := newTestAdapter(t, f, statePath, func(c *config.Config) { c.StripeAutoSubscribe = true })
	if err := a.Bootstrap(context.Background(), usage.Catalog()); err != nil {
		t.Fatal(err)
	}
	for _, m := range f.meters {
		if m.EventName == usage.MetricGPUHours {
			p := Price{ID: "price_gpu", Active: true, Currency: "usd", Metadata: map[string]string{metaPlan: "standard"}}
			p.Recurring = &PriceRecurring{UsageType: "metered", Meter: m.ID, Interval: interval, IntervalCount: 1}
			f.prices = []Price{p}
		}
	}
	if len(f.prices) == 0 {
		t.Fatal("no GPU-hour meter")
	}
	return a
}

func onlySub(t *testing.T, f *fakeStripe) *Subscription {
	t.Helper()
	if len(f.subs) != 1 {
		t.Fatalf("%d subscriptions, want 1", len(f.subs))
	}
	for _, s := range f.subs {
		return s
	}
	return nil
}

// Stripe invoices meter usage only from a subscription's start. Seen live in
// a Stripe sandbox: a subscription created after the usage it was created
// for invoiced 0 of it, while the meter summary showed all of it.
func TestNewSubscriptionStartsAtTheFirstUsage(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := subscribedAdapter(t, f, "", "month")
	first := now.Add(-3 * time.Hour)
	ctx := destinations.WithUsageStart(context.Background(), first)
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if got := onlySub(t, f).StartDate; got != first.Unix() {
		t.Fatalf("subscription starts %s, want the first usage %s", time.Unix(got, 0).UTC(), first)
	}
	// The first window is invoiced; usage dated before the start (a late
	// record for an earlier window) is dead-lettered, not silently sent.
	atStart := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", first, 1)
	before := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", first.Add(-time.Minute), 1)
	err := a.SendEvents(context.Background(), []usage.Event{atStart, before})
	var partial *destinations.PartialError
	if !errors.As(err, &partial) || len(partial.Rejected) != 1 || !strings.Contains(partial.Rejected[before.ID], "never invoice") {
		t.Fatalf("want only the earlier event rejected, got %v", err)
	}
	if len(f.events) != 1 || f.events[0].Identifier != atStart.ID {
		t.Fatalf("sent %+v, want only the event at the subscription start", f.events)
	}
}

// A tenant created today with no usage yet starts at its creation; one
// created on an earlier day starts today (no backdate to a day it had no
// usage to show for), like Lago.
func TestNewSubscriptionStartFromCreation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created time.Time
		want    time.Time
	}{
		{"created today", now.Add(-2 * time.Hour), now.Add(-2 * time.Hour)},
		// Usage is dated by window start: the window holding usage from a
		// 10:17:42 creation starts at 10:17:00, before the creation second.
		{"created mid-window", now.Add(-2*time.Hour + 17*time.Minute + 42*time.Second), now.Add(-2 * time.Hour)},
		{"created last week", now.Add(-7 * 24 * time.Hour), now.UTC().Truncate(24 * time.Hour)},
	} {
		f := newFakeStripe(t, func() time.Time { return now })
		a := subscribedAdapter(t, f, "", "month")
		if err := a.EnsureTenant(context.Background(), usage.Tenant{ID: "acme", CreatedAt: tc.created}); err != nil {
			t.Fatal(err)
		}
		if got := onlySub(t, f).StartDate; got != tc.want.Unix() {
			t.Errorf("%s: starts %s, want %s", tc.name, time.Unix(got, 0).UTC(), tc.want)
		}
	}
}

// Backdating a whole period makes Stripe close the first period at once and
// invoice none of it (seen live: 33 days back on a monthly price). So the
// start is clamped to less than one period back, and older usage is
// dead-lettered.
func TestBackdateNeverReachesAWholePeriod(t *testing.T) {
	for _, tc := range []struct {
		interval string
		period   time.Duration
	}{
		{"month", 28 * 24 * time.Hour},
		{"week", 7 * 24 * time.Hour},
		{"day", 24 * time.Hour},
	} {
		f := newFakeStripe(t, func() time.Time { return now })
		a := subscribedAdapter(t, f, "", tc.interval)
		usageStart := now.Add(-30 * 24 * time.Hour)
		ctx := destinations.WithUsageStart(context.Background(), usageStart)
		if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme"}); err != nil {
			t.Fatal(err)
		}
		start := time.Unix(onlySub(t, f).StartDate, 0)
		if back := now.Sub(start); back >= tc.period || back < tc.period-time.Hour {
			t.Errorf("%s: backdated %s, want just under %s", tc.interval, back, tc.period)
		}
		if !start.Add(tc.period).After(now) {
			t.Errorf("%s: first period ends %s, before now", tc.interval, start.Add(tc.period))
		}
		old := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", usageStart, 1)
		recent := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", now.Add(-time.Hour), 1)
		err := a.SendEvents(context.Background(), []usage.Event{old, recent})
		var partial *destinations.PartialError
		if !errors.As(err, &partial) || len(partial.Rejected) != 1 || partial.Rejected[old.ID] == "" {
			t.Errorf("%s: want the usage before the start rejected, got %v", tc.interval, err)
		}
	}
	// The clamp only moves once an hour, so a create retried within the hour
	// sends the same parameters as the first try (same idempotency key).
	p := 28 * 24 * time.Hour
	h := now.Truncate(time.Hour)
	if !earliestBackdate(h, p).Equal(earliestBackdate(h.Add(59*time.Minute), p)) {
		t.Fatal("clamp moves within the hour")
	}
}

// The start of an existing subscription is learned when vBilling finds it,
// and kept across restarts.
func TestSubscriptionStartSurvivesRestart(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	statePath := filepath.Join(t.TempDir(), "state.json")
	a := subscribedAdapter(t, f, statePath, "month")
	if err := a.EnsureTenant(context.Background(), usage.Tenant{ID: "acme"}); err != nil {
		t.Fatal(err)
	}
	if got := onlySub(t, f).StartDate; got != now.Unix() {
		t.Fatalf("no usage, no creation date: starts %s, want now", time.Unix(got, 0).UTC())
	}
	b := newTestAdapter(t, f, statePath, func(c *config.Config) { c.StripeAutoSubscribe = true })
	early := gpuEvent("acme", "NVIDIA-H100-80GB-HBM3", now.Add(-time.Hour), 1)
	err := b.SendEvents(context.Background(), []usage.Event{early})
	var partial *destinations.PartialError
	if !errors.As(err, &partial) || partial.Rejected[early.ID] == "" {
		t.Fatalf("restarted adapter forgot the subscription start: %v", err)
	}
	// Without STRIPE_AUTO_SUBSCRIBE vBilling does not own the subscriptions
	// and sends everything Stripe accepts.
	c := newTestAdapter(t, f, statePath)
	if err := c.SendEvents(context.Background(), []usage.Event{early}); err != nil {
		t.Fatalf("unmanaged subscriptions: %v", err)
	}
}

// Seen live: a subscription canceled in Stripe was re-created within the
// same hour with another start and more prices, and Stripe refused the
// reused idempotency key. With the same parameters it would have replayed
// the canceled subscription instead.
func TestResubscribeWithinTheHour(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := subscribedAdapter(t, f, "", "month")
	tenant := usage.Tenant{ID: "acme", CreatedAt: now.Add(-3 * time.Hour)}
	if err := a.EnsureTenant(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	first := onlySub(t, f)
	first.Status = "canceled"

	// Same parameters: a new subscription, not the canceled one replayed.
	if err := a.EnsureTenant(context.Background(), tenant); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	active := 0
	for _, s := range f.subs {
		if s.Status == "active" {
			active++
		}
	}
	if len(f.subs) != 2 || active != 1 {
		t.Fatalf("%d subscriptions, %d active; want a second, active one", len(f.subs), active)
	}

	// Other parameters (an earlier start) within the same hour: no
	// idempotency error.
	for _, s := range f.subs {
		s.Status = "canceled"
	}
	ctx := destinations.WithUsageStart(context.Background(), now.Add(-5*time.Hour))
	if err := a.EnsureTenant(ctx, tenant); err != nil {
		t.Fatalf("re-subscribe with an earlier start: %v", err)
	}
	if len(f.subs) != 3 {
		t.Fatalf("%d subscriptions, want 3", len(f.subs))
	}
}
