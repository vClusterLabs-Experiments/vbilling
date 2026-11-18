package records

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// The fixtures are one real request through LiteLLM v1.108.11 with a mock
// response: the StandardLoggingPayload its generic API logger posted, and
// the /spend/logs/v2 page that returned the same request from Postgres.
func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func clone(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(m)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// request is one LiteLLM request, as both copies describe it.
type request struct {
	call, resp, team string
	end              time.Time // with microseconds, as LiteLLM measures it
	in, out          int
	dur              time.Duration // from start to end, default 20ms
}

func (q request) start() time.Time {
	if q.dur == 0 {
		return q.end.Add(-20 * time.Millisecond)
	}
	return q.end.Add(-q.dur)
}

// num is a number as a JSON decoder with UseNumber returns it.
func num(n int) json.Number { return json.Number(strconv.Itoa(n)) }

// payload is the request as the generic API logger posts it: times are
// float Unix seconds with microseconds.
func (q request) payload(t *testing.T) map[string]any {
	p := clone(t, fixture(t, "litellm-standard-logging-payload.json"))
	p["id"], p["litellm_call_id"] = q.resp, q.call
	sec := func(at time.Time) json.Number {
		return json.Number(strconv.FormatFloat(float64(at.UnixMicro())/1e6, 'f', -1, 64))
	}
	p["startTime"], p["endTime"] = sec(q.start()), sec(q.end)
	p["prompt_tokens"], p["completion_tokens"] = num(q.in), num(q.out)
	p["metadata"].(map[string]any)["user_api_key_team_id"] = q.team
	return p
}

// row is the request as /spend/logs/v2 returns it: times are rounded to the
// millisecond, like Postgres stores them in timestamp(3) columns.
func (q request) row(t *testing.T) map[string]any {
	r := clone(t, fixture(t, "litellm-spend-logs-v2.json")["data"].([]any)[0].(map[string]any))
	r["request_id"], r["litellm_call_id"], r["team_id"] = q.resp, q.call, q.team
	ms := func(at time.Time) string { return at.Round(time.Millisecond).Format("2006-01-02T15:04:05.000+00:00") }
	r["startTime"], r["endTime"] = ms(q.start()), ms(q.end)
	r["prompt_tokens"], r["completion_tokens"] = num(q.in), num(q.out)
	return r
}

// fakeLiteLLM serves /spend/logs/v2 like LiteLLM does: admin key required,
// start_date and end_date compared to startTime, oldest first, pages.
type fakeLiteLLM struct {
	mu       sync.Mutex
	rows     []map[string]any
	requests []string
	status   int
}

func (f *fakeLiteLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.RawQuery)
	if r.URL.Path != "/spend/logs/v2" || r.Header.Get("Authorization") != "Bearer sk-admin" {
		http.Error(w, `{"error":{"message":"Authentication Error, Invalid proxy server token passed","type":"auth_error","code":"401"}}`, http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		http.Error(w, `{"error":{"message":"db unavailable"}}`, f.status)
		return
	}
	q := r.URL.Query()
	if q.Get("sort_by") != "startTime" || q.Get("sort_order") != "asc" || q.Get("status_filter") != "success" {
		http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
		return
	}
	from, err1 := time.Parse("2006-01-02 15:04:05", q.Get("start_date"))
	to, err2 := time.Parse("2006-01-02 15:04:05", q.Get("end_date"))
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if err1 != nil || err2 != nil || page < 1 || size < 1 || size > 1000 {
		http.Error(w, "bad query "+r.URL.RawQuery, http.StatusBadRequest)
		return
	}
	var match []map[string]any
	for _, row := range f.rows {
		st, _ := time.Parse(time.RFC3339Nano, row["startTime"].(string))
		if s, _ := row["status"].(string); s != "success" || st.Before(from) || st.After(to) {
			continue
		}
		match = append(match, row)
	}
	sort.SliceStable(match, func(i, j int) bool { return match[i]["startTime"].(string) < match[j]["startTime"].(string) })
	data := []map[string]any{}
	if lo := (page - 1) * size; lo < len(match) {
		data = match[lo:min(lo+size, len(match))]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "total": len(match), "page": page, "page_size": size,
		"total_pages": (len(match) + size - 1) / size, "total_is_capped": false})
}

