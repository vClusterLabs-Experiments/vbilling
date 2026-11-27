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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Long-term archive.
//
// Retention bounds how long consumed segments stay on the ledger volume. With
// Options.ArchiveDir set, Compact first archives each segment it is about to
// delete, and deletes it only once its archived copy and its index entry are
// durable. A segment that fails to archive stays on the volume and is retried
// by the next compaction.
//
// The archive is written once. Every file in it is created under its final
// name, written from start to end, fsynced and closed, and never renamed,
// appended to or overwritten. That is all a bucket mounted through FUSE
// offers (Mountpoint for Amazon S3 refuses appends, and renames on general
// purpose buckets; Cloud Storage FUSE uploads the whole object again for
// either), and no crash can tear a file that is in the archive already:
//
//	<ArchiveDir>/seg-<first>.log.gz      a gzip of the segment's exact bytes
//	<ArchiveDir>/index/seg-<first>.json  its index entry (ArchiveEntry)
//
// The index entry is written after the data file is durable: it is what
// makes the data file count. A data file without an entry, or an index file
// that is not a valid entry (a crash in the middle of writing either), is
// ignored, and the segment is written again under the next name no file has:
// seg-<first>-2.log.gz and index/seg-<first>-2.json, and so on. The index is
// read by listing index/. vBilling never deletes a file it finished writing.
// Retention, which is up to the operator, deletes whole files, the oldest
// segments first: a segment's index entry, then its data file.
//
// Archives written before v0.3.3 have one index, archive.jsonl, with a line
// per segment and data files named seg-<first>.log.gz. They are read with the
// entries in index/; nothing is written to archive.jsonl any more.
//
// Archived lines keep their hashes and their links to the record before them,
// so VerifyChain (chain.go) checks the archive and the live ledger as one
// chain, offline, long after the ledger itself has moved on, and ScanLedger
// reads the usage events behind an invoice back from both.

// ArchiveIndexDir is the directory in an archive directory that holds one
// index entry file per archived segment.
const ArchiveIndexDir = "index"

// ArchiveIndexFile is the index of archives written before v0.3.3: one line
// per archived segment. It is read, never written.
const ArchiveIndexFile = "archive.jsonl"

const (
	// archiveDataExt and archiveIndexExt end the names of a segment's data
	// file and index entry file.
	archiveDataExt  = ".log.gz"
	archiveIndexExt = ".json"
	// maxArchiveCopies bounds the names one segment's files can take: every
	// copy a crash left unfinished keeps its name.
	maxArchiveCopies = 1000
	// maxIndexFileBytes bounds an index file; an entry is a few hundred bytes.
	maxIndexFileBytes = 64 << 10
	// indexReaders is how many index files a read opens at once: on a bucket
	// every file is a request.
	indexReaders = 16
)

// archiveLevel trades compaction CPU for archive size (BenchmarkArchiveSegment).
const archiveLevel = gzip.BestCompression

// ErrNoArchive is returned by archive operations when ArchiveDir is not set.
var ErrNoArchive = errors.New("no archive directory configured (ARCHIVE_DIR)")

// archiveMu serializes compactions that archive with archive verifications
// and reads in this process, so a verification never sees a segment halfway
// between the live ledger and the archive. It also guards indexCaches.
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

// ArchiveEntry describes one archived segment: the content of its file in
// index/, or a line of archive.jsonl.
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
	// GzFile is the data file: seg-<first>.log.gz, or seg-<first>-<n>.log.gz
	// when a copy a crash left had taken the name. Lines of archive.jsonl
	// have none: their data file is <name>.gz.
	GzFile string `json:"file,omitempty"`

	from string // where the entry was read, for messages
}

// File is the archived segment's data file name in the archive directory.
func (e ArchiveEntry) File() string {
	if e.GzFile != "" {
		return e.GzFile
	}
	return e.Name + ".gz"
}

// source names where the entry was read: index/<file>, or a line of
// archive.jsonl.
func (e ArchiveEntry) source() string {
	if e.from != "" {
		return e.from
	}
	return ArchiveIndexFile
}

