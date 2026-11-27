package metrics

import (
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	"k8s.io/client-go/kubernetes/fake"
)

// cadvisor builds the transmit counters cAdvisor reports for one node: its
// own interfaces (no pod label), the same interfaces again for every pod on
// the host network, and eth0 of every other pod. hostPerScrape and
// podPerScrape are the bytes added at every 15s scrape.
func cadvisor(nodeInterfaces []string, hostPods, pods []string, hostPerScrape, podPerScrape float64, from, to time.Time) []fakeSeries {
	const name = "container_network_transmit_bytes_total"
	scrape := func(start, per float64) []promSample {
		var out []promSample
		v := start
		for ts := from; !ts.After(to); ts = ts.Add(15 * time.Second) {
			v += per
			out = append(out, promSample{T: ts, V: v})
		}
		return out
	}
	var out []fakeSeries
	for i, itf := range nodeInterfaces {
		host := scrape(1e9*float64(i+1), hostPerScrape*float64(i+1))
		out = append(out, fakeSeries{name: name, labels: map[string]string{"id": "/", "interface": itf, "vcluster": "gpu"}, samples: host})
		for _, p := range hostPods {
			out = append(out, fakeSeries{name: name, labels: map[string]string{"namespace": "kube-system", "pod": p, "interface": itf, "vcluster": "gpu"}, samples: host})
		}
	}
	for i, p := range pods {
		out = append(out, fakeSeries{name: name, labels: map[string]string{"namespace": "default", "pod": p, "interface": "eth0", "vcluster": "gpu"},
			samples: scrape(1000*float64(i+1), podPerScrape)})
	}
	return out
}

func egressBilled(t *testing.T, series []fakeSeries) float64 {
	t.Helper()
	fp := &fakeProm{series: series}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", PrometheusURL: srv.URL, GPUUtilQuery: "none",
		Now: func() time.Time { return we.Add(time.Hour) }})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p, err := c.NewPromSource(PromOptions{Health: h})
	if err != nil {
		t.Fatal(err)
	}
	// A cluster on its own nodes: every series of its nodes is selected.
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", External: true}, Tenant: "acme"}
	var bytes float64
	for _, w := range windowsFrom(ws, 3) {
		res := collectOK(t, p, []Target{tg}, []Window{w})
		if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, e := range res[0].Events {
			if e.Metric != usage.MetricNetworkEgressGB {
				t.Fatalf("unexpected metric %s", e.Metric)
			}
			bytes += e.Quantity * gib
		}
	}
	return bytes
}

// The built-in egress metric bills what the cluster's pods transmit on
// their own interfaces. The node's interface counters, and the same
// counters again for every pod on the host network, are not pods' traffic:
// counted, they billed an idle cluster about ten times what its pods sent,
// and more with every host-network pod.
func TestEgressCountsPodInterfacesNotTheNodes(t *testing.T) {
	from, to := ws.Add(-10*time.Minute), ws.Add(3*time.Minute)
	pods := []string{"web-1", "web-2", "db-0"}
	want := float64(len(pods)) * 12 * 500 // three pods, 12 scrapes in the three windows, 500 bytes each
	for _, tc := range []struct {
		name   string
		series []fakeSeries
	}{
		{"pods only", cadvisor(nil, nil, pods, 0, 500, from, to)},
		{"with the node's interfaces and three host-network pods",
			cadvisor([]string{"cni0", "enp1s0", "flannel.1"}, []string{"kube-flannel-x", "kube-proxy-x", "speaker-x"}, pods, 70_000, 500, from, to)},
		{"the node's interface is called eth0 too",
			cadvisor([]string{"eth0", "cni0"}, []string{"kube-proxy-x"}, pods, 70_000, 500, from, to)},
		{"the node's own series are not stored", func() []fakeSeries {
			var out []fakeSeries
			for _, s := range cadvisor([]string{"cni0", "enp1s0"}, []string{"kube-flannel-x", "kube-proxy-x"}, pods, 70_000, 500, from, to) {
				if s.labels["pod"] != "" {
					out = append(out, s)
				}
			}
			return out
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressBilled(t, tc.series); math.Abs(got-want) > 1 {
				t.Errorf("billed %.0f bytes of egress, want %.0f: only what the pods sent on their own interfaces", got, want)
			}
		})
	}
}

// Pods that send the same bytes at the same times are still pods: nothing
// else reports their interface, so both are billed.
func TestEgressKeepsPodsWithIdenticalCounters(t *testing.T) {
	from, to := ws.Add(-10*time.Minute), ws.Add(3*time.Minute)
	series := cadvisor(nil, nil, []string{"worker-1", "worker-2"}, 0, 500, from, to)
	series[1].samples = series[0].samples
	if got, want := egressBilled(t, series), float64(2*12*500); math.Abs(got-want) > 1 {
		t.Errorf("billed %.0f bytes, want %.0f: two pods with the same counter values are two pods", got, want)
	}
}

// A Prometheus whose series name no pod is left as it is.
func TestEgressWithoutPodLabelsIsLeftAlone(t *testing.T) {
	from, to := ws.Add(-10*time.Minute), ws.Add(3*time.Minute)
	series := cadvisor(nil, nil, []string{"a", "b"}, 0, 500, from, to)
	for i := range series {
		series[i].labels = map[string]string{"vcluster": "gpu", "interface": "eth0", "id": series[i].labels["pod"]}
	}
	if got, want := egressBilled(t, series), float64(2*12*500); math.Abs(got-want) > 1 {
		t.Errorf("billed %.0f bytes, want %.0f", got, want)
	}
}
