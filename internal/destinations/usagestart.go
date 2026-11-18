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
