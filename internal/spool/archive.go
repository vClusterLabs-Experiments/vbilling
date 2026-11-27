package spool

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Long-term archive.
//
// Retention bounds how long consumed segments stay on the ledger volume. With
// Options.ArchiveDir set, Compact first writes each segment it is about to
// delete to <ArchiveDir>/seg-<first>.log.gz, a gzip of the segment's exact
// bytes, and appends one line describing it to <ArchiveDir>/archive.jsonl.
// Only then is the segment deleted. A segment that fails to archive stays on
// the volume and is retried by the next compaction.
//
// Archived lines keep their hashes and their links to the record before them,
// so VerifyChain (chain.go) checks the archive and the live ledger as one
// chain, offline, long after the ledger itself has moved on, and ScanLedger
// reads the usage events behind an invoice back from both.

// ArchiveIndexFile is the append-only index of archived segments.
const ArchiveIndexFile = "archive.jsonl"

// archiveLevel trades compaction CPU for archive size (BenchmarkArchiveSegment).
const archiveLevel = gzip.BestCompression

// ErrNoArchive is returned by archive operations when ArchiveDir is not set.
var ErrNoArchive = errors.New("no archive directory configured (ARCHIVE_DIR)")

// archiveMu serializes compactions that archive with archive verifications in
// this process, so a verification never sees a segment halfway between the
// live ledger and the archive.
var archiveMu sync.Mutex

// EnsureArchiveDir creates the archive directory, parents included, and does
// nothing when it exists.
//
// vBilling calls it at start-up and with every volume check, not only when
// the first compaction has a segment to archive (seven days after an
// install): the directory is one of its own under the mount (the chart sets
// ARCHIVE_DIR to <mount path>/<pod name>), so until it exists a healthy,
// empty archive volume cannot be told from one that is not mounted. A
// failure here is that signal: the volume is not mounted, or vBilling's user
// cannot write to it.
func EnsureArchiveDir(dir string) error {
	if dir == "" {
		return ErrNoArchive
	}
	return os.MkdirAll(dir, 0o750)
}

// ArchiveEntry is one line of archive.jsonl: one archived segment.
type ArchiveEntry struct {
	Name       string    `json:"name"`        // segment file name, seg-<first>.log
	First      uint64    `json:"first"`       // first sequence number
	Last       uint64    `json:"last"`        // last sequence number
	FirstAt    time.Time `json:"first_at"`    // when the first record was appended
	LastAt     time.Time `json:"last_at"`     // when the last record was appended
	Prev       string    `json:"prev"`        // hash the first record links to, empty at genesis
	LastHash   string    `json:"last_hash"`   // hash of the last record
	GzSHA256   string    `json:"gz_sha256"`   // sha256 of the .gz file
	GzBytes    int64     `json:"gz_bytes"`    // size of the .gz file
	ArchivedAt time.Time `json:"archived_at"` // when compaction archived the segment
}

// File is the archived segment's file name in the archive directory.
func (e ArchiveEntry) File() string { return e.Name + ".gz" }

// ReadArchive reads archive.jsonl in dir and returns its entries in sequence
// order. Lines that are not valid entries, such as one torn by a crash before
// it was fsynced, are skipped and described in skipped. A directory without
// an index is an empty archive. Two different entries for one segment are an
// error.
func ReadArchive(dir string) (entries []ArchiveEntry, skipped []string, err error) {
	idx, err := readArchiveIndex(dir)
	if err != nil {
		return nil, nil, err
	}
	if len(idx.conflicts) > 0 {
		return idx.entries, idx.skipped, errors.New(idx.conflicts[0])
	}
	return idx.entries, idx.skipped, nil
}

type archiveIndex struct {
	entries   []ArchiveEntry // valid entries in sequence order
	byName    map[string]ArchiveEntry
	skipped   []string // lines that are not valid entries
	conflicts []string // segments recorded twice with different contents
	exists    bool     // archive.jsonl exists
	newline   bool     // archive.jsonl is empty or ends with a newline
}

