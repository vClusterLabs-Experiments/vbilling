package records

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pullFromConfig builds the LiteLLM puller the way cmd/vbilling/main.go
// does, from the settings vBilling loads.
func pullFromConfig(t *testing.T, r *rig, srv *httptest.Server) *LiteLLMPuller {
	t.Helper()
	cfg := config.Load()
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{Client: cfg.RecordsPullLiteLLMClient, Tenant: cfg.RecordsPullLiteLLMTenant})
	return p
}

// v0.3.0's documented LiteLLM setups (usage-sources.html, "Same identity
// as the push", still in the v0.3.1 page): a LiteLLM that serves one tenant
// pushes with a tenant-bound token and pins its pull with
// records.pullLiteLLM.tenant; a push that sends its own X-VBilling-Client
// sets records.pullLiteLLM.client to it. v0.3.1 no longer reads either
// setting (config.RemovedSettings; the chart no longer renders them, so
// there is not even a warning), and bound records moved to an ID namespace
// of their own. The pulled copy of every pushed request then gets another
// record ID and is billed again as a late adjustment.
func TestReview3LiteLLMPullOfABoundPushBillsOnce(t *testing.T) {
	t.Setenv("RECORDS_PULL_LITELLM_TENANT", "globex") // v0.3.0 values, still documented
	t.Setenv("LITELLM_URL", "http://litellm.invalid")
	t.Setenv("LITELLM_API_KEY", "sk-admin")
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	// Teams carry the tenant ID, as the docs recommend.
	q := request{call: "c-1", resp: "chatcmpl-1", team: "globex", end: t0.Add(10 * time.Second), in: 30, out: 3}
	body, _ := json.Marshal([]map[string]any{q.payload(t)})
	r.clk.set(t0.Add(15 * time.Second))
	// POST /api/v1/records with the token INGEST_TOKENS binds to globex; the
	// client is the format name, as LiteLLM sends no X-VBilling-Client.
	if _, err := r.in.Ingest("litellm", "litellm", "globex", body); err != nil {
		t.Fatal(err)
	}
	r.tick(t, t0.Add(81*time.Second)) // the window is committed with the pushed request
	fake.add(q.row(t))
	r.clk.set(t0.Add(3 * time.Minute))
	if _, err := pullFromConfig(t, r, srv).Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tick(t, t0.Add(4*time.Minute+30*time.Second))
	onTime, late := byMetric(r.ledgerEvents(t), "inference_input_tokens")
	if onTime+late != 30 {
		t.Errorf("one request of 30 input tokens billed %v (%v on time, %v as a late adjustment): the pull did not recognize the pushed copy", onTime+late, onTime, late)
	}
}

func TestReview3LiteLLMPullWithTheClientOfThePushBillsOnce(t *testing.T) {
	t.Setenv("RECORDS_PULL_LITELLM_CLIENT", "litellm-eu") // v0.3.0 value, still documented
	t.Setenv("LITELLM_URL", "http://litellm.invalid")
	t.Setenv("LITELLM_API_KEY", "sk-admin")
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	q := request{call: "c-1", resp: "chatcmpl-1", team: "acme", end: t0.Add(10 * time.Second), in: 30, out: 3}
	r.clk.set(t0.Add(15 * time.Second))
	r.push(t, "litellm-eu", q) // X-VBilling-Client: litellm-eu, shared ingest token
	r.tick(t, t0.Add(81*time.Second))
	fake.add(q.row(t))
	r.clk.set(t0.Add(3 * time.Minute))
	if _, err := pullFromConfig(t, r, srv).Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tick(t, t0.Add(4*time.Minute+30*time.Second))
	onTime, late := byMetric(r.ledgerEvents(t), "inference_input_tokens")
	if onTime+late != 30 {
		t.Errorf("one request of 30 input tokens billed %v (%v on time, %v as a late adjustment)", onTime+late, onTime, late)
	}
}

// A pull pinned to a tenant (RECORDS_PULL_FILE tenant:, or v0.3.0's
// records.pullLiteLLM.tenant) re-reads its lookback (1h by default, up to
// 166h) on every pass and drops the rows already stored, by record ID.
// v0.3.0 stored bound pushes and pinned pulls under RecordID(client,
// tenant, ...); v0.3.1 derives them under "bound\x00"+client. The first pull
// after the upgrade finds none of the rows v0.3.0 stored and bills the whole
// lookback again. The v0.3.0 row is stored here as v0.3.1 stores an
// unbound record whose tenant value is the tenant: the same ID v0.3.0 gave
// the bound copy.
func TestReview3PinnedPullAfterTheUpgradeBillsOnce(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	q := request{call: "c-1", resp: "chatcmpl-1", team: "globex", end: t0.Add(10 * time.Second), in: 30, out: 3}
	body, _ := json.Marshal([]map[string]any{q.payload(t)})
	r.clk.set(t0.Add(15 * time.Second))
	before := RecordID("litellm", "globex", LiteLLMRequestID(q.payload(t)), "inference_input_tokens") // v0.3.0's bound ID
	res, err := r.in.Ingest("litellm", "litellm", "", body)
	if err != nil || res.UsageRows == 0 {
		t.Fatalf("store the v0.3.0 copy: %+v %v", res, err)
	}
	stored := false
	_ = r.records.ScanRecords(t0, t0.Add(time.Minute), func(e *usage.Event, _ time.Time) error {
		stored = stored || e.ID == before
		return nil
	})
	if !stored {
		t.Fatalf("the stand-in for v0.3.0's row does not carry v0.3.0's ID %s", before)
	}
	r.tick(t, t0.Add(81*time.Second))

	// vBilling v0.3.1 starts; its pinned pull re-reads the last hour.
	fake.add(q.row(t))
	r.clk.set(t0.Add(3 * time.Minute))
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{Tenant: "globex"})
	if _, err := p.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.tick(t, t0.Add(4*time.Minute+30*time.Second))
	onTime, late := byMetric(r.ledgerEvents(t), "inference_input_tokens")
	if onTime+late != 30 {
		t.Errorf("one request of 30 input tokens billed %v (%v on time, %v late): the pinned pull's new ID namespace hid the stored copy", onTime+late, onTime, late)
	}
}
