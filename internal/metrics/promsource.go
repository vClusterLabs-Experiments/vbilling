package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
//     - namespace="<namespace>" for clusters on shared nodes, or
//     namespace=~"<namespace>|<synced>|..." for one that syncs objects to
//     other namespaces (synced, see readSynced).
func (p *PromSource) scope(ctx context.Context, t Target, size time.Duration, synced []string) (*prometheusClient, queryVars, string, error) {
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
			vars.Matchers, vars.synced = namespaceMatcher(cl.Namespace, synced), synced
			how = "shared nodes, selected by namespace"
			if len(synced) > 0 {
				how = "shared nodes, selected by its namespace and the namespaces it syncs to"
			}
		}
	}
	return p.c.promDefault, vars, how, nil
}

// namespaceMatcher selects the series of a cluster on shared nodes: in its
// namespace, and in the namespaces it syncs objects to.
func namespaceMatcher(own string, synced []string) string {
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace
	if len(synced) == 0 {
		return fmt.Sprintf(`namespace="%s"`, esc(own))
	}
	return fmt.Sprintf(`namespace=~"%s"`, esc(namespaceRegexp(own, synced)))
}

// namespaceRegexp matches exactly the namespaces given.
func namespaceRegexp(own string, synced []string) string {
	alts := make([]string, 0, 1+len(synced))
	alts = append(alts, regexp.QuoteMeta(own))
	for _, ns := range synced {
		alts = append(alts, regexp.QuoteMeta(ns))
	}
	return strings.Join(alts, "|")
}

var errNoPrometheus = errors.New("no Prometheus for this cluster")

// namespaceScoped reports whether the namespace row of scope applies to a
// cluster when it has no prometheus-matchers: shared or dedicated nodes,
// read through PROMETHEUS_URL without the vCluster Platform preset.
func (p *PromSource) namespaceScoped(t Target) bool {
	return t.Cluster.Meta("prometheus-url") == "" && p.c.promDefault != nil &&
		p.c.opts.PrometheusPreset != PresetVClusterPlatform && !p.hasOwnNodes(t)
}

// bareNamespace reports whether a cluster's series are selected by
// namespace="<namespace>" alone.
func (p *PromSource) bareNamespace(t Target) bool {
	return p.namespaceScoped(t) && t.Cluster.Meta("prometheus-matchers") == ""
}

// namespaceSharers lists, per control plane cluster namespace, the clusters
// whose series would be scoped by that namespace. vCluster can run several
// clusters in one namespace, and the namespace alone cannot tell their
// series apart: each one would be billed every series in it.
func (p *PromSource) namespaceSharers(targets []Target) map[string][]string {
	out := map[string][]string{}
	for _, t := range targets {
		if p.namespaceScoped(t) {
			out[t.Cluster.Namespace] = append(out[t.Cluster.Namespace], t.Cluster.Name)
		}
	}
	return out
}

// holdShared skips a cluster whose namespace other clusters share and that
// no operator scoped with prometheus-matchers. None of its Prometheus usage
// is billed meanwhile: its sources fail with a hint, and each counter
// remembers when this began, so once the cluster is scoped its usage since
// then is billed (up to maxCounterGap back), as after a partial outage.
func (p *PromSource) holdShared(t Target, sharers []string, windows []Window, out []PromWindow) {
	id := t.Cluster.ExternalID()
	sorted := append([]string(nil), sharers...)
	sort.Strings(sorted)
	err := fmt.Errorf("clusters %s share namespace %s on the control plane cluster, so namespace=%q selects every one's series: this cluster's Prometheus usage is not billed until it is scoped",
		strings.Join(sorted, ", "), t.Cluster.Namespace, t.Cluster.Namespace)
	hint := "set vbilling.vcluster.com/prometheus-matchers on every cluster in this namespace to labels only its own series carry; " +
		"its counters then catch up on the usage since this began, up to 24 hours back"
	for _, m := range p.specs() {
		p.health.Fail(health.Name("prometheus", m.Code, id), "prometheus", id, err, hint)
		if m.Counter == "" {
			continue
		}
		key := m.Code + "|" + id
		if _, held := p.store.sharedSince(key); !held {
			out[0].shared[key] = windows[0].Start
		}
	}
}

