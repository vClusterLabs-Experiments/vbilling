package destinations

import (
	"context"
	"time"
)

type usageStartKey struct{}

// WithUsageStart tells EnsureTenant when the usage that made vBilling ensure
// the tenant starts: the earliest window start of the tenant's events in the
// batch being delivered (or held). Adapters whose billing period starts when
// the customer is subscribed (Lago) start it early enough to count that usage.
func WithUsageStart(ctx context.Context, t time.Time) context.Context {
	if t.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, usageStartKey{}, t)
}

// UsageStart returns the time set by WithUsageStart, if any.
func UsageStart(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(usageStartKey{}).(time.Time)
	return t, ok && !t.IsZero()
}

// SubscriptionStart is when a billing platform subscription created now
// should start. Billing platforms count usage from a subscription's start,
// and a subscription created when the first usage is delivered starts after
// that usage's window: the first window would never be billed. So it starts
// at the earliest of the usage being delivered and the tenant's creation,
// both in the past. The creation time alone never moves the start to an
// earlier UTC day than today: an old tenant that is new to the billing
// platform starts its billing cycle today, not at some date it had no usage
// to show for. Usage from earlier days (backfill, held usage) does move it:
// that usage must count. Zero means "now".
func SubscriptionStart(created, usageStart, now time.Time) time.Time {
	var start time.Time
	if !created.IsZero() && created.Before(now) {
		// Usage is dated by its window's start, and windows divide an hour:
		// the window holding a tenant's first usage starts no earlier than
		// the hour the tenant was created in. Starting at the creation
		// second would leave that first window out (seen live on Stripe).
		start = created.Truncate(time.Hour)
		if today := now.UTC().Truncate(24 * time.Hour); start.Before(today) {
			start = today
		}
	}
	if !usageStart.IsZero() && usageStart.Before(now) && (start.IsZero() || usageStart.Before(start)) {
		start = usageStart
	}
	return start
}
