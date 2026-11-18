package spool

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

const consumer = "stripe"

// sixWindows appends six one-minute windows of two events, each appended as
// its window closes. With SegmentMaxRecords 3 every window is its own
// segment: seq 1-3, 4-6, ... 16-18.
func sixWindows(t *testing.T, s *Spool, c *clock) {
	t.Helper()
	for w := 0; w < 6; w++ {
		c.Add(time.Minute)
		ws := t0.Add(time.Duration(w) * time.Minute)
		events := []usage.Event{ev(fmt.Sprintf("tenant-%d", w), ws, 0.1), ev("shared", ws, float64(w+1))}
		if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), events, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func archiveOpts(arch string) Options {
	return Options{SegmentMaxRecords: 3, Retention: time.Hour, ArchiveDir: arch}
}

// compactedLedger archives and deletes the first three of six segments:
// seq 1-9 are in the archive, 10-18 in the ledger.
func compactedLedger(t *testing.T) (dir, arch string) {
	t.Helper()
	dir, arch = t.TempDir(), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	s := open(t, dir, c, archiveOpts(arch))
	s.Cursor(consumer)
	sixWindows(t, s, c)
	c.Add(2 * time.Hour)
	s.Ack(consumer, 9)
	if n, err := s.Compact([]string{consumer}); n != 3 || err != nil {
		t.Fatalf("compact: removed %d, err %v", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, arch
}

func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	restoreDir(t, src, dst)
	return dst
}

// restoreDir makes dst hold exactly the files of src.
func restoreDir(t *testing.T, src, dst string) {
	t.Helper()
	old, _ := os.ReadDir(dst)
	for _, e := range old {
		os.Remove(filepath.Join(dst, e.Name()))
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

func fileSums(t *testing.T, dir, pattern string) map[string]string {
	t.Helper()
	out := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		out[filepath.Base(m)] = hex.EncodeToString(sum[:])
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasError(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

func TestCompactArchivesBeforeDeleting(t *testing.T) {
	data := t.TempDir()
	dir, arch := filepath.Join(data, "ledger"), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	// Real fsyncs: archiving fsyncs the file, the index and the directory.
	opts := archiveOpts(arch)
	opts.Now = c.Now
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	s.Cursor(consumer)
	sixWindows(t, s, c)
	live := map[string][]byte{}
	for _, first := range []uint64{1, 4, 7} {
		live[segName(first)] = mustRead(t, filepath.Join(dir, segName(first)))
	}

	c.Add(2 * time.Hour)
	if n, err := s.Compact([]string{consumer}); n != 0 || err != nil {
		t.Fatalf("compacted %d (err %v) before the destination consumed anything", n, err)
	}
	if _, err := os.Stat(filepath.Join(arch, ArchiveIndexFile)); !os.IsNotExist(err) {
		t.Fatal("archived segments that were not going to be deleted")
	}
	s.Ack(consumer, 9)
	n, err := s.Compact([]string{consumer})
	if err != nil || n != 3 {
		t.Fatalf("compact removed %d (err %v), want 3", n, err)
	}

	entries, skipped, err := ReadArchive(arch)
	if err != nil || len(skipped) > 0 || len(entries) != 3 {
		t.Fatalf("archive index: %d entries, skipped %v, err %v", len(entries), skipped, err)
	}
	prev := ""
	for i, e := range entries {
		first := uint64(3*i + 1)
		appended := t0.Add(time.Duration(i+1) * time.Minute)
		if e.Name != segName(first) || e.First != first || e.Last != first+2 || e.Prev != prev || len(e.LastHash) != 64 ||
			!e.FirstAt.Equal(appended) || !e.LastAt.Equal(appended) || !e.ArchivedAt.Equal(c.Now()) {
			t.Fatalf("entry %d: %+v", i, e)
		}
		gz := mustRead(t, filepath.Join(arch, e.File()))
		sum := sha256.Sum256(gz)
		if hex.EncodeToString(sum[:]) != e.GzSHA256 || int64(len(gz)) != e.GzBytes {
			t.Fatalf("%s: sha256 or size differs from its entry", e.File())
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(raw, live[e.Name]) {
			t.Fatalf("%s is not the segment's exact bytes (err %v)", e.File(), err)
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still in the ledger", e.Name)
		}
		prev = e.LastHash
	}
	// archive.jsonl lines carry exactly the documented fields.
	var line map[string]any
	if err := json.Unmarshal(bytes.SplitN(mustRead(t, filepath.Join(arch, ArchiveIndexFile)), []byte("\n"), 2)[0], &line); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range line {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, " "); got != "archived_at first first_at gz_bytes gz_sha256 last last_at last_hash name prev" {
		t.Fatalf("archive.jsonl fields: %s", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(arch, "*.tmp")); len(matches) > 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}

	// The anchor moved to the last archived hash, on disk too.
	s.Close()
	s = open(t, dir, c, archiveOpts(arch))
	if s.anchor != prev || s.FirstSeq() != 10 {
		t.Fatalf("after reopen: anchor %q first seq %d, want %q and 10", s.anchor, s.FirstSeq(), prev)
	}
	if rep := s.Verify(); !rep.OK || rep.FirstSeq != 10 || rep.Records != 9 {
		t.Fatalf("live verify: %+v", rep)
	}

	// Archive and ledger verify as one chain from the first record, offline
	// (the ledger dir or DATA_DIR above it) and in process.
	inProcess, err := s.VerifyWithArchive()
	if err != nil {
		t.Fatal(err)
	}
	for name, rep := range map[string]ChainReport{
		"offline":    VerifyChain(dir, arch),
		"DATA_DIR":   VerifyChain(data, arch),
		"in process": inProcess,
	} {
		if !rep.OK || rep.Records != 18 || rep.FirstSeq != 1 || rep.LastSeq != 18 || rep.Anchor != "" || rep.Head != s.head ||
			rep.ArchivedSegments != 3 || rep.LiveSegments != 3 || len(rep.Warnings) > 0 ||
			rep.FirstAt == nil || !rep.FirstAt.Equal(t0.Add(time.Minute)) || !rep.LastAt.Equal(t0.Add(6*time.Minute)) {
			t.Fatalf("%s: %+v", name, rep)
		}
	}
	// Either half verifies on its own, starting or ending at the boundary.
	if rep := VerifyChain(dir, ""); !rep.OK || rep.Records != 9 || rep.Anchor != prev {
		t.Fatalf("ledger alone: %+v", rep)
	}
	if rep := VerifyChain("", arch); !rep.OK || rep.Records != 9 || rep.Head != prev || rep.LiveSegments != 0 {
		t.Fatalf("archive alone: %+v", rep)
	}
}

func TestCompactArchiveRetriesAndIsIdempotent(t *testing.T) {
	dir, arch := t.TempDir(), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	s := open(t, dir, c, archiveOpts(arch))
	s.Cursor(consumer)
	sixWindows(t, s, c)
	c.Add(2 * time.Hour)
	s.Ack(consumer, 9)

	// The archive cannot be written: nothing is deleted and the error says so.
	if err := os.WriteFile(arch, []byte("a file, not a directory"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Compact([]string{consumer}); n != 0 || err == nil {
		t.Fatalf("compact with a broken archive removed %d, err %v", n, err)
	}
	if s.FirstSeq() != 1 || s.Committed() != 18 {
		t.Fatalf("segments lost when archiving failed: first seq %d", s.FirstSeq())
	}
	os.Remove(arch)
	s.Close()
	before := copyDir(t, dir) // the ledger just before archiving

	// The next compaction retries and succeeds.
	s = open(t, dir, c, archiveOpts(arch))
	if n, err := s.Compact([]string{consumer}); n != 3 || err != nil {
		t.Fatalf("retry removed %d, err %v", n, err)
	}
	s.Close()
	index := mustRead(t, filepath.Join(arch, ArchiveIndexFile))
	gzSums := fileSums(t, arch, "*.gz")

	// A crash after archiving, before deleting: the ledger still holds the
	// archived segments. Compaction deletes them without archiving twice.
	restoreDir(t, before, dir)
	s = open(t, dir, c, archiveOpts(arch))
	if n, err := s.Compact([]string{consumer}); n != 3 || err != nil {
		t.Fatalf("compaction after a crash removed %d, err %v", n, err)
	}
	s.Close()
	if !bytes.Equal(mustRead(t, filepath.Join(arch, ArchiveIndexFile)), index) {
		t.Fatal("archive.jsonl changed when compaction repeated after a crash")
	}
	if !reflect.DeepEqual(fileSums(t, arch, "*.gz"), gzSums) {
		t.Fatal("archived files changed when compaction repeated after a crash")
	}
	if rep := VerifyChain(dir, arch); !rep.OK || rep.Records != 18 {
		t.Fatalf("verify after the repeated compaction: %+v", rep)
	}

	// A crash after renaming the last .gz into place, with its index line
	// torn halfway: the file is replaced by the same bytes, and the new entry
	// starts on its own line.
	restoreDir(t, before, dir)
	lines := strings.SplitAfter(string(index), "\n")
	torn := lines[0] + lines[1] + lines[2][:40]
	if err := os.WriteFile(filepath.Join(arch, ArchiveIndexFile), []byte(torn), 0o640); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir, c, archiveOpts(arch))
	if n, err := s.Compact([]string{consumer}); n != 3 || err != nil {
		t.Fatalf("compaction after a torn index line removed %d, err %v", n, err)
	}
	s.Close()
	entries, skipped, err := ReadArchive(arch)
	if err != nil || len(entries) != 3 || len(skipped) != 1 {
		t.Fatalf("index after a torn line: %d entries, skipped %v, err %v", len(entries), skipped, err)
	}
	if !reflect.DeepEqual(fileSums(t, arch, "*.gz"), gzSums) {
		t.Fatal("re-archiving the same segment did not produce the same file")
	}
	if rep := VerifyChain(dir, arch); !rep.OK || rep.Records != 18 || len(rep.Warnings) != 1 {
		t.Fatalf("verify with a torn index line: %+v", rep)
	}
}

// Archiving runs without the spool lock: appends, acks, compactions and
// in-process verifications interleave freely and the chain stays whole.
func TestArchivingCompactionAlongsideAppends(t *testing.T) {
	dir, arch := t.TempDir(), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	s := open(t, dir, c, Options{SegmentMaxRecords: 9, Retention: time.Minute, ArchiveDir: arch})
	defer s.Close()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(done)
		for w := 0; w < 200; w++ {
			c.Add(time.Minute)
			ws := t0.Add(time.Duration(w) * time.Minute)
			if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), []usage.Event{ev("a", ws, 1), ev("b", ws, 2)}, ""); err != nil {
				t.Error(err)
				return
			}
			s.Ack(consumer, s.Committed())
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, err := s.Compact([]string{consumer}); err != nil {
				t.Error(err)
				return
			}
			if rep, err := s.VerifyWithArchive(); err != nil || !rep.OK {
				t.Errorf("verify while appending: %+v (err %v)", rep, err)
				return
			}
		}
	}()
	wg.Wait()
	c.Add(time.Hour)
	if _, err := s.Compact([]string{consumer}); err != nil {
		t.Fatal(err)
	}
	if rep := VerifyChain(dir, arch); !rep.OK || rep.Records != 600 || rep.FirstSeq != 1 || rep.ArchivedSegments < 60 {
		t.Fatalf("after concurrent archiving: %+v", rep)
	}
}

func TestArchiveRefusesAnotherLedgersSegments(t *testing.T) {
	_, arch := compactedLedger(t)
	index := mustRead(t, filepath.Join(arch, ArchiveIndexFile))
	gzSums := fileSums(t, arch, "*.gz")

	// A second ledger in the same archive (say its volume was recreated)
	// numbers segments from 1 again: its seg-1 must not replace the first's.
	c := &clock{t: t0.Add(24 * time.Hour)}
	s := open(t, t.TempDir(), c, archiveOpts(arch))
	defer s.Close()
	s.Cursor(consumer)
	sixWindows(t, s, c)
	c.Add(2 * time.Hour)
	s.Ack(consumer, 9)
	n, err := s.Compact([]string{consumer})
	if n != 0 || err == nil || !strings.Contains(err.Error(), "give each ledger its own ARCHIVE_DIR") {
		t.Fatalf("compaction into another ledger's archive: removed %d, err %v", n, err)
	}
	if s.FirstSeq() != 1 {
		t.Fatal("segments deleted although they were not archived")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(arch, ArchiveIndexFile)), index) || !reflect.DeepEqual(fileSums(t, arch, "*.gz"), gzSums) {
		t.Fatal("the other ledger's archive was modified")
	}
}

// gunzipLines returns an archived segment's lines without their newlines.
func gunzipLines(t *testing.T, path string) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(mustRead(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// gzipLines writes lines as an archived segment and returns its sha256 and size.
func gzipLines(t *testing.T, path string, lines []string) (string, int64) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(strings.Join(lines, "\n") + "\n"))
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), int64(buf.Len())
}

// relink re-chains lines from prev, as a forger would, applying edit first.
func relink(t *testing.T, lines []string, prev string, edit func(*Record)) ([]string, string) {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		_, body, _ := strings.Cut(l, "\t")
		var r Record
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(&r)
		}
		r.Prev = prev
		b, err := json.Marshal(&r)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		prev = hex.EncodeToString(sum[:])
		out[i] = prev + "\t" + string(b)
	}
	return out, prev
}

// rewriteIndex rewrites archive.jsonl; edit returns false to drop an entry.
func rewriteIndex(t *testing.T, arch string, edit func(*ArchiveEntry) bool) {
	t.Helper()
	entries, _, err := ReadArchive(arch)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for i := range entries {
		if !edit(&entries[i]) {
			continue
		}
		b, _ := json.Marshal(entries[i])
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(arch, ArchiveIndexFile), buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyChainDetectsArchiveTampering(t *testing.T) {
	dir, arch := compactedLedger(t)
	seg4, seg7 := segName(4), segName(7)
	inflate := func(r *Record) {
		if r.Seq == 5 {
			r.Event.Quantity = 100 // the second event of window 1
		}
	}
	for name, tc := range map[string]struct {
		tamper func(t *testing.T, a string)
		want   string
	}{
		"a byte changed inside an archived segment": {func(t *testing.T, a string) {
			path := filepath.Join(a, seg4+".gz")
			b := mustRead(t, path)
			b[len(b)/2] ^= 0x20
			os.WriteFile(path, b, 0o640)
		}, seg4 + ".gz: sha256 does not match"},

		"a record edited, its segment re-hashed and re-recorded": {func(t *testing.T, a string) {
			e := archiveEntry(t, a, seg4)
			lines, last := relink(t, gunzipLines(t, filepath.Join(a, e.File())), e.Prev, inflate)
			sum, size := gzipLines(t, filepath.Join(a, e.File()), lines)
			rewriteIndex(t, a, func(x *ArchiveEntry) bool {
				if x.Name == seg4 {
					x.GzSHA256, x.GzBytes, x.LastHash = sum, size, last
				}
				return true
			})
		}, seg7 + ": chain break at seq 7"},

		"the whole archive re-hashed after an edit": {func(t *testing.T, a string) {
			prev := archiveEntry(t, a, seg4).Prev
			fixed := map[string]ArchiveEntry{}
			for _, name := range []string{seg4, seg7} {
				e := archiveEntry(t, a, name)
				var lines []string
				lines, e.LastHash = relink(t, gunzipLines(t, filepath.Join(a, e.File())), prev, inflate)
				e.Prev, prev = prev, e.LastHash
				e.GzSHA256, e.GzBytes = gzipLines(t, filepath.Join(a, e.File()), lines)
				fixed[name] = e
			}
			rewriteIndex(t, a, func(x *ArchiveEntry) bool {
				if e, ok := fixed[x.Name]; ok {
					*x = e
				}
				return true
			})
		}, "live ledger anchor"},

		"an archived file deleted": {func(t *testing.T, a string) {
			os.Remove(filepath.Join(a, seg4+".gz"))
		}, seg4 + ".gz: archived file is missing"},

		"an archive entry deleted": {func(t *testing.T, a string) {
			rewriteIndex(t, a, func(x *ArchiveEntry) bool { return x.Name != seg4 })
		}, seg7 + ": expected seq 4, found 7"},

		"the newest archived segment deleted": {func(t *testing.T, a string) {
			rewriteIndex(t, a, func(x *ArchiveEntry) bool { return x.Name != seg7 })
			os.Remove(filepath.Join(a, seg7+".gz"))
		}, "the live ledger starts at seq 10, which does not follow an archived segment"},
	} {
		t.Run(name, func(t *testing.T) {
			a := copyDir(t, arch)
			if rep := VerifyChain(dir, a); !rep.OK {
				t.Fatalf("untouched copy fails: %+v", rep)
			}
			tc.tamper(t, a)
			rep := VerifyChain(dir, a)
			if rep.OK || !hasError(rep.Errors, tc.want) {
				t.Fatalf("want an error containing %q, got %+v", tc.want, rep)
			}
		})
	}
}

// archiveEntry returns the entry for one segment.
func archiveEntry(t *testing.T, arch, name string) ArchiveEntry {
	t.Helper()
	entries, _, err := ReadArchive(arch)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry for %s", name)
	return ArchiveEntry{}
}

func TestScanLedgerAcrossArchiveBoundary(t *testing.T) {
	dir, arch := compactedLedger(t)
	scan := func(dir, arch string, from, to time.Time) ([]string, ScanSummary, error) {
		var got []string
		sum, err := ScanLedger(dir, arch, from, to, func(e *usage.Event) error {
			got = append(got, e.Tenant+"@"+e.WindowStart.Format("15:04"))
			return nil
		})
		return got, sum, err
	}

	// Windows 0-2 are archived, 3-5 live; [10:01, 10:05) spans both.
	got, sum, err := scan(dir, arch, t0.Add(time.Minute), t0.Add(5*time.Minute))
	want := []string{"tenant-1@10:01", "shared@10:01", "tenant-2@10:02", "shared@10:02", "tenant-3@10:03", "shared@10:03", "tenant-4@10:04", "shared@10:04"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("scan across the boundary: %v (err %v), want %v", got, err, want)
	}
	if sum.FirstSeq != 1 || sum.LastSeq != 18 || sum.Anchor != "" || !sum.FirstAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("summary: %+v", sum)
	}

	// Each half alone holds its own windows; the ledger alone starts past genesis.
	if got, _, err := scan("", arch, t0, t0.Add(time.Hour)); err != nil || len(got) != 6 || got[0] != "tenant-0@10:00" {
		t.Fatalf("archive alone: %v (err %v)", got, err)
	}
	got, sum, err = scan(dir, "", t0, t0.Add(time.Hour))
	if err != nil || len(got) != 6 || got[0] != "tenant-3@10:03" || sum.FirstSeq != 10 || sum.Anchor == "" {
		t.Fatalf("ledger alone: %v %+v (err %v)", got, sum, err)
	}

	// Segments written well before the range are skipped without being read.
	a := copyDir(t, arch)
	for _, m := range []string{segName(1), segName(4), segName(7)} {
		os.Remove(filepath.Join(a, m+".gz"))
	}
	if got, _, err := scan(dir, a, t0.Add(3*time.Hour), t0.Add(4*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("later range read old archived files: %v (err %v)", got, err)
	}

	// A missing archived segment fails the export instead of shortening it.
	a = copyDir(t, arch)
	rewriteIndex(t, a, func(x *ArchiveEntry) bool { return x.Name != segName(4) })
	if _, _, err := scan(dir, a, t0, t0.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "records are missing") {
		t.Fatalf("scan with a missing segment: %v", err)
	}
	a = copyDir(t, arch)
	rewriteIndex(t, a, func(x *ArchiveEntry) bool { return x.Name != segName(7) })
	if _, _, err := scan(dir, a, t0, t0.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "expected seq 7, found 10") {
		t.Fatalf("scan with the archive/ledger seam missing: %v", err)
	}
}

// chainedLine builds a valid ledger line for rec after prev.
func chainedLine(t *testing.T, rec Record, prev string) (string, string) {
	t.Helper()
	rec.Prev = prev
	b, err := json.Marshal(&rec)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	return h + "\t" + string(b) + "\n", h
}

func TestOfflineReadsLeaveOutAnUncommittedTail(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0}
	s := open(t, dir, c, Options{})
	appendWindows(t, s, t0, 2, 2) // 6 records, committed
	head := s.head
	s.Close()

	// An append in progress (or a crash): two valid event lines of window 2
	// without their commit, then a torn line.
	f, err := os.OpenFile(activeSegment(t, dir), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(7); seq <= 8; seq++ {
		e := ev(fmt.Sprintf("late-%d", seq), t0.Add(2*time.Minute), 1)
		var line string
		line, head = chainedLine(t, Record{Seq: seq, Type: TypeEvent, At: t0, Event: &e}, head)
		f.WriteString(line)
	}
	f.WriteString(head[:20])
	f.Close()

	rep := VerifyChain(dir, "")
	if !rep.OK || rep.Records != 6 || rep.LastSeq != 6 || len(rep.Warnings) != 2 {
		t.Fatalf("offline verify with an uncommitted tail: %+v", rep)
	}
	n := 0
	if _, err := ScanLedger(dir, "", t0, t0.Add(time.Hour), func(*usage.Event) error { n++; return nil }); err != nil || n != 4 {
		t.Fatalf("export with an uncommitted tail: %d events (err %v), want the 4 committed", n, err)
	}
}

func TestQuickHeaderAgreesWithJSON(t *testing.T) {
	e := ev("t", t0, 1)
	e.Dimensions = map[string]string{"zone": "a"}
	event, _ := json.Marshal(&Record{Seq: 42, Type: TypeEvent, At: t0.Add(time.Second / 3), Prev: strings.Repeat("ab", 32), Event: &e})
	commit, _ := json.Marshal(&Record{Seq: 43, Type: TypeCommit, At: t0, Commit: &Commit{Source: SourceCollector, WindowEnd: t0, Events: 1}})
	e.Dimensions = map[string]string{"type": "x", "prev": "y"} // nested keys named like header fields
	nested, _ := json.Marshal(&Record{Seq: 44, Type: TypeEvent, At: t0, Prev: "p", Event: &e})
	at := `"at":"2026-10-05T10:00:00Z"`
	backslash := string(rune(92))
	for name, tc := range map[string]struct {
		body  string
		quick bool
	}{
		"event":                    {string(event), true},
		"commit at genesis":        {string(commit), true},
		"nested keys like header":  {string(nested), false},
		"top-level prev repeated":  {`{"seq":5,"type":"event",` + at + `,"prev":"a","x":1,"prev":"b"}`, false},
		"leading zero":             {`{"seq":05,"type":"event",` + at + `,"prev":"a"}`, false},
		"escaped string":           {`{"seq":5,"type":"ev` + backslash + `u0065nt",` + at + `,"prev":"a"}`, false},
		"other key order":          {`{"seq":5,` + at + `,"type":"event","prev":"a"}`, false},
		"fractional seq":           {`{"seq":5.0,"type":"event",` + at + `,"prev":"a"}`, false},
		"nothing after prev field": {`{"seq":5,"type":"event",` + at + `,"prev":"a"x}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := quickHeader([]byte(tc.body)); ok != tc.quick {
				t.Fatalf("quickHeader ok = %v, want %v", ok, tc.quick)
			}
			var want recordHeader
			werr := json.Unmarshal([]byte(tc.body), &want)
			sum := sha256.Sum256([]byte(tc.body))
			got, _, gerr := checkLine([]byte(hex.EncodeToString(sum[:]) + "\t" + tc.body + "\n"))
			if (werr == nil) != (gerr == nil) || (werr == nil && (got.Seq != want.Seq || got.Type != want.Type || got.Prev != want.Prev || !got.At.Equal(want.At))) {
				t.Fatalf("checkLine %+v (%v), json.Unmarshal %+v (%v)", got, gerr, want, werr)
			}
		})
	}
}

func TestVerifyWithArchiveNeedsArchiveDir(t *testing.T) {
	s := open(t, t.TempDir(), &clock{t: t0}, Options{})
	defer s.Close()
	if _, err := s.VerifyWithArchive(); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("got %v, want ErrNoArchive", err)
	}
}

// realisticWindow is one minute of collector usage for n clusters: the eight
// metrics a GPU cluster typically reports, with the dimensions and
// properties the collector sets.
func realisticWindow(rng *rand.Rand, ws time.Time, n int) []usage.Event {
	we := ws.Add(time.Minute)
	var out []usage.Event
	for i := 0; i < n; i++ {
		cluster := fmt.Sprintf("vcluster-team-%02d-train-%03d", i%20, i)
		dims := func(extra ...string) map[string]string {
			d := map[string]string{usage.DimTenantCluster: cluster, usage.DimTenantClass: "enterprise",
				usage.DimZone: fmt.Sprintf("us-east-1%c", 'a'+i%3), usage.DimCapacityType: "on-demand", usage.DimBillingMode: "dedicated_node"}
			for j := 0; j+1 < len(extra); j += 2 {
				d[extra[j]] = extra[j+1]
			}
			return d
		}
		add := func(metric, sku string, q float64, d map[string]string, props map[string]any) {
			e := usage.Event{Tenant: fmt.Sprintf("tenant-%03d", i%60), Metric: metric, Quantity: q, WindowStart: ws, WindowEnd: we,
				Region: "us-east-1", Project: fmt.Sprintf("team-%02d", i%20), SKU: sku, ResourceID: cluster, Dimensions: d,
				Properties: props, Source: "collector", RecordedAt: we.Add(time.Duration(rng.Intn(3000)) * time.Millisecond)}
			e.Finalize()
			out = append(out, e)
		}
		gpu := []string{usage.DimGPUType, "NVIDIA-H100-80GB-HBM3", usage.DimGPUProfile, "full"}
		add(usage.MetricCPUCoreHours, "", rng.Float64()*96/60, dims(), map[string]any{"basis": "usage"})
		add(usage.MetricMemoryGBHours, "", rng.Float64()*1024/60, dims(), map[string]any{"basis": "usage"})
		add(usage.MetricGPUHours, "NVIDIA-H100", 8.0/60, dims(gpu...), map[string]any{"gpu_devices": 8, "pods": 1 + rng.Intn(8)})
		add(usage.MetricGPUUtilization, "NVIDIA-H100", rng.Float64(), dims(gpu...), nil)
		add(usage.MetricStorageGBHours, "", float64(100*(1+i%5))/60, dims(), map[string]any{"volumes": 1 + i%3})
		add(usage.MetricNetworkEgressGB, "", rng.Float64()*0.5, dims(), nil)
		add(usage.MetricLBHours, "", 1.0/60, dims(), map[string]any{"load_balancers": 1})
		add(usage.MetricInstanceHours, "p5.48xlarge", 1.0/60, dims(usage.DimNode, fmt.Sprintf("gpu-node-%03d", i), usage.DimInstanceType, "p5.48xlarge"), nil)
	}
	return out
}

// BenchmarkArchiveSegment archives one full segment (20,000+ records) of
// realistic collector usage, 100 GPU clusters at eight events per minute, and
// reports bytes per event in the ledger and in the archive. The gzip-*
// sub-benchmarks compare compression levels on the same bytes, and verify
// measures offline verification of the archived segment.
func BenchmarkArchiveSegment(b *testing.B) {
	c := &clock{t: t0}
	s, err := Open(b.TempDir(), Options{NoSync: true, Now: c.Now})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	rng := rand.New(rand.NewSource(1))
	events := 0
	for w := 0; len(s.segs) < 2; w++ {
		ws := t0.Add(time.Duration(w) * time.Minute)
		c.t = ws.Add(time.Minute + 2*time.Second)
		evs := realisticWindow(rng, ws, 100)
		if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
			b.Fatal(err)
		}
		if len(s.segs) == 1 {
			events += len(evs)
		}
	}
	sg := *s.segs[0]
	raw, err := os.ReadFile(filepath.Join(s.dir, sg.Name))
	if err != nil {
		b.Fatal(err)
	}
	arch := b.TempDir()
	path := filepath.Join(arch, sg.Name+".gz")

	b.Run("archive", func(b *testing.B) {
		var e ArchiveEntry
		b.SetBytes(sg.Size)
		for i := 0; i < b.N; i++ {
			if e, err = s.writeArchiveFile(&sg, path); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(sg.Size)/float64(events), "raw-B/event")
		b.ReportMetric(float64(e.GzBytes)/float64(events), "gz-B/event")
		b.ReportMetric(float64(sg.Size)/float64(e.GzBytes), "x-smaller")
	})
	for _, l := range []struct {
		name  string
		level int
	}{{"gzip-fastest", gzip.BestSpeed}, {"gzip-default", gzip.DefaultCompression}, {"gzip-best", gzip.BestCompression}} {
		b.Run(l.name, func(b *testing.B) {
			var size int
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				var buf bytes.Buffer
				zw, _ := gzip.NewWriterLevel(&buf, l.level)
				zw.Write(raw)
				zw.Close()
				size = buf.Len()
			}
			b.ReportMetric(float64(size)/float64(events), "gz-B/event")
		})
	}
	b.Run("verify", func(b *testing.B) {
		e, err := s.writeArchiveFile(&sg, path)
		if err != nil {
			b.Fatal(err)
		}
		os.Remove(filepath.Join(arch, ArchiveIndexFile))
		if err := appendArchiveIndex(arch, emptyArchiveIndex(), e, true); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(sg.Size)
		for i := 0; i < b.N; i++ {
			if rep := VerifyChain("", arch); !rep.OK || rep.Records != sg.Last {
				b.Fatalf("%+v", rep)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(sg.Last), "ns/record")
	})
}
