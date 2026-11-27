package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	"k8s.io/client-go/kubernetes/fake"
)

// requests is a counter exposed with OpenMetrics _created series, as
// prometheus_client does.
var requests = PromMetric{Code: "requests", Counter: "app_requests_total{{selector}}", Dimensions: []string{"model"}, SKULabel: "model"}

// every returns a counter sampled every step from start to end, growing by
// per at each sample, starting at per.
func every(start, end time.Time, step time.Duration, per float64) []promSample {
	var out []promSample
	v := 0.0
	for ts := start; !ts.After(end); ts = ts.Add(step) {
		v += per
		out = append(out, promSample{T: ts, V: v})
	}
	return out
}

// readEach reads and commits one window at a time, as the controller does
// when it keeps up, and returns what each window billed.
func readEach(t *testing.T, p *PromSource, tg Target, windows []Window) map[time.Time]float64 {
	t.Helper()
	billed := map[time.Time]float64{}
	for _, w := range windows {
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			billed[w.Start] += e.Quantity
		}
	}
	return billed
}

// An exporter restarts 2 seconds before a window closes and is first
// scraped 47 seconds later, in the next window, holding the 60 requests it
// counted before that scrape. Its _created series says when its counter
// started, and nothing of it was read before, so it counts from zero:
// every window holds one step and nothing is lost. Before, a counter had to
// start after the last read to count from zero, and one that started
// seconds before a window closed lost its first scrape interval.
func TestCounterThatStartedJustBeforeAWindowClosedCountsFromZero(t *testing.T) {
	old := map[string]string{"namespace": ns, "model": "a", "job": "app", "instance": "10.0.0.7:8000"}
	fresh := map[string]string{"namespace": ns, "model": "a", "job": "app", "instance": "10.0.0.9:8000"}
	restart := ws.Add(4*time.Minute + 58*time.Second) // 10:04:58
	firstScrape := ws.Add(5*time.Minute + 45*time.Second)
	end := ws.Add(10 * time.Minute)
	fp := &fakeProm{series: []fakeSeries{
		// The old pod: one step of 60 a minute, last scraped at 10:04:30.
		{name: "app_requests_total", labels: old, samples: every(ws.Add(-10*time.Minute+30*time.Second), ws.Add(4*time.Minute+30*time.Second), time.Minute, 60)},
		// The new pod: 60 at its first scrape, then 60 a minute.
		{name: "app_requests_total", labels: fresh, samples: every(firstScrape, end, time.Minute, 60)},
		{name: "app_requests_created", labels: fresh, samples: func() []promSample {
			var out []promSample
			for ts := firstScrape; !ts.After(end); ts = ts.Add(time.Minute) {
				out = append(out, promSample{T: ts, V: float64(restart.Unix())})
			}
			return out
		}()},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, h := promSourceFor(t, srv.URL, "", nil, requests)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	billed := readEach(t, p, tg, windowsFrom(ws, 10))
	total := 0.0
	for i := 0; i < 10; i++ {
		w := ws.Add(time.Duration(i) * time.Minute)
		if billed[w] != 60 {
			t.Errorf("window %s billed %v, want one step of 60", w.Format("15:04"), billed[w])
		}
		total += billed[w]
	}
	if total != 600 {
		t.Errorf("billed %v over ten windows, want 600: the count before the new pod's first scrape is usage", total)
	}
	for _, s := range h.List() {
		if strings.HasSuffix(s.Name, "/history") {
			t.Errorf("the new pod's count was treated as history: %s", s.Message)
		}
	}
}

// The same first sample, but another series of the same target was scraped
// between the counter's start and it: the series is not new, a relabel or a
// changed label set made an existing counter look new. Its first sample is
// its baseline, so nothing already billed is billed again.
func TestCounterSeenUnderOtherLabelsBeforeIsNotCountedFromZero(t *testing.T) {
	before := map[string]string{"namespace": ns, "model": "a", "job": "app", "instance": "10.0.0.7:8000"}
	after := map[string]string{"namespace": ns, "model": "a", "job": "app", "instance": "10.0.0.7:8000", "team": "ml"}
	created := ws.Add(58 * time.Second) // two seconds before the first window closes
	relabel := ws.Add(2 * time.Minute)
	end := ws.Add(4 * time.Minute)
	steps := every(ws.Add(75*time.Second), end, 15*time.Second, 5)
	var first, second []promSample
	for _, smp := range steps {
		if smp.T.Before(relabel) {
			first = append(first, smp)
		} else {
			second = append(second, smp)
		}
	}
	createdAt := func(samples []promSample) []promSample {
		out := make([]promSample, len(samples))
		for i, smp := range samples {
			out[i] = promSample{T: smp.T, V: float64(created.Unix())}
		}
		return out
	}
	fp := &fakeProm{series: []fakeSeries{
		{name: "app_requests_total", labels: before, samples: first},
		{name: "app_requests_created", labels: before, samples: createdAt(first)},
		{name: "app_requests_total", labels: after, samples: second},
		{name: "app_requests_created", labels: after, samples: createdAt(second)},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, requests)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	billed := readEach(t, p, tg, windowsFrom(ws, 4))
	total := 0.0
	for _, v := range billed {
		total += v
	}
	// The counter reached len(steps)*5. The step between the last sample
	// under the old labels and the first under the new ones (5) is not
	// billed: the relabelled series starts from its first sample.
	if want := float64(len(steps)*5 - 5); total != want {
		t.Errorf("billed %v, want %v: a relabelled counter must not be counted from zero again (it holds %v)", total, want, len(steps)*5)
	}
}

// Samples reach this Prometheus 20 seconds after they are taken. Read the
// moment a window closes, the sample taken 15 seconds before its end is
// not there yet: the window bills nothing of it and the next one bills it,
// so it is counted once, and the source says that samples arrive late.
func TestCounterSampleThatArrivesLateIsBilledOnceAndReported(t *testing.T) {
	labels := map[string]string{"namespace": ns, "model": "a"}
	fp := &fakeProm{lag: 20 * time.Second, series: []fakeSeries{
		{name: "app_requests_total", labels: labels, samples: every(ws.Add(-10*time.Minute+45*time.Second), ws.Add(5*time.Minute), time.Minute, 60)},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, h := promSourceFor(t, srv.URL, "", nil, requests)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	late := 0
	billed := map[time.Time]float64{}
	for _, w := range windowsFrom(ws, 4) {
		fp.mu.Lock()
		fp.now = w.End // read the moment the window closes
		fp.mu.Unlock()
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		late += res[0].Late
		for _, e := range res[0].Events {
			billed[w.Start] += e.Quantity
		}
	}
	// Window 10:00 reads the sample of 09:59:45 (in its lookback, its
	// baseline) and nothing else; each later window reads the sample taken
	// in the window before it.
	if billed[ws] != 0 {
		t.Errorf("first window billed %v, want 0: its sample had not arrived", billed[ws])
	}
	for i := 1; i < 4; i++ {
		if w := ws.Add(time.Duration(i) * time.Minute); billed[w] != 60 {
			t.Errorf("window %s billed %v, want 60 (the sample of the window before)", w.Format("15:04"), billed[w])
		}
	}
	if late != 3 {
		t.Errorf("late samples = %d, want 3", late)
	}
	var note string
	for _, s := range h.List() {
		if s.Name == health.Name("prometheus", "requests", tg.Cluster.ExternalID()) {
			if s.Status != health.OK {
				t.Errorf("late samples are billed, the source stays ok: %s", s.Describe())
			}
			note = s.Message
		}
	}
	if !strings.Contains(note, "reached Prometheus more than") {
		t.Errorf("the source does not say its samples arrive late: %q", note)
	}
}

// A pod started while its cluster's metrics could not be shipped. When they
// arrive, its first samples are older than the last read, and none of them
// was ever read: with its _created series as evidence it is billed in full.
// Before, such a series started from its last sample before the last read,
// and everything it had counted until then was dropped as history.
func TestNewSeriesWhoseFirstSamplesArriveLateIsBilledInFull(t *testing.T) {
	labels := map[string]string{"namespace": ns, "model": "a", "job": "app", "instance": "10.0.0.9:8000"}
	created := ws.Add(20 * time.Second)
	samples := every(ws.Add(30*time.Second), ws.Add(4*time.Minute), 30*time.Second, 10)
	createdSamples := make([]promSample, len(samples))
	for i, smp := range samples {
		createdSamples[i] = promSample{T: smp.T, V: float64(created.Unix())}
	}
	fp := &fakeProm{series: []fakeSeries{
		{name: "app_requests_total", labels: labels, samples: samples},
		{name: "app_requests_created", labels: labels, samples: createdSamples},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, requests)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	total := 0.0
	for i, w := range windowsFrom(ws, 4) {
		fp.mu.Lock()
		fp.now, fp.lag = w.End, 0
		if i < 2 {
			fp.lag = time.Hour // nothing of this cluster arrives during the first two windows
		}
		fp.mu.Unlock()
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			total += e.Quantity
		}
	}
	if want := samples[len(samples)-1].V; total != want {
		t.Errorf("billed %v, want %v: everything the new pod counted, including before its samples arrived", total, want)
	}
}

// A window is read once the cluster's newest sample is past its end: then
// every sample of the window has arrived. It never waits for a cluster
// that has nothing on its way.
func TestWindowIsSettledOnceItsSamplesHaveArrived(t *testing.T) {
	end := we
	series := func(lastSample time.Time) []fakeSeries {
		return []fakeSeries{{name: "app_requests_total", labels: map[string]string{"namespace": ns, "model": "a"},
			samples: every(ws.Add(-10*time.Minute), lastSample, 15*time.Second, 1)}}
	}
	gauge := PromMetric{Code: "power", Query: "sum(power_watts{{selector}})"}
	for _, tc := range []struct {
		name    string
		series  []fakeSeries
		lag     time.Duration
		at      time.Duration // after the window's end
		metrics []PromMetric
		want    bool
	}{
		{"read at once, samples 20s behind", series(we.Add(5 * time.Minute)), 20 * time.Second, 5 * time.Second, []PromMetric{requests}, false},
		{"a sample taken 15s after the end has arrived", series(we.Add(5 * time.Minute)), 20 * time.Second, 35 * time.Second, []PromMetric{requests}, true},
		{"a sample taken right at the end is not past it", series(we), 0, 10 * time.Second, []PromMetric{requests}, false},
		{"its pods are gone: nothing on its way", series(we.Add(-3 * time.Minute)), 0, 5 * time.Second, []PromMetric{requests}, true},
		{"no series at all", nil, 0, 5 * time.Second, []PromMetric{requests}, true},
		{"never longer than the maximum", series(we.Add(5 * time.Minute)), time.Hour, PromMaxWait, []PromMetric{requests}, true},
		{"only a query: a fixed wait", nil, 0, 10 * time.Second, []PromMetric{gauge}, false},
		{"only a query: read after it", nil, 0, promQuerySettle, []PromMetric{gauge}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := end.Add(tc.at)
			fp := &fakeProm{series: tc.series, lag: tc.lag, now: now}
			srv := httptest.NewServer(fp.handler(t))
			defer srv.Close()
			c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", PrometheusURL: srv.URL, GPUUtilQuery: "none", EgressQuery: "none",
				PromMetrics: tc.metrics, Now: func() time.Time { return now }})
			p, err := c.NewPromSource(PromOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
			if got, waiting := p.Settled(context.Background(), []Target{tg}, end); got != tc.want {
				t.Errorf("settled = %t (waiting for %q), want %t", got, waiting, tc.want)
			}
		})
	}
}

// Idle GPUs are recorded at 0% utilization. Before, a quantity of zero was
// dropped, so an idle fleet had no utilization events at all while its
// source read ok.
func TestIdleGPUsAreRecordedAtZeroUtilization(t *testing.T) {
	prom, _ := promServer(t, nil, func(q string) (string, map[string]string) {
		switch {
		case strings.Contains(q, "avg_over_time(DCGM_FI_DEV_GPU_UTIL"):
			return "0", map[string]string{"modelName": "NVIDIA A100-SXM4-80GB"}
		case strings.Contains(q, "count by"):
			return "8", map[string]string{"modelName": "NVIDIA A100-SXM4-80GB"}
		}
		return "", nil
	})
	evs := collect(t, Options{PrometheusURL: prom.URL, EgressQuery: "none"}, nil, latest())[we]
	var found *usage.Event
	for i := range evs {
		if evs[i].Metric == usage.MetricGPUUtilization {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatal("no utilization event for idle GPUs")
	}
	if found.Quantity != 0 || found.Properties["avg_utilization_pct"] != 0.0 || found.Properties["gpu_devices"] != 8.0 || found.Properties["estimate"] != true {
		t.Errorf("idle GPUs: quantity %v, properties %v; want 0 util-hours, avg_utilization_pct 0, gpu_devices 8, estimate", found.Quantity, found.Properties)
	}
	if found.Dimensions[usage.DimGPUType] == "" {
		t.Errorf("no gpu_type on %v", found.Dimensions)
	}
	if err := found.Validate(); err != nil {
		t.Errorf("a zero reading must be a valid event: %v", err)
	}
}
