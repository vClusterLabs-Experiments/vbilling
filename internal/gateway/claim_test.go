package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

func TestDefiniteFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&StatusError{Code: 500, Body: "boom"}, true},
		{fmt.Errorf("block: %w", &StatusError{Code: 403}), true},
		{&StatusError{Code: http.StatusBadGateway}, false}, // a proxy: LiteLLM may have got it
		{&StatusError{Code: http.StatusGatewayTimeout}, false},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{&net.DNSError{Err: "no such host", Name: "litellm"}, true},
		{errors.New(`Post "http://litellm:4000/team/block": dial tcp 10.0.0.9:4000: connect: connection refused`), true},
		{context.DeadlineExceeded, false},
		{io.ErrUnexpectedEOF, false},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
	} {
		if got := Definite(tc.err); got != tc.want {
			t.Errorf("Definite(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

// A block LiteLLM answered with an error certainly did not happen: the
// claim goes, so a block an administrator makes before the retry stays
// theirs. A block whose outcome is unknown stays claimed.
func TestBlockClaimFollowsTheOutcome(t *testing.T) {
	var mu sync.Mutex
	blocked, fail := false, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/team/info":
			fmt.Fprintf(w, `{"team_info":{"blocked":%t}}`, blocked)
		case "/team/block":
			if fail != 0 {
				w.WriteHeader(fail)
				return
			}
			blocked = true
		case "/team/unblock":
			blocked = false
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		status  int
		claimed bool
	}{{http.StatusInternalServerError, false}, {http.StatusGatewayTimeout, true}} {
		mu.Lock()
		blocked, fail = false, tc.status
		mu.Unlock()
		state := kv.Memory()
		g := New(state, nil, func(string) []string { return []string{"team-acme"} }, NewLiteLLM(srv.URL, "sk-admin"))
		if err := g.Apply(ctx, "acme", true); err == nil {
			t.Fatalf("HTTP %d: block reported success", tc.status)
		}
		if _, ok := state.Get(key("litellm", "team-acme")); ok != tc.claimed {
			t.Errorf("HTTP %d: claimed %t, want %t", tc.status, ok, tc.claimed)
		}
	}
}
