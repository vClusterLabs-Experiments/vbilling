package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// checker is a billing adapter that prices on the SKU and on other keys, as
// Metronome does with a pricing group key, and answers what the test says.
type checker struct {
	name string
	dims []string

	mu     sync.Mutex
	answer func(s destinations.PriceSeries) destinations.PriceCoverage
	plans  map[string]string // tenant -> plan (default "list")
	calls  int
	// before, when set, runs at the start of each check with its number
	// (from 1), before the platform is read.
	before func(call int)
}

func (c *checker) Name() string                                       { return c.name }
func (c *checker) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (c *checker) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (c *checker) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (c *checker) SendEvents(context.Context, []usage.Event) error    { return nil }
func (c *checker) Accepts(metric string) bool                         { return destinations.BillableOnly(metric) }
func (c *checker) PriceDims(string) []string                          { return c.dims }

func (c *checker) PricePlan(t usage.Tenant) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.plans[t.ID]; ok {
		return p
	}
	return "list"
}

func (c *checker) CheckPrices(_ context.Context, series []destinations.PriceSeries) ([]destinations.PriceCoverage, error) {
	c.mu.Lock()
	c.calls++
	call, before := c.calls, c.before
	c.mu.Unlock()
	if before != nil {
		before(call)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]destinations.PriceCoverage, 0, len(series))
	for _, s := range series {
		cov := destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3"}
		if !s.Catalog && c.answer != nil {
			cov = c.answer(s)
		}
		cov.Series = s
		out = append(out, cov)
	}
	return out, nil
}

func (c *checker) set(answer func(s destinations.PriceSeries) destinations.PriceCoverage) {
	c.mu.Lock()
	c.answer = answer
	c.mu.Unlock()
}

// byCapacity answers a CPU series by its capacity type, and prices the rest.
func byCapacity(status map[string]string) func(destinations.PriceSeries) destinations.PriceCoverage {
	return func(s destinations.PriceSeries) destinations.PriceCoverage {
		st := destinations.PriceCovered
		if s.Metric == usage.MetricCPUCoreHours {
			st = status[s.Dims[usage.DimCapacityType]]
		}
		switch st {
		case destinations.PriceMissing:
			return destinations.PriceCoverage{Status: st, Detail: "no rate for capacity_type=" + s.Dims[usage.DimCapacityType],
				Hint: "add a rate for capacity_type=" + s.Dims[usage.DimCapacityType]}
		case destinations.PriceZero:
			return destinations.PriceCoverage{Status: st, Detail: `product "CPU" is priced at 0 on rate card "list"`}
		case destinations.PriceUnknown:
			return destinations.PriceCoverage{Status: st, Detail: `product "CPU" prices on project, which vBilling does not track per series`,
				Hint: "price the product on keys vBilling checks"}
		}
		return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3"}
	}
}

func event(tenant, metric, sku string, dims map[string]string) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: metric, SKU: sku, Quantity: 1, WindowStart: t0, WindowEnd: t0.Add(time.Minute), Dimensions: dims}
	e.Finalize()
	return e
}

func cpu(capacity string) usage.Event {
	return event("acme", usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: capacity})
}

// find returns the row of a series with usage.
func find(rep Report, metric, sku string, dims map[string]string) *Row {
	for _, d := range rep.Destinations {
		for i, r := range d.Series {
			if r.Metric != metric || r.SKU != sku || r.NoUsageYet || len(r.Dims) != len(dims) {
				continue
			}
			same := true
			for k, v := range dims {
				same = same && r.Dims[k] == v
			}
			if same {
				return &d.Series[i]
			}
		}
	}
	return nil
}

// logBuffer collects the process log.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) lines(containing string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(line, containing) {
			out = append(out, line)
		}
	}
	return out
}

