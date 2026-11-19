package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Separate tenants can legitimately choose the same local request ID.
// A tenant-bound token must not let one tenant consume another's ID.
func TestReviewTenantBoundEventIDsAreIsolated(t *testing.T) {
	e := setup(t, func(cfg *config.Config) {
		cfg.IngestToken = ""
		cfg.IngestTokens = map[string]string{"token-a": "tenant-a", "token-b": "tenant-b"}
	})
	body := fmt.Sprintf(`[{"id":"request-1","metric":%q,"quantity":10,"window_start":%q,"window_end":%q}]`,
		usage.MetricGPUHours, now.Add(-2*time.Minute).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339))
	for _, token := range []string{"token-a", "token-b"} {
		code, out := postJSON(t, e.srv.URL+"/api/v1/events", token, body)
		if code != http.StatusOK || out["accepted"] != float64(1) {
			t.Fatalf("independent tenant event dropped: token=%s status=%d response=%v", token, code, out)
		}
	}
}

// A tenant's retry of its own ID still deduplicates, and the ledger keeps the
// caller's ID as a property.
func TestTenantBoundEventIDRetryDeduplicates(t *testing.T) {
	e := setup(t, func(cfg *config.Config) {
		cfg.IngestToken = ""
		cfg.IngestTokens = map[string]string{"token-a": "tenant-a"}
	})
	body := fmt.Sprintf(`[{"id":"request-1","metric":%q,"quantity":10,"window_start":%q,"window_end":%q}]`,
		usage.MetricGPUHours, now.Add(-2*time.Minute).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339))
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", "token-a", body)
	if code != http.StatusOK || out["accepted"] != float64(1) {
		t.Fatalf("first send: %d %v", code, out)
	}
	ids, _ := out["ids"].([]any)
	if len(ids) != 1 || ids[0] != TenantEventID("tenant-a", limitKey("token-a"), "api", "request-1") {
		t.Fatalf("ids = %v, want the tenant-scoped ID", out["ids"])
	}
	code, out = postJSON(t, e.srv.URL+"/api/v1/events", "token-a", body)
	if code != http.StatusOK || out["accepted"] != float64(0) || out["duplicates"] != float64(1) {
		t.Fatalf("retry: %d %v, want a duplicate", code, out)
	}
}
