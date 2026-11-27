package spool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// fileState is what a file looked like: a file written once keeps all of it.
type fileState struct {
	info os.FileInfo
	sum  string
}

func archiveState(t *testing.T, arch string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	for _, rel := range archiveFiles(t, arch) {
		path := filepath.Join(arch, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(mustRead(t, path))
		out[rel] = fileState{info: info, sum: hex.EncodeToString(sum[:])}
	}
	return out
}

// writtenOnce fails when a file of before is gone, or was renamed into place
// (another inode), appended to, overwritten or touched since.
func writtenOnce(t *testing.T, before, after map[string]fileState) {
	t.Helper()
	for rel, b := range before {
		a, ok := after[rel]
		switch {
		case !ok:
			t.Fatalf("%s was removed", rel)
		case !os.SameFile(a.info, b.info):
			t.Fatalf("%s was replaced (renamed into place)", rel)
		case a.info.Size() != b.info.Size() || a.sum != b.sum:
			t.Fatalf("%s was appended to or overwritten", rel)
		case !a.info.ModTime().Equal(b.info.ModTime()):
			t.Fatalf("%s was written to again", rel)
		}
	}
	for rel := range after {
		if strings.HasSuffix(rel, ".tmp") || strings.Contains(rel, "/.") || strings.HasPrefix(rel, ".") {
			t.Fatalf("temporary file %s", rel)
		}
	}
}

// Every file of the archive is created under its final name and written
// once: compactions, crashes in the middle of a write and the retries after
// them never rename, append to, overwrite or remove a file, and the archive
// reads as one chain all along.
func TestArchiveIsWrittenOnce(t *testing.T) {
	dir, arch := t.TempDir(), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	s := open(t, dir, c, archiveOpts(arch))
	defer func() { s.Close() }()
	s.Cursor(consumer)
	window := 0
	appendWindows := func(n int) {
		for i := 0; i < n; i++ {
			c.Add(time.Minute)
			ws := t0.Add(time.Duration(window) * time.Minute)
			if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), []usage.Event{ev("a", ws, 1), ev("b", ws, 2)}, ""); err != nil {
				t.Fatal(err)
			}
			window++
		}
	}
	compact := func() int {
		t.Helper()
		c.Add(2 * time.Hour)
		if err := s.Ack(consumer, s.Committed()); err != nil {
			t.Fatal(err)
		}
		n, err := s.Compact([]string{consumer})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(records uint64) map[string]fileState {
		t.Helper()
		if rep := VerifyChain(dir, arch); !rep.OK || rep.Records != records || rep.FirstSeq != 1 || rep.Anchor != "" {
			t.Fatalf("verify: %+v", rep)
		}
		return archiveState(t, arch)
	}

	appendWindows(4)
	if n := compact(); n != 3 {
		t.Fatalf("first compaction removed %d", n)
	}
	state := check(12)

	for round, crash := range []func(next uint64){
		// A crash while writing the next segment's data file: a partial copy.
		func(next uint64) {
			os.WriteFile(filepath.Join(arch, archiveCopyName(next, 1, archiveDataExt)), []byte("\x1f\x8b\x08\x00partial"), 0o640)
		},
		// A crash while writing its index entry: a complete data file, a
		// partial entry.
		func(next uint64) {
			os.WriteFile(filepath.Join(arch, archiveCopyName(next, 1, archiveDataExt)), []byte("\x1f\x8b\x08\x00whole"), 0o640)
			os.WriteFile(filepath.Join(arch, ArchiveIndexDir, archiveCopyName(next, 1, archiveIndexExt)), []byte(`{"name":"seg-`), 0o640)
		},
		// The same segment crashes twice: the next free names are taken.
		func(next uint64) {
			for n := 1; n <= 2; n++ {
				os.WriteFile(filepath.Join(arch, archiveCopyName(next, n, archiveDataExt)), []byte("partial"), 0o640)
			}
		},
	} {
		appendWindows(3)
		next := s.FirstSeq()
		crash(next)
		before := archiveState(t, arch)
		writtenOnce(t, state, before)
		if n := compact(); n != 3 {
			t.Fatalf("round %d: compaction removed %d", round, n)
		}
		state = check(uint64(6 * window / 2))
		writtenOnce(t, before, state)
		// The segment the crash interrupted is archived under new names.
		e := archiveEntry(t, arch, segName(next))
		if e.File() == archiveCopyName(next, 1, archiveDataExt) {
			t.Fatalf("round %d: archived into the name a crash had taken: %+v", round, e)
		}
	}
	if _, err := os.Stat(filepath.Join(arch, ArchiveIndexFile)); !os.IsNotExist(err) {
		t.Fatal("archive.jsonl was written")
	}
}