func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// Live finding: a CPU on-demand series rated at 0 read "unpriced", and the
// zero count stayed 0, because the view gave a (plan, metric, SKU) row the
// worst status of all its pricing-group combinations and stale spot and
// preemptible combinations of the same metric had no rate. Every series is
// its own row now, with its own status, and the counts count series.
func TestEverySeriesHasItsOwnRowAndStatus(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", dims: []string{usage.DimCapacityType}}
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceZero, "spot": destinations.PriceMissing,
		"preemptible": destinations.PriceMissing, "reserved": destinations.PriceCovered}))
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{c}, nil, nil, h, tel, Options{})
	svc.Observe([]usage.Event{cpu("on-demand"), cpu("spot"), cpu("preemptible"), cpu("reserved")}, time.Now())
	rep := svc.CheckNow(context.Background())

	want := map[string]string{"on-demand": destinations.PriceZero, "spot": destinations.PriceMissing,
		"preemptible": destinations.PriceMissing, "reserved": destinations.PriceCovered}
	for capacity, status := range want {
		r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: capacity})
		if r == nil {
			t.Fatalf("no row for the %s series: %+v", capacity, rep.Destinations[0].Series)
		}
		if r.Status != status || r.Plan != "list" || r.CheckedAt.IsZero() || r.LastSeen.IsZero() {
			t.Errorf("%s series: %+v, want status %s of its own", capacity, r, status)
		}
	}
	if r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: "on-demand"}); !strings.Contains(r.Detail, "priced at 0") || r.Hint != "" {
		t.Errorf("the zero series keeps its own detail, not an unpriced neighbour's: %+v", r)
	}
	d := rep.Destinations[0]
	if d.Zero != 1 || d.Unpriced != 2 || rep.Unpriced != 2 || d.Unknown != 0 {
		t.Errorf("counts count series: priced %d unpriced %d zero %d unknown %d", d.Priced, d.Unpriced, d.Zero, d.Unknown)
	}
	if d.Priced+d.Unpriced+d.Zero+d.Unknown != len(d.Series) {
		t.Errorf("counts %d+%d+%d+%d do not add up to the %d rows", d.Priced, d.Unpriced, d.Zero, d.Unknown, len(d.Series))
	}
	for status, n := range map[string]float64{"zero": 1, "unpriced": 2, "unknown": 0, "priced": float64(d.Priced)} {
		if v := tel.Get("vbilling_price_series", map[string]string{"destination": "metronome", "status": status}); v != n {
			t.Errorf("vbilling_price_series{status=%q} = %v, want %v", status, v, n)
		}
	}
	if v := tel.Get("vbilling_unpriced_series", map[string]string{"destination": "metronome"}); v != 2 {
		t.Errorf("vbilling_unpriced_series = %v, want the 2 unpriced series", v)
	}
	// Worst first: the two unpriced series, then the one at 0.
	if d.Series[0].Status != destinations.PriceMissing || d.Series[1].Status != destinations.PriceMissing || d.Series[2].Status != destinations.PriceZero {
		t.Errorf("rows are not sorted worst first: %s, %s, %s", d.Series[0].Status, d.Series[1].Status, d.Series[2].Status)
	}

	// The API's JSON carries the other pricing keys of each row.
	b, _ := json.Marshal(d.Series[0])
	var wire map[string]any
	json.Unmarshal(b, &wire)
	dims, _ := wire["dims"].(map[string]any)
	if dims[usage.DimCapacityType] != "preemptible" {
		t.Errorf("row JSON %s: want dims with the capacity type", b)
	}
	for _, gone := range []string{"combinations", "missing"} {
		if _, ok := wire[gone]; ok {
			t.Errorf("row JSON still has %q, which described a row of several series", gone)
		}
	}

	// Each unpriced series has its own source, named with its pricing keys.
	sources := map[string]health.Source{}
	for _, src := range h.List() {
		sources[src.Name] = src
	}
	for _, capacity := range []string{"spot", "preemptible"} {
		src, ok := sources["pricing/metronome/list/vcluster_cpu_core_hours/capacity_type="+capacity]
		if !ok || src.Status != health.Unpriced || !strings.Contains(src.Hint, "capacity_type="+capacity) {
			t.Errorf("source of the %s series: %+v (have %v)", capacity, src, names(h))
		}
	}
	if _, ok := sources["pricing/metronome/list/vcluster_cpu_core_hours/capacity_type=on-demand"]; ok {
		t.Error("a series priced at 0 is not a gap: it has no source")
	}

	// Finance prices spot: only that row changes.
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceZero, "spot": destinations.PriceCovered,
		"preemptible": destinations.PriceMissing, "reserved": destinations.PriceCovered}))
	rep = svc.CheckNow(context.Background())
	if r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: "spot"}); r.Status != destinations.PriceCovered {
		t.Errorf("spot after its rate was added: %+v", r)
	}
	if r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: "preemptible"}); r.Status != destinations.PriceMissing {
		t.Errorf("preemptible has no rate yet: %+v", r)
	}
	if rep.Destinations[0].Unpriced != 1 || rep.Destinations[0].Zero != 1 {
		t.Errorf("counts after pricing spot: %+v", rep.Destinations[0])
	}
}

