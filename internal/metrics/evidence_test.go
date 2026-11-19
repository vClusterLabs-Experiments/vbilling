package metrics

import (
	"testing"
	"time"
)

// With one sample there is no rate to judge by: the process start counts
// as evidence when that sample is the process's first scrape, within two
// scrape intervals of its start (a window when the interval is unknown).
// A sample before the start is another process's.
func TestOneSampleCountsFromItsProcessStartOnlyAtItsFirstScrape(t *testing.T) {
	started := ws.Add(65 * time.Second)
	one := func(after time.Duration) promSeries {
		return promSeries{Samples: []promSample{{T: started.Add(after), V: 30}}}
	}
	process := func(every time.Duration) []promSample {
		var out []promSample
		for ts := started.Add(every); ts.Before(started.Add(4 * every)); ts = ts.Add(every) {
			out = append(out, promSample{T: ts, V: float64(started.Unix())})
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		after time.Duration
		every time.Duration // the process series' scrape interval (0: one sample)
		want  bool
	}{
		{"first scrape, 60s interval", 25 * time.Second, time.Minute, true},
		{"first scrape, 15s interval", 25 * time.Second, 15 * time.Second, true},
		{"three 15s intervals after its start", 50 * time.Second, 15 * time.Second, false},
		{"interval unknown, within a window", 50 * time.Second, 0, true},
		{"interval unknown, past a window", 70 * time.Second, 0, false},
		{"before its process started", -5 * time.Second, time.Minute, false},
	} {
		ev := &startEvidence{process: map[string][]promSample{"gateway\x0010.0.0.7:4000": {{T: started.Add(tc.after), V: float64(started.Unix())}}}}
		if tc.every > 0 {
			ev.process["gateway\x0010.0.0.7:4000"] = process(tc.every)
		}
		s := one(tc.after)
		s.Labels = map[string]string{"job": "gateway", "instance": "10.0.0.7:4000"}
		if got := ev.startedSince(s, ws.Add(time.Minute), time.Minute); got != tc.want {
			t.Errorf("%s: counted from zero %t, want %t", tc.name, got, tc.want)
		}
	}
}
