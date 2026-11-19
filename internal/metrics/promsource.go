package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// SourcePrometheus is the ledger source Prometheus-read usage is committed
// under. It has its own watermark: when Prometheus is down, these windows
// wait and are backfilled once it answers, instead of being skipped, and
// Kubernetes metering keeps committing in the meantime.
const SourcePrometheus = "prometheus"

// PromOptions configure Prometheus-read usage.
type PromOptions struct {
	// SourceLabel is the label that names the source cluster on series from
	// clusters with their own nodes (Private Nodes, Auto Nodes, Standalone),
	// typically an external label their Prometheus agent adds when it remote
	// writes, e.g. {vcluster: team-a}. Default "vcluster".
	SourceLabel string
	// Lookback is how far before a window vBilling looks for the sample a
	// counter's increase starts from. Default 10m.
	Lookback time.Duration
	// StatePath persists counter baselines (two generations, see
	// counterStore). Empty keeps them in memory.
	StatePath string
	// Watermark returns the last committed window of SourcePrometheus.
	Watermark func() (time.Time, bool)
	Health    *health.Registry
}

// PromSource reads usage from Prometheus for every cluster and window.
type PromSource struct {
	c      *Collector
	opts   PromOptions
	health *health.Registry
	store  *counterStore

	mu      sync.Mutex
	clients map[string]*prometheusClient // per-cluster URLs
	ownNode map[string]bool              // cluster external ID -> has its own nodes (last seen)
}

// promMetricSpec is a Prometheus metric, built in or operator defined.
type promMetricSpec struct {
	PromMetric
	builtin string // "egress" | "gpu-util" for built-ins with special handling
}

// NewPromSource returns nil when no Prometheus is configured anywhere: no
// PROMETHEUS_URL and no cluster may name its own (checked per cluster).
func (c *Collector) NewPromSource(opts PromOptions) (*PromSource, error) {
	if opts.SourceLabel == "" {
		opts.SourceLabel = "vcluster"
	}
	if opts.Lookback <= 0 {
		opts.Lookback = 10 * time.Minute
	}
	if opts.Health == nil {
		opts.Health = health.New(nil)
	}
	store, err := openCounterStore(opts.StatePath, opts.Watermark)
	if err != nil {
		return nil, err
	}
	p := &PromSource{c: c, opts: opts, health: opts.Health, store: store,
		clients: map[string]*prometheusClient{}, ownNode: map[string]bool{}}
	c.prom = p
	return p, nil
}

// noteOwnNodes remembers whether a cluster runs on its own nodes, which
// decides how its series are scoped.
func (p *PromSource) noteOwnNodes(extID string, own bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.ownNode[extID] = own
	p.mu.Unlock()
}