// archiveCopyName is the name of copy n (from 1) of a segment's data file or
// index entry: seg-<first><ext>, then seg-<first>-<n><ext>.
func archiveCopyName(first uint64, n int, ext string) string {
	if n > 1 {
		return fmt.Sprintf("seg-%020d-%d%s", first, n, ext)
	}
	return fmt.Sprintf("seg-%020d%s", first, ext)
}

// parseArchiveCopyName reads a name archiveCopyName made.
func parseArchiveCopyName(name, ext string) (first uint64, n int, ok bool) {
	rest, ok := strings.CutSuffix(name, ext)
	if !ok {
		return 0, 0, false
	}
	if rest, ok = strings.CutPrefix(rest, "seg-"); !ok || len(rest) < 20 {
		return 0, 0, false
	}
	digits, copyNo := rest[:20], rest[20:]
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, 0, false
		}
	}
	first, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || first == 0 {
		return 0, 0, false
	}
	n = 1
	if copyNo != "" {
		c, ok := strings.CutPrefix(copyNo, "-")
		if !ok {
			return 0, 0, false
		}
		if n, err = strconv.Atoi(c); err != nil || n < 2 || strconv.Itoa(n) != c {
			return 0, 0, false
		}
	}
	return first, n, true
}

// validEntry reports whether e can describe an archived segment. The chain
// walk then checks it against its data file.
func validEntry(e ArchiveEntry) bool {
	if e.Name == "" || e.First == 0 || e.Last < e.First {
		return false
	}
	if e.GzFile != "" {
		first, _, ok := parseArchiveCopyName(e.GzFile, archiveDataExt)
		return ok && first == e.First
	}
	return true
}

// ReadArchive reads the index of the archive in dir, the entries in index/
// and the lines of archive.jsonl, and returns its entries in sequence order.
// Index files and lines that are not valid entries, such as one a crash left
// unfinished, are skipped and described in skipped. A directory without an
// index is an empty archive. Two different entries for one segment are an
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
	files     map[string]bool // data files some valid entry names, repeats included
	skipped   []string        // index files and lines that are not valid entries, and repeats
	conflicts []string        // segments recorded twice with different contents
	exists    bool            // archive.jsonl or an index file exists
}

func emptyArchiveIndex() *archiveIndex {
	return &archiveIndex{byName: map[string]ArchiveEntry{}, files: map[string]bool{}}
}

