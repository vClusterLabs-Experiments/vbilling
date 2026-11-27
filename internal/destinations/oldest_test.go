package destinations

import (
	"testing"
	"time"
)

// agedDest declares the oldest usage it accepts.
type agedDest struct {
	fakeDest
	age time.Duration
}

func (d *agedDest) OldestUsage() time.Duration { return d.age }

// The ingest bound is the tightest limit a destination declares, with the
// name of the destination that sets it; destinations without a limit do not
// set one.
func TestOldestUsageIsTheTightestDeclaredLimit(t *testing.T) {
	plain := &fakeDest{name: "webhook"}
	loose := &agedDest{fakeDest{"stripe-like"}, 35 * 24 * time.Hour}
	strict := &agedDest{fakeDest{"metronome-like"}, 34*24*time.Hour - time.Hour}
	none := &agedDest{fakeDest{"unlimited"}, 0}

	if _, _, ok := OldestUsage(nil); ok {
		t.Error("no destinations: a limit")
	}
	if limit, by, ok := OldestUsage([]Destination{plain, none}); ok {
		t.Errorf("destinations that declare no limit set one: %s by %s", limit, by)
	}
	for _, dests := range [][]Destination{{plain, loose, strict}, {strict, none, loose, plain}} {
		limit, by, ok := OldestUsage(dests)
		if !ok || limit != strict.age || by != "metronome-like" {
			t.Errorf("limit %s by %q (%v), want %s by metronome-like", limit, by, ok, strict.age)
		}
	}
	if limit, by, ok := OldestUsage([]Destination{loose, plain}); !ok || limit != loose.age || by != "stripe-like" {
		t.Errorf("limit %s by %q (%v), want the only declared one", limit, by, ok)
	}
}
