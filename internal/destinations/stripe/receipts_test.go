package stripe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type lostMeterReply struct{ base http.RoundTripper }

func (l lostMeterReply) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(r)
	if err == nil && r.URL.Path == "/v1/billing/meter_events" && resp.StatusCode == http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return resp, err
}

func TestConfirmedReceiptPreventsDuplicateBeyondProviderDedupe(t *testing.T) {
	clock := now
	f := newFakeStripe(t, func() time.Time { return clock })
	path := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, path)
	a.now = func() time.Time { return clock }
	e := gpuEvent("acme", "H100", now.Add(-time.Minute), 1)
	if err := a.SendEvents(context.Background(), []usage.Event{e}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(36 * 24 * time.Hour) // also beyond the provider's event-age limit
	f.mu.Lock()
	f.idents = map[string]bool{} // provider has expired its deduplication memory
	f.mu.Unlock()
	b := newTestAdapter(t, f, path)
	b.now = func() time.Time { return clock }
	if err := b.SendEvents(context.Background(), []usage.Event{e}); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 1 {
		t.Fatalf("confirmed event delivered %d times", len(f.events))
	}
	if err := b.CheckpointDelivery([]usage.Event{e}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := b.DeliveryReceipt(e.ID); err != nil || exists {
		t.Fatalf("receipt retained after durable cursor: %t %v", exists, err)
	}
}

func TestLostReplyRequiresReviewAfterDedupeWindow(t *testing.T) {
	clock := now
	f := newFakeStripe(t, func() time.Time { return clock })
	path := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, path)
	a.now = func() time.Time { return clock }
	a.c.http.Transport = lostMeterReply{base: http.DefaultTransport}
	e := gpuEvent("acme", "H100", now.Add(-time.Minute), 1)
	if err := a.SendEvents(context.Background(), []usage.Event{e}); err == nil {
		t.Fatal("lost response did not fail")
	}
	if len(f.events) != 1 {
		t.Fatal("provider did not accept the attempted event")
	}
	clock = clock.Add(26 * time.Hour)
	f.mu.Lock()
	f.idents = map[string]bool{}
	f.mu.Unlock()
	b := newTestAdapter(t, f, path)
	b.now = func() time.Time { return clock }
	err := b.SendEvents(context.Background(), []usage.Event{e})
	var partial *destinations.PartialError
	if !errors.As(err, &partial) || !strings.Contains(partial.Rejected[e.ID], "ambiguous") || len(f.events) != 1 {
		t.Fatalf("unsafe recovery: error %v; provider deliveries %d", err, len(f.events))
	}
	if err := b.ResolveDelivery(e.ID, "delivered", "Provider reconciliation confirms the quantity"); err != nil {
		t.Fatal(err)
	}
	if err := b.SendEvents(context.Background(), []usage.Event{e}); err != nil || len(f.events) != 1 {
		t.Fatalf("reviewed receipt replayed: %v, %d deliveries", err, len(f.events))
	}
}

func TestOldCursorRewindIsRefused(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	s, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := gpuEvent("acme", "H100", now.Add(-26*time.Hour), 1)
	if err := s.AppendWindow(spool.SourceCollector, e.WindowEnd, []usage.Event{e}, ""); err != nil {
		t.Fatal(err)
	}
	a.UseLedger(s)
	if err := a.ValidateReplay(context.Background(), s.Committed(), 0); err == nil {
		t.Fatal("old usage can be replayed without reconciliation")
	}
}

func TestUnattemptedEventNeedsEvidenceBeforeOldRecovery(t *testing.T) {
	clock := now
	f := newFakeStripe(t, func() time.Time { return clock })
	path := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, path)
	a.now = func() time.Time { return clock }
	e := gpuEvent("acme", "H100", now.Add(-time.Minute), 1)
	// Simulate a crash after durable preparation, before any network call.
	if _, _, err := a.prepareAttempts([]job{{ev: e}}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(26 * time.Hour)
	b := newTestAdapter(t, f, path)
	b.now = func() time.Time { return clock }
	var partial *destinations.PartialError
	if err := b.SendEvents(context.Background(), []usage.Event{e}); !errors.As(err, &partial) || len(f.events) != 0 {
		t.Fatalf("unresolved old attempt was sent: %v", err)
	}
	if err := b.ResolveDelivery(e.ID, "not_delivered", "Provider reconciliation confirms no accepted usage"); err != nil {
		t.Fatal(err)
	}
	if err := b.SendEvents(context.Background(), []usage.Event{e}); err != nil || len(f.events) != 1 {
		t.Fatalf("reviewed missing event not recovered: %v", err)
	}
}

func TestNoMeterEventIsPostedWhenAttemptCannotBePersisted(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	path := filepath.Join(t.TempDir(), "state.json")
	a := newTestAdapter(t, f, path)
	if err := a.Bootstrap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.DeliveryReceipt("unused"); err != nil {
		t.Fatal(err)
	}
	e := gpuEvent("acme", "H100", now.Add(-time.Minute), 1)
	if _, err := a.EnsureCustomer(context.Background(), usage.Tenant{ID: e.Tenant}); err != nil {
		t.Fatal(err)
	}
	name := a.MeterName(&e)
	if _, err := a.ensureMeter(context.Background(), e.Metric, name, name); err != nil {
		t.Fatal(err)
	}
	// Block atomic state replacement while keeping the existing state readable.
	if err := os.Mkdir(path+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := a.SendEvents(context.Background(), []usage.Event{e}); err == nil || !strings.Contains(err.Error(), "persist Stripe delivery attempts") || len(f.events) != 0 {
		t.Fatalf("event sent without a durable attempt: %v, deliveries %d", err, len(f.events))
	}
}

func TestNewUsageCanHaveItsFirstAttemptAfterALongHold(t *testing.T) {
	clock := now
	f := newFakeStripe(t, func() time.Time { return clock })
	a := newTestAdapter(t, f, filepath.Join(t.TempDir(), "state.json"))
	a.now = func() time.Time { return clock }
	if err := a.Bootstrap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	e := gpuEvent("acme", "H100", now, 1)
	e.RecordedAt = now.Add(time.Minute)
	clock = now.Add(26 * time.Hour)
	if err := a.SendEvents(context.Background(), []usage.Event{e}); err != nil || len(f.events) != 1 {
		t.Fatalf("new held usage mistaken for an ambiguous old attempt: %v", err)
	}
}
