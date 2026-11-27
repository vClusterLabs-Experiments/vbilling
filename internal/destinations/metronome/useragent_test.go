package metronome

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/version"
)

// The Metronome client names the running version in its User-Agent, from the
// same source as GET /api/v1/status (v0.3.2 sent "0.3.1").
func TestUserAgentIsTheRunningVersion(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		io.WriteString(w, `{"data":[],"next_page":null}`)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "token").ListBillableMetrics(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != version.UserAgent() {
		t.Fatalf("User-Agent %q, want %q", got, version.UserAgent())
	}
}
