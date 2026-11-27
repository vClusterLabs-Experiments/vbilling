package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/version"
)

// Queries to PROMETHEUS_URL name vBilling and its running version, from the
// same source as GET /api/v1/status. A User-Agent the operator sets in
// PROMETHEUS_HEADERS replaces it.
func TestPrometheusUserAgentIsTheRunningVersion(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer srv.Close()

	if _, err := newPrometheusClient(srv.URL, nil, "").Query(context.Background(), "up", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got != version.UserAgent() {
		t.Fatalf("User-Agent %q, want %q", got, version.UserAgent())
	}

	headers := map[string]string{"User-Agent": "metering-probe"}
	if _, err := newPrometheusClient(srv.URL, headers, "").Query(context.Background(), "up", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got != "metering-probe" {
		t.Fatalf("User-Agent %q, want the one from PROMETHEUS_HEADERS", got)
	}
}
