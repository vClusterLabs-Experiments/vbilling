package lago

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func subscriptionAt(t *testing.T, f *fakeLago) string {
	t.Helper()
	reqs := f.requests("POST", "/api/v1/subscriptions")
	if len(reqs) != 1 {
		t.Fatalf("%d subscription requests", len(reqs))
	}
	var body struct {
		Subscription map[string]any `json:"subscription"`
	}
	json.Unmarshal(reqs[0].Body, &body)
	at, _ := body.Subscription["subscription_at"].(string)
	return at
}

// Lago counts usage from a subscription's start. The subscription is created
// when the tenant's first usage is delivered, after that usage's window
// started, so without subscription_at the first window is never billed.
func TestSubscriptionStartsAtTheFirstDeliveredWindow(t *testing.T) {
	f := newFakeLago(t)
	a := newTestAdapter(t, f, nil)
	now := time.Date(2026, 11, 18, 4, 30, 15, 0, time.UTC)
	a.now = func() time.Time { return now }
	first := time.Date(2026, 11, 18, 4, 28, 0, 0, time.UTC)
	ctx := destinations.WithUsageStart(context.Background(), first)
	if err := a.EnsureTenant(ctx, usage.Tenant{ID: "acme-ai"}); err != nil {
		t.Fatal(err)
	}
	if got := subscriptionAt(t, f); got != "2026-11-18T04:28:00Z" {
		t.Fatalf("subscription_at %q, want the first window's start", got)
	}
}

func TestSubscriptionStart(t *testing.T) {
	now := time.Date(2026, 11, 18, 4, 30, 0, 0, time.UTC)
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct {
		name           string
		created, usage time.Time
		want           string
	}{
		{"ingest-only tenant: the first window", time.Time{}, at("2026-11-18T04:28:00Z"), "2026-11-18T04:28:00Z"},
		// The hour it was created in, so its first window (01:17:00, or
		// 01:15:00 with 5-minute windows, for a 01:17:42 creation) is in.
		{"cluster created earlier today", at("2026-11-18T01:15:00Z"), at("2026-11-18T04:28:00Z"), "2026-11-18T01:00:00Z"},
		{"created mid-window", at("2026-11-18T01:17:42Z"), at("2026-11-18T04:28:00Z"), "2026-11-18T01:00:00Z"},
		{"cluster created in August: not before today", at("2026-08-15T00:00:00Z"), at("2026-11-18T04:28:00Z"), "2026-11-18T00:00:00Z"},
		{"backfilled usage from yesterday counts", at("2026-08-15T00:00:00Z"), at("2026-11-17T22:00:00Z"), "2026-11-17T22:00:00Z"},
		{"nothing known: Lago's default (now)", time.Time{}, time.Time{}, ""},
		{"times in the future are ignored", at("2026-11-19T00:00:00Z"), at("2026-11-18T05:00:00Z"), ""},
	} {
		got := subscriptionStart(c.created, c.usage, now)
		s := ""
		if !got.IsZero() {
			s = got.UTC().Format(time.RFC3339)
		}
		if s != c.want {
			t.Errorf("%s: %q, want %q", c.name, s, c.want)
		}
	}
}
