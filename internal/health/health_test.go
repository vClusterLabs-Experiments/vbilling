package health

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Every change of state is logged, and a source that stays broken is logged
// again at 10, 100, ... so it is never silent.
func TestStateChangesAreLoggedAndRepeatsThrottled(t *testing.T) {
	var logs []string
	r := New(nil)
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return now })
	r.SetLogger(func(f string, a ...any) { logs = append(logs, f) })
	r.OK("prometheus/x/c1", "prometheus", "c1", 3)
	for i := 0; i < 12; i++ {
		r.Empty("prometheus/x/c1", "prometheus", "c1", "no series", "check the selector")
	}
	r.Fail("prometheus/x/c1", "prometheus", "c1", errors.New("timeout"), "")
	r.OK("prometheus/x/c1", "prometheus", "c1", 3)
	joined := strings.Join(logs, "\n")
	if strings.Count(joined, "returned no data") != 2 || strings.Count(joined, "failing") != 1 || strings.Count(joined, "recovered") != 1 {
		t.Errorf("logs:\n%s", joined)
	}
	s := r.List()[0]
	if s.Status != OK || s.ConsecutiveEmpty != 0 || s.LastError.IsZero() || s.LastErrorMessage != "timeout" {
		t.Errorf("source = %+v", s)
	}
	r.Empty("prometheus/y/c2", "prometheus", "c2", "no series", "")
	if p := r.Problems(); len(p) != 1 || p[0].Name != "prometheus/y/c2" {
		t.Errorf("problems = %+v", p)
	}
	now = now.Add(48 * time.Hour)
	r.OK("records/litellm", "records", "litellm", 1)
	r.Prune(now.Add(-time.Hour))
	if l := r.List(); len(l) != 1 || l[0].Name != "records/litellm" {
		t.Errorf("after prune = %+v", l)
	}
}
