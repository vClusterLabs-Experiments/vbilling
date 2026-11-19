package pipeline

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Late adjustments are appended after current windows, so ledger order does
// not imply event-time order. A newer event must not strand an expired one.
func TestReviewMaxHoldReleasesLateUsageBehindRecentWindow(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	held := openHeld(t, filepath.Join(dir, "held"))
	dest := newOrdered("stripe")
	gate := newSKUGate()
	now := time.Now().UTC().Truncate(time.Minute)
	series := usage.MetricGPUHours + "/H100"
	var entries []HeldEntry
	for i, age := range []time.Duration{time.Hour, 33 * 24 * time.Hour} {
		ws := now.Add(-age)
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "H100", Quantity: 1,
			WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
		ev.Finalize()
		entries = append(entries, HeldEntry{Seq: uint64(i + 1), At: now, Series: series, Reason: "unpriced", Event: ev})
	}
	if _, err := held.Add(dest.Name(), entries); err != nil {
		t.Fatal(err)
	}
	d := New(s, []destinations.Destination{dest}, Options{Held: held, Gate: gate, MaxHold: 28 * 24 * time.Hour}, nil)
	if err := d.release(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if dest.got[entries[1].Event.ID] != 1 {
		t.Fatalf("33-day-old usage was not released before Stripe's 35-day limit; held=%d, received=%d", held.Count(dest.Name()), dest.received())
	}
}

// Held usage must reach the billing platform before its invoice period is
// final: the period's end plus the platform's grace (Metronome 24 hours by
// default). Usage whose window started 23 hours ago is released unpriced,
// although PRICE_CHECK_MAX_HOLD is 28 days; younger usage stays held for
// its price.
func TestHeldUsageIsReleasedBeforeItsPeriodCanBeFinal(t *testing.T) {
	dir := t.TempDir()
	s := openSpool(t, filepath.Join(dir, "ledger"))
	defer s.Close()
	held := openHeld(t, filepath.Join(dir, "held"))
	dest := newOrdered("metronome")
	gate := newSKUGate() // B200 never gets a price
	now := time.Now().UTC().Truncate(time.Minute)
	series := usage.MetricGPUHours + "/B200"
	var entries []HeldEntry
	// The last minute of a period that ended a day ago, then usage from
	// 22 hours ago.
	for i, age := range []time.Duration{24 * time.Hour, 22 * time.Hour} {
		ws := now.Add(-age)
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "B200", Quantity: 1,
			WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
		ev.Finalize()
		entries = append(entries, HeldEntry{Seq: uint64(i + 1), At: now, Series: series, Reason: "unpriced", Event: ev})
	}
	if _, err := held.Add(dest.Name(), entries); err != nil {
		t.Fatal(err)
	}
	d := New(s, []destinations.Destination{dest}, Options{Held: held, Gate: gate, MaxHold: 28 * 24 * time.Hour}, nil)
	if err := d.release(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if dest.got[entries[0].Event.ID] != 1 {
		t.Fatalf("usage held 24 hours was not released before Metronome finalizes its period (held %d)", held.Count(dest.Name()))
	}
	if dest.got[entries[1].Event.ID] != 0 || held.Count(dest.Name()) != 1 {
		t.Fatalf("usage held 22 hours was released before its time (held %d)", held.Count(dest.Name()))
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.expired["B200"] != 1 {
		t.Fatalf("gate told about %d events released unpriced, want 1 (its source turns failing)", gate.expired["B200"])
	}
}

// After a release, the series' oldest window is the oldest of what is left,
// not the window of the next entry in ledger order.
func TestHeldOldestAfterReleaseIsMinimumWindow(t *testing.T) {
	held := openHeld(t, filepath.Join(t.TempDir(), "held"))
	now := time.Now().UTC().Truncate(time.Minute)
	series := usage.MetricGPUHours + "/H100"
	var entries []HeldEntry
	for i, age := range []time.Duration{time.Minute, 2 * time.Minute, 20 * 24 * time.Hour} {
		ws := now.Add(-age)
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, SKU: "H100", Quantity: 1,
			WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
		ev.Finalize()
		entries = append(entries, HeldEntry{Seq: uint64(i + 1), At: now, Series: series, Reason: "unpriced", Event: ev})
	}
	if _, err := held.Add("stripe", entries); err != nil {
		t.Fatal(err)
	}
	got, next, err := held.Peek("stripe", series, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("peek: %v %v", got, err)
	}
	if err := held.Release("stripe", series, got[0].Seq, 1, next); err != nil {
		t.Fatal(err)
	}
	s := held.Series("stripe")
	if len(s) != 1 || !s[0].Oldest.Equal(entries[2].Event.WindowStart) || s[0].FirstSeq != 2 {
		t.Fatalf("summary after release = %+v, want first seq 2 and oldest window %s", s, entries[2].Event.WindowStart)
	}
	if seq, err := held.ExpiredThrough("stripe", series, now.Add(-24*time.Hour)); err != nil || seq != 3 {
		t.Fatalf("ExpiredThrough = %d, %v; want 3", seq, err)
	}
}
