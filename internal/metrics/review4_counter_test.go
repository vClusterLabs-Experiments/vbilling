package metrics

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A process-local counter (an inference engine or gateway without _created
// series: a Go service, or prometheus_client with created series disabled)
// starts after vBilling's last read and is scraped every 60 seconds,
// Prometheus' default scrape_interval. Its first read holds one sample, so
// countedSince (1aa6afe) refuses the process start as evidence ("with one
// sample there is no rate to judge by"), whatever the count: the series is
// baselined at its first sample and what the engine served before that
// scrape is never billed. Here the count is exactly the engine's own rate.
// Before 1aa6afe it was billed from zero. usage-sources.html says only an
// engine that "serves more than twice its later rate before its first
// scrape" bills from its first sample.
func TestReview4EngineScrapedOncePerWindowBillsItsFirstCount(t *testing.T) {
	engine := map[string]string{"namespace": ns, "model_name": "llama", "job": "gateway", "instance": "10.0.0.7:4000"}
	process := map[string]string{"namespace": ns, "job": "gateway", "instance": "10.0.0.7:4000", "pod": "gw-x-default-x-gpu"}
	// The process starts at 10:01:05, after the read through 10:01:00. First
	// scrape at 10:01:30: 30 tokens in 25 s (1.2 per second). Then 72 more
	// every minute: 1.2 per second.
	var samples []promSample
	for i, ts := 0, ws.Add(90*time.Second); !ts.After(ws.Add(4 * time.Minute)); i, ts = i+1, ts.Add(time.Minute) {
		samples = append(samples, promSample{T: ts, V: 30 + 72*float64(i)})
	}
	started := createdSeries("process_start_time_seconds", process, ws.Add(65*time.Second), ws.Add(90*time.Second), ws.Add(4*time.Minute))
	fp := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: engine, samples: samples}, started}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	var billed float64
	for _, w := range windowsFrom(ws, 4) { // 10:00-10:01 (before the engine), then three windows
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			billed += e.Quantity
		}
	}
	// 30 + 72 + 72 = 174 tokens served through 10:03:30.
	if billed != 174 {
		t.Fatalf("an engine started at 10:01:05, after the last read, and scraped every 60 s held 30 tokens at its first scrape 25 s later, "+
			"exactly its later rate of 1.2/s: billed %v of 174 tokens. Its first read has one sample, so process_start_time_seconds no "+
			"longer counts and its first scrape interval is never billed", billed)
	}
}
