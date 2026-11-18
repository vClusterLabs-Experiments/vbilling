package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// fakeLiteLLM answers /team/block and /team/unblock like LiteLLM v1.108.11
// does: 200 for a known team (also when it is already in that state), 404
// {"detail":{"error":"Team not found, passed team_id=..."}} otherwise.
type fakeLiteLLM struct {
	mu      sync.Mutex
	key     string
	teams   map[string]bool // team -> blocked
	failing bool
	calls   []string
}

func (f *fakeLiteLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		http.Error(w, `{"error":{"message":"Authentication Error"}}`, http.StatusUnauthorized)
		return
	}
	var body struct {
		TeamID string `json:"team_id"`
	}
	if r.Method == http.MethodGet {
		body.TeamID = r.URL.Query().Get("team_id")
	} else {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.calls = append(f.calls, r.URL.Path+" "+body.TeamID)
	if f.failing {
		http.Error(w, `{"detail":"database unavailable"}`, http.StatusInternalServerError)
		return
	}
	blocked, ok := f.teams[body.TeamID]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":{"error":"Team not found, passed team_id=` + body.TeamID + `"}}`))
		return
	}
	if r.URL.Path == "/team/info" {
		_ = json.NewEncoder(w).Encode(map[string]any{"team_id": body.TeamID, "team_info": map[string]any{"blocked": blocked}})
		return
	}
	f.teams[body.TeamID] = r.URL.Path == "/team/block"
	_ = json.NewEncoder(w).Encode(map[string]any{"team_id": body.TeamID, "blocked": f.teams[body.TeamID]})
}

func newFake(t *testing.T, teams ...string) (*fakeLiteLLM, *LiteLLM) {
	f := &fakeLiteLLM{key: "sk-admin", teams: map[string]bool{}}
	for _, tm := range teams {
		f.teams[tm] = false
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewLiteLLM(srv.URL, "sk-admin")
}

func TestGateBlocksTheTenantsTeamsAndUnblocksOnlyItsOwn(t *testing.T) {
	f, ll := newFake(t, "team-acme", "team-ops")
	f.teams["team-ops"] = true // blocked by an administrator by hand
	identities := map[string][]string{"acme": {"acme", "team-acme", "team-ops"}}
	g := New(kv.Memory(), nil, func(t string) []string { return identities[t] }, ll)
	ctx := context.Background()

	if err := g.Apply(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if !f.teams["team-acme"] {
		t.Fatal("the tenant's team was not blocked")
	}
	// "acme" is not a LiteLLM team: skipped, not an error, not remembered.
	// team-ops was already blocked by an administrator: not vBilling's.
	if got := g.Blocked()["litellm"]; len(got) != 1 || got["team-acme"] != "acme" {
		t.Fatalf("blocked = %v", got)
	}
	if err := g.Apply(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if f.teams["team-acme"] {
		t.Error("the tenant's team is still blocked")
	}
	if !f.teams["team-ops"] {
		t.Error("vBilling lifted a block it did not set")
	}
	if len(g.Blocked()["litellm"]) != 0 {
		t.Errorf("state not cleared: %v", g.Blocked())
	}
	// Idempotent: applying the same state again calls nothing it must not.
	if err := g.Apply(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
}

func TestGateRetriesFailures(t *testing.T) {
	f, ll := newFake(t, "team-acme")
	g := New(kv.Memory(), nil, func(string) []string { return []string{"team-acme"} }, ll)
	ctx := context.Background()
	f.failing = true
	err := g.Apply(ctx, "acme", true)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a failing gateway must be reported: %v", err)
	}
	if len(g.Blocked()) != 0 {
		t.Fatal("a failed block was remembered as done")
	}
	f.failing = false
	if err := g.Apply(ctx, "acme", true); err != nil || !f.teams["team-acme"] {
		t.Fatalf("retry: %v blocked=%v", err, f.teams["team-acme"])
	}
}

func TestLiteLLMUnknownTeamAndAuth(t *testing.T) {
	_, ll := newFake(t)
	if err := ll.Block(context.Background(), "no-such-team"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown team: %v", err)
	}
	ll.Key = "sk-wrong"
	if err := ll.Block(context.Background(), "x"); err == nil || errors.Is(err, ErrUnknown) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong key must fail loudly: %v", err)
	}
}

// TestLiveLiteLLM runs against a real LiteLLM proxy when
// VBILLING_LIVE_LITELLM_URL and VBILLING_LIVE_LITELLM_KEY_FILE are set, and
// a team VBILLING_LIVE_LITELLM_TEAM exists.
func TestLiveLiteLLM(t *testing.T) {
	url, keyFile, team := os.Getenv("VBILLING_LIVE_LITELLM_URL"), os.Getenv("VBILLING_LIVE_LITELLM_KEY_FILE"), os.Getenv("VBILLING_LIVE_LITELLM_TEAM")
	if url == "" || keyFile == "" || team == "" {
		t.Skip("set VBILLING_LIVE_LITELLM_URL, VBILLING_LIVE_LITELLM_KEY_FILE and VBILLING_LIVE_LITELLM_TEAM")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ll := NewLiteLLM(url, strings.TrimSpace(string(key)))
	g := New(kv.Memory(), nil, func(string) []string { return []string{"tenant-without-a-team", team} }, ll)
	ctx := context.Background()
	if err := g.Apply(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if got := g.Blocked()["litellm"]; len(got) != 1 || got[team] != "acme" {
		t.Fatalf("blocked = %v", got)
	}
	if err := g.Apply(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if len(g.Blocked()) != 0 {
		t.Fatalf("still blocked: %v", g.Blocked())
	}
	// An administrator's block survives the tenant's suspension and
	// reactivation.
	if err := ll.Block(ctx, team); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ll.Unblock(context.Background(), team) })
	if err := g.Apply(ctx, "acme", true); err != nil {
		t.Fatal(err)
	}
	if err := g.Apply(ctx, "acme", false); err != nil {
		t.Fatal(err)
	}
	if blocked, err := ll.IsBlocked(ctx, team); err != nil || !blocked {
		t.Fatalf("the administrator's block was lifted: blocked=%v err=%v", blocked, err)
	}
}
