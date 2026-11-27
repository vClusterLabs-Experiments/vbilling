package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// A destination never seen before starts at the ledger head, and that is
// recorded as where it starts. A cursor an earlier version wrote has no
// recorded start: it started "at the beginning".
func TestWhereADestinationStartsIsRecorded(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	s.Cursor("stripe") // a cursor of an earlier version: no marks yet
	if err := os.Remove(filepath.Join(dir, destinationsFile)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = open(t, dir, c, Options{})
	appendWindows(t, s, t0, 2, 2) // 6 records
	if st := s.DestinationState("stripe"); st.Start != nil || st.Paused != nil || st.Detached != nil {
		t.Fatalf("a cursor of an earlier version has marks: %+v", st)
	}
	c.Add(time.Hour)
	if cur := s.Cursor("metronome"); cur != 6 {
		t.Fatalf("a new destination starts at %d, want the head 6", cur)
	}
	want := DestinationStart{Seq: 6, At: t0.Add(time.Hour), How: StartCreated}
	if st := s.DestinationState("metronome"); st.Start == nil || *st.Start != want {
		t.Fatalf("start of a new destination: %+v, want %+v", st.Start, want)
	}
	// Asking again changes nothing, and unknown names are not created by a look.
	appendWindows(t, s, t0.Add(2*time.Minute), 1, 2)
	if cur := s.Cursor("metronome"); cur != 6 || s.DestinationState("metronome").Start.Seq != 6 {
		t.Fatal("the start of a destination moved")
	}
	if _, ok := s.CursorOf("lago"); ok {
		t.Fatal("CursorOf created a cursor")
	}
	if got := strings.Join(s.KnownDestinations(), ","); got != "metronome,stripe" {
		t.Fatalf("known destinations %q", got)
	}
	s.Close()

	s = open(t, dir, c, Options{})
	defer s.Close()
	if st := s.DestinationState("metronome"); st.Start == nil || *st.Start != want {
		t.Fatalf("start after a restart: %+v", st.Start)
	}
	if st := s.DestinationState("stripe"); st.Start != nil {
		t.Fatalf("an old cursor got a start: %+v", st.Start)
	}
}

// An operator's positioning is recorded as the destination's start, and a
// delivery that read the cursor before it can no longer acknowledge: its
// acknowledgement would undo the positioning.
func TestPositioningIsRecordedAndFencesEarlierDeliveries(t *testing.T) {
	c := &clock{t: t0}
	s := open(t, t.TempDir(), c, Options{})
	defer s.Close()
	s.Cursor("lago")
	appendWindows(t, s, t0, 3, 2) // 9 records
	cur, gen, _ := s.Delivery("lago")
	if cur != 0 {
		t.Fatalf("cursor %d", cur)
	}
	if mine, err := s.AckAt("lago", 3, gen); err != nil || !mine || s.Cursor("lago") != 3 {
		t.Fatalf("ack of an undisturbed delivery: %v %v, cursor %d", mine, err, s.Cursor("lago"))
	}

	_, gen, _ = s.Delivery("lago") // a delivery of records 4 to 9 begins
	c.Add(time.Minute)
	if err := s.SetCursor("lago", 1); err != nil { // the operator rewinds meanwhile
		t.Fatal(err)
	}
	if mine, err := s.AckAt("lago", 9, gen); err != nil || mine {
		t.Fatalf("a delivery that began before the rewind acknowledged: %v %v", mine, err)
	}
	if s.Cursor("lago") != 1 {
		t.Fatalf("the rewind was undone: cursor %d", s.Cursor("lago"))
	}
	if st := s.DestinationState("lago").Start; st == nil || st.Seq != 1 || st.How != StartPositioned || !st.At.Equal(t0.Add(time.Minute)) {
		t.Fatalf("start after positioning: %+v", st)
	}
	cur, gen, _ = s.Delivery("lago")
	if mine, err := s.AckAt("lago", 9, gen); err != nil || !mine || cur != 1 || s.Cursor("lago") != 9 {
		t.Fatalf("ack of a delivery that began after the rewind: %v %v", mine, err)
	}
	// A position past the head is the head.
	if err := s.SetCursor("lago", 100); err != nil || s.Cursor("lago") != 9 || s.DestinationState("lago").Start.Seq != 9 {
		t.Fatalf("position past the head: cursor %d, %v", s.Cursor("lago"), err)
	}
}

// Pause and detach marks are written before the call returns and survive a
// restart. A detached mark keeps the time and ledger head of the first
// start without the destination.
func TestDestinationMarksSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	s.Cursor("metronome")
	appendWindows(t, s, t0, 1, 2) // 3 records

	if p, err := s.PauseDestination("metronome", "sandbox cleanup"); err != nil || p.Reason != "sandbox cleanup" || !p.At.Equal(t0) {
		t.Fatalf("pause: %+v %v", p, err)
	}
	c.Add(time.Hour)
	if p, _ := s.PauseDestination("metronome", ""); !p.At.Equal(t0) || p.Reason != "sandbox cleanup" {
		t.Fatalf("a second pause replaced the first: %+v", p)
	}
	if fresh, err := s.MarkDetached("metronome"); err != nil || !fresh {
		t.Fatalf("detach: %v %v", fresh, err)
	}
	appendWindows(t, s, t0.Add(time.Minute), 1, 2)
	c.Add(time.Hour)
	if fresh, _ := s.MarkDetached("metronome"); fresh {
		t.Fatal("a destination was detached twice")
	}
	if fresh, err := s.AwaitStart("metronome"); err != nil || !fresh {
		t.Fatalf("await start: %v %v", fresh, err)
	}
	s.Close()

	s = open(t, dir, c, Options{})
	defer s.Close()
	_, _, st := s.Delivery("metronome")
	if st.Paused == nil || st.Paused.Reason != "sandbox cleanup" || !st.Paused.At.Equal(t0) {
		t.Fatalf("pause after a restart: %+v", st.Paused)
	}
	d := st.Detached
	if d == nil || !d.At.Equal(t0.Add(time.Hour)) || d.Committed != 3 || !d.AwaitingStart || !d.AwaitingSince.Equal(t0.Add(2*time.Hour)) || d.Decided {
		t.Fatalf("detach mark after a restart: %+v", d)
	}
	// Removed again while it awaited its start: the wait ends, the mark stays.
	if fresh, err := s.MarkDetached("metronome"); err != nil || fresh {
		t.Fatalf("detach again: %v %v", fresh, err)
	}
	if d := s.DestinationState("metronome").Detached; d == nil || d.AwaitingStart || !d.At.Equal(t0.Add(time.Hour)) {
		t.Fatalf("detach mark of a destination removed again: %+v", d)
	}
	if err := s.DecideDetached("metronome"); err != nil || !s.DestinationState("metronome").Detached.Decided {
		t.Fatalf("decide: %v", err)
	}
	if was, err := s.ResumeDestination("metronome"); err != nil || was == nil || was.Reason != "sandbox cleanup" {
		t.Fatalf("resume: %+v %v", was, err)
	}
	if was, _ := s.ResumeDestination("metronome"); was != nil {
		t.Fatalf("resumed twice: %+v", was)
	}
	if err := s.ClearDetached("metronome"); err != nil {
		t.Fatal(err)
	}
	if st := s.DestinationState("metronome"); st.Paused != nil || st.Detached != nil || st.Start == nil {
		t.Fatalf("marks after clearing: %+v", st)
	}
	// The file is what the documentation says: an object keyed by destination.
	var file map[string]DestinationState
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, destinationsFile)), &file); err != nil || len(file) != 1 || file["metronome"].Start.How != StartCreated {
		t.Fatalf("%s: %v %+v", destinationsFile, err, file)
	}
}

