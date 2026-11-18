package spool

// Reading the ledger and its archive as one hash chain, offline and
// read-only: VerifyChain and Spool.VerifyWithArchive check it, ScanLedger
// exports the usage events behind an invoice from it.

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
	"strconv"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// --- reading lines ---

// segReader keeps the buffers that reading one segment after another reuses,
// so a pass over years of archived segments does not allocate per segment.
type segReader struct {
	br   *bufio.Reader
	zr   *gzip.Reader
	gz   []byte
	line []byte
}

// lines returns a buffered reader over r.
func (sr *segReader) lines(r io.Reader) *bufio.Reader {
	if sr.br == nil {
		sr.br = bufio.NewReaderSize(r, 1<<20)
	} else {
		sr.br.Reset(r)
	}
	return sr.br
}

// openArchived checks an archived segment against its entry and returns a
// reader of its decompressed bytes.
func (sr *segReader) openArchived(dir string, e ArchiveEntry) (io.Reader, error) {
	b, err := readArchivedFile(dir, e, sr.gz)
	if err != nil {
		return nil, err
	}
	sr.gz = b
	if sr.zr == nil {
		if sr.zr, err = gzip.NewReader(bytes.NewReader(b)); err != nil {
			sr.zr = nil
			return nil, err
		}
		return sr.zr, nil
	}
	if err := sr.zr.Reset(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	return sr.zr, nil
}

// maxLineBytes bounds one ledger line; real records are a few kilobytes.
const maxLineBytes = 64 << 20

var errLineTooLong = errors.New("line longer than 64 MiB")

// readLine reads one line, newline included, reusing *buf. The line is only
// valid until the next call.
func readLine(br *bufio.Reader, buf *[]byte) ([]byte, error) {
	b := (*buf)[:0]
	for {
		chunk, err := br.ReadSlice('\n')
		b = append(b, chunk...)
		if err != bufio.ErrBufferFull {
			*buf = b
			return b, err
		}
		if len(b) > maxLineBytes {
			*buf = b[:0]
			return nil, errLineTooLong
		}
	}
}

// recordHeader is the part of a record that chain verification reads.
type recordHeader struct {
	Seq  uint64    `json:"seq"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Prev string    `json:"prev"`
}

// checkLine verifies a line ("<sha256 hex>\t<record json>\n", as parseLine
// reads it) and decodes only the fields chain verification needs.
func checkLine(line []byte) (recordHeader, string, error) {
	var h recordHeader
	line = bytes.TrimSuffix(line, []byte("\n"))
	if len(line) < 66 || line[64] != '\t' {
		return h, "", errors.New("malformed line")
	}
	var hexSum [64]byte
	sum := sha256.Sum256(line[65:])
	hex.Encode(hexSum[:], sum[:])
	if !bytes.Equal(hexSum[:], line[:64]) {
		return h, "", errors.New("hash mismatch")
	}
	if h, ok := quickHeader(line[65:]); ok {
		return h, string(hexSum[:]), nil
	}
	if err := json.Unmarshal(line[65:], &h); err != nil {
		return h, "", err
	}
	return h, string(hexSum[:]), nil
}

var (
	seqKey  = []byte(`seq":`)
	typeKey = []byte(`type":`)
	atKey   = []byte(`at":`)
	prevKey = []byte(`prev":`)
)

// repeatsHeaderKey reports whether b contains "seq":, "type":, "at": or
// "prev": anywhere, in one pass over its quotes.
func repeatsHeaderKey(b []byte) bool {
	for {
		i := bytes.IndexByte(b, '"')
		if i < 0 || i+1 >= len(b) {
			return false
		}
		b = b[i+1:]
		var key []byte
		switch b[0] {
		case 's':
			key = seqKey
		case 't':
			key = typeKey
		case 'a':
			key = atKey
		case 'p':
			key = prevKey
		default:
			continue
		}
		if bytes.HasPrefix(b, key) {
			return true
		}
	}
}

// quickHeader reads the header from the fixed prefix json.Marshal writes for
// a Record, {"seq":N,"type":"T","at":"RFC 3339","prev":"H",..., without
// scanning the event after it (verification runs over years of records). It
// declines anything else, including a body where one of these keys appears
// again, so whenever it answers, json.Unmarshal would answer the same. The
// rest of the body is not parsed: the line's hash already binds it.
func quickHeader(body []byte) (h recordHeader, ok bool) {
	rest, ok := bytes.CutPrefix(body, seqPrefix)
	if !ok {
		return h, false
	}
	i := bytes.IndexByte(rest, ',')
	if i <= 0 || (rest[0] == '0' && i > 1) { // JSON numbers have no leading zeros
		return h, false
	}
	for _, c := range rest[:i] {
		if c < '0' || c > '9' {
			return h, false
		}
	}
	seq, err := strconv.ParseUint(string(rest[:i]), 10, 64)
	if err != nil {
		return h, false
	}
	var typ, at, prev []byte
	rest = rest[i:]
	if typ, rest, ok = cutJSONString(rest, `,"type":"`); !ok {
		return h, false
	}
	if at, rest, ok = cutJSONString(rest, `,"at":"`); !ok {
		return h, false
	}
	if prev, rest, ok = cutJSONString(rest, `,"prev":"`); !ok || len(rest) == 0 || (rest[0] != ',' && rest[0] != '}') {
		return h, false
	}
	if repeatsHeaderKey(rest) {
		return h, false
	}
	if err := h.At.UnmarshalText(at); err != nil { // the parser time.Time's UnmarshalJSON uses
		return h, false
	}
	h.Seq, h.Type, h.Prev = seq, string(typ), string(prev)
	return h, true
}

// cutJSONString cuts key and the plain JSON string value after it (no escapes
// or control characters), returning the value and what follows its quote.
func cutJSONString(b []byte, key string) (val, rest []byte, ok bool) {
	rest, ok = bytes.CutPrefix(b, []byte(key))
	if !ok {
		return nil, b, false
	}
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return nil, b, false
	}
	for _, c := range rest[:j] {
		if c == '\\' || c < 0x20 {
			return nil, b, false
		}
	}
	return rest[:j], rest[j+1:], true
}

