package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The operator's Prometheus is read in chunks of at most an hour, and a
// chunk whose answer is past the bound is read in halves: every sample
// arrives once, in order. A Prometheus a tenant influences is read in one
// answer, bounded at 16 MiB.
func TestOperatorPrometheusIsReadInChunks(t *testing.T) {
	start := ws.Add(-3 * time.Hour)
	var mu sync.Mutex
	var spans []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var b strings.Builder
		b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"__name__":"x"},"values":[`)
		first := true
		emit := func(ts time.Time, v string) {
			if !first {
				b.WriteByte(',')
			}
			first = false
			fmt.Fprintf(&b, `[%d,"%s"]`, ts.Unix(), v)
		}
		if r.URL.Path == "/api/v1/query_range" {
			s, _ := strconv.ParseInt(q.Get("start"), 10, 64)
			e, _ := strconv.ParseInt(q.Get("end"), 10, 64)
			step, _ := strconv.Atoi(q.Get("step"))
			mu.Lock()
			spans = append(spans, int(e-s)/step+1)
			mu.Unlock()
			for ts := s; ts <= e; ts += int64(step) {
				emit(time.Unix(ts, 0), "1")
			}
		} else {
			at, _ := strconv.ParseInt(q.Get("time"), 10, 64)
			query := q.Get("query")
			secs, _ := strconv.Atoi(strings.TrimSuffix(query[strings.LastIndex(query, "[")+1:], "s]"))
			mu.Lock()
			spans = append(spans, secs)
			mu.Unlock()
			for ts := start; !ts.After(time.Unix(at, 0)); ts = ts.Add(15 * time.Second) {
				if ts.Unix() > at-int64(secs) {
					emit(ts, strconv.FormatInt(ts.Unix()-start.Unix(), 10))
				}
			}
		}
		b.WriteString(`]}]}}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	// An hour of 15 s samples is about 5 KiB: with a 4 KiB bound each hour
	// is read in halves.
	p := newPrometheusClient(srv.URL, nil, "")
	p.t.(*httpTransport).limit = 4 << 10
	series, err := p.QueryRaw(context.Background(), "x", 2*time.Hour, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Samples) != 2*240 {
		t.Fatalf("read %d series with %d samples, want 1 with 480", len(series), len(series[0].Samples))
	}
	for i := 1; i < len(series[0].Samples); i++ {
		if !series[0].Samples[i].T.Equal(series[0].Samples[i-1].T.Add(15 * time.Second)) {
			t.Fatalf("sample %d at %s follows %s", i, series[0].Samples[i].T, series[0].Samples[i-1].T)
		}
	}
	if want := []int{3600, 1800, 1800, 3600, 1800, 1800}; fmt.Sprint(spans) != fmt.Sprint(want) {
		t.Fatalf("raw reads of %v seconds, want %v (each hour too large, then its halves)", spans, want)
	}

	spans = nil
	p.t.(*httpTransport).limit = maxOperatorPromAnswer
	rng, err := p.QueryRange(context.Background(), "y", ws, ws.Add(119*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(rng) != 1 || len(rng[0].Samples) != 120 || fmt.Sprint(spans) != "[60 60]" {
		t.Fatalf("range read %d steps in requests of %v steps, want 120 in two of 60", len(rng[0].Samples), spans)
	}

	// A cluster's own Prometheus: one answer, bounded at 16 MiB.
	if c := newClusterPrometheusClient(srv.URL, nil, ""); c.split || c.t.(*httpTransport).limit != maxPromAnswer {
		t.Fatalf("a cluster's own Prometheus is read in chunks (%t) or past 16 MiB (%d)", c.split, c.t.(*httpTransport).limit)
	}
}
