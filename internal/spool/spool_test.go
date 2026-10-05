package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

var t0 = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func ev(tenant string, ws time.Time, q float64) usage.Event {
	e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: q, WindowStart: ws, WindowEnd: ws.Add(time.Minute)}
	e.Finalize()
	return e
}

func open(t *testing.T, dir string, c *clock, opts Options) *Spool {
	t.Helper()
	opts.NoSync = true
	opts.Now = c.Now
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func appendWindows(t *testing.T, s *Spool, from time.Time, n, perWindow int) {
	t.Helper()
	for w := 0; w < n; w++ {
		ws := from.Add(time.Duration(w) * time.Minute)
		var events []usage.Event
		for i := 0; i < perWindow; i++ {
			events = append(events, ev(fmt.Sprintf("tenant-%d", i), ws, 0.1))
		}
		if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), events, ""); err != nil {
			t.Fatalf("append window %d: %v", w, err)
		}
	}
}

func TestAppendReadAckReopen(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	if got := s.Cursor("stripe"); got != 0 {
		t.Fatalf("new cursor on empty spool = %d", got)
	}
	appendWindows(t, s, t0, 3, 2) // 3 windows x (2 events + 1 commit) = 9 records

	recs, err := s.Read(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 9 || recs[0].Seq != 1 || recs[8].Seq != 9 {
		t.Fatalf("read %d records, first=%d", len(recs), recs[0].Seq)
	}
	if recs[2].Type != TypeCommit || recs[2].Commit.Events != 2 {
		t.Fatalf("expected commit record, got %+v", recs[2])
	}
	if err := s.Ack("stripe", 6); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir, c, Options{})
	defer s.Close()
	if s.Committed() != 9 {
		t.Fatalf("committed after reopen = %d", s.Committed())
	}
	if s.Cursor("stripe") != 6 {
		t.Fatalf("cursor after reopen = %d", s.Cursor("stripe"))
	}
	wm, ok := s.Watermark(SourceCollector)
	if !ok || !wm.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("watermark after reopen = %s", wm)
	}
	recs, _ = s.Read(6, 100)
	if len(recs) != 3 || recs[0].Seq != 7 {
		t.Fatalf("read after cursor: %d records", len(recs))
	}
	// A destination added later starts at the head: no surprise replay.
	if s.Cursor("webhook") != 9 {
		t.Fatalf("new destination cursor = %d, want head", s.Cursor("webhook"))
	}
	if rep := s.Verify(); !rep.OK || rep.Records != 9 {
		t.Fatalf("verify: %+v", rep)
	}
}

func TestWindowCannotBeCommittedTwice(t *testing.T) {
	c := &clock{t: t0}
	s := open(t, t.TempDir(), c, Options{})
	defer s.Close()
	appendWindows(t, s, t0, 1, 1)
	err := s.AppendWindow(SourceCollector, t0.Add(time.Minute), []usage.Event{ev("a", t0, 1)}, "")
	if err == nil || !strings.Contains(err.Error(), "already committed") {
		t.Fatalf("expected ErrWindowCommitted, got %v", err)
	}
	// Other sources keep independent watermarks.
	if err := s.AppendWindow("slurm", t0.Add(time.Minute), []usage.Event{ev("a", t0, 1)}, ""); err != nil {
		t.Fatalf("independent source rejected: %v", err)
	}
}

func activeSegment(t *testing.T, dir string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(matches) == 0 {
		t.Fatal("no segments")
	}
	return matches[len(matches)-1]
}

func TestRecoveryTruncatesTornAndUncommittedTail(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	appendWindows(t, s, t0, 2, 2) // 6 records, committed
	s.Close()

	// Simulate a crash mid-batch: two complete event lines with valid hashes
	// and no commit, followed by a torn line.
	path := activeSegment(t, dir)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	prev := s.head
	for seq := 7; seq <= 8; seq++ {
		body := fmt.Sprintf(`{"seq":%d,"type":"event","at":"2026-10-05T10:03:00Z","prev":"%s","event":{"tenant":"x"}}`, seq, prev)
		sum := sha256.Sum256([]byte(body))
		prev = hex.EncodeToString(sum[:])
		fmt.Fprintf(f, "%s\t%s\n", prev, body)
	}
	f.WriteString(`deadbeef	{"seq":9,"ty`)
	f.Close()

	s = open(t, dir, c, Options{})
	defer s.Close()
	if s.Committed() != 6 {
		t.Fatalf("committed after recovery = %d, want 6", s.Committed())
	}
	recs, _ := s.Read(0, 100)
	if len(recs) != 6 {
		t.Fatalf("recovered %d records, want 6", len(recs))
	}
	// The truncated window can be collected again and the chain stays intact.
	appendWindows(t, s, t0.Add(2*time.Minute), 1, 1)
	if rep := s.Verify(); !rep.OK || rep.Records != 8 {
		t.Fatalf("verify after recovery: %+v", rep)
	}
}

func TestCorruptCommittedLineRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	appendWindows(t, s, t0, 1, 2)
	s.Close()

	path := activeSegment(t, dir)
	b, _ := os.ReadFile(path)
	tampered := strings.Replace(string(b), `"quantity":0.1`, `"quantity":0.2`, 1)
	os.WriteFile(path, []byte(tampered), 0o640)

	if _, err := Open(dir, Options{NoSync: true, Now: c.Now}); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corruption error, got %v", err)
	}
}

func TestVerifyDetectsRehashedTampering(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	appendWindows(t, s, t0, 2, 2)
	s.Close()

	// Rewrite one record and recompute its own hash: the line now looks
	// valid on its own, but the next record no longer links to it.
	path := activeSegment(t, dir)
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	_, body, _ := strings.Cut(lines[1], "\t")
	body = strings.Replace(body, `"quantity":0.1`, `"quantity":9.9`, 1)
	sum := sha256.Sum256([]byte(body))
	lines[1] = hex.EncodeToString(sum[:]) + "\t" + body
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o640)

	s = open(t, dir, c, Options{})
	defer s.Close()
	rep := s.Verify()
	if rep.OK || len(rep.Errors) == 0 || !strings.Contains(rep.Errors[0], "chain break at seq 3") {
		t.Fatalf("tampering not detected: %+v", rep)
	}
}

func TestRotationReadAcrossSegmentsAndScan(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{SegmentMaxRecords: 7})
	for w := 0; w < 10; w++ { // 30 records; rotation only at batch boundaries
		c.Add(time.Minute) // append each window right after it closes
		appendWindows(t, s, t0.Add(time.Duration(w)*time.Minute), 1, 2)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if len(segs) < 3 {
		t.Fatalf("expected rotation, got %d segments", len(segs))
	}

	var seqs []uint64
	after := uint64(0)
	for {
		recs, err := s.Read(after, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 0 {
			break
		}
		for _, r := range recs {
			seqs = append(seqs, r.Seq)
		}
		after = recs[len(recs)-1].Seq
	}
	if len(seqs) != 30 {
		t.Fatalf("paged read returned %d records", len(seqs))
	}
	for i, q := range seqs {
		if q != uint64(i+1) {
			t.Fatalf("sequence gap at %d: %d", i, q)
		}
	}

	var n int
	err := s.Scan(t0.Add(3*time.Minute), t0.Add(5*time.Minute), func(e *usage.Event) error { n++; return nil })
	if err != nil || n != 4 {
		t.Fatalf("scan windows 3-4: n=%d err=%v", n, err)
	}
	s.Close()

	// Closed segments come back from the manifest; reads still work.
	s = open(t, dir, c, Options{SegmentMaxRecords: 7})
	defer s.Close()
	recs, _ := s.Read(10, 5)
	if len(recs) != 5 || recs[0].Seq != 11 {
		t.Fatalf("read after reopen: %d records starting %d", len(recs), recs[0].Seq)
	}
	if rep := s.Verify(); !rep.OK || rep.Records != 30 {
		t.Fatalf("verify: %+v", rep)
	}
}

func TestCompactHonorsRetentionAndCursors(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{SegmentMaxRecords: 3, Retention: time.Hour})
	defer s.Close()
	s.Cursor("metronome")
	appendWindows(t, s, t0, 6, 2) // each window is its own segment

	c.Add(2 * time.Hour)
	if n, _ := s.Compact([]string{"metronome"}); n != 0 {
		t.Fatalf("compacted %d segments that metronome has not consumed", n)
	}
	s.Ack("metronome", 9) // consumed the first three windows
	n, err := s.Compact([]string{"metronome"})
	if err != nil || n != 3 {
		t.Fatalf("compact removed %d (err=%v), want 3", n, err)
	}
	if s.FirstSeq() != 10 {
		t.Fatalf("first retained seq = %d", s.FirstSeq())
	}
	if rep := s.Verify(); !rep.OK || rep.FirstSeq != 10 {
		t.Fatalf("verify after compaction: %+v", rep)
	}
	// Reads below the retained range simply start at what is left.
	recs, _ := s.Read(0, 1)
	if len(recs) != 1 || recs[0].Seq != 10 {
		t.Fatalf("read after compaction: %+v", recs)
	}
}

func TestIngestDedupeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{DedupeWindow: time.Hour})
	a, b := ev("t1", t0, 1), ev("t2", t0, 1)
	acc, dup, err := s.AppendIngest("ingest:gateway", []usage.Event{a, b, a})
	if err != nil || len(acc) != 2 || len(dup) != 1 {
		t.Fatalf("first ingest acc=%v dup=%v err=%v", acc, dup, err)
	}
	s.Close()

	s = open(t, dir, c, Options{DedupeWindow: time.Hour})
	acc, dup, _ = s.AppendIngest("ingest:gateway", []usage.Event{a})
	if len(acc) != 0 || len(dup) != 1 {
		t.Fatalf("replayed ID accepted after restart: acc=%v", acc)
	}
	s.Close()

	c.Add(2 * time.Hour) // past the dedupe window
	s = open(t, dir, c, Options{DedupeWindow: time.Hour})
	defer s.Close()
	acc, _, _ = s.AppendIngest("ingest:gateway", []usage.Event{a})
	if len(acc) != 1 {
		t.Fatal("ID outside dedupe window should be accepted")
	}
}

func TestConcurrentAppendAndRead(t *testing.T) {
	c := &clock{t: t0}
	s := open(t, t.TempDir(), c, Options{SegmentMaxRecords: 50})
	defer s.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		appendWindows(t, s, t0, 100, 3)
	}()
	go func() {
		defer wg.Done()
		after := uint64(0)
		for after < 400 {
			recs, err := s.Read(after, 17)
			if err != nil {
				t.Error(err)
				return
			}
			for _, r := range recs {
				if r.Seq != after+1 {
					t.Errorf("out of order: %d after %d", r.Seq, after)
					return
				}
				after = r.Seq
			}
			if len(recs) == 0 {
				<-s.Changed()
			}
		}
	}()
	wg.Wait()
}

func TestDeadLetters(t *testing.T) {
	c := &clock{t: t0}
	s := open(t, t.TempDir(), c, Options{})
	defer s.Close()
	if err := s.AddDeadLetter("stripe", ev("t", t0, 1), "customer not found"); err != nil {
		t.Fatal(err)
	}
	dls, _ := s.DeadLetters("stripe")
	if len(dls) != 1 || dls[0].Reason != "customer not found" {
		t.Fatalf("dead letters: %+v", dls)
	}
	s.ClearDeadLetters("stripe")
	if dls, _ := s.DeadLetters("stripe"); len(dls) != 0 {
		t.Fatal("dead letters not cleared")
	}
}

func TestScanPrefilterMatchesFullDecode(t *testing.T) {
	c := &clock{t: t0}
	s := open(t, t.TempDir(), c, Options{SegmentMaxRecords: 10})
	defer s.Close()
	for w := 0; w < 30; w++ {
		c.Add(time.Minute)
		appendWindows(t, s, t0.Add(time.Duration(w)*time.Minute), 1, 3)
	}
	count := func(from, to time.Time) int {
		n := 0
		if err := s.Scan(from, to, func(e *usage.Event) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := count(t0, t0.Add(30*time.Minute)); got != 90 {
		t.Fatalf("full range: %d", got)
	}
	if got := count(t0.Add(10*time.Minute), t0.Add(12*time.Minute)); got != 6 {
		t.Fatalf("two windows: %d", got)
	}
	if got := count(t0.Add(time.Hour), t0.Add(2*time.Hour)); got != 0 {
		t.Fatalf("empty range: %d", got)
	}
	ws, we, ok := peekWindow([]byte(`aaaa\t{"seq":1,"event":{"window_start":"2026-10-05T10:00:00Z","window_end":"2026-10-05T10:01:00Z"}}`))
	if !ok || !ws.Equal(t0) || !we.Equal(t0.Add(time.Minute)) {
		t.Fatalf("peekWindow: %v %v %v", ws, we, ok)
	}
}