var commitType = []byte(`,"type":"commit"`)

// isCommitLine reports whether a line is a commit record without decoding it.
// Record marshals seq first and type second; call it after peekSeq succeeds.
func isCommitLine(line []byte) bool {
	rest := line[65+len(seqPrefix):]
	i := bytes.IndexByte(rest, ',')
	return i > 0 && bytes.HasPrefix(rest[i:], commitType)
}

// --- verification ---

// ChainReport is the result of verifying archived and live segments as one
// hash chain.
type ChainReport struct {
	OK               bool       `json:"ok"`
	Records          uint64     `json:"records"`
	ArchivedSegments int        `json:"archived_segments"`
	LiveSegments     int        `json:"live_segments"`
	FirstSeq         uint64     `json:"first_seq"`
	LastSeq          uint64     `json:"last_seq"`
	FirstAt          *time.Time `json:"first_at,omitempty"` // when the first record was appended
	LastAt           *time.Time `json:"last_at,omitempty"`  // when the last record was appended
	// Anchor is the hash the first verified record links to. Empty means the
	// chain starts at the ledger's first record; otherwise older records were
	// deleted before archiving was enabled.
	Anchor   string   `json:"anchor"`
	Head     string   `json:"head"` // hash of the last verified record
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// noBound reads a segment to its end.
const noBound = ^uint64(0)

// maxReportItems caps the errors and warnings a report lists.
const maxReportItems = 20

// chainPos is how far a chain walk has got.
type chainPos struct {
	started bool   // prev is known
	prev    string // hash the next record must link to
	next    uint64 // sequence number the next record must carry, 0 for any
	anchor  string // what the first record links to
	records uint64
	first   uint64
	last    uint64
	firstAt time.Time
	lastAt  time.Time
}

// chainWalk checks records in order: each line must match its hash, carry the
// next sequence number and link to the hash of the record before it.
type chainWalk struct {
	chainPos
	segReader
	errs     []string
	dropped  int
	warnings []string
}

func (w *chainWalk) fail(format string, args ...any) {
	if len(w.errs) < maxReportItems {
		w.errs = append(w.errs, fmt.Sprintf(format, args...))
	} else {
		w.dropped++
	}
}

func (w *chainWalk) warn(format string, args ...any) {
	if len(w.warnings) < maxReportItems {
		w.warnings = append(w.warnings, fmt.Sprintf(format, args...))
	}
}

// start fixes where the chain must begin.
func (w *chainWalk) start(prev string, next uint64) {
	w.started, w.prev, w.next, w.anchor = true, prev, next, prev
}

// record checks one line and advances the walk. ok is false when the line
// itself is bad: malformed, or not matching its hash.
func (w *chainWalk) record(name string, line []byte) (recordHeader, bool) {
	hdr, hash, err := checkLine(line)
	if err != nil {
		w.fail("%s: %v", name, err)
		// Resynchronize on what the line claims, so one bad line is one error.
		if seq, ok := peekSeq(line); ok {
			w.started, w.prev, w.next = true, string(line[:64]), seq+1
		}
		return hdr, false
	}
	if w.next != 0 && hdr.Seq != w.next {
		w.fail("%s: expected seq %d, found %d", name, w.next, hdr.Seq)
	}
	if !w.started {
		w.started, w.anchor = true, hdr.Prev
	} else if hdr.Prev != w.prev {
		w.fail("%s: chain break at seq %d", name, hdr.Seq)
	}
	if w.records == 0 {
		w.first, w.firstAt = hdr.Seq, hdr.At
	}
	w.records++
	w.last, w.lastAt = hdr.Seq, hdr.At
	w.prev, w.next = hash, hdr.Seq+1
	return hdr, true
}

// segStats summarizes what one segment added to a walk.
type segStats struct {
	records   uint64
	first     uint64
	last      uint64
	firstPrev string
	lastHash  string
	firstAt   time.Time
	lastAt    time.Time
}

// segment walks the lines of one segment, ignoring records past upto. active
// marks the newest live segment, where an append may be in progress: a torn
// final line is not an error there, and when upto is noBound (an offline
// read, which cannot know the committed point) records after the last commit
// are left out, as the next start would truncate them.
func (w *chainWalk) segment(name string, r io.Reader, active bool, upto uint64) (segStats, error) {
	var st segStats
	before, committed := w.chainPos, w.chainPos
	pending, seenFirst := 0, false
	br := w.lines(r)
	var rerr error
	for {
		line, err := readLine(br, &w.line)
		if err != nil && err != io.EOF {
			rerr = err
			break
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' {
			if !active {
				w.fail("%s: torn line at the end of a closed segment", name)
			} else if upto == noBound {
				w.warn("%s: left out a partial line at the end (an append in progress, or a crash the next start repairs)", name)
			}
			break
		}
		if upto != noBound {
			if seq, ok := peekSeq(line); ok && seq > upto {
				break
			}
		}
		hdr, ok := w.record(name, line)
		if ok && !seenFirst {
			st.first, st.firstPrev, st.firstAt, seenFirst = hdr.Seq, hdr.Prev, hdr.At, true
		}
		if ok && hdr.Type == TypeCommit {
			committed, pending = w.chainPos, 0
		} else {
			pending++
		}
	}
	if active && upto == noBound && pending > 0 {
		w.chainPos = committed
		w.warn("%s: left out %d uncommitted record(s) at the end (an append in progress, or a crash the next start repairs)", name, pending)
	}
	st.records = w.records - before.records
	if st.records > 0 {
		st.last, st.lastHash, st.lastAt = w.last, w.prev, w.lastAt
	}
	return st, rerr
}

// archived walks one archived segment after checking its file against its
// archive.jsonl entry, then checks that the entry describes what the file
// holds.
func (w *chainWalk) archived(dir string, e ArchiveEntry) {
	if e.Name != segName(e.First) {
		w.fail("%s: entry %s does not match its first seq %d", ArchiveIndexFile, e.Name, e.First)
	}
	zr, err := w.openArchived(dir, e)
	if err != nil {
		w.fail("%s: %v", e.File(), err)
		w.skip(e)
		return
	}
	st, err := w.segment(e.Name, zr, false, noBound)
	if err != nil {
		w.fail("%s: %v", e.File(), err)
	}
	switch {
	case st.records == 0:
		w.fail("%s: holds no records", e.File())
		w.skip(e)
	case st.first != e.First || st.last != e.Last:
		w.fail("%s: holds seq %d-%d, %s says %d-%d", e.File(), st.first, st.last, ArchiveIndexFile, e.First, e.Last)
	case st.firstPrev != e.Prev || st.lastHash != e.LastHash:
		w.fail("%s: its hashes do not match %s", e.File(), ArchiveIndexFile)
	case !st.firstAt.Equal(e.FirstAt) || !st.lastAt.Equal(e.LastAt):
		w.fail("%s: its record times do not match %s", e.File(), ArchiveIndexFile)
	}
}

// skip continues the walk past an unreadable archived segment from what its
// entry claims, so later segments are still checked on their own.
func (w *chainWalk) skip(e ArchiveEntry) {
	if !w.started {
		w.anchor = e.Prev
	}
	w.started, w.prev, w.next = true, e.LastHash, e.Last+1
}

// liveLedger is the live ledger as a verification or an export sees it.
type liveLedger struct {
	dir    string
	anchor string        // what the first retained record links to
	segs   []liveSegment // in sequence order
	upto   uint64        // highest committed seq when known (in process), else noBound
}

type liveSegment struct {
	name  string
	first uint64
}

// loadLiveLedger reads a ledger directory's manifest and lists its segments
// without modifying anything. A compaction may run in between, so both are
// read again until the manifest did not change around the listing.
func loadLiveLedger(dir string) (*liveLedger, error) {
	dir, err := findLedgerDir(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "manifest.json")
	for attempt := 0; ; attempt++ {
		before, err := readOptional(path)
		if err != nil {
			return nil, err
		}
		segs, err := listSegments(dir)
		if err != nil {
			return nil, err
		}
		after, err := readOptional(path)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(before, after) && attempt < 5 {
			continue
		}
		var man manifest
		if len(after) > 0 {
			if err := json.Unmarshal(after, &man); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		}
		return &liveLedger{dir: dir, anchor: man.Anchor, segs: segs, upto: noBound}, nil
	}
}

// findLedgerDir accepts the ledger directory itself or DATA_DIR above it.
func findLedgerDir(dir string) (string, error) {
	for _, d := range []string{dir, filepath.Join(dir, "ledger")} {
		if _, err := os.Stat(filepath.Join(d, "manifest.json")); err == nil {
			return d, nil
		}
		if segs, _ := listSegments(d); len(segs) > 0 {
			return d, nil
		}
	}
	return "", fmt.Errorf("no ledger in %s: expected manifest.json or seg-*.log files (the ledger is DATA_DIR/ledger)", dir)
}

func listSegments(dir string) ([]liveSegment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []liveSegment
	for _, e := range entries {
		if first, ok := parseSegName(e.Name()); ok && !e.IsDir() {
			out = append(out, liveSegment{name: e.Name(), first: first})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].first < out[j].first })
	return out, nil
}