func emptyArchiveIndex() *archiveIndex {
	return &archiveIndex{byName: map[string]ArchiveEntry{}, newline: true}
}

func readArchiveIndex(dir string) (*archiveIndex, error) {
	idx := emptyArchiveIndex()
	b, err := os.ReadFile(filepath.Join(dir, ArchiveIndexFile))
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ArchiveIndexFile, err)
	}
	idx.exists = true
	idx.newline = len(b) == 0 || b[len(b)-1] == '\n'
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e ArchiveEntry
		if err := json.Unmarshal(line, &e); err != nil || e.Name == "" || e.First == 0 || e.Last < e.First {
			idx.skipped = append(idx.skipped, fmt.Sprintf("%s line %d is not a valid entry and was ignored", ArchiveIndexFile, i+1))
			continue
		}
		if seen, dup := idx.byName[e.Name]; dup {
			if seen.First != e.First || seen.Last != e.Last || seen.LastHash != e.LastHash || seen.GzSHA256 != e.GzSHA256 {
				idx.conflicts = append(idx.conflicts, fmt.Sprintf("%s line %d records %s again with different contents", ArchiveIndexFile, i+1, e.Name))
			} else {
				idx.skipped = append(idx.skipped, fmt.Sprintf("%s line %d repeats the entry for %s and was ignored", ArchiveIndexFile, i+1, e.Name))
			}
			continue
		}
		idx.byName[e.Name] = e
		idx.entries = append(idx.entries, e)
	}
	sort.SliceStable(idx.entries, func(i, j int) bool { return idx.entries[i].First < idx.entries[j].First })
	return idx, nil
}

// hashBefore returns the archived hash of the record before seq, when seq
// starts an archived segment or follows one.
func (idx *archiveIndex) hashBefore(seq uint64) (string, bool) {
	for _, e := range idx.entries {
		if e.Last+1 == seq {
			return e.LastHash, true
		}
		if e.First == seq {
			return e.Prev, true
		}
	}
	return "", false
}

// --- compaction ---

// compactableLocked returns how many of the oldest closed segments compaction
// may remove: past retention and consumed by every listed destination.
func (s *Spool) compactableLocked(consumers []string) int {
	if s.opts.Retention <= 0 || len(s.segs) < 2 {
		return 0
	}
	minCursor := s.committed
	for _, c := range consumers {
		cur, ok := s.cursors[c]
		if !ok {
			cur = s.committed
		}
		if cur < minCursor {
			minCursor = cur
		}
	}
	cutoff := s.opts.Now().Add(-s.opts.Retention)
	n := 0
	for _, sg := range s.segs[:len(s.segs)-1] {
		if sg.Last != 0 && (sg.LastAt.After(cutoff) || sg.Last > minCursor) {
			break
		}
		n++
	}
	return n
}

// removeLocked deletes the oldest n segments, moves the anchor past them and
// rewrites the manifest. With want set, it stops at the first segment that is
// not the next one in want.
func (s *Spool) removeLocked(n int, want []segment) (removed int, err error) {
	for ; removed < n; removed++ {
		sg := s.segs[removed]
		if want != nil && (removed >= len(want) || want[removed].Name != sg.Name) {
			break
		}
		if rerr := os.Remove(filepath.Join(s.dir, sg.Name)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = rerr
			break
		}
		if sg.LastHash != "" {
			s.anchor = sg.LastHash
		}
	}
	if removed > 0 {
		s.segs = append([]*segment(nil), s.segs[removed:]...)
		if merr := s.writeManifestLocked(); err == nil {
			err = merr
		}
	}
	return removed, err
}

// compactArchived archives the compactable segments without holding the spool
// lock, so appends and reads go on while segments are compressed, then
// deletes the ones that are archived and still compactable.
func (s *Spool) compactArchived(consumers []string) (int, error) {
	archiveMu.Lock()
	defer archiveMu.Unlock()

	s.mu.Lock()
	cands := make([]segment, s.compactableLocked(consumers))
	for i := range cands {
		cands[i] = *s.segs[i] // closed segments never change
	}
	s.mu.Unlock()

	archived, archErr := s.archiveSegments(cands)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, archErr
	}
	// A cursor may have been rewound for a replay in the meantime.
	removed, err := s.removeLocked(min(archived, s.compactableLocked(consumers)), cands)
	if derr := s.compactDedupeLocked(); err == nil {
		err = derr
	}
	return removed, errors.Join(archErr, err)
}

