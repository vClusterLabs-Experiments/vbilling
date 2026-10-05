package spool

import (
	"fmt"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// BenchmarkScan measures query scans over 50k events (one hour of 100
// tenant clusters at ~8 events/minute).
func BenchmarkScan(b *testing.B) {
	c := &clock{t: t0}
	s, err := Open(b.TempDir(), Options{NoSync: true, Now: c.Now})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for w := 0; w < 60; w++ {
		ws := t0.Add(time.Duration(w) * time.Minute)
		c.t = ws.Add(time.Minute)
		var evs []usage.Event
		for i := 0; i < 833; i++ {
			e := usage.Event{Tenant: fmt.Sprintf("tenant-%d", i%100), Metric: usage.MetricGPUHours, Quantity: 0.1, SKU: "NVIDIA-H100",
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{"tenant_cluster": fmt.Sprintf("c-%d", i)}}
			e.Finalize()
			evs = append(evs, e)
		}
		if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			b.Fatal(err)
		}
	}
	for _, r := range []struct {
		name     string
		from, to time.Time
	}{{"full-hour", t0, t0.Add(time.Hour)}, {"five-minutes", t0.Add(30 * time.Minute), t0.Add(35 * time.Minute)}} {
		b.Run(r.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				n := 0
				s.Scan(r.from, r.to, func(*usage.Event) error { n++; return nil })
			}
		})
	}
}