func (f *fakeLiteLLM) add(rows ...map[string]any) {
	f.mu.Lock()
	f.rows = append(f.rows, rows...)
	f.mu.Unlock()
}

func newPuller(t *testing.T, r *rig, srv *httptest.Server, opts LiteLLMPullOptions) (*LiteLLMPuller, *health.Registry) {
	t.Helper()
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	h.SetClock(r.clk.now)
	opts.URL, opts.Key, opts.Now = srv.URL, "sk-admin", r.clk.now
	if opts.PageSize == 0 {
		opts.PageSize = 2 // several pages in every test
	}
	return NewLiteLLMPuller(r.in, opts, h, nil), h
}

func (r *rig) push(t *testing.T, client string, qs ...request) Result {
	t.Helper()
	var objs []map[string]any
	for _, q := range qs {
		objs = append(objs, q.payload(t))
	}
	body, _ := json.Marshal(objs)
	res, err := r.in.Ingest("litellm", client, "", body)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (r *rig) tick(t *testing.T, at time.Time) {
	t.Helper()
	r.clk.set(at)
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
}

func byMetric(evs map[string]usage.Event, metric string) (onTime, late float64) {
	for _, e := range evs {
		if e.Metric != metric {
			continue
		}
		if e.Properties["late"] == true {
			late += e.Quantity
		} else {
			onTime += e.Quantity
		}
	}
	return onTime, late
}

func TestLiteLLMRequestID(t *testing.T) {
	id := func(call, resp string) string {
		return LiteLLMRequestID(map[string]any{"litellm_call_id": call, "id": resp})
	}
	if got := id("c-1", "chatcmpl-9"); got != "c-1/chatcmpl-9" {
		t.Errorf("id = %q", got)
	}
	// Each copy of a cache hit appends its own timestamp to the response ID.
	if id("c-1", "chatcmpl-9_cache_hit1794972782.959040") != id("c-1", "chatcmpl-9_cache_hit1794972783.1") {
		t.Error("the cache-hit suffix must not be part of the request ID")
	}
	if id("c-1", "batch_7_batch_cost") != id("c-1", "batch_7") {
		t.Error("the spend log's batch cost suffix must not be part of the request ID")
	}
	// A response ID repeated by the provider (or a cache) must not merge
	// requests, and neither must a call ID the client chose twice.
	if id("c-1", "chatcmpl-same") == id("c-2", "chatcmpl-same") || id("c-1", "chatcmpl-a") == id("c-1", "chatcmpl-b") {
		t.Error("distinct requests must keep distinct IDs")
	}
	if id("", "chatcmpl-9") != "chatcmpl-9" || id("c-1", "") != "c-1" {
		t.Error("one part alone must still identify the request")
	}
}

// The payload carries microseconds, the spend log milliseconds. A request
// that ended in the last half millisecond of a window is stored in the next
// one; the format rounds both copies the same way, so they share a window
// and an ID, and count once.
func TestLiteLLMPushAndSpendLogShareWindowAndID(t *testing.T) {
	end := t0.Add(59*time.Second + 999_600*time.Microsecond) // 10:00:59.9996 -> stored as 10:01:00.000
	q := request{call: "c-edge", resp: "chatcmpl-edge", team: "acme", end: end, in: 40, out: 7}
	m := LiteLLM
	push, _, err := Map("litellm", m, q.payload(t))
	if err != nil {
		t.Fatal(err)
	}
	pull, _, err := Map("litellm", m, SpendLogPayload(q.row(t)))
	if err != nil {
		t.Fatal(err)
	}
	if push.ID != pull.ID || !push.Time.Equal(pull.Time) || push.TenantKey != pull.TenantKey || push.SKU != pull.SKU {
		t.Fatalf("push %+v\npull %+v", push, pull)
	}
	if want := t0.Add(time.Minute); !push.Time.Equal(want) {
		t.Errorf("time = %s, want %s (rounded half up to the millisecond)", push.Time, want)
	}
	if push.Usage["inference_input_tokens"] != 40 || pull.Usage["inference_output_tokens"] != 7 {
		t.Errorf("usage push %v pull %v", push.Usage, pull.Usage)
	}
}

// The pull fills what the push lost, a request seen by both counts once,
// and pulling the same range again stores nothing.
func TestLiteLLMPullFillsGapsWithoutDoubleCounting(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var qs []request
	for i := 0; i < 5; i++ {
		qs = append(qs, request{call: fmt.Sprintf("c-%d", i), resp: fmt.Sprintf("chatcmpl-%d", i), team: "acme",
			end: t0.Add(time.Duration(5+i*10)*time.Second + 123_456*time.Microsecond), in: 100 + i, out: 10 + i})
	}
	// The push delivered requests 0-2; LiteLLM's batch with 3 and 4 was lost.
	r.clk.set(t0.Add(56 * time.Second))
	if res := r.push(t, "litellm", qs[:3]...); res.Accepted != 3 {
		t.Fatalf("push = %+v", res)
	}
	for _, q := range qs {
		fake.add(q.row(t))
	}
	p, h := newPuller(t, r, srv, LiteLLMPullOptions{})
	r.clk.set(t0.Add(70 * time.Second)) // within Settle: the pulled records are on time
	res, err := p.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != 5 || res.Accepted != 2 || res.AlreadyStored != 3 || res.Pages != 3 {
		t.Fatalf("pull = %+v", res)
	}
	// Pulling the same range again stores nothing.
	r.clk.set(t0.Add(75 * time.Second))
	if again, err := p.Pull(context.Background()); err != nil || again.Accepted != 0 || again.AlreadyStored != 5 {
		t.Fatalf("second pull = %+v, %v", again, err)
	}
	// Even a request stored twice (pushed again after the pull) counts once.
	r.push(t, "litellm", qs[4])
	r.tick(t, t0.Add(2*time.Minute))
	onTime, late := byMetric(r.ledgerEvents(t), "inference_input_tokens")
	if onTime != 100+101+102+103+104 || late != 0 {
		t.Errorf("input tokens on time %v late %v, want 510 on time", onTime, late)
	}
	src := h.List()
	if len(src) != 1 || src[0].Name != "records/litellm/pull" || src[0].Status != health.OK || src[0].Series != 5 {
		t.Errorf("health = %+v", src)
	}
}

// Requests the push lost while vBilling was down are pulled after the
// window settled: they become a late adjustment, and requests the push did
// deliver are not billed again.
func TestLiteLLMPullAfterSettleIsLateAdjustment(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	pushed := request{call: "c-a", resp: "chatcmpl-a", team: "acme", end: t0.Add(10 * time.Second), in: 50, out: 5}
	lost := request{call: "c-b", resp: "chatcmpl-b", team: "acme", end: t0.Add(30 * time.Second), in: 70, out: 7}
	r.clk.set(t0.Add(31 * time.Second))
	r.push(t, "litellm", pushed)
	r.tick(t, t0.Add(81*time.Second)) // window 10:00 committed with "a" only
	fake.add(pushed.row(t), lost.row(t))
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{})
	r.clk.set(t0.Add(3 * time.Minute))
	if res, err := p.Pull(context.Background()); err != nil || res.Accepted != 1 || res.AlreadyStored != 1 {
		t.Fatalf("pull = %+v, %v", res, err)
	}
	r.tick(t, t0.Add(4*time.Minute+30*time.Second))
	evs := r.ledgerEvents(t)
	onTime, late := byMetric(evs, "inference_input_tokens")
	if onTime != 50 || late != 70 {
		t.Fatalf("input tokens on time %v late %v, want 50 and a late adjustment of 70", onTime, late)
	}
	for _, e := range evs {
		if e.Properties["late"] == true && (!e.WindowStart.Equal(t0) || e.Source != SourceLate) {
			t.Errorf("late adjustment must keep its window: %+v", e)
		}
	}
}