// archiveSegments archives segs in order and returns how many are safely in
// the archive (empty segments hold nothing and count). It stops at the first
// failure: deletion has to stay a prefix for the anchor to stay exact.
func (s *Spool) archiveSegments(segs []segment) (int, error) {
	if len(segs) == 0 {
		return 0, nil
	}
	dir := s.opts.ArchiveDir
	if err := EnsureArchiveDir(dir); err != nil {
		return 0, fmt.Errorf("archive: %w", err)
	}
	idx, err := readArchiveIndex(dir)
	if err != nil {
		return 0, fmt.Errorf("archive: %w", err)
	}
	for i := range segs {
		if segs[i].Last == 0 {
			continue
		}
		if err := s.archiveSegment(idx, &segs[i]); err != nil {
			return i, fmt.Errorf("archive %s (kept in the ledger): %w", segs[i].Name, err)
		}
	}
	return len(segs), nil
}

// archiveSegment makes sure a closed segment is in the archive: recorded in
// archive.jsonl, with an intact copy. It is idempotent, so a crash between
// archiving and deleting only repeats the check.
func (s *Spool) archiveSegment(idx *archiveIndex, sg *segment) error {
	dir := s.opts.ArchiveDir
	if e, ok := idx.byName[sg.Name]; ok {
		if e.First != sg.First || e.Last != sg.Last || e.LastHash != sg.LastHash {
			return fmt.Errorf("%s already records a different %s (seq %d-%d, last hash %s; this ledger has seq %d-%d, last hash %s): give each ledger its own ARCHIVE_DIR",
				ArchiveIndexFile, sg.Name, e.First, e.Last, e.LastHash, sg.First, sg.Last, sg.LastHash)
		}
		if _, err := readArchivedFile(dir, e, nil); err != nil {
			return fmt.Errorf("recorded in %s, but the archived copy is not intact: %w", ArchiveIndexFile, err)
		}
		return nil
	}
	path := filepath.Join(dir, sg.Name+".gz")
	if _, err := os.Stat(path); err == nil {
		// Left over from a crash between the rename and the index append?
		// Replace it only if it holds this very segment.
		if h, herr := lastGzipLineHash(path); herr != nil || h != sg.LastHash {
			return fmt.Errorf("%s exists, is not recorded in %s and does not hold this segment: move it away", path, ArchiveIndexFile)
		}
	}
	e, err := s.writeArchiveFile(sg, path)
	if err != nil {
		return err
	}
	return appendArchiveIndex(dir, idx, e, s.opts.NoSync)
}