func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// archivedSince looks a segment up in a fresh read of the archive index.
func archivedSince(dir, name string) (ArchiveEntry, bool) {
	idx, err := readArchiveIndex(dir)
	if err != nil {
		return ArchiveEntry{}, false
	}
	e, ok := idx.byName[name]
	return e, ok
}

// VerifyChain verifies a ledger offline: every segment archived in
// archiveDir, then the live ledger in dir (or DATA_DIR above it), as one hash
// chain. Either directory may be empty. Archived files must match the sha256
// in archive.jsonl, every record must match its hash, carry the next sequence
// number and link to the record before it, and the live ledger, whose anchor
// must be the archived hash before it, must continue where the archive ends.
// It only reads files, so it can run next to a live vBilling or on a copy.
func VerifyChain(dir, archiveDir string) ChainReport {
	var live *liveLedger
	if dir != "" {
		// Listed before the archive index is read: a segment compacted after
		// the listing is in the index, because it is archived first.
		l, err := loadLiveLedger(dir)
		if err != nil {
			return ChainReport{Errors: []string{err.Error()}}
		}
		live = l
	}
	return verifyChain(live, archiveDir)
}

// VerifyWithArchive verifies the archived segments and then the retained
// ledger as one hash chain, reaching back past retention where Verify stops.
// It returns ErrNoArchive without an ArchiveDir.
func (s *Spool) VerifyWithArchive() (ChainReport, error) {
	if s.opts.ArchiveDir == "" {
		return ChainReport{}, ErrNoArchive
	}
	archiveMu.Lock() // no segment is archived or deleted meanwhile
	defer archiveMu.Unlock()
	s.mu.Lock()
	live := &liveLedger{dir: s.dir, anchor: s.anchor, upto: s.committed}
	for _, sg := range s.segs {
		live.segs = append(live.segs, liveSegment{name: sg.Name, first: sg.First})
	}
	s.mu.Unlock()
	return verifyChain(live, s.opts.ArchiveDir), nil
}