// add takes one index file or line into the index.
func (idx *archiveIndex) add(f indexFile) {
	if f.bad != "" {
		idx.skipped = append(idx.skipped, f.bad)
		return
	}
	e := f.entry
	idx.files[e.File()] = true
	if seen, dup := idx.byName[e.Name]; dup {
		// Two copies of one segment hold the same records; two entries for
		// one file must also agree on its bytes.
		if seen.First != e.First || seen.Last != e.Last || seen.Prev != e.Prev || seen.LastHash != e.LastHash ||
			(seen.File() == e.File() && (seen.GzSHA256 != e.GzSHA256 || seen.GzBytes != e.GzBytes)) {
			idx.conflicts = append(idx.conflicts, fmt.Sprintf("%s records %s again with different contents than %s", e.source(), e.Name, seen.source()))
		} else {
			idx.skipped = append(idx.skipped, fmt.Sprintf("%s repeats the entry for %s in %s and was ignored", e.source(), e.Name, seen.source()))
		}
		return
	}
	idx.byName[e.Name] = e
	idx.entries = append(idx.entries, e)
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

// --- reading the index ---

// indexFile is one index file, or one line of archive.jsonl, as read.
type indexFile struct {
	entry ArchiveEntry
	bad   string // why it is not a valid entry; "" when it is one
}

// indexCache keeps the index of one archive directory as this process read
// and wrote it. An index file never changes once written, so a read lists
// index/ and opens only the files it has not seen; archive.jsonl is read
// again when its size or time changed. Guarded by archiveMu.
type indexCache struct {
	files      map[string]indexFile
	legacy     []indexFile
	legacyRead bool
	legacySize int64
	legacyMod  time.Time
}

// indexCaches holds the cache of every archive directory a ledger of this
// process archives into. Guarded by archiveMu.
var indexCaches = map[string]*indexCache{}

// cachedArchiveIndex reads the index of the archive in dir through this
// process's cache. The caller holds archiveMu.
func cachedArchiveIndex(dir string) (*archiveIndex, error) {
	c := indexCaches[dir]
	if c == nil {
		c = &indexCache{}
		indexCaches[dir] = c
	}
	return loadArchiveIndex(dir, c, 0)
}

// readArchiveIndex reads the whole index of the archive in dir, every file
// of it.
func readArchiveIndex(dir string) (*archiveIndex, error) {
	return loadArchiveIndex(dir, &indexCache{}, 0)
}

// loadArchiveIndex reads the index of the archive in dir: the lines of
// archive.jsonl, then the files in index/. With only set it reads the index
// files of the segment starting at that sequence number only.
func loadArchiveIndex(dir string, c *indexCache, only uint64) (*archiveIndex, error) {
	legacy, legacyFound, err := c.readLegacy(dir)
	if err != nil {
		return nil, err
	}
	files, indexFound, err := c.readIndexDir(dir, only)
	if err != nil {
		return nil, err
	}
	idx := emptyArchiveIndex()
	idx.exists = legacyFound || indexFound
	for _, f := range legacy {
		if only == 0 || f.bad == "" && f.entry.First == only {
			idx.add(f)
		}
	}
	for _, f := range files {
		idx.add(f)
	}
	sort.SliceStable(idx.entries, func(i, j int) bool { return idx.entries[i].First < idx.entries[j].First })
	return idx, nil
}

// readLegacy reads archive.jsonl, the index of archives written before
// v0.3.3.
func (c *indexCache) readLegacy(dir string) ([]indexFile, bool, error) {
	path := filepath.Join(dir, ArchiveIndexFile)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		c.legacy, c.legacyRead = nil, false
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", ArchiveIndexFile, err)
	}
	if c.legacyRead && info.Size() == c.legacySize && info.ModTime().Equal(c.legacyMod) {
		return c.legacy, true, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", ArchiveIndexFile, err)
	}
	var out []indexFile
	for i, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		where := fmt.Sprintf("%s line %d", ArchiveIndexFile, i+1)
		var e ArchiveEntry
		if err := json.Unmarshal(line, &e); err != nil || !validEntry(e) {
			out = append(out, indexFile{bad: where + " is not a valid entry and was ignored"})
			continue
		}
		e.from = where
		out = append(out, indexFile{entry: e})
	}
	c.legacy, c.legacyRead, c.legacySize, c.legacyMod = out, true, info.Size(), info.ModTime()
	return out, true, nil
}

// readIndexDir lists index/ and reads the index files the cache does not
// hold, a few at a time. found reports whether index/ holds any file.
func (c *indexCache) readIndexDir(dir string, only uint64) (files []indexFile, found bool, err error) {
	indexDir := filepath.Join(dir, ArchiveIndexDir)
	list, err := os.ReadDir(indexDir)
	if errors.Is(err, os.ErrNotExist) {
		c.files = nil
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", indexDir, err)
	}
	type named struct {
		name  string
		first uint64
		n     int
	}
	var names []named
	var bad []indexFile
	for _, de := range list {
		if de.IsDir() {
			continue
		}
		found = true
		first, n, ok := parseArchiveCopyName(de.Name(), archiveIndexExt)
		if !ok {
			if only == 0 {
				bad = append(bad, indexFile{bad: fmt.Sprintf("%s/%s is not an index entry file and was ignored", ArchiveIndexDir, de.Name())})
			}
			continue
		}
		if only == 0 || first == only {
			names = append(names, named{de.Name(), first, n})
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].first != names[j].first {
			return names[i].first < names[j].first
		}
		return names[i].n < names[j].n
	})
	if c.files == nil {
		c.files = map[string]indexFile{}
	}
	var todo []named
	for _, nm := range names {
		if _, ok := c.files[nm.name]; !ok {
			todo = append(todo, nm)
		}
	}
	read := make([]indexFile, len(todo))
	errs := make([]error, len(todo))
	var wg sync.WaitGroup
	slots := make(chan struct{}, indexReaders)
	for i, nm := range todo {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			read[i], errs[i] = readIndexFile(indexDir, nm.name, nm.first)
		}()
	}
	wg.Wait()
	for i, nm := range todo {
		switch {
		case errors.Is(errs[i], os.ErrNotExist):
			// Removed since the listing: retention.
		case errs[i] != nil:
			return nil, found, errs[i]
		default:
			c.files[nm.name] = read[i]
		}
	}
	if only == 0 {
		listed := make(map[string]bool, len(names))
		for _, nm := range names {
			listed[nm.name] = true
		}
		for name := range c.files {
			if !listed[name] {
				delete(c.files, name)
			}
		}
	}
	files = bad
	for _, nm := range names {
		if f, ok := c.files[nm.name]; ok {
			files = append(files, f)
		}
	}
	return files, found, nil
}

