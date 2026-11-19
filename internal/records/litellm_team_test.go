package records

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// LiteLLM records bill to the virtual key's team only. end_user (which the
// client sends), the key's organization and its user do not decide the
// tenant: a request whose key has no team is refused.
func TestLiteLLMBillsTheTeamOnly(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	r.clk.set(time.Unix(1794996005, 0).UTC())
	body := `[
 {"id":"c-1","status":"success","model_group":"llama-70b","prompt_tokens":5,"completion_tokens":1,"endTime":1794996001,
  "end_user":"someone-else","metadata":{"user_api_key_team_id":"team-a","user_api_key_org_id":"org-x","user_api_key_user_id":"u-1"}},
 {"id":"c-2","status":"success","model_group":"llama-70b","prompt_tokens":5,"completion_tokens":1,"endTime":1794996002,
  "end_user":"other-tenant","metadata":{"user_api_key_org_id":"org-x","user_api_key_user_id":"u-1"}}
]`
	res := r.post(t, "litellm", body)
	if res.Accepted != 1 || len(res.Rejected) != 1 || res.Rejected[0].Index != 1 {
		t.Fatalf("result: %+v", res)
	}
	tenants := map[string]bool{}
	_ = r.records.ScanRecords(t0.Add(-2*time.Hour), t0.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		tenants[ev.Tenant] = true
		return nil
	})
	if len(tenants) != 1 || !tenants["team-a"] {
		t.Fatalf("billed tenants %v, want only the key's team", tenants)
	}
}
