package metrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
	"k8s.io/client-go/kubernetes/fake"
)

// review4EgressProm answers counter reads (selector[Ns] at time) of the
// default egress counter as Prometheus does: pods series, one per pod (as
// cAdvisor reports them), scraped every 15 seconds since base and growing
// by 1 MiB per scrape, written as a matrix with millisecond timestamps and
// byte counts. Anything else gets no series.
func review4EgressProm(pods int, base time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("query")
		if !strings.HasPrefix(q, "container_network_transmit_bytes_total{") {
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
			return
		}
		at, _ := strconv.ParseInt(r.URL.Query().Get("time"), 10, 64)
		open := strings.LastIndex(q, "[")
		secs, _ := strconv.Atoi(strings.TrimSuffix(q[open+1:], "s]"))
		from, to := time.Unix(at-int64(secs), 0), time.Unix(at, 0)
		bw := bufio.NewWriterSize(w, 1<<16)
		_, _ = bw.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
		for i := 0; i < pods; i++ {
			if i > 0 {
				_ = bw.WriteByte(',')
			}
			fmt.Fprintf(bw, `{"metric":{"__name__":"container_network_transmit_bytes_total","namespace":%q,"pod":"worker-%04d","interface":"eth0"},"values":[`, ns, i)
			first := true
			for ts, v := base, int64(80_000_000_000); !ts.After(to); ts, v = ts.Add(15*time.Second), v+1<<20 {
				if !ts.After(from) {
					continue
				}
				if !first {
					_ = bw.WriteByte(',')
				}
				first = false
				fmt.Fprintf(bw, `[%d.%03d,"%d"]`, ts.Unix(), 123, v)
			}
			_, _ = bw.WriteString(`]}`)
		}
		_, _ = bw.WriteString(`]}}`)
		_ = bw.Flush()
	}
}

// vBilling is down for 6 hours (MAX_BACKFILL). When it starts again,
// CollectPrometheusDue reads the 360 windows since the Prometheus watermark
// in one run, and a counter is read with one query over all of them. For a
// cluster running 500 pods on shared nodes, the raw samples of the default
// egress counter over those 6 hours (15 s scrapes) are about 24 MB of JSON.
// 1ee25a9 fails every answer past 16 MiB (64 MiB before), so the read
// fails. It is the only read, so Collect returns ErrPrometheusUnavailable,
// nothing is committed, and the next pass reads the same range again, and
// fails again. With other clusters' reads succeeding, this cluster's
// counter instead keeps catching up from its last read, a range that only
// grows, until maxCounterGap (24 hours) drops the catch-up: that usage is
// never billed. 16 MiB was sized for one window, not for a backfill or a
// catch-up.
func TestReview4PrometheusBackfillOfABusyClusterIsBilled(t *testing.T) {
	const pods = 500
	start := ws.Add(-6 * time.Hour) // the last read before the outage ended here
	srv := httptest.NewServer(review4EgressProm(pods, start.Add(-time.Hour)))
	defer srv.Close()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{Region: "r1", PrometheusURL: srv.URL, GPUUtilQuery: "none",
		Now: func() time.Time { return ws.Add(time.Minute) }})
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	p, err := c.NewPromSource(PromOptions{Health: h})
	if err != nil {
		t.Fatal(err)
	}
	tg := Target{Cluster: discovery.TenantCluster{Name: "batch", Namespace: ns, Ready: true}, Tenant: "acme"}

	// The read before the outage sets every series' baseline.
	res := collectOK(t, p, []Target{tg}, []Window{{Start: start.Add(-time.Minute), End: start}})
	if err := p.Commit(res, func(Window, []usage.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The 360 windows of the outage, read in one run as CollectPrometheusDue does.
	backfill := windowsFrom(start, 360)
	res, err = p.Collect(context.Background(), []Target{tg}, backfill)
	if err != nil {
		why := ""
		for _, s := range h.List() {
			if s.Status == health.Failing {
				why += "; " + s.Name + ": " + s.Message
			}
		}
		t.Fatalf("vBilling restarted after 6 hours: the backfill read of the egress counter of a 500-pod cluster (about 24 MB of raw samples) "+
			"fails past the 16 MiB cap, so none of its %d GiB of egress is billed and the next pass reads the same range again: %v%s", 360*pods*4/1024, err, why)
	}
	var billed float64
	for _, pw := range res {
		for _, e := range pw.Events {
			billed += e.Quantity
		}
	}
	want := float64(pods) * 6 * 60 * 4 / 1024 // 4 scrapes of 1 MiB per minute per pod, in GiB
	if math.Abs(billed-want) > 1e-3 {
		t.Fatalf("backfill billed %v GiB of egress, want %v", billed, want)
	}
}
