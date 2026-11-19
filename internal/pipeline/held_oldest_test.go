package pipeline

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Oldest stays exact across releases and restarts although ledger order
// is not window order (a late adjustment for an old window sits behind
// newer usage), and Release reads only what it releases.
func TestHeldOldestFollowsReleasesAndReloads(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHeld(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	at := func(seq uint64, ws time.Time) HeldEntry {
		return HeldEntry{Seq: seq, At: base, Series: "s", Reason: "no price",
			Event: usage.Event{ID: "e" + string(rune('a'+seq)), Tenant: "acme", Metric: "m", WindowStart: ws, WindowEnd: ws.Add(time.Minute)}}
	}
	entries := []HeldEntry{at(1, base), at(2, base.Add(5*time.Minute)), at(3, base.Add(-time.Hour)), at(4, base.Add(10*time.Minute))}
	if _, err := h.Add("d", entries); err != nil {
		t.Fatal(err)
	}
	oldest := func(h *HeldStore) time.Time { return h.Series("d")[0].Oldest }
	if got := oldest(h); !got.Equal(base.Add(-time.Hour)) {
		t.Fatalf("oldest %s, want the late window", got)
	}
	got, next, err := h.Peek("d", "s", 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("peek %v %v", got, err)
	}
	if err := h.Release("d", "s", got[1].Seq, 2, next); err != nil {
		t.Fatal(err)
	}
	if s := h.Series("d")[0]; !s.Oldest.Equal(base.Add(-time.Hour)) || s.FirstSeq != 3 || s.Count != 2 {
		t.Fatalf("after releasing two: %+v", s)
	}
	// A restart reads the same state from the file.
	h2, err := OpenHeld(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if s := h2.Series("d")[0]; !s.Oldest.Equal(base.Add(-time.Hour)) || s.FirstSeq != 3 || s.Count != 2 {
		t.Fatalf("after reload: %+v", s)
	}
	got, next, err = h2.Peek("d", "s", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("peek %v %v", got, err)
	}
	if err := h2.Release("d", "s", got[0].Seq, 1, next); err != nil {
		t.Fatal(err)
	}
	if s := h2.Series("d")[0]; !s.Oldest.Equal(base.Add(10*time.Minute)) || s.FirstSeq != 4 {
		t.Fatalf("after releasing the late entry: %+v", s)
	}
}