// holdUnscoped skips a cluster on shared nodes while the namespaces it
// syncs objects to cannot be read: selected by its own namespace alone, its
// usage there would go unbilled unseen. Like holdShared it is not a failed
// read and never holds the windows back; its counters keep their last read
// and catch up once the namespaces can be read (up to maxCounterGap back).
func (p *PromSource) holdUnscoped(t Target, why error) {
	id := t.Cluster.ExternalID()
	err := fmt.Errorf("the namespaces this cluster syncs objects to cannot be determined, so its Prometheus usage is not billed until they can: %w", why)
	for _, m := range p.specs() {
		p.health.Fail(health.Name("prometheus", m.Code, id), "prometheus", id, err, syncedHint)
	}
}

const syncedHint = "see the synced-namespaces source: vCluster labels the namespaces it syncs a cluster's objects into, and vBilling reads them with list on pods, " +
	"persistentvolumeclaims and services and get on namespaces (the chart's ClusterRole)"

// noteHistory reports counter series that became visible with a count from
// before vBilling could see them, which was not billed.
func (p *PromSource) noteHistory(m PromMetric, id string, historic []map[string]string) {
	msg := fmt.Sprintf("%d series appeared with history; billed from its first sample (for example %s)", len(historic), describeLabels(historic[0]))
	log.Printf("[prometheus] %s/%s: %s", m.Code, id, msg)
	name := strings.TrimSuffix(m.Counter, "{{selector}}")
	if i := strings.IndexByte(name, '{'); i >= 0 {
		name = name[:i]
	}
	p.health.Set(health.Name("prometheus", m.Code, id, "history"), "prometheus", id, health.OK, msg,
		"nothing showed that its process started after vBilling last read the metric (no "+strings.TrimSuffix(name, "_total")+
			"_created series, no process_start_time_seconds for its job and instance), so its count before the first sample is treated as history: "+
			"a scrape or relabel made it visible. A new pod without that evidence is billed from its first scrape")
}

