package records

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// A push in the litellm format under another identity than the LiteLLM
// pull's (LITELLM_TENANT, LITELLM_CLIENT) is stored twice, once per record
// ID: its source turns failing with a hint naming the setting.
func TestLiteLLMPushThatThePullCannotMatchIsFlagged(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r.in.health = h
	srv := httptest.NewServer(&fakeLiteLLM{})
	defer srv.Close()
	newPuller(t, r, srv, LiteLLMPullOptions{Tenant: "globex"}) // the pull is pinned to globex, client litellm
	r.clk.set(t0.Add(15 * time.Second))
	push := func(client, bound string, n int) {
		t.Helper()
		q := request{call: fmt.Sprintf("c-%d", n), resp: fmt.Sprintf("chatcmpl-%d", n), team: "globex", end: t0.Add(10 * time.Second), in: 30, out: 3}
		body, _ := json.Marshal([]map[string]any{q.payload(t)})
		if res, err := r.in.Ingest("litellm", client, bound, body); err != nil || res.Accepted != 1 {
			t.Fatalf("push: %+v %v", res, err)
		}
	}
	push("litellm", "globex", 1) // the pull's identity
	if p := h.Problems(); len(p) != 0 {
		t.Fatalf("a push that matches the pull: %+v", p)
	}
	hints := map[string]string{}
	push("litellm", "acme", 2) // another tenant's token
	push("litellm", "", 3)     // the shared token
	push("litellm-eu", "globex", 4)
	for _, s := range h.Problems() {
		hints[s.Name] = s.Hint
		if s.Status != health.Failing {
			t.Errorf("%s: %s", s.Name, s.Status)
		}
	}
	for name, setting := range map[string]string{
		"records/litellm/acme/litellm-pull":      "LITELLM_TENANT=acme",
		"records/litellm/litellm-pull":           "LITELLM_TENANT",
		"records/litellm-eu/globex/litellm-pull": "LITELLM_CLIENT=litellm-eu",
	} {
		if !strings.Contains(hints[name], setting) {
			t.Errorf("%s: hint %q, want one naming %s (sources: %v)", name, hints[name], setting, hints)
		}
	}
}