func verifyChain(live *liveLedger, archiveDir string) ChainReport {
	var w chainWalk
	var rep ChainReport
	idx := emptyArchiveIndex()
	if archiveDir != "" {
		var err error
		if idx, err = readArchiveIndex(archiveDir); err != nil {
			w.fail("%v", err)
			return w.report(rep, live)
		}
		if !idx.exists {
			w.warn("%s has no %s: nothing has been archived there yet", archiveDir, ArchiveIndexFile)
		}
		for _, m := range idx.skipped {
			w.warn("%s", m)
		}
		for _, m := range idx.conflicts {
			w.fail("%s", m)
		}
		if len(idx.entries) > 0 {
			w.start(idx.entries[0].Prev, idx.entries[0].First)
		}
		for _, e := range idx.entries {
			w.archived(archiveDir, e)
			rep.ArchivedSegments++
		}
	}
	if live != nil {
		w.live(live, idx, archiveDir, &rep)
	}
	return w.report(rep, live)
}

// live walks the live ledger after the archive. A segment the archive also
// holds (archived but not deleted yet) is compared with its archived copy
// rather than walked twice.
func (w *chainWalk) live(l *liveLedger, idx *archiveIndex, archiveDir string, rep *ChainReport) {
	if len(l.segs) == 0 {
		if w.started && l.anchor != w.prev {
			w.fail("live ledger anchor %s does not match the last archived hash %s", l.anchor, w.prev)
		}
		return
	}
	first := l.segs[0].first
	if len(idx.entries) == 0 {
		if !w.started {
			w.start(l.anchor, first)
		}
	} else if h, ok := idx.hashBefore(first); !ok {
		w.fail("the live ledger starts at seq %d, which does not follow an archived segment (the archive holds seq %d-%d)",
			first, idx.entries[0].First, idx.entries[len(idx.entries)-1].Last)
	} else if h != l.anchor {
		w.fail("live ledger anchor %s does not match the archived hash before seq %d, %s", l.anchor, first, h)
	}
	for i, ls := range l.segs {
		if e, ok := idx.byName[ls.name]; ok {
			w.sameAsArchived(l, ls, e)
			continue
		}
		f, err := os.Open(filepath.Join(l.dir, ls.name))
		if errors.Is(err, os.ErrNotExist) && archiveDir != "" {
			// Archived and deleted by a compaction while this ran.
			if e, ok := archivedSince(archiveDir, ls.name); ok {
				w.archived(archiveDir, e)
				rep.ArchivedSegments++
				continue
			}
		}
		if err != nil {
			w.fail("%s: %v", ls.name, err)
			continue
		}
		_, err = w.segment(ls.name, f, i == len(l.segs)-1, l.upto)
		f.Close()
		if err != nil {
			w.fail("%s: %v", ls.name, err)
		}
		rep.LiveSegments++
	}
}