func names(h *health.Registry) []string {
	var out []string
	for _, s := range h.List() {
		out = append(out, s.Name+"="+string(s.Status))
	}
	return out
}

// Every change of a series' status is logged once, with the detail: also to
// a price of 0 and back, which the log never recorded.
func TestEveryStatusChangeOfASeriesIsLoggedOnce(t *testing.T) {
	logs := captureLog(t)
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", dims: []string{usage.DimCapacityType}}
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceCovered}))
	svc := New(sp, []destinations.Destination{c}, nil, nil, nil, nil, Options{})
	svc.Observe([]usage.Event{cpu("on-demand")}, time.Now())
	ctx := context.Background()
	svc.CheckNow(ctx)
	if l := logs.lines("vcluster_cpu_core_hours"); len(l) != 0 {
		t.Fatalf("a first answer that says priced needs no line: %v", l)
	}

	steps := []struct{ status, line string }{
		{destinations.PriceZero, `vcluster_cpu_core_hours capacity_type=on-demand (plan list) is priced at 0 now: product "CPU" is priced at 0 on rate card "list"`},
		{destinations.PriceCovered, "vcluster_cpu_core_hours capacity_type=on-demand (plan list) is priced now: flat 3"},
		{destinations.PriceMissing, "vcluster_cpu_core_hours capacity_type=on-demand (plan list) is unpriced: no rate for capacity_type=on-demand (hint: add a rate for capacity_type=on-demand)"},
		{destinations.PriceZero, "is priced at 0 now:"},
		{destinations.PriceUnknown, "cannot be checked any more:"},
		{destinations.PriceCovered, "is priced now: flat 3"},
	}
	total := 0
	for _, step := range steps {
		c.set(byCapacity(map[string]string{"on-demand": step.status}))
		svc.CheckNow(ctx)
		svc.CheckNow(ctx) // nothing changed: nothing to log
		total++
		all := logs.lines("[pricing] metronome: vcluster_cpu_core_hours")
		if len(all) != total || !strings.Contains(all[total-1], step.line) {
			t.Fatalf("after the change to %s: %d line(s) %q; want %d, the last with %q", step.status, len(all), all, total, step.line)
		}
	}

	// A series whose first answer is a price of 0 says so once.
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceCovered, "spot": destinations.PriceZero}))
	svc.Observe([]usage.Event{cpu("spot")}, time.Now())
	svc.CheckNow(ctx)
	svc.CheckNow(ctx)
	if l := logs.lines("capacity_type=spot (plan list) is priced at 0:"); len(l) != 1 {
		t.Fatalf("first answer of a series priced at 0: %v", l)
	}
}