// When the marks cannot be written (a full volume), a change that lets
// delivery go on does not take effect: the call fails and the destination
// stays held back, as it would be after a restart. A change that stops
// delivery holds for as long as the process runs.
func TestAMarkThatCannotBeWrittenNeverLetsDeliveryGoOn(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	defer s.Close()
	s.Cursor("metronome")
	appendWindows(t, s, t0, 1, 2)
	if _, err := s.PauseDestination("metronome", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDetached("metronome"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AwaitStart("metronome"); err != nil {
		t.Fatal(err)
	}
	// Nothing can replace the marks file from here on.
	block := filepath.Join(dir, destinationsFile+".tmp")
	if err := os.Mkdir(block, 0o750); err != nil {
		t.Fatal(err)
	}
	if was, err := s.ResumeDestination("metronome"); err == nil || was != nil || s.DestinationState("metronome").Paused == nil {
		t.Fatalf("a resume that could not be written took effect: %+v %v", was, err)
	}
	if err := s.ClearDetached("metronome"); err == nil || s.DestinationState("metronome").Detached == nil || !s.DestinationState("metronome").Detached.AwaitingStart {
		t.Fatalf("a start decision that could not be written took effect: %v", err)
	}
	if err := s.DecideDetached("metronome"); err == nil || s.DestinationState("metronome").Detached.Decided {
		t.Fatalf("a decision that could not be written took effect: %v", err)
	}
	// Stopping holds, although the call reports the failure.
	if _, err := s.PauseDestination("lago", "stop"); err == nil || s.DestinationState("lago").Paused == nil {
		t.Fatalf("a pause that could not be written: %v, paused %v", err, s.DestinationState("lago").Paused != nil)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if was, err := s.ResumeDestination("metronome"); err != nil || was == nil || was.Reason != "first" {
		t.Fatalf("resume once the volume has room: %+v %v", was, err)
	}
	if err := s.ClearDetached("metronome"); err != nil || s.DestinationState("metronome").Detached != nil {
		t.Fatalf("clear once the volume has room: %v", err)
	}
}

// A marks file that cannot be read must stop the start: read as empty, it
// would resume a paused destination and let a detached one deliver.
func TestUnreadableDestinationMarksRefuseToOpen(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	s.Cursor("metronome")
	if _, err := s.PauseDestination("metronome", ""); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.WriteFile(filepath.Join(dir, destinationsFile), []byte(`{"metronome": {"paused"`), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{NoSync: true, Now: c.Now}); err == nil || !strings.Contains(err.Error(), destinationsFile) {
		t.Fatalf("opened a ledger whose destination marks cannot be read: %v", err)
	}
}

// AddDeadLetters parks a batch with one write, each event ID once, like
// AddDeadLetter does one at a time.
func TestDeadLettersInABatch(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	a, b, d := ev("a", t0, 1), ev("b", t0, 2), ev("d", t0, 3)
	if err := s.AddDeadLetter("lago", a, "first"); err != nil {
		t.Fatal(err)
	}
	n, err := s.AddDeadLetters("lago", []DeadLetter{{Event: a, Reason: "again"}, {Event: b, Reason: "skipped"}, {Event: b, Reason: "twice"},
		{Event: d, Reason: "skipped", At: t0.Add(-time.Hour)}})
	if err != nil || n != 2 {
		t.Fatalf("added %d (%v), want the 2 new events", n, err)
	}
	if n, err := s.AddDeadLetters("lago", []DeadLetter{{Event: b, Reason: "skipped"}}); err != nil || n != 0 {
		t.Fatalf("added a parked event again: %d %v", n, err)
	}
	check := func(s *Spool) {
		t.Helper()
		dls, err := s.DeadLetters("lago")
		if err != nil || len(dls) != 3 || dls[0].Reason != "first" || dls[1].Event.ID != b.ID || dls[1].Reason != "skipped" || !dls[1].At.Equal(t0) ||
			dls[2].Event.ID != d.ID || !dls[2].At.Equal(t0.Add(-time.Hour)) {
			t.Fatalf("dead letters: %+v %v", dls, err)
		}
	}
	check(s)
	s.Close()
	// A torn last line (a crash mid-write) does not swallow the next batch.
	path := filepath.Join(dir, "deadletter-lago.log")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"at":"2026-10-05T10:00:00Z","reason":"torn`)
	f.Close()
	s = open(t, dir, c, Options{})
	defer s.Close()
	check(s)
	e := ev("e", t0, 4)
	if n, err := s.AddDeadLetters("lago", []DeadLetter{{Event: e, Reason: "after the torn line"}}); err != nil || n != 1 {
		t.Fatalf("add after a torn line: %d %v", n, err)
	}
	if dls, _ := s.DeadLetters("lago"); len(dls) != 4 || dls[3].Event.ID != e.ID {
		t.Fatalf("dead letters after a torn line: %+v", dls)
	}
	if err := s.RemoveDeadLetters("lago", []string{b.ID}); err != nil {
		t.Fatal(err)
	}
	if dls, _ := s.DeadLetters("lago"); len(dls) != 3 {
		t.Fatalf("after removing one: %+v", dls)
	}
}

// ScanAllSeq visits what ScanAll visits, in the same order, with each
// event's sequence number, through the archive too.
func TestScanAllSeqAgreesWithScanAllAndNumbersEveryEvent(t *testing.T) {
	dir, arch := compactedLedger(t) // seq 1-9 archived, 10-18 in the ledger
	c := &clock{t: t0.Add(3 * time.Hour)}
	s := open(t, dir, c, archiveOpts(arch))
	defer s.Close()
	for _, r := range [][2]time.Time{{t0, t0.Add(time.Hour)}, {t0.Add(2 * time.Minute), t0.Add(5 * time.Minute)}, {t0.Add(4 * time.Minute), t0.Add(time.Hour)}} {
		var want []string
		if err := s.ScanAll(r[0], r[1], func(e *usage.Event) error { want = append(want, e.ID); return nil }); err != nil {
			t.Fatal(err)
		}
		var got []string
		var last uint64
		err := s.ScanAllSeq(r[0], r[1], func(e *usage.Event, seq uint64) error {
			got = append(got, e.ID)
			if seq <= last {
				t.Fatalf("sequence numbers out of order: %d after %d", seq, last)
			}
			last = seq
			// Each window is three records: two events, then their commit.
			window := uint64(e.WindowStart.Sub(t0)/time.Minute) + 1
			if seq != 3*window-2 && seq != 3*window-1 {
				t.Fatalf("event of window %d has seq %d", window, seq)
			}
			return nil
		})
		if err != nil || len(got) == 0 || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("ScanAllSeq %v to %v: %v\n got %v\nwant %v", r[0], r[1], err, got, want)
		}
	}
	// A missing archived segment fails the scan: it never shortens it.
	if err := os.Remove(filepath.Join(arch, "seg-00000000000000000004.log.gz")); err != nil {
		t.Fatal(err)
	}
	if err := s.ScanAllSeq(t0, t0.Add(time.Hour), func(*usage.Event, uint64) error { return nil }); err == nil {
		t.Fatal("ScanAllSeq read past a missing archived segment")
	}
}