// The first pull after an outage starts at the end of the last successful
// pull minus the lookback, so a long outage is covered.
func TestLiteLLMPullCoversOutageSinceLastPull(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	state := kv.Memory()
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{Lookback: 10 * time.Minute, State: state})
	r.clk.set(t0)
	first, err := p.Pull(context.Background())
	if err != nil || !first.From.Equal(t0.Add(-10*time.Minute)) {
		t.Fatalf("first pull %+v, %v", first, err)
	}
	// Three hours later (vBilling was down), with a request from the outage.
	q := request{call: "c-down", resp: "chatcmpl-down", team: "acme", end: t0.Add(time.Hour), in: 9, out: 1}
	fake.add(q.row(t))
	r.clk.set(t0.Add(3 * time.Hour))
	p2, _ := newPuller(t, r, srv, LiteLLMPullOptions{Lookback: 10 * time.Minute, State: state}) // restarted
	res, err := p2.Pull(context.Background())
	if err != nil || !res.From.Equal(t0.Add(-10*time.Minute)) || res.Accepted != 1 {
		t.Fatalf("pull after outage = %+v, %v", res, err)
	}
	if through, _ := state.Get(liteLLMPullState); through != t0.Add(3*time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("state = %q", through)
	}
}

// A pull after a long outage reads the range an hour at a time. Requests
// pushed before the outage are found in every chunk, including a request
// that ran so long its window lies beyond the chunk's index.
func TestLiteLLMPullCatchUpAcrossChunks(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	var all []request
	for i := 0; i < 18; i++ { // one request every 10 minutes for 3 hours
		all = append(all, request{call: fmt.Sprintf("c-%d", i), resp: fmt.Sprintf("chatcmpl-%d", i), team: "acme",
			end: t0.Add(time.Duration(i)*10*time.Minute + 30*time.Second), in: 10 + i, out: 1})
	}
	// Started in the first hour, ended two and a half hours later.
	long := request{call: "c-long", resp: "chatcmpl-long", team: "acme", end: t0.Add(2*time.Hour + 35*time.Minute), dur: 2*time.Hour + 30*time.Minute, in: 1000, out: 1}
	all = append(all, long)
	pushed := 0
	for i, q := range all {
		if i%2 == 0 || q.call == "c-long" {
			r.clk.set(q.end.Add(time.Second))
			r.push(t, "litellm", q)
			pushed++
		}
		fake.add(q.row(t))
	}
	state := kv.Memory()
	_ = state.Set(liteLLMPullState, t0.Format(time.RFC3339Nano)) // the last pull before the outage
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{Lookback: 10 * time.Minute, State: state, PageSize: 3})
	r.clk.set(t0.Add(3*time.Hour + 5*time.Minute))
	res, err := p.Pull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != len(all)-pushed || res.AlreadyStored != pushed || res.Rows != len(all) {
		t.Fatalf("pull = %d new, %d already stored, %d rows; want %d, %d, %d", res.Accepted, res.AlreadyStored, res.Rows, len(all)-pushed, pushed, len(all))
	}
}