// A series the check cannot decide is delivered even with PRICE_CHECK=hold,
// so it must never be silent: its source fails, the destination's summary
// fails, and the log says so. A billable metric without usage that cannot be
// checked delivers nothing and is only a row.
func TestASeriesThatCannotBeCheckedIsLoudAndNotHeld(t *testing.T) {
	logs := captureLog(t)
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", dims: []string{usage.DimCapacityType}}
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceUnknown}))
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{c}, nil, nil, h, tel, Options{Mode: ModeHold})
	ctx := context.Background()
	ev := cpu("on-demand")
	if d := svc.Gate().Decide(ctx, "metronome", []usage.Event{ev}); d[0].Hold {
		t.Fatalf("a series the check cannot decide is not held (nothing could release it): %+v", d[0])
	}
	rep := svc.Report()
	r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: "on-demand"})
	if r == nil || r.Status != destinations.PriceUnknown || rep.Destinations[0].Unknown != 1 {
		t.Fatalf("row %+v, unknown count %d", r, rep.Destinations[0].Unknown)
	}
	var series, summary *health.Source
	for _, src := range h.Problems() {
		src := src
		switch src.Name {
		case "pricing/metronome/list/vcluster_cpu_core_hours/capacity_type=on-demand":
			series = &src
		case "pricing/metronome":
			summary = &src
		}
	}
	if series == nil || series.Status != health.Unchecked || !strings.Contains(series.Message, "does not track per series") ||
		!strings.Contains(series.Message, "delivered in every PRICE_CHECK mode") || series.Hint != "price the product on keys vBilling checks" {
		t.Fatalf("source of the series that cannot be checked: %+v (have %v)", series, names(h))
	}
	if summary == nil || summary.Status != health.Unchecked || !strings.Contains(summary.Message, "1 series with usage cannot be checked: vcluster_cpu_core_hours capacity_type=on-demand") {
		t.Fatalf("summary source: %+v", summary)
	}
	if v := tel.Get("vbilling_price_series", map[string]string{"destination": "metronome", "status": "unknown"}); v != 1 {
		t.Errorf("vbilling_price_series{status=unknown} = %v", v)
	}
	if l := logs.lines("capacity_type=on-demand (plan list) cannot be checked:"); len(l) != 1 {
		t.Errorf("log: %v", l)
	}

	// Once it can be checked and is priced, both sources recover.
	c.set(byCapacity(map[string]string{"on-demand": destinations.PriceCovered}))
	svc.CheckNow(ctx)
	for _, src := range h.Problems() {
		if src.Kind == "pricing" {
			t.Errorf("still a problem after the series became priced: %+v", src)
		}
	}
}

// scoped is a checker with plans that have no price list.
type scoped struct{ *checker }

func (s scoped) ChecksCatalog(plan string) bool { return !strings.HasPrefix(plan, "alias:") }

// Billable metrics without usage are checked under the plans that have a
// price list, not under the plan of a tenant that has none (Metronome: a
// tenant without customer or contract): that tenant's series with usage say
// what is missing, once each.
func TestTheCatalogIsNotCheckedUnderAPlanWithoutAPriceList(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", plans: map[string]string{"newbie": "alias:newbie"}}
	c.set(func(s destinations.PriceSeries) destinations.PriceCoverage {
		if s.Plan == "alias:newbie" {
			return destinations.PriceCoverage{Status: destinations.PriceMissing, Detail: "no customer", Hint: "create a customer"}
		}
		return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3"}
	})
	reg := pipeline.NewTenantRegistry()
	reg.Upsert(usage.Tenant{ID: "newbie"})
	reg.Upsert(usage.Tenant{ID: "acme"})
	svc := New(sp, []destinations.Destination{scoped{c}}, reg, nil, nil, nil, Options{})
	svc.Observe([]usage.Event{event("newbie", usage.MetricGPUHours, "H100", nil), event("acme", usage.MetricGPUHours, "H100", nil)}, time.Now())
	rep := svc.CheckNow(context.Background())
	byPlan := map[string]int{}
	for _, r := range rep.Destinations[0].Series {
		if r.NoUsageYet {
			byPlan[r.Plan]++
		}
	}
	if byPlan["alias:newbie"] != 0 || byPlan["list"] == 0 {
		t.Fatalf("billable metrics without usage by plan: %v, want them under the plan with a price list only", byPlan)
	}
	if rep.Unpriced != 1 {
		t.Fatalf("unpriced %d, want the tenant's one series with usage: %+v", rep.Unpriced, rep.Destinations[0].Series)
	}
}