// readIndexFile reads one index entry file. One that does not hold a valid
// entry for the segment its name gives is reported as bad, not as an error:
// a crash in the middle of writing it leaves such a file.
func readIndexFile(dir, name string, first uint64) (indexFile, error) {
	where := ArchiveIndexDir + "/" + name
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return indexFile{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxIndexFileBytes+1))
	if err != nil {
		return indexFile{}, fmt.Errorf("read %s: %w", where, err)
	}
	var e ArchiveEntry
	if len(b) > maxIndexFileBytes || json.Unmarshal(b, &e) != nil || !validEntry(e) {
		return indexFile{bad: where + " is not a valid entry (a write that did not finish) and was ignored"}, nil
	}
	if e.First != first || e.Name != segName(first) {
		return indexFile{bad: fmt.Sprintf("%s describes %s, not the segment its name gives, and was ignored", where, e.Name)}, nil
	}
	e.from = where
	return indexFile{entry: e}, nil
}

// unindexedFiles lists the data files in dir that no entry of idx names:
// copies a crash left, or one being written right now.
func unindexedFiles(dir string, idx *archiveIndex) ([]string, error) {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range list {
		if _, _, ok := parseArchiveCopyName(de.Name(), archiveDataExt); ok && !de.IsDir() && !idx.files[de.Name()] {
			out = append(out, de.Name())
		}
	}
	return out, nil
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
	idx, err := cachedArchiveIndex(dir)
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

// archiveSegment makes sure a closed segment is in the archive: an index
// entry for it whose data file is intact. It is idempotent, so a crash
// between archiving and deleting only repeats the check. Nothing it writes
// replaces a file: copies a crash left unfinished stay where they are, in no
// index entry, and the segment is written again under new names.
func (s *Spool) archiveSegment(idx *archiveIndex, sg *segment) error {
	dir := s.opts.ArchiveDir
	if e, ok := idx.byName[sg.Name]; ok {
		if e.First != sg.First || e.Last != sg.Last || e.LastHash != sg.LastHash {
			return fmt.Errorf("%s already records a different %s (seq %d-%d, last hash %s; this ledger has seq %d-%d, last hash %s): give each ledger its own ARCHIVE_DIR",
				e.source(), sg.Name, e.First, e.Last, e.LastHash, sg.First, sg.Last, sg.LastHash)
		}
		if _, err := readArchivedFile(dir, e, nil); err != nil {
			return fmt.Errorf("recorded in %s, but the archived copy %s is not intact: %w", e.source(), e.File(), err)
		}
		return nil
	}
	gz, e, err := s.gzipSegment(sg)
	if err != nil {
		return err
	}
	if e.GzFile, err = writeArchiveCopy(dir, sg.First, archiveDataExt, gz, s.opts.NoSync); err != nil {
		return fmt.Errorf("write its archived copy: %w", err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	indexDir := filepath.Join(dir, ArchiveIndexDir)
	if err := ensureDir(indexDir, s.opts.NoSync); err != nil {
		return err
	}
	name, err := writeArchiveCopy(indexDir, sg.First, archiveIndexExt, append(b, '\n'), s.opts.NoSync)
	if err != nil {
		// The data file stays: should the entry have reached the volume after
		// all, it names that file.
		return fmt.Errorf("write the index entry of %s: %w", e.GzFile, err)
	}
	e.from = ArchiveIndexDir + "/" + name
	idx.add(indexFile{entry: e})
	if c := indexCaches[dir]; c != nil {
		if c.files == nil {
			c.files = map[string]indexFile{}
		}
		c.files[name] = indexFile{entry: e}
	}
	return nil
}

// gzipSegment reads a closed segment, checks it and returns its gzip and its
// index entry, without writing anything: a segment that does not verify
// leaves no file behind, also on a bucket, where every file opened for
// writing becomes an object. Every line must match its hash, carry the next
// sequence number and link to the line before it, and the segment must end
// at the record the spool has on file for it.
func (s *Spool) gzipSegment(sg *segment) ([]byte, ArchiveEntry, error) {
	src, err := os.Open(filepath.Join(s.dir, sg.Name))
	if err != nil {
		return nil, ArchiveEntry{}, err
	}
	defer src.Close()

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, archiveLevel)
	if err != nil {
		return nil, ArchiveEntry{}, err
	}
	zw.Name, zw.ModTime = sg.Name, sg.LastAt // a stable header: same segment, same bytes

	var w chainWalk
	w.next = sg.First
	st, err := w.segment(sg.Name, io.TeeReader(io.LimitReader(src, sg.Size), zw), false, noBound)
	if err != nil {
		return nil, ArchiveEntry{}, err
	}
	if len(w.errs) > 0 {
		return nil, ArchiveEntry{}, fmt.Errorf("segment does not verify: %s", w.errs[0])
	}
	if st.records == 0 || st.first != sg.First || st.last != sg.Last || st.lastHash != sg.LastHash {
		return nil, ArchiveEntry{}, fmt.Errorf("segment holds seq %d-%d ending in %s, the manifest says %d-%d ending in %s",
			st.first, st.last, st.lastHash, sg.First, sg.Last, sg.LastHash)
	}
	if err := zw.Close(); err != nil {
		return nil, ArchiveEntry{}, err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), ArchiveEntry{
		Name: sg.Name, First: st.first, Last: st.last, FirstAt: st.firstAt, LastAt: st.lastAt,
		Prev: st.firstPrev, LastHash: st.lastHash,
		GzSHA256: hex.EncodeToString(sum[:]), GzBytes: int64(buf.Len()),
		ArchivedAt: s.opts.Now().UTC(),
	}, nil
}

// writeArchiveCopy writes b to a new file in dir, under the first copy name
// of the segment starting at first that no file has, and returns that name.
// The file is created for this write alone (O_EXCL), written from start to
// end, fsynced and closed, and never opened for writing again; then dir is
// fsynced. A file that is there already is a copy a crash left, or one in no
// index entry: it is never opened, replaced or removed. A copy this call
// fails to write is removed where the filesystem allows that (a bucket
// mounted without delete keeps it, in no index entry).
func writeArchiveCopy(dir string, first uint64, ext string, b []byte, noSync bool) (string, error) {
	for n := 1; n <= maxArchiveCopies; n++ {
		name := archiveCopyName(first, n, ext)
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(b)
		if err == nil && !noSync {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && !noSync {
			err = syncDir(dir)
		}
		if err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("%s: %w", path, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("%s holds %d copies of %s already: remove the ones no index entry names", dir, maxArchiveCopies, archiveCopyName(first, 1, ext))
}

// ensureDir creates dir when it is missing and makes its name durable.
func ensureDir(dir string, noSync bool) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if noSync {
		return nil
	}
	return syncDir(filepath.Dir(dir))
}

// syncDir makes renames and new files in dir durable. Filesystems that cannot
// fsync a directory (some FUSE and network mounts) report EINVAL or "not
// supported"; there the new name is as durable as it gets.
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
		return nil, fmt.Errorf("file is %d bytes, %s says %d", info.Size(), e.source(), e.GzBytes)
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
		return nil, fmt.Errorf("sha256 does not match %s: the file was changed", e.source())
	}
	return buf, nil
}