// writeArchiveFile gzips a closed segment's bytes to path through a temporary
// file (fsync, rename, fsync the directory). It checks the segment while
// copying it: every line must match its hash, carry the next sequence number
// and link to the line before it, and the segment must end at the record the
// spool has on file for it.
func (s *Spool) writeArchiveFile(sg *segment, path string) (ArchiveEntry, error) {
	src, err := os.Open(filepath.Join(s.dir, sg.Name))
	if err != nil {
		return ArchiveEntry{}, err
	}
	defer src.Close()

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return ArchiveEntry{}, err
	}
	renamed := false
	defer func() {
		if !renamed {
			f.Close()
			os.Remove(tmp)
		}
	}()
	sum := sha256.New()
	out := &countingWriter{w: io.MultiWriter(f, sum)}
	zw, err := gzip.NewWriterLevel(out, archiveLevel)
	if err != nil {
		return ArchiveEntry{}, err
	}
	zw.Name, zw.ModTime = sg.Name, sg.LastAt // a stable header: same segment, same bytes

	var w chainWalk
	w.next = sg.First
	st, err := w.segment(sg.Name, io.TeeReader(io.LimitReader(src, sg.Size), zw), false, noBound)
	if err != nil {
		return ArchiveEntry{}, err
	}
	if len(w.errs) > 0 {
		return ArchiveEntry{}, fmt.Errorf("segment does not verify: %s", w.errs[0])
	}
	if st.records == 0 || st.first != sg.First || st.last != sg.Last || st.lastHash != sg.LastHash {
		return ArchiveEntry{}, fmt.Errorf("segment holds seq %d-%d ending in %s, the manifest says %d-%d ending in %s",
			st.first, st.last, st.lastHash, sg.First, sg.Last, sg.LastHash)
	}
	if err := zw.Close(); err != nil {
		return ArchiveEntry{}, err
	}
	if !s.opts.NoSync {
		if err := f.Sync(); err != nil {
			return ArchiveEntry{}, err
		}
	}
	if err := f.Close(); err != nil {
		return ArchiveEntry{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return ArchiveEntry{}, err
	}
	renamed = true
	if !s.opts.NoSync {
		if err := syncDir(filepath.Dir(path)); err != nil {
			return ArchiveEntry{}, err
		}
	}
	return ArchiveEntry{
		Name: sg.Name, First: st.first, Last: st.last, FirstAt: st.firstAt, LastAt: st.lastAt,
		Prev: st.firstPrev, LastHash: st.lastHash,
		GzSHA256: hex.EncodeToString(sum.Sum(nil)), GzBytes: out.n,
		ArchivedAt: s.opts.Now().UTC(),
	}, nil
}

// appendArchiveIndex appends one entry to archive.jsonl and fsyncs it. The
// entry is what makes an archived file count: a .gz without one is replaced
// by the next compaction.
func appendArchiveIndex(dir string, idx *archiveIndex, e ArchiveEntry, noSync bool) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line := make([]byte, 0, len(b)+2)
	if !idx.newline {
		line = append(line, '\n') // end a line a crash left torn, so this entry stands alone
	}
	line = append(append(line, b...), '\n')
	f, err := os.OpenFile(filepath.Join(dir, ArchiveIndexFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return fmt.Errorf("append to %s: %w", ArchiveIndexFile, err)
	}
	if !noSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("fsync %s: %w", ArchiveIndexFile, err)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !idx.exists && !noSync {
		if err := syncDir(dir); err != nil { // a new file is durable once its directory entry is
			return err
		}
	}
	idx.exists, idx.newline = true, true
	idx.byName[e.Name] = e
	idx.entries = append(idx.entries, e)
	return nil
}

// syncDir makes renames and new files in dir durable. Filesystems that cannot
// fsync a directory (some FUSE and network mounts) report EINVAL or "not
// supported"; there the rename is as durable as it gets.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

// lastGzipLineHash returns the hash of the last complete line of a gzipped
// segment.
func lastGzipLineHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	br := bufio.NewReaderSize(zr, 1<<20)
	var buf []byte
	last := ""
	for {
		line, err := readLine(br, &buf)
		if err != nil && err != io.EOF {
			return "", err
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return last, nil
		}
		if len(line) > 65 && line[64] == '\t' {
			last = string(line[:64])
		}
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// readArchivedFile reads an archived segment into buf (reused when large
// enough) and checks it against its entry before anything decompresses it.
func readArchivedFile(dir string, e ArchiveEntry, buf []byte) ([]byte, error) {
	f, err := os.Open(filepath.Join(dir, e.File()))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("archived file is missing")
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != e.GzBytes {
		return nil, fmt.Errorf("file is %d bytes, %s says %d", info.Size(), ArchiveIndexFile, e.GzBytes)
	}
	if int64(cap(buf)) < info.Size() {
		buf = make([]byte, info.Size())
	}
	buf = buf[:info.Size()]
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(buf)
	if hex.EncodeToString(sum[:]) != e.GzSHA256 {
		return nil, fmt.Errorf("sha256 does not match %s: the file was changed", ArchiveIndexFile)
	}
	return buf, nil
}