func (p *PromSource) hasOwnNodes(t Target) bool {
	if t.Cluster.External {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ownNode[t.Cluster.ExternalID()]
}

func (p *PromSource) specs() []promMetricSpec {
	o := p.c.opts
	var out []promMetricSpec
	egress := PromMetric{Code: usage.MetricNetworkEgressGB, Counter: defaultEgressCounter, Scale: 1.0 / gib}
	if o.EgressQuery != "" {
		egress = PromMetric{Code: usage.MetricNetworkEgressGB, Query: o.EgressQuery, Scale: 1.0 / gib}
	}
	if o.EgressQuery != "none" {
		out = append(out, promMetricSpec{PromMetric: egress, builtin: "egress"})
	}
	if o.GPUUtilQuery != "none" {
		out = append(out, promMetricSpec{PromMetric: PromMetric{Code: usage.MetricGPUUtilization, Query: "gpu-util"}, builtin: "gpu-util"})
	}
	for _, m := range o.PromMetrics {
		out = append(out, promMetricSpec{PromMetric: m})
	}
	return out
}

// scope resolves where a cluster's series live and how to select them:
//
//  1. the vbilling.vcluster.com/prometheus-url annotation names a
//     Prometheus that only sees this cluster (an https:// URL, or
//     tenant-service://NAMESPACE/NAME:PORT to reach one inside the cluster
//     through its API server). The source is the scope, so {{matchers}}
//     matches everything;
//  2. otherwise PROMETHEUS_URL, with {{matchers}} set to
//     - the vbilling.vcluster.com/prometheus-matchers annotation, if set,
//     - the vCluster Platform labels with PROMETHEUS_PRESET=vcluster-platform,
//     - SOURCE_LABEL="<name>" for clusters with their own nodes, whose pods
//     never appear in the control plane cluster,
//     - namespace="<namespace>" for clusters on shared nodes.
func (p *PromSource) scope(ctx context.Context, t Target, size time.Duration) (*prometheusClient, queryVars, string, error) {
	cl := t.Cluster
	vars := varsFor(t, size)
	esc := func(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }
	vars.Matchers = cl.Meta("prometheus-matchers")
	if u := cl.Meta("prometheus-url"); u != "" {
		client, err := p.clientFor(ctx, t, u)
		if err != nil {
			return nil, vars, "", err
		}
		if vars.Matchers == "" {
			vars.Matchers = noopMatcher
		}
		return client, vars, "own Prometheus " + client.t.String(), nil
	}
	if p.c.promDefault == nil {
		return nil, vars, "", errNoPrometheus
	}
	how := "matchers from annotation"
	if vars.Matchers == "" {
		switch {
		case p.c.opts.PrometheusPreset == PresetVClusterPlatform:
			vars.Matchers = expand(platformSelector, vars)
			how = "vCluster Platform labels"
		case p.hasOwnNodes(t):
			name := cl.Meta("prometheus-source")
			if name == "" {
				name = cl.Name
			}
			vars.Matchers = fmt.Sprintf(`%s="%s"`, p.opts.SourceLabel, esc(name))
			how = "own nodes, selected by " + p.opts.SourceLabel + " label"
		default:
			vars.Matchers = fmt.Sprintf(`namespace="%s"`, esc(cl.Namespace))
			how = "shared nodes, selected by namespace"
		}
	}
	return p.c.promDefault, vars, how, nil
}

var errNoPrometheus = errors.New("no Prometheus for this cluster")

func (p *PromSource) clientFor(ctx context.Context, t Target, raw string) (*prometheusClient, error) {
	if ns, svc, prefix, ok := parseTenantService(raw); ok {
		if p.c.tenants == nil {
			return nil, fmt.Errorf("%s needs the tenant API (TENANT_API=true)", raw)
		}
		conn, err := p.c.tenants.Conn(ctx, t.Cluster)
		if err != nil {
			return nil, fmt.Errorf("tenant API for %s: %w", raw, err)
		}
		return &prometheusClient{t: &kubeProxyTransport{kube: conn.Kube, namespace: ns, service: svc, prefix: prefix}}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[raw]; ok {
		return c, nil
	}
	// PROMETHEUS_HEADERS and the bearer token belong to PROMETHEUS_URL. A
	// cluster's own Prometheus on another host gets neither (put credentials
	// for it in its URL), so an annotation can never send them elsewhere,
	// and its redirects are never followed.
	headers, token := p.c.opts.PromHeaders, p.c.opts.PromTokenFile
	if !sameHost(raw, p.c.opts.PrometheusURL) {
		headers, token = nil, ""
	}
	c := newClusterPrometheusClient(raw, headers, token)
	p.clients[raw] = c
	return c, nil
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host != "" && ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// PromWindow is one window of Prometheus-read usage, committed under
// SourcePrometheus.
type PromWindow struct {
	Window  Window
	Events  []usage.Event
	updates map[string]counterSample // counter baselines after this window
	seen    map[string]time.Time     // metric|cluster observed through this window
}

// ErrPrometheusUnavailable means every read of a run failed (Prometheus is
// down or unreachable). The caller should not commit the windows: they are
// read again, window by window, once Prometheus answers.
var ErrPrometheusUnavailable = errors.New("every Prometheus read failed")

// Collect reads every metric for every cluster over contiguous windows.
// When some reads fail (one cluster's Prometheus, one metric), they are
// reported to health and skipped: their counters keep their baselines, so
// that usage is billed in full by the first window that reads them again.
// When every read fails it returns ErrPrometheusUnavailable.
func (p *PromSource) Collect(ctx context.Context, targets []Target, windows []Window) ([]PromWindow, error) {
	out := make([]PromWindow, len(windows))
	accs := map[time.Time]*accumulator{}
	for i, w := range windows {
		out[i] = PromWindow{Window: w, updates: map[string]counterSample{}, seen: map[string]time.Time{}}
		accs[w.End] = newAccumulator(w)
	}
	if len(windows) == 0 {
		return out, nil
	}
	size := windows[0].End.Sub(windows[0].Start)
	attempts, failures := 0, 0
	for _, t := range targets {
		id := t.Cluster.ExternalID()
		client, vars, how, err := p.scope(ctx, t, size)
		if errors.Is(err, errNoPrometheus) {
			continue
		}
		base := p.c.baseFor(t)
		for _, m := range p.specs() {
			name := health.Name("prometheus", m.Code, id)
			attempts++
			if err != nil {
				failures++
				p.health.Fail(name, "prometheus", id, err, "")
				continue
			}
			var n int
			var merr error
			switch {
			case m.builtin == "gpu-util":
				n, merr = p.gpuUtil(ctx, client, t, vars, windows, accs, base)
			case m.Counter != "":
				n, merr = p.counter(ctx, client, m.PromMetric, id, vars, windows, out, accs, base)
			default:
				n, merr = p.query(ctx, client, m.PromMetric, vars, windows, accs, base)
			}
			switch {
			case merr != nil:
				failures++
				p.health.Fail(name, "prometheus", id, merr, how)
			case n == 0:
				p.health.Empty(name, "prometheus", id, "no series for "+id+" ("+how+")", emptyHint(m.PromMetric, vars, p.opts.SourceLabel))
			default:
				p.health.OK(name, "prometheus", id, n)
			}
		}
	}
	now := p.c.opts.Now().UTC()
	for i, w := range windows {
		var evs []usage.Event
		for _, ev := range accs[w.End].list() {
			ev.Source = SourcePrometheus
			ev.RecordedAt = now
			if !w.Latest {
				if ev.Properties == nil {
					ev.Properties = map[string]any{}
				}
				ev.Properties["backfilled"] = true
			}
			ev.Finalize()
			if err := ev.Validate(); err != nil {
				p.health.Fail(health.Name("prometheus", ev.Metric), "prometheus", ev.Tenant, err, "")
				continue
			}
			evs = append(evs, ev)
		}
		sort.Slice(evs, func(a, b int) bool { return evs[a].ID < evs[b].ID })
		out[i].Events = evs
	}
	if attempts > 0 && failures == attempts {
		return nil, ErrPrometheusUnavailable
	}
	return out, nil
}

func emptyHint(m PromMetric, v queryVars, sourceLabel string) string {
	h := "the selector matched nothing with {{matchers}} = " + v.Matchers + ". "
	switch {
	case strings.Contains(m.Query+m.Counter, "{{namespace}}"):
		h += "{{namespace}} is the cluster's namespace on the control plane cluster: it matches only a Prometheus that scrapes the control plane cluster, and only clusters on shared nodes. Use {{selector}} or {{matchers}} instead. "
	case strings.HasPrefix(v.Matchers, sourceLabel+"="):
		h += "This cluster runs on its own nodes: its series must carry " + sourceLabel + "=<cluster name> (an external label on the Prometheus agent that remote writes them), or set the vbilling.vcluster.com/prometheus-url annotation to a Prometheus that only sees this cluster. "
	}
	if m.Query != "" && (strings.Contains(m.Query, "increase(") || strings.Contains(m.Query, "rate(")) {
		h += "increase() and rate() over {{window}} need two samples inside the window; use counter: instead for exact billing."
	}
	return strings.TrimSpace(h)
}

// maxCounterGap bounds how far back a counter read goes to cover windows
// committed while a cluster's Prometheus failed. After a longer outage, a
// series first seen at recovery starts from its first sample instead.
const maxCounterGap = 24 * time.Hour

// counter bills the exact increase of every matching series per window.
func (p *PromSource) counter(ctx context.Context, client *prometheusClient, m PromMetric, cluster string, vars queryVars, windows []Window,
	out []PromWindow, accs map[time.Time]*accumulator, base baseFn) (int, error) {
	first, last := windows[0], windows[len(windows)-1]
	selector := expand(m.Counter, vars)
	key := m.Code + "|" + cluster
	prevObserved, observedBefore := p.store.observed(key)
	from := first.Start.Add(-p.opts.Lookback)
	if observedBefore && prevObserved.Before(first.Start) && first.Start.Sub(prevObserved) <= maxCounterGap {
		// This cluster's Prometheus failed while other sources succeeded,
		// so windows were committed without it. Read back to its last
		// successful read: a series that appeared since counts from zero,
		// and one that existed then does not.
		if back := prevObserved.Add(-p.opts.Lookback); back.Before(from) {
			from = back
		}
	}
	// Whether the samples read reach the last successful read, so a series
	// whose first sample is newer really appeared after it.
	reachesPrev := observedBefore && !from.After(prevObserved)
	series, err := client.QueryRaw(ctx, selector, last.End.Sub(from), last.End)
	if err != nil {
		return 0, err
	}
	for _, s := range series {
		sk := key + "|" + fingerprint(s.Labels)
		var prev *promSample
		if b, ok := p.store.baseline(sk); ok {
			prev = &promSample{T: time.UnixMilli(b.T).UTC(), V: b.V}
		}
		samples := s.Samples
		if prev == nil {
			// No baseline. A series that appeared after we last read this
			// metric is new: it counts from zero, including samples taken
			// while this cluster's Prometheus could not be read. Otherwise
			// start from the last sample at or before the first window; on
			// the very first read its history is not billed.
			idx := -1
			for i, smp := range samples {
				if !smp.T.After(first.Start) {
					idx = i
				}
			}
			switch {
			case len(samples) > 0 && reachesPrev && samples[0].T.After(prevObserved):
				prev = &promSample{T: prevObserved, V: 0}
			case idx >= 0:
				prev = &samples[idx]
				samples = samples[idx+1:]
			case len(samples) > 0:
				prev = &samples[0]
				samples = samples[1:]
			}
		}
		incs := make([]float64, len(windows))
		var lastPerWindow = make([]*promSample, len(windows))
		k := 0
		for i := range samples {
			smp := samples[i]
			if prev != nil && !smp.T.After(prev.T) {
				continue // already counted
			}
			if smp.T.After(last.End) {
				break
			}
			for k < len(windows) && smp.T.After(windows[k].End) {
				k++
			}
			if k >= len(windows) {
				break
			}
			inc := smp.V - prev.V
			if inc < 0 {
				inc = smp.V // counter reset: it restarted from zero
			}
			incs[k] += inc
			prev = &samples[i]
			lastPerWindow[k] = prev
		}
		ev := base(m.Code, nil)
		for _, l := range m.Dimensions {
			if v := s.Labels[l]; v != "" {
				ev.Dimensions[dimKey(l)] = v
			}
		}
		if l := m.skuLabel(); l != "" {
			ev.SKU = s.Labels[l]
		}
		for i, w := range windows {
			if incs[i] > 0 {
				accs[w.End].add(ev, incs[i]*m.scale(), map[string]any{"series": 1})
			}
			if lastPerWindow[i] != nil {
				out[i].updates[sk] = counterSample{T: lastPerWindow[i].T.UnixMilli(), V: lastPerWindow[i].V}
			}
		}
		// A baseline found by lookback (not from state) must be remembered
		// even if no sample landed in these windows.
		if _, ok := p.store.baseline(sk); !ok && prev != nil {
			lastIdx := len(windows) - 1
			if _, set := out[lastIdx].updates[sk]; !set {
				out[lastIdx].updates[sk] = counterSample{T: prev.T.UnixMilli(), V: prev.V}
			}
		}
	}
	for i, w := range windows {
		out[i].seen[key] = w.End
	}
	return len(series), nil
}

// query evaluates an estimate at every window end.
func (p *PromSource) query(ctx context.Context, client *prometheusClient, m PromMetric, vars queryVars, windows []Window,
	accs map[time.Time]*accumulator, base baseFn) (int, error) {
	q := expand(m.Query, vars)
	byWindow, err := p.evaluate(ctx, client, q, windows)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, w := range windows {
		for _, r := range byWindow[w.End] {
			n++
			ev := base(m.Code, nil)
			for _, l := range m.Dimensions {
				if v := r.Labels[l]; v != "" {
					ev.Dimensions[dimKey(l)] = v
				}
			}
			if l := m.skuLabel(); l != "" {
				ev.SKU = r.Labels[l]
			}
			accs[w.End].add(ev, r.Value*m.scale(), map[string]any{"estimate": true})
		}
	}
	return n, nil
}

// evaluate runs an instant query for one window or a range query for many.
func (p *PromSource) evaluate(ctx context.Context, client *prometheusClient, q string, windows []Window) (map[time.Time][]promResult, error) {
	out := map[time.Time][]promResult{}
	if len(windows) == 1 {
		res, err := client.Query(ctx, q, windows[0].End)
		if err != nil {
			return nil, err
		}
		out[windows[0].End] = res
		return out, nil
	}
	step := windows[0].End.Sub(windows[0].Start)
	series, err := client.QueryRange(ctx, q, windows[0].End, windows[len(windows)-1].End, step)
	if err != nil {
		return nil, err
	}
	for _, s := range series {
		for _, smp := range s.Samples {
			out[smp.T] = append(out[smp.T], promResult{Labels: s.Labels, Value: smp.V})
		}
	}
	return out, nil
}

// gpuUtil records average DCGM utilization per GPU model (informational).
func (p *PromSource) gpuUtil(ctx context.Context, client *prometheusClient, t Target, vars queryVars, windows []Window,
	accs map[time.Time]*accumulator, base baseFn) (int, error) {
	o := p.c.opts
	utilQ, countQ := o.GPUUtilQuery, o.GPUCountQuery
	if utilQ == "" {
		switch {
		case o.PrometheusPreset == PresetVClusterPlatform:
			utilQ, countQ = platformQueries.util, platformQueries.count
		case vars.Matchers != "" && !strings.HasPrefix(vars.Matchers, "namespace="):
			utilQ, countQ = scopedGPUUtilQuery, scopedGPUCountQuery
		default:
			utilQ, countQ = defaultGPUUtilQuery, defaultGPUCountQuery
		}
	}
	util, err := p.evaluate(ctx, client, expand(utilQ, vars), windows)
	if err != nil {
		return 0, err
	}
	counts := map[time.Time][]promResult{}
	if countQ != "" {
		counts, _ = p.evaluate(ctx, client, expand(countQ, vars), windows)
	}
	n := 0
	for _, w := range windows {
		countBy := map[string]float64{}
		for _, r := range counts[w.End] {
			countBy[promGPUModel(r.Labels)] = r.Value
		}
		for _, r := range util[w.End] {
			n++
			model := promGPUModel(r.Labels)
			ev := base(usage.MetricGPUUtilization, nil)
			ev.Dimensions[usage.DimGPUType] = model
			props := map[string]any{"avg_utilization_pct": r.Value, "estimate": true}
			if c, ok := countBy[model]; ok {
				props["gpu_devices"] = c
			}
			accs[w.End].add(ev, r.Value*w.End.Sub(w.Start).Hours(), props)
		}
	}
	return n, nil
}

var platformQueries = struct{ util, count string }{
	util:  dcgmUtilQuery(platformSelector),
	count: dcgmCountQuery(platformSelector),
}

// promGPUModel reads the GPU model from a utilization result: DCGM's
// modelName, or gpu_type/model from a custom GPU_UTIL_QUERY.
func promGPUModel(labels map[string]string) string {
	if v := firstLabel(labels, "modelName", "gpu_type", "model"); v != "" {
		return normalizeGPUType(v)
	}
	return "unknown"
}

// Commit commits windows in order. For each window it first persists the
// counter baselines that window produces, then calls commit (the ledger
// append). A crash between the two leaves the previous generation in force,
// because the ledger watermark is what decides which generation is used on
// restart. That is what makes counters exactly-once across restarts.
func (p *PromSource) Commit(results []PromWindow, commit func(w Window, evs []usage.Event) error) error {
	for _, r := range results {
		next := p.store.next(r.updates, r.seen)
		if err := p.store.stage(r.Window.End, next); err != nil {
			return fmt.Errorf("persist counter baselines: %w", err)
		}
		if err := commit(r.Window, r.Events); err != nil {
			return err
		}
		p.store.promote(next)
	}
	p.store.prune(p.c.opts.Now().Add(-7 * 24 * time.Hour))
	return nil
}

// --- counter baselines ---

type counterSample struct {
	T int64   `json:"t"` // unix ms
	V float64 `json:"v"`
}

type counterState struct {
	WindowEnd time.Time                `json:"window_end"`
	Series    map[string]counterSample `json:"series"`
	Observed  map[string]time.Time     `json:"observed"`
}

func (s counterState) clone() counterState {
	out := counterState{WindowEnd: s.WindowEnd, Series: make(map[string]counterSample, len(s.Series)), Observed: make(map[string]time.Time, len(s.Observed))}
	for k, v := range s.Series {
		out.Series[k] = v
	}
	for k, v := range s.Observed {
		out.Observed[k] = v
	}
	return out
}

// counterStore keeps the last two generations of baselines on disk. The
// newest generation whose window is at or before the ledger watermark is
// the one in force.
type counterStore struct {
	path string
	mu   sync.Mutex
	cur  counterState
	gens []counterState
}

func openCounterStore(path string, watermark func() (time.Time, bool)) (*counterStore, error) {
	s := &counterStore{path: path, cur: counterState{Series: map[string]counterSample{}, Observed: map[string]time.Time{}}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, os.MkdirAll(filepath.Dir(path), 0o750)
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Generations []counterState `json:"generations"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var wm time.Time
	if watermark != nil {
		wm, _ = watermark()
	}
	for _, g := range file.Generations { // oldest first
		if !g.WindowEnd.After(wm) {
			if g.Series == nil {
				g.Series = map[string]counterSample{}
			}
			if g.Observed == nil {
				g.Observed = map[string]time.Time{}
			}
			s.cur = g
		}
	}
	s.gens = file.Generations
	return s, nil
}

func (s *counterStore) baseline(key string) (counterSample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cur.Series[key]
	return v, ok
}

func (s *counterStore) observed(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cur.Observed[key]
	return v, ok
}

func (s *counterStore) next(updates map[string]counterSample, seen map[string]time.Time) counterState {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.cur.clone()
	for k, v := range updates {
		n.Series[k] = v
	}
	for k, v := range seen {
		n.Observed[k] = v
	}
	return n
}

func (s *counterStore) stage(windowEnd time.Time, next counterState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.WindowEnd = windowEnd
	gens := []counterState{s.cur, next}
	if s.path == "" {
		s.gens = gens
		return nil
	}
	b, err := json.Marshal(struct {
		Generations []counterState `json:"generations"`
	}{gens})
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, b); err != nil {
		return err
	}
	s.gens = gens
	return nil
}

func (s *counterStore) promote(next counterState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.gens) == 2 {
		next.WindowEnd = s.gens[1].WindowEnd
	}
	s.cur = next
}

// prune forgets series not seen for a week, so pod churn does not grow the
// state forever. A series that comes back after that counts from zero.
func (s *counterStore) prune(cutoff time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := cutoff.UnixMilli()
	for k, v := range s.cur.Series {
		if v.T < ms {
			delete(s.cur.Series, k)
		}
	}
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// fingerprint identifies a series by its sorted label set.
func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}
