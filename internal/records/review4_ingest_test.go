package records

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// Fourth review: the ingest fixes made after the third review.

// The operator's LiteLLM serves one tenant, globex. vBilling pulls its spend
// logs pinned to globex (LITELLM_TENANT=globex), as usage-sources.html and
// the v0.3.1 health hint ask for a LiteLLM that serves one tenant. globex
// also holds a token bound to it, for a gateway of its own (security.html:
// "Give each tenant's own gateway its own token").
//
// Bound records and pinned pulls share one ID namespace ("bound\x00"+client,
// tenant), so a tenant's own bound copy of a request has the very ID of the
// operator's pulled row. globex knows both halves of the record ID: it sets
// litellm_call_id itself (x-litellm-call-id) and reads the response id. It
// posts its own copy under client litellm with 1 input token; the pull then
// finds that ID stored and drops the real row. security.html (v0.3.1) says
// "a tenant's copy of a request never takes the ID of the gateway's".
func TestReview4TenantBoundPushReplacesThePinnedPullOfTheOperatorsGateway(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	real := request{call: "c-1", resp: "chatcmpl-1", team: "globex", end: t0.Add(10 * time.Second), in: 50000, out: 2000}
	forged := real
	forged.in, forged.out = 1, 1
	body, _ := json.Marshal([]map[string]any{forged.payload(t)})
	r.clk.set(t0.Add(12 * time.Second))
	// POST /api/v1/records?format=litellm with globex's own bound token and
	// X-VBilling-Client: litellm.
	if res, err := r.in.Ingest("litellm", "litellm", "globex", body); err != nil || res.Accepted != 1 {
		t.Fatalf("globex's own push: %+v %v", res, err)
	}
	// The operator's LiteLLM wrote the real request to its spend logs.
	fake.add(real.row(t))
	r.clk.set(t0.Add(3 * time.Minute))
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{Tenant: "globex"})
	res, err := p.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.tick(t, t0.Add(4*time.Minute+30*time.Second))
	onTime, late := byMetric(r.ledgerEvents(t), "inference_input_tokens")
	if onTime+late != 50000 {
		t.Errorf("the operator's pinned pull read a request of 50000 input tokens and billed globex %v (%v on time, %v late; pull: %d already stored): "+
			"globex's own bound token posted the request's record ID first with 1 token, and the pull dropped the real row as already stored",
			onTime+late, onTime, late, res.AlreadyStored)
	}
}

// The same with the operator's LiteLLM pushing with a token bound to globex
// ("A LiteLLM that serves one tenant can use a tenant-bound token
// instead"), while globex holds a second token bound to itself: both
// tokens give the same record IDs, and billing keeps the first copy.
func TestReview4TenantBoundPushReplacesThePushOfTheOperatorsBoundGateway(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	real := request{call: "c-1", resp: "chatcmpl-1", team: "globex", end: t0.Add(10 * time.Second), in: 50000, out: 2000}
	forged := real
	forged.in, forged.out = 1, 1
	r.clk.set(t0.Add(11 * time.Second))
	own, _ := json.Marshal([]map[string]any{forged.payload(t)})
	if res, err := r.in.Ingest("litellm", "litellm", "globex", own); err != nil || res.Accepted != 1 { // globex's own token
		t.Fatalf("globex's own push: %+v %v", res, err)
	}
	r.clk.set(t0.Add(15 * time.Second)) // LiteLLM flushes its batch every 5 seconds
	gw, _ := json.Marshal([]map[string]any{real.payload(t)})
	if res, err := r.in.Ingest("litellm", "litellm", "globex", gw); err != nil || res.Accepted != 1 { // the operator's LiteLLM, its own globex token
		t.Fatalf("the operator's push: %+v %v", res, err)
	}
	r.tick(t, t0.Add(2*time.Minute))
	if got := total(r.ledgerEvents(t), "inference_input_tokens"); got != 50000 {
		t.Errorf("the operator's LiteLLM pushed 50000 input tokens for globex with its globex-bound token and globex was billed %v: "+
			"globex's own bound token sent the same record ID first", got)
	}
}

// The operator's LiteLLM pushes every request with the shared ingest token,
// and vBilling does not pull its spend logs (the push is the fast path, the
// pull optional). A tenant sends x-litellm-call-id with 300 characters
// (usage-sources.html: "A client can set litellm_call_id with the
// x-litellm-call-id header"). Since 9f61a9b, a pushed record whose request
// ID is longer than 256 bytes is refused, so that request is never billed:
// LiteLLM ignores per-record rejections, and drops a batch answered with a
// 4xx. Pulled records are repaired instead, because "the request was
// served, and dropping its usage would be worse"; so is a pushed one from
// the operator's gateway.
func TestReview4TenantChosenCallIDMakesItsRequestUnbillable(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	normal := request{call: "c-1", resp: "chatcmpl-1", team: "acme", end: t0.Add(10 * time.Second), in: 30, out: 3}
	long := request{call: strings.Repeat("x", 300), resp: "chatcmpl-2", team: "acme", end: t0.Add(11 * time.Second), in: 50000, out: 2000}
	r.clk.set(t0.Add(15 * time.Second))
	res := r.push(t, "litellm", normal, long) // the operator's LiteLLM, shared ingest token
	r.tick(t, t0.Add(2*time.Minute))
	if got := total(r.ledgerEvents(t), "inference_input_tokens"); got != 50030 {
		reason := ""
		if len(res.Rejected) > 0 {
			reason = res.Rejected[0].Error
		}
		t.Errorf("the operator's LiteLLM pushed two of acme's requests (30 and 50000 input tokens); acme was billed %v: "+
			"the request whose client-chosen call ID is 300 characters was refused (%q) and is billed nowhere", got, reason)
	}
}