// The tenant is part of the record ID as the gateway sent it, before
// aliases resolve it, so a request pushed before an alias existed and
// pulled after counts once.
func TestLiteLLMPullAfterAliasChangeCountsOnce(t *testing.T) {
	r := newRig(t, Options{Settle: 20 * time.Second})
	aliases := map[string]string{}
	var mu sync.Mutex
	r.resolve = func(v string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := aliases[v]; ok {
			return t, true
		}
		return v, false
	}
	r.open(t)
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	q := request{call: "c-1", resp: "chatcmpl-1", team: "team-f9b2", end: t0.Add(10 * time.Second), in: 30, out: 3}
	r.clk.set(t0.Add(15 * time.Second))
	r.push(t, "litellm", q) // billed to the raw team ID for now
	mu.Lock()
	aliases["team-f9b2"] = "initech"
	mu.Unlock()
	fake.add(q.row(t))
	p, _ := newPuller(t, r, srv, LiteLLMPullOptions{})
	r.clk.set(t0.Add(70 * time.Second))
	if res, err := p.Pull(context.Background()); err != nil || res.Accepted != 0 || res.AlreadyStored != 1 {
		t.Fatalf("pull = %+v, %v", res, err)
	}
}

// A puller with a tenant matches a push through a tenant-bound token, and a
// puller with another client name never matches (its IDs differ).
func TestLiteLLMPullTenantAndClientMustMatchPush(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	q := request{call: "c-1", resp: "chatcmpl-1", team: "team-x", end: t0.Add(10 * time.Second), in: 30, out: 3}
	body, _ := json.Marshal([]map[string]any{q.payload(t)})
	r.clk.set(t0.Add(15 * time.Second))
	if _, err := r.in.Ingest("litellm", "litellm", "globex", body); err != nil {
		t.Fatal(err)
	}
	fake.add(q.row(t))
	bound, _ := newPuller(t, r, srv, LiteLLMPullOptions{Tenant: "globex"})
	if res, err := bound.Pull(context.Background()); err != nil || res.Accepted != 0 || res.AlreadyStored != 1 {
		t.Fatalf("bound pull = %+v, %v", res, err)
	}
	other, _ := newPuller(t, r, srv, LiteLLMPullOptions{Tenant: "globex", Client: "gateway-eu"})
	if res, err := other.Pull(context.Background()); err != nil || res.Accepted != 1 {
		t.Fatalf("a different client is a different emitter: %+v, %v", res, err)
	}
}