// A note of the billing adapter (where the series is rated) reaches the row,
// and goes away with it.
func TestTheAdaptersNoteReachesTheRow(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome"}
	note := `rate card setting "payg" points to another rate card now`
	c.set(func(s destinations.PriceSeries) destinations.PriceCoverage {
		return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3", Note: note}
	})
	svc := New(sp, []destinations.Destination{c}, nil, nil, nil, nil, Options{})
	svc.Observe([]usage.Event{event("acme", usage.MetricGPUHours, "H100", nil)}, time.Now())
	if r := find(svc.CheckNow(context.Background()), usage.MetricGPUHours, "H100", nil); r == nil || r.Note != note {
		t.Fatalf("row %+v, want the note", r)
	}
	c.set(nil)
	if r := find(svc.CheckNow(context.Background()), usage.MetricGPUHours, "H100", nil); r.Note != "" {
		t.Fatalf("the note stayed after the adapter dropped it: %+v", r)
	}
}

// A report never shows a check's answers before /metrics and the source
// health show them. CI failed once on TestWarnModeReportsGapsAndClears
// ("gauge 0"): it read the report, saw the new status, and then read a gauge
// the check had not set yet. Here the check is stopped between the two.
func TestAReportNeverRunsAheadOfMetricsAndHealth(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", dims: []string{usage.DimCapacityType}}
	c.set(byCapacity(map[string]string{"spot": destinations.PriceMissing}))
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	svc := New(sp, []destinations.Destination{c}, nil, nil, h, tel, Options{})
	svc.Observe([]usage.Event{cpu("spot")}, time.Now())

	// The check logs the change to unpriced after it recorded the answer and
	// before it publishes: hold it there.
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("is unpriced")) {
			once.Do(func() {
				close(reached)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
			})
		}
		return len(p), nil
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	checked := make(chan struct{})
	go func() { defer close(checked); svc.CheckAll(context.Background()) }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the check never logged the unpriced series")
	}
	type seen struct {
		status string
		gauge  float64
		source bool
	}
	got := make(chan seen, 1)
	go func() {
		rep := svc.Report()
		s := seen{gauge: tel.Get("vbilling_unpriced_series", map[string]string{"destination": "metronome"})}
		if r := find(rep, usage.MetricCPUCoreHours, "", map[string]string{usage.DimCapacityType: "spot"}); r != nil {
			s.status = r.Status
		}
		for _, src := range h.Problems() {
			s.source = s.source || src.Name == "pricing/metronome/list/vcluster_cpu_core_hours/capacity_type=spot"
		}
		got <- s
	}()
	var s seen
	select {
	case s = <-got: // the report did not wait: it must not show the answer yet
	case <-time.After(200 * time.Millisecond):
		close(release)
		s = <-got
	}
	select {
	case <-release:
	default:
		close(release)
	}
	<-checked
	if s.status == destinations.PriceMissing && (s.gauge != 1 || !s.source) {
		t.Fatalf("the report showed the series unpriced while vbilling_unpriced_series was %v and its source reported %v", s.gauge, s.source)
	}
	if s.status != destinations.PriceMissing {
		t.Fatalf("the report did not wait for the check that was publishing: status %q", s.status)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A whole node emits node-hours and, for the same hardware, GPU, CPU and
// memory hours. A plan that prices all of them above 0 charges the node
// about twice: the report warns, and stops once one side is priced at 0.
func TestAPlanThatPricesWholeNodesTwiceIsWarnedAbout(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", dims: []string{usage.DimBillingMode}}
	svc := New(sp, []destinations.Destination{c}, nil, nil, nil, nil, Options{})
	node := event("acme", usage.MetricPrivateNodeHours, "metal.rack-2", nil)
	gpuWhole := event("acme", usage.MetricGPUHours, "A100", map[string]string{usage.DimBillingMode: "private_node"})
	cpuWhole := event("acme", usage.MetricCPUCoreHours, "", map[string]string{usage.DimBillingMode: "private_node"})
	gpuShared := event("globex", usage.MetricGPUHours, "A100", map[string]string{usage.DimBillingMode: "shared"})
	ctx := context.Background()

	// Shared GPU usage and node-hours of another kind of usage: nothing to warn about.
	svc.Observe([]usage.Event{node, gpuShared}, time.Now())
	if w := svc.CheckNow(ctx).Destinations[0].Warnings; len(w) != 0 {
		t.Fatalf("shared usage is not whole-node usage: %v", w)
	}
	svc.Observe([]usage.Event{gpuWhole, cpuWhole}, time.Now())
	w := svc.CheckNow(ctx).Destinations[0].Warnings
	if len(w) != 1 || !strings.Contains(w[0], "plan list prices whole nodes twice") ||
		!strings.Contains(w[0], "vcluster_private_node_hours and vcluster_gpu_hours, vcluster_cpu_core_hours") {
		t.Fatalf("warnings %v", w)
	}

	// The per-resource usage of whole nodes priced at 0: the node is charged once.
	c.set(func(s destinations.PriceSeries) destinations.PriceCoverage {
		if s.Dims[usage.DimBillingMode] == "private_node" {
			return destinations.PriceCoverage{Status: destinations.PriceZero, Detail: "priced at 0"}
		}
		return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3"}
	})
	if w := svc.CheckNow(ctx).Destinations[0].Warnings; len(w) != 0 {
		t.Fatalf("warned although whole-node GPU and CPU hours are priced at 0: %v", w)
	}
}

// Usage held for a tenant without a price list (at Metronome: no customer or
// contract) is held under that tenant's own plan. Once the tenant has one,
// its usage belongs to the plan's series, under another key. The held usage
// is released when that series may ship, and newer usage of the series waits
// behind it, so the series still ships in ledger order. Usage held under a
// plan that still has tenants never makes another plan's usage wait.
func TestUsageQueuesBehindUsageHeldBeforeItsTenantsPlanChanged(t *testing.T) {
	dir := t.TempDir()
	sp := openSpool(t, dir)
	held, err := pipeline.OpenHeld(dir+"/held", true)
	if err != nil {
		t.Fatal(err)
	}
	c := &checker{name: "metronome", plans: map[string]string{"newbie": "alias:newbie", "a": "standard", "b": "standard"}}
	c.set(func(s destinations.PriceSeries) destinations.PriceCoverage {
		if s.Plan == "alias:newbie" || s.Plan == "standard" {
			return destinations.PriceCoverage{Status: destinations.PriceMissing, Detail: "nothing rates it", Hint: "add what rates it"}
		}
		return destinations.PriceCoverage{Status: destinations.PriceCovered, Detail: "flat 3"}
	})
	svc := New(sp, []destinations.Destination{c}, nil, held, nil, nil, Options{Mode: ModeHold})
	g, ctx := svc.Gate(), context.Background()
	hold := func(seq uint64, ev usage.Event) string {
		t.Helper()
		d := g.Decide(ctx, "metronome", []usage.Event{ev})
		if !d[0].Hold {
			t.Fatalf("not held: %+v", d[0])
		}
		if _, err := held.Add("metronome", []pipeline.HeldEntry{{Seq: seq, At: time.Now(), Series: d[0].Series, Reason: d[0].Reason, Event: ev}}); err != nil {
			t.Fatal(err)
		}
		return d[0].Series
	}
	first := event("newbie", usage.MetricGPUHours, "H100", nil)
	oldKey := hold(1, first)
	hold(2, event("a", usage.MetricGPUHours, "H100", nil)) // tenants a and b share the unpriced plan "standard"
	svc.Observe([]usage.Event{event("b", usage.MetricGPUHours, "H100", nil)}, time.Now())

	// The tenant gets a price list ("list", where the H100 is priced), and
	// tenant a moves there too. A check drops the series nobody is left in.
	c.mu.Lock()
	c.plans["newbie"], c.plans["a"] = "list", "list"
	c.mu.Unlock()
	svc.CheckAll(ctx)

	next := event("newbie", usage.MetricGPUHours, "H100", map[string]string{"node": "n2"})
	d := g.Decide(ctx, "metronome", []usage.Event{next})
	if d[0].Series == oldKey || !d[0].Hold || !strings.Contains(d[0].Reason, "queued behind usage of the same series") {
		t.Fatalf("newer usage of the series must wait behind what was held before the plan changed: %+v", d[0])
	}
	if !g.Releasable(ctx, "metronome", oldKey, &first) {
		t.Fatal("the usage held before the plan changed may ship: its series is priced now")
	}
	// The dispatcher releases it; nothing is left to wait behind. Tenant a's
	// held usage is under "standard", which still has tenant b: it is judged
	// by that series, and holds up nobody on "list".
	if err := held.Release("metronome", oldKey, 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	if d := g.Decide(ctx, "metronome", []usage.Event{next}); d[0].Hold {
		t.Fatalf("still held after the older usage was released: %+v", d[0])
	}
	if held.Count("metronome") != 1 {
		t.Fatalf("held %d, want tenant a's event under the plan it was held on", held.Count("metronome"))
	}
}

// A decision that waits for a check decides by the series its event is in
// when the check is done. At Metronome a check reads a new tenant's contract
// and the next one moves the tenant's usage to the series of that rate card:
// when two checks ran while a decision waited, the series the decision had
// looked at was gone, and usage that is priced was held as "not confirmed".
func TestADecisionFollowsItsEventToTheSeriesACheckMovedItTo(t *testing.T) {
	sp := openSpool(t, t.TempDir())
	c := &checker{name: "metronome", plans: map[string]string{"newbie": "alias:newbie"}}
	first, release := make(chan struct{}), make(chan struct{})
	c.before = func(call int) {
		if call == 1 {
			close(first)
			<-release
		}
	}
	svc := New(sp, []destinations.Destination{c}, nil, nil, nil, nil, Options{Mode: ModeHold, DecideWait: 10 * time.Second})
	ev := event("newbie", usage.MetricGPUHours, "H100", nil)
	svc.Observe([]usage.Event{ev}, time.Now())
	ctx := context.Background()

	// A check is reading the billing platform when the dispatcher asks.
	checked := make(chan struct{})
	go func() { defer close(checked); svc.CheckAll(ctx) }()
	<-first
	decided := make(chan []pipeline.Decision, 1)
	go func() { decided <- svc.Gate().Decide(ctx, "metronome", []usage.Event{ev}) }()
	// The decision waits for a check of its own, queued behind the first.
	// That first check reads the tenant's price list, so the second moves
	// the tenant's usage to the list's series.
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	c.plans["newbie"] = "list"
	c.mu.Unlock()
	close(release)
	<-checked
	d := <-decided
	if d[0].Hold || !strings.HasPrefix(d[0].Series, "list\x00") {
		t.Fatalf("decision %+v: want the priced series of plan list, not held", d[0])
	}
}

// PRICE_CHECK=off says in words what it means.
func TestTheOffReportSaysThatNothingIsChecked(t *testing.T) {
	rep := Off()
	if rep.Mode != ModeOff || !rep.OK || rep.Destinations == nil {
		t.Fatalf("off report %+v", rep)
	}
	for _, want := range []string{"PRICE_CHECK=off", "checks no billing platform's prices", "holds nothing", "delivered and rated at 0"} {
		if !strings.Contains(rep.Note, want) {
			t.Errorf("note %q lacks %q", rep.Note, want)
		}
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"note":"PRICE_CHECK=off`) {
		t.Errorf("JSON %s", b)
	}
}