// sameAsArchived checks that a segment both archived and still live (after a
// crash, or a rewound cursor, between archiving and deleting) holds exactly
// what its archived copy holds.
func (w *chainWalk) sameAsArchived(l *liveLedger, ls liveSegment, e ArchiveEntry) {
	f, err := os.Open(filepath.Join(l.dir, ls.name))
	if errors.Is(err, os.ErrNotExist) {
		return // deleted after archiving while this ran
	}
	if err != nil {
		w.fail("%s: %v", ls.name, err)
		return
	}
	defer f.Close()
	var c chainWalk
	c.start(e.Prev, e.First)
	st, err := c.segment(ls.name, f, false, noBound)
	if err != nil || len(c.errs) > 0 || st.last != e.Last || st.lastHash != e.LastHash {
		w.fail("%s: the live segment differs from its archived copy", ls.name)
	}
}

func (w *chainWalk) report(rep ChainReport, live *liveLedger) ChainReport {
	rep.Records = w.records
	if w.records > 0 {
		firstAt, lastAt := w.firstAt.UTC(), w.lastAt.UTC()
		rep.FirstSeq, rep.LastSeq, rep.FirstAt, rep.LastAt = w.first, w.last, &firstAt, &lastAt
	}
	rep.Anchor, rep.Head = w.anchor, w.prev
	if !w.started && live != nil {
		rep.Anchor, rep.Head = live.anchor, live.anchor
	}
	rep.Errors = w.errs
	if w.dropped > 0 {
		rep.Errors = append(rep.Errors, fmt.Sprintf("and %d more error(s)", w.dropped))
	}
	rep.Warnings = w.warnings
	rep.OK = len(rep.Errors) == 0
	return rep
}