// Records without a time are dated on arrival, and their row IDs are kept
// for an hour (arrivals), so a retry keeps the time. The IDs are taken for
// every usage key before any metric is checked, so records that are then
// refused (unknown metrics) still leave an entry per usage key for an hour.
// A tenant-bound token (500 requests per second, 16 MiB per request) then
// fills vBilling's memory with requests that are all refused (400) and
// bill nothing; the chart's limit is 256Mi. None of the v0.3.1 bounds
// (headers, client names, 64 MiB per push to the records ledger) applies:
// nothing is written.
func TestReview4RefusedRecordsWithoutATimeHoldMemoryForAnHour(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	var b strings.Builder
	b.WriteByte('[')
	const recs, keys = 500, 400
	for i := 0; i < recs; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"r%d","usage":{`, i)
		for k := 0; k < keys; k++ {
			if k > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"m%d":1`, k)
		}
		b.WriteString("}}")
	}
	b.WriteByte(']')
	body := []byte(b.String())

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res, err := r.in.IngestPush(Native, Push{Client: "edge-gw", Bound: "acme"}, body)
	if err != nil || res.Accepted != 0 || len(res.Rejected) != recs {
		t.Fatalf("every record must be refused (unknown metrics): %d accepted, %d refused, %v", res.Accepted, len(res.Rejected), err)
	}
	answer, _ := json.Marshal(res) // what POST /api/v1/records answers (400)
	answerLen := len(answer)
	res, answer = Result{}, nil
	runtime.GC()
	runtime.ReadMemStats(&after) // what vBilling still holds once the answer is gone
	runtime.KeepAlive(r.in)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if retained > int64(len(body)) {
		t.Errorf("one push of %.1f MiB from a tenant-bound token, every record refused (400, nothing stored or billed), left %.1f MiB "+
			"on the heap for an hour (%d arrival entries; about %.0f MiB for one 16 MiB push), and its answer is %.1f MiB of rejections: "+
			"at the token's 500 requests per second, a tenant exhausts the pod's 256Mi with refused requests",
			float64(len(body))/(1<<20), float64(retained)/(1<<20), len(r.in.arrivals.first),
			float64(retained)/float64(len(body))*16, float64(answerLen)/(1<<20))
	}
	_, _ = res, answer
}

// LiteLLM's pull reads the operator's LiteLLM, which serves every tenant
// (not pinned). A tenant runs a LiteLLM of its own inside its cluster and
// pushes with the token bound to it, as security.html recommends. That
// LiteLLM is not the one vBilling pulls, and none of its requests is ever
// read by the pull, yet the push turns a source failing that says every
// request is billed twice and tells the operator to pin the pull to the
// tenant. Pinned, the pull bills every request of the shared LiteLLM, of
// every team, to that tenant. The tenant raises it at will, and it never
// clears while the tenant keeps pushing.
func TestReview4TenantsOwnLiteLLMIsNotFlaggedAsTheOneVBillingPulls(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r.in.health = h
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{}) // the operator's shared LiteLLM, billed by team
	shared := request{call: "c-shared", resp: "chatcmpl-shared", team: "initech", end: t0.Add(5 * time.Second), in: 70, out: 7}
	fake.add(shared.row(t))

	// acme's own LiteLLM: a different proxy, its own call IDs.
	own := request{call: "c-acme-own", resp: "chatcmpl-acme-own", team: "acme-team", end: t0.Add(10 * time.Second), in: 30, out: 3}
	body, _ := json.Marshal([]map[string]any{own.payload(t)})
	r.clk.set(t0.Add(15 * time.Second))
	if res, err := r.in.Ingest("litellm", "litellm", "acme", body); err != nil || res.Accepted != 1 {
		t.Fatalf("acme's push: %+v %v", res, err)
	}
	r.clk.set(t0.Add(3 * time.Minute))
	if _, err := p.Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range h.Problems() {
		if strings.Contains(s.Hint, "LITELLM_TENANT=acme") {
			t.Errorf("%s is %s: %q (hint: %q), although the pull never read a request acme pushed: "+
				"following the hint bills every request of the shared LiteLLM (initech's too) to acme", s.Name, s.Status, s.Message, s.Hint)
		}
	}
}