// legacyFixture copies the archive and the ledger v0.3.2 wrote in
// testdata/archive-v0.3.2: seq 1-9 archived with archive.jsonl, 10-18 live.
func legacyFixture(t *testing.T) (dir, arch string) {
	t.Helper()
	dir, arch = filepath.Join(t.TempDir(), "ledger"), filepath.Join(t.TempDir(), "archive")
	for _, d := range [][2]string{{"ledger", dir}, {"archive", arch}} {
		if err := os.CopyFS(d[1], os.DirFS(filepath.Join("testdata", "archive-v0.3.2", d[0]))); err != nil {
			t.Fatal(err)
		}
	}
	return dir, arch
}

// An archive written by v0.3.2 (rename into place, one line per segment in
// archive.jsonl) is still read and verified, and v0.3.3 continues it: new
// segments get index files of their own, archive.jsonl and the old files
// stay as they are, and both read as one chain with the live ledger.
func TestArchiveWrittenByV032(t *testing.T) {
	dir, arch := legacyFixture(t)
	legacy := archiveState(t, arch)

	entries, skipped, err := ReadArchive(arch)
	if err != nil || len(skipped) > 0 || len(entries) != 3 {
		t.Fatalf("read the v0.3.2 archive: %d entries, skipped %v, err %v", len(entries), skipped, err)
	}
	for i, e := range entries {
		if e.First != uint64(3*i+1) || e.File() != e.Name+".gz" || e.source() != fmt.Sprintf("%s line %d", ArchiveIndexFile, i+1) {
			t.Fatalf("entry %d: %+v from %s", i, e, e.source())
		}
	}
	if rep := VerifyChain(dir, arch); !rep.OK || rep.Records != 18 || rep.FirstSeq != 1 || rep.ArchivedSegments != 3 || rep.Anchor != "" || len(rep.Warnings) > 0 {
		t.Fatalf("verify the v0.3.2 archive: %+v", rep)
	}
	n := 0
	if _, err := ScanLedger(dir, arch, t0, t0.Add(time.Hour), func(*usage.Event) error { n++; return nil }); err != nil || n != 12 {
		t.Fatalf("export from the v0.3.2 archive: %d events (err %v)", n, err)
	}

	// v0.3.3 archives the next segments into it.
	c := &clock{t: t0.Add(3 * time.Hour)}
	s := open(t, dir, c, archiveOpts(arch))
	defer s.Close()
	for w := 6; w < 9; w++ {
		c.Add(time.Minute)
		ws := t0.Add(time.Duration(w) * time.Minute)
		if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), []usage.Event{ev("a", ws, 1), ev("b", ws, 2)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	c.Add(2 * time.Hour)
	if err := s.Ack(consumer, s.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Compact([]string{consumer}); n != 5 || err != nil {
		t.Fatalf("compaction into the v0.3.2 archive removed %d, err %v", n, err)
	}
	writtenOnce(t, legacy, archiveState(t, arch))
	entries, skipped, err = ReadArchive(arch)
	if err != nil || len(skipped) > 0 || len(entries) != 8 {
		t.Fatalf("the continued archive: %d entries, skipped %v, err %v", len(entries), skipped, err)
	}
	if e := entries[3]; e.First != 10 || e.source() != ArchiveIndexDir+"/seg-00000000000000000010.json" || e.File() != "seg-00000000000000000010.log.gz" {
		t.Fatalf("first new entry: %+v from %s", e, e.source())
	}
	for name, rep := range map[string]ChainReport{"offline": VerifyChain(dir, arch), "in process": must(s.VerifyWithArchive())} {
		if !rep.OK || rep.Records != 27 || rep.FirstSeq != 1 || rep.ArchivedSegments != 8 || rep.Anchor != "" || len(rep.Warnings) > 0 {
			t.Fatalf("%s: verify across both index formats: %+v", name, rep)
		}
	}
	var got []string
	if err := s.ScanAll(t0, t0.Add(time.Hour), func(e *usage.Event) error { got = append(got, e.WindowStart.Format("15:04")); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 18 || got[0] != "10:00" || got[17] != "10:08" {
		t.Fatalf("ScanAll across both index formats: %v", got)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// retainedArchive archives five segments, seq 1-15, and keeps seq 16-18 live.
func retainedArchive(t *testing.T) (dir, arch string) {
	t.Helper()
	dir, arch = t.TempDir(), filepath.Join(t.TempDir(), "archive")
	c := &clock{t: t0}
	s := open(t, dir, c, archiveOpts(arch))
	defer s.Close()
	s.Cursor(consumer)
	sixWindows(t, s, c)
	c.Add(2 * time.Hour)
	s.Ack(consumer, 18)
	if n, err := s.Compact([]string{consumer}); n != 5 || err != nil {
		t.Fatalf("compact: removed %d, err %v", n, err)
	}
	return dir, arch
}

// Retention deletes whole files, the oldest segments first, each one's index
// entry before its data file: what is left verifies and exports as a chain
// that starts later. Deleting anything else is found.
func TestArchiveRetentionDeletesWholeFiles(t *testing.T) {
	dir, arch := retainedArchive(t)
	prune := func(a string, firsts ...uint64) {
		for _, first := range firsts {
			for _, f := range []string{filepath.Join(ArchiveIndexDir, archiveCopyName(first, 1, archiveIndexExt)), archiveCopyName(first, 1, archiveDataExt)} {
				if err := os.Remove(filepath.Join(a, f)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	hashOf := func(a string, first uint64) string { return archiveEntry(t, a, segName(first)).LastHash }

	a := copyDir(t, arch)
	h4 := hashOf(a, 4)
	prune(a, 1, 4)
	rep := VerifyChain(dir, a)
	if !rep.OK || rep.FirstSeq != 7 || rep.Records != 12 || rep.Anchor != h4 || len(rep.Warnings) > 0 {
		t.Fatalf("after retention removed seq 1-6: %+v", rep)
	}
	var n int
	sum, err := ScanLedger(dir, a, t0, t0.Add(time.Hour), func(*usage.Event) error { n++; return nil })
	if err != nil || n != 8 || sum.FirstSeq != 7 || sum.Anchor != h4 {
		t.Fatalf("export after retention: %d events, %+v, %v", n, sum, err)
	}

	for name, tc := range map[string]struct {
		cut  func(a string)
		want string
	}{
		"a segment in the middle": {func(a string) { prune(a, 7) }, "expected seq 7, found 10"},
		"the oldest data file without its entry": {func(a string) {
			os.Remove(filepath.Join(a, archiveCopyName(1, 1, archiveDataExt)))
		}, "archived file is missing"},
	} {
		t.Run(name, func(t *testing.T) {
			a := copyDir(t, arch)
			tc.cut(a)
			if rep := VerifyChain(dir, a); rep.OK || !hasError(rep.Errors, tc.want) {
				t.Fatalf("want an error containing %q, got %+v", tc.want, rep)
			}
		})
	}
	// An entry removed before its data file (a retention that stopped in
	// between): the data file is in no entry and reported, nothing fails.
	a = copyDir(t, arch)
	os.Remove(filepath.Join(a, ArchiveIndexDir, archiveCopyName(1, 1, archiveIndexExt)))
	if rep := VerifyChain(dir, a); !rep.OK || rep.FirstSeq != 4 || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "seg-00000000000000000001.log.gz is in no index entry") {
		t.Fatalf("after an entry was removed alone: %+v", rep)
	}
}

// A running vBilling reads the archive's index through a cache. Retention
// that deletes files meanwhile, and files changed meanwhile, are seen.
func TestArchiveIndexCacheFollowsTheDirectory(t *testing.T) {
	dir, arch := retainedArchive(t)
	c := &clock{t: t0.Add(3 * time.Hour)}
	s := open(t, dir, c, archiveOpts(arch))
	defer s.Close()
	count := func() (int, error) {
		n := 0
		err := s.ScanAll(t0, t0.Add(time.Hour), func(*usage.Event) error { n++; return nil })
		return n, err
	}
	if n, err := count(); n != 12 || err != nil {
		t.Fatalf("ScanAll: %d, %v", n, err)
	}
	// Retention removes the oldest segment.
	os.Remove(filepath.Join(arch, ArchiveIndexDir, archiveCopyName(1, 1, archiveIndexExt)))
	os.Remove(filepath.Join(arch, archiveCopyName(1, 1, archiveDataExt)))
	if n, err := count(); n != 10 || err != nil {
		t.Fatalf("ScanAll after retention: %d, %v", n, err)
	}
	// A changed data file fails the read, whatever the cache holds.
	path := filepath.Join(arch, archiveCopyName(4, 1, archiveDataExt))
	b := mustRead(t, path)
	b[len(b)/2] ^= 1
	os.WriteFile(path, b, 0o640)
	if _, err := count(); err == nil || !strings.Contains(err.Error(), "sha256 does not match") {
		t.Fatalf("ScanAll over a changed file: %v", err)
	}
	// Compaction keeps archiving into it.
	b[len(b)/2] ^= 1
	os.WriteFile(path, b, 0o640)
	c.Add(time.Minute)
	ws := t0.Add(6 * time.Minute)
	if err := s.AppendWindow(SourceCollector, ws.Add(time.Minute), []usage.Event{ev("a", ws, 1)}, ""); err != nil {
		t.Fatal(err)
	}
	c.Add(2 * time.Hour)
	s.Ack(consumer, s.Committed())
	if n, err := s.Compact([]string{consumer}); n != 1 || err != nil {
		t.Fatalf("compaction: %d, %v", n, err)
	}
	if n, err := count(); n != 11 || err != nil {
		t.Fatalf("ScanAll after compaction: %d, %v", n, err)
	}
}

// What the index tolerates and what it refuses.
func TestArchiveIndexFiles(t *testing.T) {
	dir, arch := compactedLedger(t)
	entry := func(a string, first uint64) []byte {
		return mustRead(t, filepath.Join(a, ArchiveIndexDir, archiveCopyName(first, 1, archiveIndexExt)))
	}
	put := func(a, name string, b []byte) {
		if err := os.WriteFile(filepath.Join(a, ArchiveIndexDir, name), b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(b []byte, change func(*ArchiveEntry)) []byte {
		var e ArchiveEntry
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		change(&e)
		out, _ := json.Marshal(e)
		return out
	}
	for name, tc := range map[string]struct {
		change   func(a string)
		ok       bool
		message  string
		warnings int
	}{
		"a second entry for a segment, with the same records": {func(a string) {
			put(a, archiveCopyName(4, 2, archiveIndexExt), entry(a, 4))
		}, true, "repeats the entry for " + segName(4), 1},
		"a second entry for a segment, with other records": {func(a string) {
			put(a, archiveCopyName(4, 2, archiveIndexExt), edit(entry(a, 4), func(e *ArchiveEntry) { e.LastHash = strings.Repeat("0", 64) }))
		}, false, "records " + segName(4) + " again with different contents", 0},
		"another file in index/": {func(a string) {
			put(a, "notes.txt", []byte("kept by an operator"))
		}, true, "index/notes.txt is not an index entry file", 1},
		"an entry under another segment's name": {func(a string) {
			os.Remove(filepath.Join(a, ArchiveIndexDir, archiveCopyName(4, 1, archiveIndexExt)))
			put(a, archiveCopyName(4, 1, archiveIndexExt), entry(a, 7))
		}, false, "expected seq 4, found 7", 0},
		"an entry that names another segment's data file": {func(a string) {
			put(a, archiveCopyName(4, 1, archiveIndexExt), edit(entry(a, 4), func(e *ArchiveEntry) { e.GzFile = archiveCopyName(7, 1, archiveDataExt) }))
		}, false, "expected seq 4, found 7", 0},
	} {
		t.Run(name, func(t *testing.T) {
			a := copyDir(t, arch)
			tc.change(a)
			rep := VerifyChain(dir, a)
			text := strings.Join(append(rep.Errors, rep.Warnings...), "\n")
			if rep.OK != tc.ok || !strings.Contains(text, tc.message) || (tc.ok && len(rep.Warnings) != tc.warnings) {
				t.Fatalf("want ok %v and %q, got %+v", tc.ok, tc.message, rep)
			}
		})
	}
}

func TestArchiveCopyNames(t *testing.T) {
	for _, tc := range []struct {
		first uint64
		n     int
		ext   string
		name  string
	}{
		{1, 1, archiveDataExt, "seg-00000000000000000001.log.gz"},
		{4, 2, archiveDataExt, "seg-00000000000000000004-2.log.gz"},
		{18446744073709551615, 17, archiveIndexExt, "seg-18446744073709551615-17.json"},
	} {
		if got := archiveCopyName(tc.first, tc.n, tc.ext); got != tc.name {
			t.Fatalf("archiveCopyName(%d, %d) = %s, want %s", tc.first, tc.n, got, tc.name)
		}
		if first, n, ok := parseArchiveCopyName(tc.name, tc.ext); !ok || first != tc.first || n != tc.n {
			t.Fatalf("parse %s: %d %d %v", tc.name, first, n, ok)
		}
	}
	for _, name := range []string{"seg-00000000000000000001.log", "seg-1.log.gz", "seg-00000000000000000000.log.gz", "seg-00000000000000000001-1.log.gz",
		"seg-00000000000000000001-02.log.gz", "seg-00000000000000000001-x.log.gz", "seg-0000000000000000000a.log.gz", "../seg-00000000000000000001.log.gz",
		"seg-00000000000000000001.log.gz.tmp", "seg-00000000000000000001_2.log.gz"} {
		if _, _, ok := parseArchiveCopyName(name, archiveDataExt); ok {
			t.Fatalf("%s parsed as an archive copy", name)
		}
	}
	if !bytes.Equal([]byte(archiveCopyName(7, 1, archiveDataExt)), []byte(segName(7)+".gz")) {
		t.Fatal("the first copy of a data file is not named as before v0.3.3")
	}
}
