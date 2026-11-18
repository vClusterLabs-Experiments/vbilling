package records

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type rig struct {
	clk             *clock
	dir             string
	records, ledger *spool.Spool
	in              *Ingester
	agg             *Aggregator
	opts            Options
	resolve         TenantResolver
}

func init() {
	for _, code := range []string{"tokens_in", "tokens_out"} {
		_ = usage.RegisterCustom(usage.MetricDef{Code: code, Unit: "tokens", Billable: true})
	}
	if err := (Formats{"litellm": LiteLLM}).Declare(); err != nil {
		panic(err)
	}
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	r := &rig{clk: &clock{t: t0}, dir: t.TempDir(), opts: opts}
	r.open(t)
	return r
}

func (r *rig) open(t *testing.T) {
	t.Helper()
	var err error
	r.records, err = spool.Open(filepath.Join(r.dir, "records"), spool.Options{NoSync: true, Now: r.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	r.ledger, err = spool.Open(filepath.Join(r.dir, "ledger"), spool.Options{NoSync: true, Now: r.clk.now})
	if err != nil {
		t.Fatal(err)
	}
	o := r.opts
	o.Now = r.clk.now
	o.Window = time.Minute
	formats, err := LoadFormats("", true)
	if err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	r.in = NewIngester(r.records, formats, o, r.resolve, h, nil)
	r.agg = NewAggregator(r.records, r.ledger, o, h, nil)
}

func (r *rig) close() {
	r.records.Close()
	r.ledger.Close()
}

func (r *rig) post(t *testing.T, format, body string) Result {
	t.Helper()
	res, err := r.in.Ingest(format, format, "", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ledgerEvents returns committed usage events by ID.
func (r *rig) ledgerEvents(t *testing.T) map[string]usage.Event {
	t.Helper()
	out := map[string]usage.Event{}
	if err := r.ledger.Scan(t0.Add(-time.Hour), t0.Add(24*time.Hour), func(e *usage.Event) error {
		out[e.ID] = *e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func total(evs map[string]usage.Event, metric string) float64 {
	var s float64
	for _, e := range evs {
		if e.Metric == metric {
			s += e.Quantity
		}
	}
	return s
}

func native(id, tenant string, at time.Time, in, out float64) string {
	return fmt.Sprintf(`{"id":%q,"tenant":%q,"time":%q,"usage":{"tokens_in":%v,"tokens_out":%v},"sku":"llama"}`, id, tenant, at.Format(time.RFC3339Nano), in, out)
}

// Per-request records in the same minute are summed, not collapsed, and a
// retried batch is not billed twice.
func TestRequestsInOneWindowAreSummedAndRetriesDeduplicated(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, native(fmt.Sprintf("req-%d", i), "acme", t0.Add(time.Duration(i)*time.Second), 100, 7))
	}
	body := strings.Join(lines, "\n")
	r.clk.set(t0.Add(50 * time.Second))
	if res := r.post(t, Native, body); res.Accepted != 50 || res.UsageRows != 100 {
		t.Fatalf("first post = %+v", res)
	}
	r.post(t, Native, body) // the emitter retried
	r.clk.set(t0.Add(time.Minute + 31*time.Second))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	evs := r.ledgerEvents(t)
	if got := total(evs, "tokens_in"); got != 5000 {
		t.Errorf("tokens_in = %v, want 5000 (50 requests x 100, retries ignored)", got)
	}
	if got := total(evs, "tokens_out"); got != 350 {
		t.Errorf("tokens_out = %v, want 350", got)
	}
	if len(evs) != 2 {
		t.Errorf("want one event per metric for the window, got %d", len(evs))
	}
	for _, e := range evs {
		if e.Properties["records"] != float64(50) && e.Properties["records"] != 50 {
			t.Errorf("records property = %v", e.Properties["records"])
		}
		if e.SKU != "llama" || e.Source != SourceBase || !e.WindowStart.Equal(t0) {
			t.Errorf("event = %+v", e)
		}
	}
}

// Records arriving after their window closed are billed as a separate late
// adjustment to that window; a late copy of an on-time record is ignored,
// even when both arrive in the same arrival window.
func TestLateRecordsAreAdjustmentsAndNeverDoubleCount(t *testing.T) {
	r := newRig(t, Options{Settle: 30 * time.Second})
	defer r.close()
	// On time: arrives 10s after the window ends (within Settle).
	r.clk.set(t0.Add(70 * time.Second))
	r.post(t, Native, native("a", "acme", t0.Add(10*time.Second), 10, 0))
	// Late: arrives 45s after the window ends, plus a retry of "a".
	r.clk.set(t0.Add(105 * time.Second))
	r.post(t, Native, native("b", "acme", t0.Add(20*time.Second), 5, 0)+"\n"+native("a", "acme", t0.Add(10*time.Second), 10, 0))
	r.clk.set(t0.Add(2*time.Minute + 1*time.Second))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	evs := r.ledgerEvents(t)
	if got := total(evs, "tokens_in"); got != 15 {
		t.Fatalf("tokens_in = %v, want 15 (a once, b late)", got)
	}
	var onTime, late int
	for _, e := range evs {
		if !e.WindowStart.Equal(t0) {
			t.Errorf("late usage must keep its own window, got %s", e.WindowStart)
		}
		if e.Properties["late"] == true {
			late++
			if e.Quantity != 5 {
				t.Errorf("late adjustment = %v, want 5", e.Quantity)
			}
		} else {
			onTime++
		}
	}
	if onTime != 1 || late != 1 {
		t.Errorf("on-time %d, late %d", onTime, late)
	}
}

// After a restart the aggregator resumes from the ledger watermarks and
// produces the same events, and a record appended after the restart can
// never fall into a window that was already closed, even if the clock went
// back.
func TestAggregationResumesDeterministically(t *testing.T) {
	r := newRig(t, Options{})
	r.clk.set(t0.Add(30 * time.Second))
	r.post(t, Native, native("x", "acme", t0.Add(5*time.Second), 1, 0))
	r.clk.set(t0.Add(95 * time.Second))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	before := r.ledgerEvents(t)
	r.close()

	// Restart with the clock a minute behind (still inside MaxFuture, so
	// the record is accepted), then a record for the closed window arrives.
	r.clk.set(t0.Add(-time.Minute))
	r.open(t)
	defer r.close()
	r.post(t, Native, native("y", "acme", t0.Add(6*time.Second), 2, 0))
	r.clk.set(t0.Add(3 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	after := r.ledgerEvents(t)
	for id := range before {
		if _, ok := after[id]; !ok {
			t.Errorf("event %s disappeared", id)
		}
	}
	if got := total(after, "tokens_in"); got != 3 {
		t.Errorf("tokens_in = %v, want 3 (x on time, y as a late adjustment)", got)
	}
}

func TestLiteLLMPayloads(t *testing.T) {
	r := newRig(t, Options{})
	r.resolve = func(v string) (string, bool) {
		if v == "team-123" {
			return "acme", true
		}
		return v, false
	}
	r.open(t)
	defer r.close()
	body := `[
 {"id":"chatcmpl-1","status":"success","model":"meta-llama/Llama-3.1-70B","model_group":"llama-70b","prompt_tokens":512,"completion_tokens":128,
  "startTime":1794996000.5,"endTime":1794996001.25,"metadata":{"user_api_key_team_id":"team-123"}},
 {"id":"chatcmpl-2","status":"failure","model_group":"llama-70b","prompt_tokens":0,"completion_tokens":0,"endTime":1794996002,
  "metadata":{"user_api_key_team_id":"team-123"}},
 {"id":"chatcmpl-3","status":"success","model_group":"llama-8b","prompt_tokens":10,"completion_tokens":3,"endTime":"2026-11-18T10:00:03",
  "metadata":{"user_api_key_team_id":"team-999"}}
]`
	r.clk.set(time.Unix(1794996005, 0).UTC())
	res := r.post(t, "litellm", body)
	if res.Accepted != 2 || res.Skipped != 1 || res.UsageRows != 4 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.UnknownTenants) != 1 || res.UnknownTenants[0] != "team-999" || len(res.Warnings) == 0 {
		t.Errorf("unknown tenants must be reported: %+v", res)
	}
}

func TestCustomMappingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "formats.yaml")
	if err := os.WriteFile(path, []byte(`kong:
  id: request.id
  time: started_at
  tenant: consumer.username
  tenantMap: {acme-prod: acme}
  usage:
    tokens_in: ai.proxy.usage.prompt_tokens
    tokens_out: ai.proxy.usage.completion_tokens
  sku: ai.proxy.meta.response_model
  dimensions:
    route: route.name
`), 0o600); err != nil {
		t.Fatal(err)
	}
	formats, err := LoadFormats(path, true)
	if err != nil {
		t.Fatal(err)
	}
	rec, skip, err := Map("kong", formats["kong"], mustParse(t, `{"request":{"id":"k-1"},"started_at":1794996000123,"consumer":{"username":"acme-prod"},
	  "route":{"name":"chat"},"ai":{"proxy":{"usage":{"prompt_tokens":"40","completion_tokens":2},"meta":{"response_model":"gpt-oss-120b"}}}}`))
	if err != nil || skip {
		t.Fatal(err, skip)
	}
	if rec.ID != "k-1" || rec.Tenant != "acme" || rec.Usage["tokens_in"] != 40 || rec.Usage["tokens_out"] != 2 ||
		rec.SKU != "gpt-oss-120b" || rec.Dimensions["route"] != "chat" || !rec.Time.Equal(time.UnixMilli(1794996000123).UTC()) {
		t.Errorf("mapped record = %+v", rec)
	}
	if _, err := LoadFormats(writeFile(t, "vbilling:\n  id: x\n  time: y\n  usage: {a: b}\n"), true); err == nil {
		t.Error("redefining the native format must fail")
	}
}

func TestRejectionsAndTenantBoundTokens(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	r.clk.set(t0)
	res := r.post(t, Native, `{"tenant":"acme","usage":{"tokens_in":1}}
{"id":"r2","usage":{"tokens_in":1}}
{"id":"r3","tenant":"acme","usage":{"nope":1}}
{"id":"r4","tenant":"acme","time":"2030-01-01T00:00:00Z","usage":{"tokens_in":1}}
{"id":"r5","tenant":"acme","usage":{"tokens_in":-3}}`)
	if res.Accepted != 0 || len(res.Rejected) != 5 {
		t.Fatalf("result = %+v", res)
	}
	for i, want := range []string{"no request ID", "no tenant", "unknown metric", "future", "non-negative"} {
		if !strings.Contains(res.Rejected[i].Error, want) {
			t.Errorf("rejection %d = %q, want %q", i, res.Rejected[i].Error, want)
		}
	}
	// A token bound to a tenant decides the tenant, whatever the payload says.
	bound, err := r.in.Ingest(Native, "gw", "globex", []byte(`{"id":"r6","tenant":"acme","usage":{"tokens_in":4}}`))
	if err != nil || bound.Accepted != 1 {
		t.Fatalf("bound = %+v %v", bound, err)
	}
	r.clk.set(t0.Add(2 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, e := range r.ledgerEvents(t) {
		if e.Tenant != "globex" {
			t.Errorf("tenant = %s, want globex from the token binding", e.Tenant)
		}
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 11, 18, 10, 0, 1, 250_000_000, time.UTC)
	for _, v := range []any{"2026-11-18T10:00:01.25Z", "2026-11-18T10:00:01.25", float64(want.Unix()) + 0.25,
		float64(want.UnixMilli()), float64(want.UnixMicro()), "1794996001.25"} {
		got, err := parseTime(v)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTime(%v) = %v, %v", v, got, err)
		}
	}
}

func mustParse(t *testing.T, s string) map[string]any {
	objs, err := Parse([]byte(s))
	if err != nil || len(objs) != 1 {
		t.Fatal(err)
	}
	return objs[0]
}

func writeFile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "f.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
