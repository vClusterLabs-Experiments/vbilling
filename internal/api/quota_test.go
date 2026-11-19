package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Each ingest token has its own request rate; over it, 429 with Retry-After.
func TestIngestRateLimitPerToken(t *testing.T) {
	clock := now
	l := newRateLimiter(func() time.Time { return clock })
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", 1, 3); !ok {
			t.Fatalf("request %d of a burst of 3 refused", i)
		}
	}
	if ok, wait := l.allow("a", 1, 3); ok || wait <= 0 {
		t.Fatalf("the 4th request must wait: %v %v", ok, wait)
	}
	if ok, _ := l.allow("b", 1, 3); !ok {
		t.Fatal("another token has its own bucket")
	}
	clock = clock.Add(time.Second)
	if ok, _ := l.allow("a", 1, 3); !ok {
		t.Fatal("a token refills over time")
	}

	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	e.server.limiter = newRateLimiter(func() time.Time { return now }) // no refill while the test runs
	for i := 0; i < boundTokenBurst; i++ {
		e.server.limiter.allow(limitKey("tok-acme"), boundTokenRate, boundTokenBurst)
	}
	resp := do(t, http.MethodPost, e.srv.URL+"/api/v1/events", "tok-acme", strings.NewReader(`[]`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over its rate a bound token gets %d (Retry-After %q), want 429", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if code, _ := postJSON(t, e.srv.URL+"/api/v1/records", "ingest-secret", `{}`); code == http.StatusTooManyRequests {
		t.Fatal("the shared token has its own limit")
	}
}

// The new SKUs a source brings in for a tenant per day are capped: usage
// beyond the cap is billed under OverflowSKU, never dropped, the tenant's
// source turns failing in health, and the count starts again the next UTC
// day.
func TestSKUCapPerTenantPerDay(t *testing.T) {
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	e.server.WithHealth(h)
	ws := now.Add(-5 * time.Minute)
	var evs []map[string]any
	for i := 0; i <= maxSKUsPerTenantPerDay; i++ {
		evs = append(evs, map[string]any{"id": fmt.Sprintf("e-%d", i), "metric": usage.MetricGPUHours, "quantity": 1,
			"sku": fmt.Sprintf("SKU-%d", i), "window_start": ws, "window_end": ws.Add(time.Minute)})
	}
	b, _ := json.Marshal(evs)
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "tok-acme", string(b))
	if code != 200 || out["accepted"] != float64(maxSKUsPerTenantPerDay+1) || out["warnings"] == nil {
		t.Fatalf("ingest: %d %v", code, out)
	}
	skus := map[string]float64{}
	_ = e.sp.Scan(ws.Add(-time.Hour), now, func(ev *usage.Event) error { skus[ev.SKU] += ev.Quantity; return nil })
	if len(skus) != maxSKUsPerTenantPerDay+1 || skus[OverflowSKU] != 1 || skus[fmt.Sprintf("SKU-%d", maxSKUsPerTenantPerDay)] != 0 {
		t.Fatalf("stored SKUs: %d distinct, overflow %v", len(skus), skus[OverflowSKU])
	}
	if p := h.Problems(); len(p) != 1 || p[0].Name != "records/acme/sku-limit" || p[0].Status != health.Failing || p[0].Hint == "" {
		t.Fatalf("overflow must turn the tenant's source failing: %+v", p)
	}
	// A SKU already seen today is not new; another tenant, and another
	// kind of token for the same tenant, have caps of their own.
	if got, over := e.server.skus.Admit(records.SKUSourceBound, "acme", "SKU-0"); got != "SKU-0" || over {
		t.Fatalf("a known SKU: %q %v", got, over)
	}
	if got, _ := e.server.skus.Admit(records.SKUSourceBound, "globex", "SKU-500"); got != "SKU-500" {
		t.Fatalf("another tenant: %q", got)
	}
	if got, _ := e.server.skus.Admit(records.SKUSourceShared, "acme", "SKU-501"); got != "SKU-501" {
		t.Fatalf("the shared token for the same tenant: %q", got)
	}
	q := newSKUQuota(func() time.Time { return now.Add(24 * time.Hour) })
	q.day, q.seen = e.server.skus.day, e.server.skus.seen
	if got, _ := q.Admit(records.SKUSourceBound, "acme", "SKU-new"); got != "SKU-new" {
		t.Fatalf("the cap starts again the next day: %q", got)
	}
}

// A SKU the tenant has in its recent ledger, or one a billing platform
// prices, is never new: after a restart (the day's count is in memory) a
// tenant's models keep their SKUs, and the operator's catalog never counts.
func TestSKUCapAdmitsKnownAndPricedSKUs(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
	ws := now.Add(-5 * time.Minute)
	old := usage.Event{Tenant: "acme", Metric: usage.MetricGPUHours, Quantity: 1, SKU: "fine-tune-7", WindowStart: ws.Add(-24 * time.Hour),
		WindowEnd: ws.Add(-24*time.Hour + time.Minute), Source: "ingest:gw", RecordedAt: now}
	old.Finalize()
	if _, _, err := e.sp.AppendIngest(old.Source, []usage.Event{old}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.server.skus.known.follow(ctx, e.sp, func() time.Time { return now }); close(done) }()
	defer func() { cancel(); <-done }()
	e.server.priced.mu.Lock()
	e.server.priced.skus = map[string]bool{"NVIDIA-H100": true}
	e.server.priced.mu.Unlock()
	for i := 0; i < maxSKUsPerTenantPerDay; i++ { // the day's cap, used up
		e.server.skus.Admit(records.SKUSourceBound, "acme", fmt.Sprintf("junk-%d", i))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !e.server.skus.known.has("acme", "fine-tune-7") {
		if time.Now().After(deadline) {
			t.Fatal("the ledger's SKUs were never read")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, sku := range []string{"fine-tune-7", "NVIDIA-H100", "junk-3"} {
		if got, over := e.server.skus.Admit(records.SKUSourceBound, "acme", sku); got != sku || over {
			t.Errorf("%s: stored as %q (overflow %v), want it admitted", sku, got, over)
		}
	}
	if got, over := e.server.skus.Admit(records.SKUSourceBound, "acme", "junk-new"); got != OverflowSKU || !over {
		t.Errorf("a new, unpriced SKU past the cap: %q %v", got, over)
	}
	// What the cap admitted is known from then on, as it is in the ledger.
	if !e.server.skus.known.has("acme", "junk-3") {
		t.Error("an admitted SKU is known")
	}
}