// --- export ---

// ScanSummary describes the ledger a ScanLedger call read.
type ScanSummary struct {
	FirstSeq uint64    // oldest retained record, archived or live
	FirstAt  time.Time // when it was appended
	// Anchor is what the oldest retained record links to. Empty means the
	// ledger is retained from its first record; otherwise older records were
	// deleted before archiving was enabled.
	Anchor  string
	LastSeq uint64 // newest committed record
}

// ScanLedger visits, in sequence order, the committed usage events whose
// window overlaps [from, to) in the archive in archiveDir and the live ledger
// in dir (either may be empty). It only reads files, so it can run next to a
// live vBilling or on a copy. Archived files must match archive.jsonl and each
// visited event's line must match its hash; missing sequence numbers fail the
// scan rather than shorten it. VerifyChain checks the whole chain.
func ScanLedger(dir, archiveDir string, from, to time.Time, fn func(*usage.Event) error) (ScanSummary, error) {
	sc := &eventScan{from: from, to: to, fn: fn}
	var live *liveLedger
	if dir != "" {
		l, err := loadLiveLedger(dir)
		if err != nil {
			return sc.sum, err
		}
		live = l
	}
	idx := emptyArchiveIndex()
	if archiveDir != "" {
		var err error
		if idx, err = readArchiveIndex(archiveDir); err != nil {
			return sc.sum, err
		}
		if len(idx.conflicts) > 0 {
			return sc.sum, errors.New(idx.conflicts[0])
		}
	}
	for _, e := range idx.entries {
		if err := sc.archived(archiveDir, e); err != nil {
			return sc.sum, err
		}
	}
	if live == nil {
		return sc.sum, nil
	}
	if sc.sum.FirstSeq == 0 {
		sc.sum.Anchor = live.anchor
	}
	for i, ls := range live.segs {
		if _, ok := idx.byName[ls.name]; ok {
			continue // read from the archive
		}
		f, err := os.Open(filepath.Join(live.dir, ls.name))
		if errors.Is(err, os.ErrNotExist) && archiveDir != "" {
			if e, ok := archivedSince(archiveDir, ls.name); ok { // compacted while this ran
				if err := sc.archived(archiveDir, e); err != nil {
					return sc.sum, err
				}
				continue
			}
		}
		if err != nil {
			return sc.sum, err
		}
		err = sc.segment(ls.name, f, i == len(live.segs)-1)
		f.Close()
		if err != nil {
			return sc.sum, err
		}
	}
	return sc.sum, nil
}

