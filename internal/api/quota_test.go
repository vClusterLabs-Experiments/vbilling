package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
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

// A tenant's distinct SKUs per day are capped: usage beyond the cap is
// billed under OverflowSKU, never dropped, and the count starts again the
// next UTC day.
func TestSKUCapPerTenantPerDay(t *testing.T) {
	e := setup(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-acme": "acme"} })
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
	// A SKU already seen today is not new; another tenant has its own cap.
	if got, over := e.server.skus.admit("acme", "SKU-0"); got != "SKU-0" || over {
		t.Fatalf("a known SKU: %q %v", got, over)
	}
	if got, _ := e.server.skus.admit("globex", "SKU-500"); got != "SKU-500" {
		t.Fatalf("another tenant: %q", got)
	}
	q := newSKUQuota(func() time.Time { return now.Add(24 * time.Hour) })
	q.day, q.seen = e.server.skus.day, e.server.skus.seen
	if got, _ := q.admit("acme", "SKU-new"); got != "SKU-new" {
		t.Fatalf("the cap starts again the next day: %q", got)
	}
}
