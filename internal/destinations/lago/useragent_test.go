package lago

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/version"
)

// The Lago client names vBilling and its running version in its User-Agent,
// like every other billing adapter (it sent Go's default before v0.3.3).
func TestUserAgentIsTheRunningVersion(t *testing.T) {
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.Method] = r.UserAgent()
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	if _, err := c.GetPlan(context.Background(), "standard"); err != nil {
		t.Fatal(err)
	}
	if err := c.TerminateSubscription(context.Background(), "sub-1"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if got[method] != version.UserAgent() {
			t.Errorf("%s: User-Agent %q, want %q", method, got[method], version.UserAgent())
		}
	}
}