type eventScan struct {
	segReader
	from, to time.Time
	fn       func(*usage.Event) error
	next     uint64 // seq the next record must carry, 0 before the first
	sum      ScanSummary
}

func (sc *eventScan) gap(name string, got uint64) error {
	return fmt.Errorf("%s: expected seq %d, found %d: records are missing, so the export would be incomplete (run vbilling ledger verify)",
		name, sc.next, got)
}

// archived scans one archived segment. One written before the range holds no
// event in it (see Scan) and is skipped unread.
func (sc *eventScan) archived(dir string, e ArchiveEntry) error {
	if sc.next != 0 && e.First != sc.next {
		return sc.gap(e.Name, e.First)
	}
	if sc.sum.FirstSeq == 0 {
		sc.sum.FirstSeq, sc.sum.FirstAt, sc.sum.Anchor = e.First, e.FirstAt, e.Prev
	}
	if e.LastAt.Before(sc.from.Add(-scanSkew)) {
		sc.next, sc.sum.LastSeq = e.Last+1, e.Last
		return nil
	}
	zr, err := sc.openArchived(dir, e)
	if err != nil {
		return fmt.Errorf("%s: %w", e.File(), err)
	}
	if err := sc.segment(e.Name, zr, false); err != nil {
		return err
	}
	if sc.next != e.Last+1 {
		return fmt.Errorf("%s ends at seq %d, %s says %d", e.File(), sc.next-1, ArchiveIndexFile, e.Last)
	}
	return nil
}

// segment scans one segment. Events are passed on when the commit that closes
// their batch is read, so an uncommitted tail of the active segment is never
// exported.
func (sc *eventScan) segment(name string, r io.Reader, active bool) error {
	br := sc.lines(r)
	var pending []*usage.Event
	for {
		line, err := readLine(br, &sc.line)
		if err != nil && err != io.EOF {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(line) == 0 {
			return nil
		}
		if line[len(line)-1] != '\n' {
			if active {
				return nil // an append in progress
			}
			return fmt.Errorf("%s: torn line in a closed segment", name)
		}
		seq, ok := peekSeq(line)
		if !ok {
			return fmt.Errorf("%s: malformed line", name)
		}
		if sc.next != 0 && seq != sc.next {
			return sc.gap(name, seq)
		}
		sc.next = seq + 1
		if sc.sum.FirstSeq == 0 { // the oldest retained record is live
			hdr, _, err := checkLine(line)
			if err != nil {
				return fmt.Errorf("%s: seq %d: %w", name, seq, err)
			}
			sc.sum.FirstSeq, sc.sum.FirstAt = hdr.Seq, hdr.At
		}
		if isCommitLine(line) {
			for _, e := range pending {
				if err := sc.fn(e); err != nil {
					return err
				}
			}
			pending = pending[:0]
			sc.sum.LastSeq = seq
			continue
		}
		ws, we, ok := peekWindow(line)
		if !ok || !we.After(sc.from) || !ws.Before(sc.to) {
			continue
		}
		rec, _, err := parseLine(line)
		if err != nil {
			return fmt.Errorf("%s: seq %d: %w", name, seq, err)
		}
		if e := rec.Event; rec.Type == TypeEvent && e != nil && e.WindowEnd.After(sc.from) && e.WindowStart.Before(sc.to) {
			pending = append(pending, e)
		}
	}
}
