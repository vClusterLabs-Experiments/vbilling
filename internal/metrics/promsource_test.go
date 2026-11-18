package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeSeries is a stored series for fakeProm.
type fakeSeries struct {
	name    string
	labels  map[string]string
	samples []promSample
}

// fakeProm stores raw series and answers counter reads (selector[Ns]) with
// the samples in (time-N, time], like Prometheus does.
type fakeProm struct {
	mu      sync.Mutex
	series  []fakeSeries
	down    bool
	queries []string
}

var matcherRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)(=|!=)"((?:[^"\\]|\\.)*)"`)

func (f *fakeProm) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query().Get("query")
		f.queries = append(f.queries, q)
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"error","error":"prometheus is down"}`))
			return
		}
		at, _ := strconv.ParseInt(r.URL.Query().Get("time"), 10, 64)
		open := strings.LastIndex(q, "[")
		secs, _ := strconv.Atoi(strings.TrimSuffix(q[open+1:], "s]"))
		sel := q[:open]
		name := sel
		if i := strings.Index(sel, "{"); i >= 0 {
			name = sel[:i]
		}
		var result []map[string]any
		for _, s := range f.series {
			if s.name != name || !matches(sel, s.labels) {
				continue
			}
			var values [][]any
			for _, smp := range s.samples {
				if smp.T.Unix() > at-int64(secs) && smp.T.Unix() <= at {
					values = append(values, []any{float64(smp.T.UnixMilli()) / 1000, strconv.FormatFloat(smp.V, 'f', -1, 64)})
				}
			}
			if len(values) > 0 {
				lbl := map[string]string{"__name__": s.name}
				for k, v := range s.labels {
					lbl[k] = v
				}
				result = append(result, map[string]any{"metric": lbl, "values": values})
			}
		}
		if result == nil {
			result = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	}
}

func matches(sel string, labels map[string]string) bool {
	for _, m := range matcherRe.FindAllStringSubmatch(sel, -1) {
		k, op, v := m[1], m[2], m[3]
		if k == "__name__" {
			continue
		}
		if (op == "=") != (labels[k] == v) {
			return false
		}
	}
	return true
}

// counterSamples returns a counter sampled every 15s from start to end that
// grows by perSample, resetting to zero at resetAt (if non-zero).
func counterSamples(start, end time.Time, perSample float64, resetAt time.Time) []promSample {
	var out []promSample
	v := 0.0
	for ts := start; !ts.After(end); ts = ts.Add(15 * time.Second) {
		if !resetAt.IsZero() && !ts.Before(resetAt) && ts.Add(-15*time.Second).Before(resetAt) {
			v = 0
		}
		v += perSample
		out = append(out, promSample{T: ts, V: v})
	}
	return out
}

func promSourceFor(t *testing.T, url, statePath string, wm func() (time.Time, bool), metrics ...PromMetric) (*Collector, *PromSource, *health.Registry) {
	t.Helper()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", PrometheusURL: url, GPUUtilQuery: "none", EgressQuery: "none",
		PromMetrics: metrics, Now: func() time.Time { return we.Add(time.Hour) }})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p, err := c.NewPromSource(PromOptions{StatePath: statePath, Watermark: wm, Health: h})
	if err != nil {
		t.Fatal(err)
	}
	return c, p, h
}

var tokens = PromMetric{Code: "tokens_out", Counter: "vllm:generation_tokens_total{{selector}}", Dimensions: []string{"model_name"}, SKULabel: "model_name"}

// collectOK runs Collect and fails the test on any error.
func collectOK(t *testing.T, p *PromSource, targets []Target, windows []Window) []PromWindow {
	t.Helper()
	res, err := p.Collect(context.Background(), targets, windows)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func windowsFrom(start time.Time, n int) []Window {
	var out []Window
	for i := 0; i < n; i++ {
		out = append(out, Window{Start: start.Add(time.Duration(i) * time.Minute), End: start.Add(time.Duration(i+1) * time.Minute)})
	}
	out[len(out)-1].Latest = true
	return out
}

func sumBy(pws []PromWindow) map[time.Time]float64 {
	out := map[time.Time]float64{}
	for _, pw := range pws {
		for _, e := range pw.Events {
			out[pw.Window.End] += e.Quantity
		}
	}
	return out
}

// The windows of a counter add up to its true increase, with a reset in
// the middle, whether they are read one at a time or in one backfill.
func TestCounterWindowsTileExactly(t *testing.T) {
	start := ws.Add(-10 * time.Minute)
	reset := ws.Add(90 * time.Second)
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "llama"},
		samples: counterSamples(start, ws.Add(5*time.Minute), 7, reset)}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	target := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	wins := windowsFrom(ws, 4)

	// One window at a time, committing each.
	_, p1, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	one := map[time.Time]float64{}
	for _, w := range wins {
		res := collectOK(t, p1, []Target{target}, []Window{w})
		if err := p1.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for k, v := range sumBy(res) {
			one[k] += v
		}
	}
	// All windows in one backfill.
	_, p2, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	all := sumBy(collectOK(t, p2, []Target{target}, wins))

	total := 0.0
	for _, w := range wins {
		if math.Abs(one[w.End]-all[w.End]) > 1e-9 {
			t.Errorf("window %s: one-by-one %v, backfill %v", w.End.Format("15:04"), one[w.End], all[w.End])
		}
		total += one[w.End]
	}
	// 16 samples after ws (4 per minute); every one adds 7, the reset sample
	// counts its own value (7) because the counter restarted from zero.
	if total != 16*7 {
		t.Errorf("total = %v, want %v", total, 16*7)
	}
}

// A crash after the baselines are staged but before the window is committed
// must not count that window twice: the ledger watermark picks the
// generation, and the window is recomputed from the previous one.
func TestCounterBaselinesAreCrashSafe(t *testing.T) {
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "llama"},
		samples: counterSamples(ws.Add(-5*time.Minute), ws.Add(3*time.Minute), 10, time.Time{})}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	target := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	state := filepath.Join(t.TempDir(), "prom-counters.json")
	committed := time.Time{}
	wm := func() (time.Time, bool) { return committed, !committed.IsZero() }
	ledger := map[time.Time]float64{}
	commit := func(w Window, evs []usage.Event) error {
		for _, e := range evs {
			ledger[w.End] += e.Quantity
		}
		committed = w.End
		return nil
	}

	wins := windowsFrom(ws, 3)
	_, p, _ := promSourceFor(t, srv.URL, state, wm, tokens)
	if err := p.Commit(collectOK(t, p, []Target{target}, wins[:1]), commit); err != nil {
		t.Fatal(err)
	}
	// Window 2: baselines staged, then the ledger append fails (a crash).
	failing := func(Window, []usage.Event) error { return errors.New("disk full") }
	if err := p.Commit(collectOK(t, p, []Target{target}, wins[1:2]), failing); err == nil {
		t.Fatal("expected the commit error")
	}
	// Restart: a new store loads the generation the watermark allows.
	_, p2, _ := promSourceFor(t, srv.URL, state, wm, tokens)
	if err := p2.Commit(collectOK(t, p2, []Target{target}, wins[1:]), commit); err != nil {
		t.Fatal(err)
	}
	for _, w := range wins {
		if ledger[w.End] != 40 {
			t.Errorf("window %s = %v, want 40 (4 samples x 10)", w.End.Format("15:04"), ledger[w.End])
		}
	}
}

// When Prometheus is down, the windows wait (as the controller does: it
// does not commit on ErrPrometheusUnavailable) and each one gets its own
// exact increase once Prometheus answers again.
func TestCounterWindowsWaitForPrometheus(t *testing.T) {
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "llama"},
		samples: counterSamples(ws.Add(-5*time.Minute), ws.Add(3*time.Minute), 5, time.Time{})}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	target := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	_, p, h := promSourceFor(t, srv.URL, "", nil, tokens)
	wins := windowsFrom(ws, 3)
	got := map[time.Time]float64{}
	var pending []Window
	run := func(w Window) {
		pending = append(pending, w)
		res, err := p.Collect(context.Background(), []Target{target}, pending)
		if errors.Is(err, ErrPrometheusUnavailable) {
			return // nothing committed; the window waits
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = p.Commit(res, func(Window, []usage.Event) error { return nil })
		for k, v := range sumBy(res) {
			got[k] += v
		}
		pending = nil
	}
	run(wins[0])
	fp.down = true
	run(wins[1])
	if s := h.List(); len(s) != 1 || s[0].Status != health.Failing {
		t.Fatalf("health during outage = %+v", s)
	}
	fp.down = false
	run(wins[2])
	for _, w := range wins {
		if got[w.End] != 20 {
			t.Errorf("window %s = %v, want 20 (4 samples x 5, each in its own window)", w.End.Format("15:04"), got[w.End])
		}
	}
	if s := h.List(); s[0].Status != health.OK {
		t.Errorf("health after recovery = %+v", s[0])
	}
}

// When only some reads fail, the window is committed without them, and a
// failed counter bills its whole increase in the first window that reads it.
func TestCounterCatchesUpAfterPartialFailure(t *testing.T) {
	good := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "llama"},
		samples: counterSamples(ws.Add(-5*time.Minute), ws.Add(3*time.Minute), 5, time.Time{})}}}
	goodSrv := httptest.NewServer(good.handler(t))
	defer goodSrv.Close()
	flaky := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"model_name": "llama"},
		samples: counterSamples(ws.Add(-5*time.Minute), ws.Add(3*time.Minute), 5, time.Time{})}}}
	flakySrv := httptest.NewServer(flaky.handler(t))
	defer flakySrv.Close()
	shared := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	own := Target{Cluster: discovery.TenantCluster{Name: "own", Namespace: "vc-own",
		Annotations: map[string]string{discovery.AnnotationPrefix + "prometheus-url": flakySrv.URL}}, Tenant: "globex"}
	_, p, _ := promSourceFor(t, goodSrv.URL, "", nil, tokens)
	byTenant := map[string][]float64{}
	for i, w := range windowsFrom(ws, 3) {
		flaky.down = i == 1
		res := collectOK(t, p, []Target{shared, own}, []Window{w})
		_ = p.Commit(res, func(Window, []usage.Event) error { return nil })
		sums := map[string]float64{}
		for _, e := range res[0].Events {
			sums[e.Tenant] += e.Quantity
		}
		for _, tenant := range []string{"acme", "globex"} {
			byTenant[tenant] = append(byTenant[tenant], sums[tenant])
		}
	}
	if got := byTenant["acme"]; got[0] != 20 || got[1] != 20 || got[2] != 20 {
		t.Errorf("acme = %v, want 20 every window", got)
	}
	if got := byTenant["globex"]; got[0] != 20 || got[1] != 0 || got[2] != 40 {
		t.Errorf("globex = %v, want 20, 0, 40 (caught up, nothing lost)", got)
	}
}

// A cluster's own Prometheus on another host never receives the global
// Prometheus credentials.
func TestOwnPrometheusGetsNoGlobalCredentials(t *testing.T) {
	var gotAuth []string
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Scope-OrgID"))
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": []any{}}})
	}))
	defer own.Close()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{PrometheusURL: "https://prom.example:9090", GPUUtilQuery: "none", EgressQuery: "none",
		PromHeaders: map[string]string{"Authorization": "Bearer global-secret", "X-Scope-OrgID": "ops"}, PromMetrics: []PromMetric{tokens}})
	p, _ := c.NewPromSource(PromOptions{})
	target := Target{Cluster: discovery.TenantCluster{Name: "x", Namespace: "vc-x",
		Annotations: map[string]string{discovery.AnnotationPrefix + "prometheus-url": own.URL}}, Tenant: "acme"}
	collectOK(t, p, []Target{target}, latest())
	if len(gotAuth) == 0 || gotAuth[0] != "|" {
		t.Fatalf("the cluster's own Prometheus received %q", gotAuth)
	}
}

// On the first read a counter's history is not billed; a series that
// appears later counts from zero.
func TestCounterFirstReadAndNewSeries(t *testing.T) {
	fp := &fakeProm{series: []fakeSeries{
		{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "old"},
			samples: counterSamples(ws.Add(-30*time.Minute), ws.Add(2*time.Minute), 100, time.Time{})},
		{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "new"},
			samples: counterSamples(ws.Add(75*time.Second), ws.Add(2*time.Minute), 3, time.Time{})},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	target := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	wins := windowsFrom(ws, 2)
	bySKU := map[string]float64{}
	for _, w := range wins {
		res := collectOK(t, p, []Target{target}, []Window{w})
		_ = p.Commit(res, func(Window, []usage.Event) error { return nil })
		for _, pw := range res {
			for _, e := range pw.Events {
				bySKU[e.SKU] += e.Quantity
				if e.Properties["estimate"] != nil {
					t.Errorf("counter events must not be marked as estimates: %v", e.Properties)
				}
			}
		}
	}
	if bySKU["old"] != 8*100 {
		t.Errorf("old series billed %v, want only the 8 samples inside the windows (800)", bySKU["old"])
	}
	// The new series appears at ws+75s with value 3 and grows by 3 every
	// 15s through ws+2m: 4 samples, 3+3+3+3 = 12, counted from zero.
	if bySKU["new"] != 12 {
		t.Errorf("new series billed %v, want 12", bySKU["new"])
	}
}

// Clusters with their own nodes are selected by the source label; a
// cluster with its own Prometheus is read there with no extra matchers; an
// empty result is reported with a hint.
func TestPrometheusScopeBySource(t *testing.T) {
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"vcluster": "private-a", "model_name": "llama"},
		samples: counterSamples(ws.Add(-time.Minute), we, 1, time.Time{})}}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	own := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": "default", "model_name": "llama"},
		samples: counterSamples(ws.Add(-time.Minute), we, 2, time.Time{})}}}
	ownSrv := httptest.NewServer(own.handler(t))
	defer ownSrv.Close()

	_, p, h := promSourceFor(t, srv.URL, "", nil, tokens)
	private := Target{Cluster: discovery.TenantCluster{Name: "private-a", Namespace: "vc-private-a"}, Tenant: "acme"}
	p.noteOwnNodes(private.Cluster.ExternalID(), true)
	selfHosted := Target{Cluster: discovery.TenantCluster{Name: "inside", Namespace: "vc-inside",
		Annotations: map[string]string{discovery.AnnotationPrefix + "prometheus-url": ownSrv.URL}}, Tenant: "globex"}
	shared := Target{Cluster: discovery.TenantCluster{Name: "shared", Namespace: "vc-shared"}, Tenant: "initech"}

	res := collectOK(t, p, []Target{private, selfHosted, shared}, []Window{{Start: ws, End: we, Latest: true}})
	byTenant := map[string]float64{}
	for _, e := range res[0].Events {
		byTenant[e.Tenant] += e.Quantity
	}
	if byTenant["acme"] != 4 || byTenant["globex"] != 8 {
		t.Errorf("by tenant = %v, want acme 4 (source label), globex 8 (own Prometheus)", byTenant)
	}
	if !strings.Contains(strings.Join(fp.queries, "\n"), `vllm:generation_tokens_total{vcluster="private-a"}[660s]`) {
		t.Errorf("private cluster query = %q", fp.queries)
	}
	if !strings.Contains(strings.Join(own.queries, "\n"), `vllm:generation_tokens_total{__name__!=""}[660s]`) {
		t.Errorf("own Prometheus query = %q", own.queries)
	}
	var empty *health.Source
	for _, s := range h.List() {
		if s.Subject == shared.Cluster.ExternalID() {
			s := s
			empty = &s
		}
	}
	if empty == nil || empty.Status != health.Empty || !strings.Contains(empty.Message, "selected by namespace") {
		t.Fatalf("shared cluster health = %+v", empty)
	}
}

// Query metrics are estimates and say so on every event.
func TestQueryMetricsAreMarkedEstimates(t *testing.T) {
	prom, _ := promServer(t, nil, func(q string) (string, map[string]string) {
		if strings.Contains(q, "increase(vllm:prompt_tokens_total") {
			return "170.68", map[string]string{"model_name": "llama"}
		}
		return "", nil
	})
	m := PromMetric{Code: "tokens_in_est", Query: `sum by (model_name) (increase(vllm:prompt_tokens_total{{selector}}[{{window}}]))`, Dimensions: []string{"model_name"}}
	_, p, _ := promSourceFor(t, prom.URL, "", nil, m)
	res := collectOK(t, p, []Target{{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}}, latest())
	if len(res[0].Events) != 1 || res[0].Events[0].Properties["estimate"] != true || res[0].Events[0].Quantity != 170.68 {
		t.Fatalf("events = %+v", res[0].Events)
	}
	if m.Accuracy() != usage.AccuracyEstimate || tokens.Accuracy() != usage.AccuracyExact {
		t.Error("accuracy of query vs counter metrics")
	}
}

func TestParsePromMetricsValidation(t *testing.T) {
	t.Cleanup(func() { usage.RemoveCustom("a4") })
	for body, want := range map[string]string{
		"- code: a1\n":                                              "needs a counter",
		"- code: a2\n  counter: x\n  query: y\n":                    "pick one",
		"- code: a3\n  counter: rate(x[5m])\n":                      "plain series selector",
		"- code: a4\n  counter: x{{selector}}\n  billable: false\n": "",
	} {
		_, err := ParsePromMetrics([]byte(body), "test")
		if want == "" {
			if err != nil {
				t.Errorf("%q: %v", body, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", body, err, want)
		}
	}
	if d, ok := usage.Lookup("a4"); !ok || d.Billable || d.Accuracy != usage.AccuracyExact {
		t.Errorf("billable: false must be honored: %+v", d)
	}
}
