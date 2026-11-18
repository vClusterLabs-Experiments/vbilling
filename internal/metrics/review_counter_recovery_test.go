package metrics

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestReviewNewCounterDuringPartialOutageKeepsItsUsage(t *testing.T) {
	good := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total", labels: map[string]string{"namespace": ns, "model_name": "old"},
		samples: counterSamples(ws.Add(-time.Minute), ws.Add(3*time.Minute), 5, time.Time{})}}}
	goodSrv := httptest.NewServer(good.handler(t))
	defer goodSrv.Close()
	flaky := &fakeProm{series: []fakeSeries{
		{name: "vllm:generation_tokens_total", labels: map[string]string{"model_name": "old"},
			samples: counterSamples(ws.Add(-time.Minute), ws.Add(3*time.Minute), 5, time.Time{})},
		// A new pod/model starts 15 seconds into the failed window.
		{name: "vllm:generation_tokens_total", labels: map[string]string{"model_name": "new"},
			samples: counterSamples(ws.Add(75*time.Second), ws.Add(3*time.Minute), 3, time.Time{})},
	}}
	flakySrv := httptest.NewServer(flaky.handler(t))
	defer flakySrv.Close()
	shared := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
	own := Target{Cluster: discovery.TenantCluster{Name: "own", Namespace: "vc-own",
		Annotations: map[string]string{discovery.AnnotationPrefix + "prometheus-url": flakySrv.URL}}, Tenant: "globex"}
	_, p, _ := promSourceFor(t, goodSrv.URL, "", nil, tokens)
	var billed float64
	for i, w := range windowsFrom(ws, 3) {
		flaky.mu.Lock()
		flaky.down = i == 1
		flaky.mu.Unlock()
		res := collectOK(t, p, []Target{shared, own}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			if e.Tenant == "globex" && e.SKU == "new" {
				billed += e.Quantity
			}
		}
	}
	// All eight scrapes are retained by Prometheus; this is not loss before
	// the first scrape or a reset between scrapes. Recovery must count 24.
	if billed != 24 {
		t.Fatalf("new counter billed %v tokens, want 24; samples from the failed window were discarded", billed)
	}
}

// Windows read and committed in consecutive runs (one per owner, after a
// cluster moved) bill the same total as one run: each run starts from the
// baselines the previous one committed.
func TestCounterRunsCommittedInTurnBillOnce(t *testing.T) {
	samples := counterSamples(ws.Add(-time.Minute), ws.Add(5*time.Minute), 5, time.Time{})
	total := func(runs ...[]Window) float64 {
		prom := &fakeProm{series: []fakeSeries{{name: "vllm:generation_tokens_total",
			labels: map[string]string{"namespace": ns, "model_name": "m"}, samples: samples}}}
		srv := httptest.NewServer(prom.handler(t))
		defer srv.Close()
		_, p, _ := promSourceFor(t, srv.URL, "", nil, tokens)
		tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}
		// Prime the baseline, as the run before the backfill did.
		if err := p.Commit(collectOK(t, p, []Target{tg}, windowsFrom(ws.Add(-time.Minute), 1)), func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		var sum float64
		for _, r := range runs {
			res := collectOK(t, p, []Target{tg}, r)
			if err := p.Commit(res, func(_ Window, evs []usage.Event) error {
				for _, e := range evs {
					sum += e.Quantity
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return sum
	}
	all := windowsFrom(ws, 5)
	one := total(all)
	split := total(all[:2], all[2:])
	if one == 0 || split != one {
		t.Fatalf("two runs billed %v, one run %v: want the same non-zero total", split, one)
	}
}