func describeLabels(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		if k != "__name__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(l[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

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
	// shared marks metric|cluster keys not read since then because the
	// cluster shares its namespace (see holdShared); unshared ends that.
	shared   map[string]time.Time
	unshared []string
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
		out[i] = PromWindow{Window: w, updates: map[string]counterSample{}, seen: map[string]time.Time{}, shared: map[string]time.Time{}}
		accs[w.End] = newAccumulator(w)
	}
	if len(windows) == 0 {
		return out, nil
	}
	size := windows[0].End.Sub(windows[0].Start)
	sharing := p.namespaceSharers(targets)
	// Clusters on shared nodes are selected by namespace, and so are the
	// objects they sync to other namespaces: read which those are (the
	// collection's own read, when it is fresh).
	var synced *syncedView
	for _, t := range targets {
		if p.bareNamespace(t) {
			v, err := p.c.syncedFor(ctx, withNamesakes(targets), windows[len(windows)-1].End)
			if err != nil {
				v = &syncedView{err: err}
			}
			synced = v
			break
		}
	}
	attempts, failures := 0, 0
	for _, t := range targets {
		id := t.Cluster.ExternalID()
		var away *elsewhere
		if p.bareNamespace(t) {
			away = synced.awayOf(t.Cluster)
		}
		client, vars, how, err := p.scope(ctx, t, size, away.syncedNamespaces())
		if errors.Is(err, errNoPrometheus) {
			continue
		}
		bare := err == nil && p.bareNamespace(t)
		if others := sharing[t.Cluster.Namespace]; bare && len(others) > 1 {
			// Not a failed read (nothing is down, so it never holds the
			// windows back): the windows commit without this cluster's
			// Prometheus usage, and its counters catch up once scoped.
			p.holdShared(t, others, windows, out)
			continue
		}
		if bare && synced.err != nil {
			p.holdUnscoped(t, synced.err) // likewise, until they can be read
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
				var historic []map[string]string
				n, historic, merr = p.counter(ctx, client, m.PromMetric, id, vars, !bare, windows, out, accs, base)
				if merr == nil && len(historic) > 0 {
					p.noteHistory(m.PromMetric, id, historic)
				}
			default:
				n, merr = p.query(ctx, client, m.PromMetric, vars, windows, accs, base)
			}
			switch {
			case merr != nil:
				failures++
				p.health.Fail(name, "prometheus", id, merr, how)
			case away.uncertainCount() > 0:
				// Read and billed where the cluster's objects are known to be;
				// not where nothing tells whose they are.
				p.health.Fail(name, "prometheus", id, fmt.Errorf("read %d series (%s); %s", n, how, away.why(t.Cluster)), syncedHint)
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
// ownScope is false when the cluster's series are selected by its namespace
// alone. It returns the series that became visible with history.
func (p *PromSource) counter(ctx context.Context, client *prometheusClient, m PromMetric, cluster string, vars queryVars, ownScope bool, windows []Window,
	out []PromWindow, accs map[time.Time]*accumulator, base baseFn) (int, []map[string]string, error) {
	first, last := windows[0], windows[len(windows)-1]
	selector := expand(m.Counter, vars)
	key := m.Code + "|" + cluster
	prevObserved, observedBefore := p.store.observed(key)
	// unsharedBare: the cluster shared its namespace and is selected by the
	// namespace again because the other clusters left it. What appeared
	// there meanwhile may be theirs, so none of it counts from before now.
	unsharedBare := false
	sharedFrom, wasShared := p.store.sharedSince(key)
	if wasShared {
		switch {
		case !ownScope:
			unsharedBare = true
		case !observedBefore:
			// Scoped by an operator since: its usage while the namespace was
			// shared catches up, as after a partial outage.
			prevObserved, observedBefore = sharedFrom, true
		}
	}
	from := first.Start.Add(-p.opts.Lookback)
	if observedBefore && prevObserved.Before(first.Start) && first.Start.Sub(prevObserved) <= maxCounterGap {
		// This cluster's Prometheus failed while other sources succeeded,
		// so windows were committed without it. Read back to its last
		// successful read and bill everything since.
		if back := prevObserved.Add(-p.opts.Lookback); back.Before(from) {
			from = back
		}
	}
	// Whether the samples read reach the last successful read, so a series
	// whose first sample is newer really appeared after it.
	reachesPrev := observedBefore && !from.After(prevObserved)
	// A series without a baseline starts from its last sample at or before
	// since: usage after it is billed.
	since := first.Start
	if reachesPrev {
		since = prevObserved
	}
	series, err := client.QueryRaw(ctx, selector, last.End.Sub(from), last.End)
	if err != nil {
		return 0, nil, err
	}
	appeared := func(s promSeries) bool {
		return reachesPrev && len(s.Samples) > 0 && s.Samples[0].T.After(prevObserved)
	}
	var evidence *startEvidence
	for _, s := range series {
		if _, ok := p.store.baseline(key + "|" + fingerprint(s.Labels)); !ok && appeared(s) && !unsharedBare {
			if evidence, err = p.startEvidence(ctx, client, selector, vars.Matchers, last.End.Sub(from), last.End); err != nil {
				// Decide with the evidence or not at all: the counter keeps
				// its baselines and catches up on the next read.
				return 0, nil, fmt.Errorf("read when its processes started: %w", err)
			}
			break
		}
	}
	var historic []map[string]string
	for _, s := range series {
		sk := key + "|" + fingerprint(s.Labels)
		var prev *promSample
		if b, ok := p.store.baseline(sk); ok {
			prev = &promSample{T: time.UnixMilli(b.T).UTC(), V: b.V}
		}
		samples := s.Samples
		if prev == nil && len(samples) > 0 {
			// No baseline. On the very first read a series' history is not
			// billed: it starts from its last sample at or before the first
			// window. A series that appeared after the last read counts from
			// zero only with evidence that its process started after that
			// read; vBilling cannot tell a new process from one a new scrape,
			// a relabel or a changed label set made visible, whose first
			// sample carries everything it counted before.
			upTo := func(t time.Time) int {
				idx := -1
				for i, smp := range samples {
					if !smp.T.After(t) {
						idx = i
					}
				}
				return idx
			}
			switch {
			case appeared(s) && unsharedBare:
				if i := upTo(first.Start); i >= 0 {
					prev, samples = &samples[i], samples[i+1:]
				} else {
					prev, samples = &samples[0], samples[1:]
				}
			case appeared(s) && evidence.startedSince(s, prevObserved, first.End.Sub(first.Start)):
				// Its process started after the last read: everything it
				// counted is usage since then, including samples taken while
				// this cluster's Prometheus could not be read.
				prev = &promSample{T: prevObserved, V: 0}
			case appeared(s):
				prev, samples = &samples[0], samples[1:]
				historic = append(historic, s.Labels)
			default:
				if i := upTo(since); i >= 0 {
					prev, samples = &samples[i], samples[i+1:]
				} else {
					prev, samples = &samples[0], samples[1:]
				}
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
		labelValues(&ev, m, s.Labels)
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
	if wasShared {
		// Read: the shared period is over (only now, so a failed read keeps
		// the point to catch up from).
		out[0].unshared = append(out[0].unshared, key)
	}
	return len(series), historic, nil
}

// startEvidence tells when the process behind a counter series started:
// the series' own <name>_created sample (OpenMetrics; prometheus_client,
// which vLLM uses, exposes one for every counter and label set), else the
// process_start_time_seconds of the job and instance it was scraped from.
type startEvidence struct {
	created map[string][]promSample // by series labels without the name
	process map[string][]promSample // by job and instance
}

func (p *PromSource) startEvidence(ctx context.Context, client *prometheusClient, counter, matchers string, rng time.Duration, at time.Time) (*startEvidence, error) {
	ev := &startEvidence{created: map[string][]promSample{}, process: map[string][]promSample{}}
	if sel, ok := createdSelector(counter); ok {
		series, err := client.QueryRaw(ctx, sel, rng, at)
		if err != nil {
			return nil, err
		}
		for _, s := range series {
			ev.created[labelKey(s.Labels)] = s.Samples
		}
	}
	if matchers == "" {
		matchers = noopMatcher
	}
	series, err := client.QueryRaw(ctx, "process_start_time_seconds{"+matchers+"}", rng, at)
	if err != nil {
		return nil, err
	}
	for _, s := range series {
		job, inst := s.Labels["job"], s.Labels["instance"]
		if job == "" || inst == "" {
			continue
		}
		k := job + "\x00" + inst
		ev.process[k] = append(ev.process[k], s.Samples...)
	}
	for _, smp := range ev.process {
		sort.Slice(smp, func(i, j int) bool { return smp[i].T.Before(smp[j].T) })
	}
	return ev, nil
}

// startedSince reports whether the evidence shows that the process behind s
// started at or after since, read at the series' first sample. Without
// evidence it reports false. window is the length of a metering window.
//
// A series' own _created sample is strong evidence: it is when that counter
// started. process_start_time_seconds is only when the process exposing it
// started: an exporter of counters that outlive it (dcgm-exporter's energy,
// a standalone cAdvisor, node and CNI exporters) restarts with the
// hardware's or the kernel's whole count, under a new instance label. So it
// counts only when the series' first value could have been counted since
// that process started (countedSince).
func (e *startEvidence) startedSince(s promSeries, since time.Time, window time.Duration) bool {
	if e == nil || len(s.Samples) == 0 {
		return false
	}
	first := s.Samples[0].T
	if smp, ok := nearest(e.created[labelKey(s.Labels)], first); ok {
		return !unixSeconds(smp.V).Before(since)
	}
	if job, inst := s.Labels["job"], s.Labels["instance"]; job != "" && inst != "" {
		process := e.process[job+"\x00"+inst]
		if smp, ok := nearest(process, first); ok {
			started := unixSeconds(smp.V)
			// The process's own series is scraped from the same target, as
			// often as the counter.
			near := window
			if every := scrapeInterval(process); every > 0 {
				near = 2 * every
			}
			return !started.Before(since) && countedSince(s, started, near)
		}
	}
	return false
}

// scrapeInterval is the shortest gap between samples (0 with fewer than two).
func scrapeInterval(samples []promSample) time.Duration {
	var every time.Duration
	for i := 1; i < len(samples); i++ {
		if d := samples[i].T.Sub(samples[i-1].T); d > 0 && (every == 0 || d < every) {
			every = d
		}
	}
	return every
}

// startRateSlack is how much faster than its own average rate a counter may
// have grown between its process's start and its first sample: a new
// engine often serves a burst first. A counter its process did not start
// carries a count orders of magnitude larger.
const startRateSlack = 2

// countedSince reports whether a series' first value could have been
// counted since started: no more than startRateSlack times the series' own
// average rate over the samples read, for the time from started to its
// first sample. A first sample older than started is another process's. The
// bound is conservative: a burst larger than that before the first scrape
// is billed from the first sample, losing at most that scrape interval,
// whereas a wrong "from zero" bills a counter's whole history again.
//
// With one sample there is no rate to judge by (a process started since the
// last read and scraped once per window shows one): it counts when that
// sample is the process's first scrape, within near (two scrape intervals,
// or a window) of its start.
func countedSince(s promSeries, started time.Time, near time.Duration) bool {
	first := s.Samples[0]
	if first.V <= 0 {
		return true // nothing counted yet: from zero bills nothing twice
	}
	elapsed := first.T.Sub(started).Seconds()
	if elapsed <= 0 {
		return false
	}
	if len(s.Samples) < 2 {
		return first.T.Sub(started) <= near
	}
	var grown float64
	prev := first
	for _, smp := range s.Samples[1:] {
		inc := smp.V - prev.V
		if inc < 0 {
			inc = smp.V // reset
		}
		grown += inc
		prev = smp
	}
	span := prev.T.Sub(first.T).Seconds()
	if span <= 0 {
		return false
	}
	return first.V <= startRateSlack*grown/span*elapsed
}

// createdSelector turns a counter selector into the selector of its
// _created series: name_total{...} becomes name_created{...}.
func createdSelector(sel string) (string, bool) {
	name, rest := sel, ""
	if i := strings.IndexByte(sel, '{'); i >= 0 {
		name, rest = sel[:i], sel[i:]
	}
	if name = strings.TrimSpace(name); name == "" {
		return "", false
	}
	return strings.TrimSuffix(name, "_total") + "_created" + rest, true
}

// nearest returns the sample taken at or last before at, else the first one.
func nearest(samples []promSample, at time.Time) (promSample, bool) {
	if len(samples) == 0 {
		return promSample{}, false
	}
	best := 0
	for i, smp := range samples {
		if !smp.T.After(at) {
			best = i
		}
	}
	return samples[best], true
}

func unixSeconds(v float64) time.Time {
	sec, frac := math.Modf(v)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// labelKey identifies a series by its labels without the metric name, so a
// counter and its _created series match.
func labelKey(labels map[string]string) string {
	l := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != "__name__" {
			l[k] = v
		}
	}
	return fingerprint(l)
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
			labelValues(&ev, m, r.Labels)
			accs[w.End].add(ev, r.Value*m.scale(), map[string]any{"estimate": true})
		}
	}
	return n, nil
}

// labelValues sets an event's dimensions and SKU from series labels. Label
// values can come from workloads a tenant runs, and SKUs and dimensions end
// up in dashboards, exports and billing platforms, so characters that are
// unsafe there are replaced (usage.SafeValue) rather than the usage dropped.
func labelValues(ev *usage.Event, m PromMetric, labels map[string]string) {
	for _, l := range m.Dimensions {
		if v := labels[l]; v != "" {
			ev.Dimensions[dimKey(l)] = usage.SafeValue(v)
		}
	}
	if l := m.skuLabel(); l != "" {
		ev.SKU = usage.SafeValue(labels[l])
	}
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
		case len(vars.synced) > 0:
			// Its namespace and those it syncs to, as DCGM's namespace label
			// or Prometheus' renamed one.
			re := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(namespaceRegexp(vars.Namespace, vars.synced))
			utilQ = dcgmUtilQuery(`namespace=~"`+re+`"`, `exported_namespace=~"`+re+`"`)
			countQ = dcgmCountQuery(`namespace=~"`+re+`"`, `exported_namespace=~"`+re+`"`)
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
		next := p.store.next(r.updates, r.seen, r.shared, r.unshared)
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
	// Shared: metric|cluster -> start of the first window it was not read
	// because the cluster shares its namespace (see holdShared).
	Shared map[string]time.Time `json:"shared,omitempty"`
}

func (s counterState) clone() counterState {
	out := counterState{WindowEnd: s.WindowEnd, Series: make(map[string]counterSample, len(s.Series)),
		Observed: make(map[string]time.Time, len(s.Observed)), Shared: make(map[string]time.Time, len(s.Shared))}
	for k, v := range s.Series {
		out.Series[k] = v
	}
	for k, v := range s.Observed {
		out.Observed[k] = v
	}
	for k, v := range s.Shared {
		out.Shared[k] = v
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

// sharedSince returns when a metric of a cluster stopped being read because
// the cluster shares its namespace.
func (s *counterStore) sharedSince(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.cur.Shared[key]
	return v, ok
}

func (s *counterStore) next(updates map[string]counterSample, seen, shared map[string]time.Time, unshared []string) counterState {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.cur.clone()
	for k, v := range updates {
		n.Series[k] = v
	}
	for k, v := range seen {
		n.Observed[k] = v
	}
	for k, v := range shared {
		n.Shared[k] = v
	}
	for _, k := range unshared {
		delete(n.Shared, k)
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
	// A cluster still sharing its namespace is marked again; one that left
	// is forgotten.
	for k, v := range s.cur.Shared {
		if v.Before(cutoff) {
			delete(s.cur.Shared, k)
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