func TestLiteLLMPullFailuresAndMissingCallID(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	fake := &fakeLiteLLM{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	r.clk.set(t0.Add(time.Minute))

	p, h := newPuller(t, r, srv, LiteLLMPullOptions{})
	p.opts.Key = "sk-wrong"
	if _, err := p.Pull(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong key must fail: %v", err)
	}
	if s := h.List()[0]; s.Status != health.Failing || s.LastErrorMessage == "" || s.Hint == "" {
		t.Errorf("health = %+v", s)
	}

	fake.status = http.StatusInternalServerError
	p, _ = newPuller(t, r, srv, LiteLLMPullOptions{})
	if _, err := p.Pull(context.Background()); err == nil {
		t.Fatal("a 500 must fail the pull")
	}
	if _, ok := p.opts.State.Get(liteLLMPullState); ok {
		t.Error("a failed pull must not move the pull state")
	}
	fake.status = 0

	// An older LiteLLM has no litellm_call_id column: its rows cannot be
	// matched to pushed requests, so they are not ingested.
	q := request{call: "c-1", resp: "chatcmpl-1", team: "acme", end: t0.Add(10 * time.Second), in: 3, out: 1}
	row := q.row(t)
	delete(row, "litellm_call_id")
	fake.add(row)
	p, h = newPuller(t, r, srv, LiteLLMPullOptions{})
	res, err := p.Pull(context.Background())
	if err != nil || res.NoCallID != 1 || res.Accepted != 0 {
		t.Fatalf("pull = %+v, %v", res, err)
	}
	if s := h.List()[0]; s.Status != health.Failing || !strings.Contains(s.Hint, "v1.103.0") {
		t.Errorf("health = %+v", s)
	}
}

func TestSpendLogPayloadFromRealRow(t *testing.T) {
	row := fixture(t, "litellm-spend-logs-v2.json")["data"].([]any)[0].(map[string]any)
	push := fixture(t, "litellm-standard-logging-payload.json")
	a, _, err := Map("litellm", LiteLLM, push)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := Map("litellm", LiteLLM, SpendLogPayload(row))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.TenantKey != b.TenantKey || a.SKU != b.SKU || !a.Time.Equal(b.Time) ||
		a.Usage["inference_input_tokens"] != b.Usage["inference_input_tokens"] || a.Usage["inference_output_tokens"] != b.Usage["inference_output_tokens"] {
		t.Errorf("the same real request maps differently:\npush %+v\npull %+v", a, b)
	}
	if a.TenantKey != "acme-ai" || a.SKU != "llama-70b" || a.Usage["inference_input_tokens"] != 10 || a.Usage["inference_output_tokens"] != 20 {
		t.Errorf("mapped %+v", a)
	}
}
