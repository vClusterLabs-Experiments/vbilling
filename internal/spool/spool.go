// Package spool is vBilling's durable usage ledger and delivery outbox.
//
// Usage is appended to segment files before any destination sees it. Each
// append is one atomic batch: the events of a window followed by a commit
// record, written with a single write and fsync. On restart, anything after
// the last commit is discarded and the window is collected again, so a crash
// can never produce half a window or a duplicate one.
//
// Every record is hash-chained to the previous one (sha256 over the record's
// JSON, which embeds the previous hash), so the ledger is tamper-evident and
// can be verified end to end. Each destination reads the ledger through its
// own cursor, which is what lets one slow or failing billing platform lag
// without blocking the others.
//
// Line format: "<sha256 hex>\t<record json>\n".
package spool

import (
	"bufio"
	"bytes"
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
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Record types.
const (
	TypeEvent  = "event"
	TypeCommit = "commit"
)

// SourceCollector is the source name the controller commits windows under.
const SourceCollector = "collector"

// ErrWindowCommitted is returned when a window at or before the source's
// watermark is appended again.
var ErrWindowCommitted = errors.New("window already committed")

// Record is one ledger entry.
type Record struct {
	Seq    uint64       `json:"seq"`
	Type   string       `json:"type"`
	At     time.Time    `json:"at"`
	Prev   string       `json:"prev"`
	Event  *usage.Event `json:"event,omitempty"`
	Commit *Commit      `json:"commit,omitempty"`

	Hash string `json:"-"`
}

// Commit closes an atomic batch.
type Commit struct {
	Source    string    `json:"source"`
	WindowEnd time.Time `json:"window_end,omitempty"`
	Events    int       `json:"events"`
	Note      string    `json:"note,omitempty"`
}

// Options tune the spool. Zero values pick defaults.
type Options struct {
	SegmentMaxRecords int
	SegmentMaxAge     time.Duration
	// Retention is how long closed segments are kept once every destination
	// has consumed them. Zero keeps data forever.
	Retention time.Duration
	// ArchiveDir, when set, receives a gzipped copy of each segment before
	// compaction deletes it, recorded in an index entry of its own (see
	// archive.go).
	ArchiveDir string
	// DedupeWindow bounds how long ingested event IDs are remembered.
	DedupeWindow time.Duration
	// NoSync skips fsync (tests only).
	NoSync bool
	Now    func() time.Time
}

func (o *Options) defaults() {
	if o.SegmentMaxRecords <= 0 {
		o.SegmentMaxRecords = 20000
	}
	if o.SegmentMaxAge <= 0 {
		o.SegmentMaxAge = time.Hour
	}
	if o.DedupeWindow <= 0 {
		o.DedupeWindow = 48 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

type segment struct {
	First    uint64    `json:"first"`
	Last     uint64    `json:"last"` // 0 when empty
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
	LastHash string    `json:"last_hash"`
	Size     int64     `json:"size"`
	Name     string    `json:"name"`

	index []offset // sparse seq -> byte offset, built lazily for closed segments
}

type offset struct {
	seq uint64
	off int64
}

const indexEvery = 256

type manifest struct {
	Segments   []*segment           `json:"segments"`
	Watermarks map[string]time.Time `json:"watermarks"`
	Anchor     string               `json:"anchor"` // prev hash of the first retained record
}

// Spool is safe for concurrent use.
type Spool struct {
	dir  string
	opts Options

	mu         sync.Mutex
	segs       []*segment
	active     *os.File
	seq        uint64 // last written
	committed  uint64 // readers see records <= committed
	head       string
	anchor     string
	watermarks map[string]time.Time
	cursors    map[string]uint64
	recentIDs  map[[16]byte]int64         // SHA-256 prefixes -> arrival Unix seconds
	deadIDs    map[string]map[string]bool // destination -> dead-lettered event IDs, loaded on first use
	changed    chan struct{}
	closed     bool

	// lastAt is the time of the newest record; floorAt is a floor set by
	// AdvanceClock. Record times never go below either, so they increase
	// with sequence numbers even if the wall clock steps back.
	lastAt  time.Time
	floorAt time.Time

	// cursorGen counts the explicit positionings of each cursor (see AckAt);
	// dests holds each destination's start, pause and detach marks
	// (destinations.go). Both are guarded by mu.
	cursorGen map[string]uint64
	dests     map[string]*DestinationState
}

// Open opens or creates a spool in dir and recovers its state.
func Open(dir string, opts Options) (*Spool, error) {
	opts.defaults()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	// A ledger restored halfway from its mirror is never opened (mirror_restore.go).
	if err := restoreInProgress(dir); err != nil {
		return nil, err
	}
	s := &Spool{
		dir:        dir,
		opts:       opts,
		watermarks: map[string]time.Time{},
		cursors:    map[string]uint64{},
		cursorGen:  map[string]uint64{},
		dests:      map[string]*DestinationState{},
		recentIDs:  map[[16]byte]int64{},
		changed:    make(chan struct{}),
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	if err := s.loadCursors(); err != nil {
		return nil, err
	}
	if err := s.loadDestinations(); err != nil {
		return nil, err
	}
	if err := s.loadDedupe(); err != nil {
		return nil, err
	}
	if err := s.openActive(); err != nil {
		return nil, err
	}
	return s, nil
}

func segName(first uint64) string { return fmt.Sprintf("seg-%020d.log", first) }

func parseSegName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".log") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".log"), 10, 64)
	return n, err == nil
}

// recover rebuilds in-memory state from the manifest and segment files,
// truncating any torn or uncommitted tail of the newest segment.
func (s *Spool) recover() error {
	var man manifest
	if b, err := os.ReadFile(filepath.Join(s.dir, "manifest.json")); err == nil {
		if err := json.Unmarshal(b, &man); err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read manifest: %w", err)
	}
	known := map[string]*segment{}
	for _, sg := range man.Segments {
		known[sg.Name] = sg
	}
	for k, v := range man.Watermarks {
		s.watermarks[k] = v
	}
	s.anchor = man.Anchor

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("list spool dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if _, ok := parseSegName(e.Name()); ok && !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // zero-padded, so lexical == numeric

	for i, name := range names {
		first, _ := parseSegName(name)
		info, err := os.Stat(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		last := i == len(names)-1
		if sg, ok := known[name]; ok && !last && sg.Size == info.Size() {
			s.segs = append(s.segs, sg)
			continue
		}
		sg, err := s.scanSegment(name, first, last)
		if err != nil {
			return err
		}
		s.segs = append(s.segs, sg)
	}

	// Derive seq/head/committed from the newest non-empty segment.
	for i := len(s.segs) - 1; i >= 0; i-- {
		if s.segs[i].Last != 0 {
			s.seq = s.segs[i].Last
			s.head = s.segs[i].LastHash
			break
		}
	}
	if s.seq == 0 && len(s.segs) > 0 {
		// Every segment is empty; continue numbering from the newest name.
		s.seq = s.segs[len(s.segs)-1].First - 1
		s.head = s.anchor
	}
	s.committed = s.seq
	for i := len(s.segs) - 1; i >= 0; i-- {
		if s.segs[i].Last != 0 {
			s.lastAt = s.segs[i].LastAt
			break
		}
	}
	return nil
}

// scanSegment reads a segment, building its metadata. For the newest segment
// it also truncates a torn line or an uncommitted tail.
func (s *Spool) scanSegment(name string, first uint64, newest bool) (*segment, error) {
	path := filepath.Join(s.dir, name)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sg := &segment{First: first, Name: name}
	r := bufio.NewReaderSize(f, 1<<20)
	var (
		off          int64
		goodEnd      int64 // end of the last committed record
		lastCommitSq uint64
		lastCommitAt time.Time
		lastCommitH  string
		pendingWM    = map[string]time.Time{}
	)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err == io.EOF {
			break
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if err == io.EOF {
			// A final line without a newline is a torn write.
			if !newest {
				return nil, fmt.Errorf("segment %s: torn line at offset %d in a closed segment", name, off)
			}
			break
		}
		rec, hash, perr := parseLine(line)
		if perr != nil {
			// A complete line that fails its hash is corruption or tampering,
			// not a crash artifact. Refuse to guess.
			return nil, fmt.Errorf("segment %s corrupt at offset %d: %v", name, off, perr)
		}
		if (rec.Seq-first)%indexEvery == 0 {
			sg.index = append(sg.index, offset{seq: rec.Seq, off: off})
		}
		off += int64(len(line))
		if rec.Type == TypeCommit && rec.Commit != nil {
			goodEnd = off
			lastCommitSq = rec.Seq
			lastCommitAt = rec.At
			lastCommitH = hash
			if !rec.Commit.WindowEnd.IsZero() {
				pendingWM[rec.Commit.Source] = rec.Commit.WindowEnd
			}
		}
		if sg.FirstAt.IsZero() {
			sg.FirstAt = rec.At
		}
	}
	if newest && info.Size() > goodEnd {
		// Drop the torn or uncommitted tail; that window gets re-collected.
		if err := f.Truncate(goodEnd); err != nil {
			return nil, fmt.Errorf("truncate %s: %w", name, err)
		}
		// Index entries past the truncation point are invalid.
		kept := sg.index[:0]
		for _, ix := range sg.index {
			if ix.off < goodEnd {
				kept = append(kept, ix)
			}
		}
		sg.index = kept
		off = goodEnd
	}
	if lastCommitSq != 0 {
		sg.Last = lastCommitSq
		sg.LastAt = lastCommitAt
		sg.LastHash = lastCommitH
	} else {
		sg.FirstAt = time.Time{}
	}
	sg.Size = off
	for k, v := range pendingWM {
		if v.After(s.watermarks[k]) {
			s.watermarks[k] = v
		}
	}
	return sg, nil
}

func parseLine(line []byte) (*Record, string, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	tab := bytes.IndexByte(line, '\t')
	if tab != 64 {
		return nil, "", errors.New("malformed line")
	}
	hash := string(line[:tab])
	body := line[tab+1:]
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, "", errors.New("hash mismatch")
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, "", err
	}
	rec.Hash = hash
	return &rec, hash, nil
}

func (s *Spool) openActive() error {
	if len(s.segs) == 0 {
		sg := &segment{First: s.seq + 1, Name: segName(s.seq + 1)}
		s.segs = append(s.segs, sg)
	}
	sg := s.segs[len(s.segs)-1]
	f, err := os.OpenFile(filepath.Join(s.dir, sg.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open active segment: %w", err)
	}
	s.active = f
	return nil
}

// Changed returns a channel closed on the next successful append.
func (s *Spool) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Watermark returns the last committed window end for a source.
func (s *Spool) Watermark(source string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.watermarks[source]
	return t, ok
}

// Committed returns the highest committed sequence number.
func (s *Spool) Committed() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// AppendWindow atomically appends a window's events for a source. A zero
// windowEnd skips watermark checks (ingested batches).
func (s *Spool) AppendWindow(source string, windowEnd time.Time, events []usage.Event, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("spool closed")
	}
	if !windowEnd.IsZero() {
		if wm, ok := s.watermarks[source]; ok && !windowEnd.After(wm) {
			return fmt.Errorf("%w: %s <= %s", ErrWindowCommitted, windowEnd.Format(time.RFC3339), wm.Format(time.RFC3339))
		}
	}
	return s.appendLocked(source, windowEnd.UTC(), events, note)
}

// AppendIngest appends externally supplied events, dropping IDs seen within
// the dedupe window. It returns the IDs that were accepted and the duplicates.
func (s *Spool) AppendIngest(source string, events []usage.Event) (accepted, duplicates []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, errors.New("spool closed")
	}
	now := s.opts.Now()
	seen := map[string]bool{}
	var keep []usage.Event
	for _, ev := range events {
		if _, dup := s.recentIDs[ingestIDHash(ev.ID)]; dup || seen[ev.ID] {
			duplicates = append(duplicates, ev.ID)
			continue
		}
		seen[ev.ID] = true
		keep = append(keep, ev)
	}
	if len(keep) == 0 {
		return nil, duplicates, nil
	}
	if err := s.appendLocked(source, time.Time{}, keep, ""); err != nil {
		return nil, nil, err
	}
	if err := s.recordDedupe(keep, now); err != nil {
		return nil, nil, err
	}
	for _, ev := range keep {
		accepted = append(accepted, ev.ID)
	}
	return accepted, duplicates, nil
}

// AppendBatch atomically appends events with no window and no ID memory.
// Per-request usage records use it: their IDs are deduplicated when
// windows are aggregated, which keeps memory bounded at any request rate.
func (s *Spool) AppendBatch(source string, events []usage.Event) (at time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return time.Time{}, errors.New("spool closed")
	}
	if err := s.appendLocked(source, time.Time{}, events, ""); err != nil {
		return time.Time{}, err
	}
	return s.lastAt, nil
}

// AdvanceClock guarantees every record appended from now on is stamped
// after t. An aggregator calls it before reading a time range, so nothing
// can later arrive inside a range it has already closed.
func (s *Spool) AdvanceClock(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t = t.UTC().Add(time.Nanosecond)
	if t.After(s.floorAt) {
		s.floorAt = t
	}
}

// Clock returns the time the next record would at least be stamped with.
func (s *Spool) Clock() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now().UTC()
	for _, t := range []time.Time{s.lastAt, s.floorAt} {
		if now.Before(t) {
			now = t
		}
	}
	return now
}

// ScanRecords visits committed event records whose window overlaps
// [from, to), with the time each was appended.
func (s *Spool) ScanRecords(from, to time.Time, fn func(ev *usage.Event, at time.Time) error) error {
	return s.ScanRecordsSeq(from, to, func(ev *usage.Event, at time.Time, _ uint64) error { return fn(ev, at) })
}

// ScanRecordsSeq is ScanRecords with each record's sequence number, which
// orders appends even when the clock gave two of them the same time.
func (s *Spool) ScanRecordsSeq(from, to time.Time, fn func(ev *usage.Event, at time.Time, seq uint64) error) error {
	committed, segs := s.snapshot()
	for i := range segs {
		sg := &segs[i]
		if sg.Last == 0 || sg.LastAt.Before(from.Add(-scanSkew)) {
			continue
		}
		var cbErr error
		inRange := func(line []byte) bool {
			ws, we, ok := peekWindow(line)
			return ok && we.After(from) && ws.Before(to)
		}
		err := s.readSegmentOpts(sg, sg.First, committed, false, inRange, func(r *Record) bool {
			if r.Type != TypeEvent || r.Event == nil {
				return true
			}
			e := r.Event
			if e.WindowEnd.After(from) && e.WindowStart.Before(to) {
				if cbErr = fn(e, r.At, r.Seq); cbErr != nil {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
		if cbErr != nil {
			return cbErr
		}
	}
	return nil
}

// ScanAppended visits committed event records appended in [from, to),
// whatever their window. Late-arriving usage is found this way.
func (s *Spool) ScanAppended(from, to time.Time, fn func(ev *usage.Event, at time.Time) error) error {
	return s.ScanAppendedSeq(from, to, func(ev *usage.Event, at time.Time, _ uint64) error { return fn(ev, at) })
}

// ScanAppendedSeq is ScanAppended with each record's sequence number.
func (s *Spool) ScanAppendedSeq(from, to time.Time, fn func(ev *usage.Event, at time.Time, seq uint64) error) error {
	committed, segs := s.snapshot()
	for i := range segs {
		sg := &segs[i]
		if sg.Last == 0 || sg.LastAt.Before(from) || !sg.FirstAt.Before(to) {
			continue
		}
		var cbErr error
		err := s.readSegmentOpts(sg, sg.First, committed, false, nil, func(r *Record) bool {
			if r.Type != TypeEvent || r.Event == nil || r.At.Before(from) || !r.At.Before(to) {
				return true
			}
			if cbErr = fn(r.Event, r.At, r.Seq); cbErr != nil {
				return false
			}
			return true
		})
		if err != nil {
			return err
		}
		if cbErr != nil {
			return cbErr
		}
	}
	return nil
}

func (s *Spool) appendLocked(source string, windowEnd time.Time, events []usage.Event, note string) error {
	if err := s.maybeRotateLocked(); err != nil {
		return err
	}
	now := s.opts.Now().UTC()
	if now.Before(s.lastAt) {
		now = s.lastAt
	}
	if now.Before(s.floorAt) {
		now = s.floorAt
	}
	sg := s.segs[len(s.segs)-1]
	startSize := sg.Size
	seq, head := s.seq, s.head

	var buf bytes.Buffer
	var newIndex []offset
	write := func(rec *Record) error {
		seq++
		rec.Seq = seq
		rec.At = now
		rec.Prev = head
		body, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		head = hex.EncodeToString(sum[:])
		if (seq-sg.First)%indexEvery == 0 {
			newIndex = append(newIndex, offset{seq: seq, off: startSize + int64(buf.Len())})
		}
		buf.WriteString(head)
		buf.WriteByte('\t')
		buf.Write(body)
		buf.WriteByte('\n')
		return nil
	}
	for i := range events {
		ev := events[i]
		if err := write(&Record{Type: TypeEvent, Event: &ev}); err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
	}
	if err := write(&Record{Type: TypeCommit, Commit: &Commit{Source: source, WindowEnd: windowEnd, Events: len(events), Note: note}}); err != nil {
		return err
	}

	if _, err := s.active.Write(buf.Bytes()); err != nil {
		_ = s.active.Truncate(startSize)
		return fmt.Errorf("write spool: %w", err)
	}
	if !s.opts.NoSync {
		if err := s.active.Sync(); err != nil {
			_ = s.active.Truncate(startSize)
			return fmt.Errorf("fsync spool: %w", err)
		}
	}

	if sg.Last == 0 {
		sg.FirstAt = now
	}
	sg.Last = seq
	sg.LastAt = now
	sg.LastHash = head
	sg.Size = startSize + int64(buf.Len())
	sg.index = append(sg.index, newIndex...)
	s.seq, s.head, s.committed = seq, head, seq
	s.lastAt = now
	if !windowEnd.IsZero() {
		s.watermarks[source] = windowEnd
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *Spool) maybeRotateLocked() error {
	sg := s.segs[len(s.segs)-1]
	if sg.Last == 0 {
		return nil
	}
	count := int(sg.Last-sg.First) + 1
	if count < s.opts.SegmentMaxRecords && s.opts.Now().Sub(sg.FirstAt) < s.opts.SegmentMaxAge {
		return nil
	}
	if err := s.active.Close(); err != nil {
		return err
	}
	next := &segment{First: s.seq + 1, Name: segName(s.seq + 1)}
	s.segs = append(s.segs, next)
	f, err := os.OpenFile(filepath.Join(s.dir, next.Name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	s.active = f
	return s.writeManifestLocked()
}

func (s *Spool) writeManifestLocked() error {
	man := manifest{Watermarks: s.watermarks, Anchor: s.anchor}
	for _, sg := range s.segs[:len(s.segs)-1] { // closed segments only
		man.Segments = append(man.Segments, sg)
	}
	return writeJSONAtomic(filepath.Join(s.dir, "manifest.json"), man, s.opts.NoSync)
}

// snapshot copies segment metadata under the lock so readers never race the
// writer on the active segment.
func (s *Spool) snapshot() (uint64, []segment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]segment, len(s.segs))
	for i, sg := range s.segs {
		out[i] = *sg
	}
	return s.committed, out
}

var (
	seqPrefix         = []byte(`{"seq":`)
	windowStartMarker = []byte(`"window_start":"`)
	windowEndMarker   = []byte(`"window_end":"`)
)

// peekWindow finds an event record's window without decoding it. Commit
// records have no window and report ok=false.
func peekWindow(line []byte) (ws, we time.Time, ok bool) {
	at := func(marker []byte) (time.Time, bool) {
		i := bytes.Index(line, marker)
		if i < 0 {
			return time.Time{}, false
		}
		rest := line[i+len(marker):]
		j := bytes.IndexByte(rest, '"')
		if j < 0 {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, string(rest[:j]))
		return t, err == nil
	}
	ws, ok1 := at(windowStartMarker)
	we, ok2 := at(windowEndMarker)
	return ws, we, ok1 && ok2
}

// parseLineNoVerify decodes a line without re-hashing it (queries only).
func parseLineNoVerify(line []byte) (*Record, error) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	if len(line) < 66 || line[64] != '\t' {
		return nil, errors.New("malformed line")
	}
	var rec Record
	if err := json.Unmarshal(line[65:], &rec); err != nil {
		return nil, err
	}
	rec.Hash = string(line[:64])
	return &rec, nil
}

// peekSeq extracts the sequence number without decoding the record. Record
// marshals Seq first, so the body always starts with {"seq":N,
func peekSeq(line []byte) (uint64, bool) {
	if len(line) < 65+len(seqPrefix) || !bytes.HasPrefix(line[65:], seqPrefix) {
		return 0, false
	}
	rest := line[65+len(seqPrefix):]
	end := bytes.IndexByte(rest, ',')
	if end <= 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(rest[:end]), 10, 64)
	return n, err == nil
}

// Read returns up to max committed records with seq > after.
func (s *Spool) Read(after uint64, max int) ([]Record, error) {
	committed, segs := s.snapshot()
	if after >= committed || max <= 0 {
		return nil, nil
	}
	// Empty segments only ever sit at the end, so this predicate is monotonic.
	i := sort.Search(len(segs), func(i int) bool { return segs[i].Last == 0 || segs[i].Last > after })
	var out []Record
	for ; i < len(segs) && len(out) < max; i++ {
		err := s.readSegment(&segs[i], after+1, committed, func(r *Record) bool {
			out = append(out, *r)
			return len(out) < max
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readSegment streams records with from <= seq <= upto from one segment.
// Delivery reads verify each line's hash; read-only queries can skip it and
// pass a prefilter that rejects lines before they are decoded.
func (s *Spool) readSegment(sg *segment, from, upto uint64, fn func(*Record) bool) error {
	return s.readSegmentOpts(sg, from, upto, true, nil, fn)
}

func (s *Spool) readSegmentOpts(sg *segment, from, upto uint64, verify bool, keep func(line []byte) bool, fn func(*Record) bool) error {
	if sg.Last == 0 || sg.Last < from || sg.First > upto {
		return nil
	}
	f, err := os.Open(filepath.Join(s.dir, sg.Name))
	if err != nil {
		return err
	}
	defer f.Close()

	var start int64
	for _, ix := range sg.index {
		if ix.seq > from {
			break
		}
		start = ix.off
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil // EOF or torn line beyond the committed point
		}
		// Skip cheaply until the first wanted record.
		seq, seqOK := peekSeq(line)
		if seqOK && seq < from {
			continue
		}
		if seqOK && seq > upto {
			return nil
		}
		if keep != nil && !keep(line) {
			continue
		}
		var rec *Record
		var perr error
		if verify {
			rec, _, perr = parseLine(line)
		} else {
			rec, perr = parseLineNoVerify(line)
		}
		if perr != nil {
			return fmt.Errorf("segment %s: %w", sg.Name, perr)
		}
		if rec.Seq > upto {
			return nil
		}
		if rec.Seq < from {
			continue
		}
		if !fn(rec) {
			return nil
		}
	}
}

// scanSkew bounds how far before its window an event can be appended. The
// ingest API rejects windows ending more than a few minutes in the future;
// the margin keeps Scan correct even with skewed clients.
const scanSkew = time.Hour

// Scan visits committed event records whose window overlaps [from, to).
// Events are appended at or after their window ends, so segments written
// well before `from` are skipped.
func (s *Spool) Scan(from, to time.Time, fn func(*usage.Event) error) error {
	committed, segs := s.snapshot()
	for i := range segs {
		sg := &segs[i]
		if sg.Last == 0 || sg.LastAt.Before(from.Add(-scanSkew)) {
			continue
		}
		var cbErr error
		inRange := func(line []byte) bool {
			ws, we, ok := peekWindow(line)
			return ok && we.After(from) && ws.Before(to)
		}
		err := s.readSegmentOpts(sg, sg.First, committed, false, inRange, func(r *Record) bool {
			if r.Type != TypeEvent || r.Event == nil {
				return true
			}
			e := r.Event
			if e.WindowEnd.After(from) && e.WindowStart.Before(to) {
				if cbErr = fn(e); cbErr != nil {
					return false
				}
			}
			return true
		})
		if err != nil {
			return err
		}
		if cbErr != nil {
			return cbErr
		}
	}
	return nil
}

// Cursor returns a destination's last acknowledged sequence number.
//
// A destination never seen before starts at the current head, and where it
// started is recorded (DestinationState.Start), so a backend that is new to
// this ledger receives no history unless asked (see SetCursor).
//
// A destination seen before keeps its cursor for as long as the ledger
// exists, also while it is not configured: the ledger alone cannot tell a
// backend that was removed from one that is down. One that is removed and
// configured again would therefore resume at its old cursor and receive
// everything recorded meanwhile. The dispatcher fences that: it marks the
// cursors of destinations that are not configured as detached, and a
// detached destination that comes back with a backlog delivers nothing until
// an operator decides where it starts (pipeline.Dispatcher.Start).
func (s *Spool) Cursor(dest string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursorLocked(dest)
}

// cursorLocked returns dest's cursor, creating it at the head (and recording
// that as where dest starts) for a destination never seen before.
func (s *Spool) cursorLocked(dest string) uint64 {
	c, ok := s.cursors[dest]
	if !ok {
		c = s.committed
		// The start first: a crash between the two writes leaves a start
		// without a cursor, which the next call replaces with the head of
		// that moment, never a cursor whose start is unknown.
		_ = s.setStartLocked(dest, c, StartCreated)
		s.cursors[dest] = c
		_ = s.writeCursorsLocked()
	}
	return c
}

// SetCursor positions a destination's cursor explicitly (an operator's
// rewind, replay or skip) and records that position as where the destination
// starts. A delivery that read the cursor before the move can no longer
// acknowledge past it (see AckAt), so it never undoes the move.
func (s *Spool) SetCursor(dest string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.committed {
		seq = s.committed
	}
	s.cursors[dest] = seq
	s.cursorGen[dest]++
	if err := s.writeCursorsLocked(); err != nil {
		return err
	}
	return s.setStartLocked(dest, seq, StartPositioned)
}

// Ack records that a destination has durably received everything <= seq.
func (s *Spool) Ack(dest string, seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.cursors[dest] {
		return nil
	}
	s.cursors[dest] = seq
	return s.writeCursorsLocked()
}

// AckAt is Ack for a delivery that read the cursor with Delivery: gen is the
// generation Delivery returned. If the cursor was positioned explicitly
// since (SetCursor), the acknowledgement is dropped and AckAt reports false:
// the delivery read from a position the operator has moved away from, and
// acknowledging it would move the cursor back to where that delivery ended.
func (s *Spool) AckAt(dest string, seq, gen uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursorGen[dest] != gen {
		return false, nil
	}
	if seq <= s.cursors[dest] {
		return true, nil
	}
	s.cursors[dest] = seq
	return true, s.writeCursorsLocked()
}

// FirstSeq returns the oldest retained sequence number (0 if empty).
func (s *Spool) FirstSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sg := range s.segs {
		if sg.Last != 0 {
			return sg.First
		}
	}
	return 0
}

func (s *Spool) writeCursorsLocked() error {
	return writeJSONAtomic(filepath.Join(s.dir, "cursors.json"), s.cursors, s.opts.NoSync)
}

func (s *Spool) loadCursors() error {
	b, err := os.ReadFile(filepath.Join(s.dir, "cursors.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.cursors); err != nil {
		return fmt.Errorf("parse cursors: %w", err)
	}
	for k, v := range s.cursors {
		if v > s.committed { // cursor ahead of a truncated tail
			s.cursors[k] = s.committed
		}
	}
	return nil
}

// --- ingest dedupe index ---

// IngestedRecently reports whether an event with this ID was appended
// through AppendIngest within the dedupe window.
func (s *Spool) IngestedRecently(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.recentIDs[ingestIDHash(id)]
	return ok
}

func ingestIDHash(id string) (key [16]byte) {
	sum := sha256.Sum256([]byte(id))
	copy(key[:], sum[:16])
	return key
}

func (s *Spool) recordDedupe(events []usage.Event, now time.Time) error {
	f, err := os.OpenFile(filepath.Join(s.dir, "ingest-ids.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	for _, ev := range events {
		key := ingestIDHash(ev.ID)
		s.recentIDs[key] = now.Unix()
		fmt.Fprintf(&buf, "v2\t%d\t%x\n", now.Unix(), key)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		return err
	}
	if !s.opts.NoSync {
		return f.Sync()
	}
	return nil
}

func (s *Spool) loadDedupe() error {
	path := filepath.Join(s.dir, "ingest-ids.log")
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	cutoff := s.opts.Now().Add(-s.opts.DedupeWindow).Unix()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		compact := strings.HasPrefix(line, "v2\t")
		if compact {
			line = strings.TrimPrefix(line, "v2\t")
		}
		ts, id, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			continue
		}
		if sec < cutoff {
			continue
		}
		key := ingestIDHash(id) // legacy files contain the original ID
		if compact {
			b, err := hex.DecodeString(id)
			if err != nil || len(b) != len(key) {
				continue
			}
			copy(key[:], b)
		}
		if previous, ok := s.recentIDs[key]; !ok || sec > previous {
			s.recentIDs[key] = sec
		}
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("read ingest IDs: %w", err)
	}
	return s.writeDedupeLocked()
}

// --- retention ---

// Compact deletes closed segments older than the retention period that
// every listed destination has already consumed. With ArchiveDir set, each
// one is archived first and deleted only once its archived copy is durable;
// a segment that fails to archive is kept and retried next time.
func (s *Spool) Compact(consumers []string) (removed int, err error) {
	if s.opts.ArchiveDir != "" {
		return s.compactArchived(consumers)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed, err = s.removeLocked(s.compactableLocked(consumers), nil)
	if derr := s.compactDedupeLocked(); err == nil {
		err = derr
	}
	return removed, err
}

func (s *Spool) compactDedupeLocked() error {
	cutoff := s.opts.Now().Add(-s.opts.DedupeWindow).Unix()
	pruned := false
	for id, at := range s.recentIDs {
		if at < cutoff {
			delete(s.recentIDs, id)
			pruned = true
		}
	}
	if !pruned {
		return nil
	}
	return s.writeDedupeLocked()
}

// writeDedupeLocked streams the compact index through an atomic replacement.
// Loading millions of IDs never needs a second in-memory copy of the file.
func (s *Spool) writeDedupeLocked() error {
	path := filepath.Join(s.dir, "ingest-ids.log")
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for id, at := range s.recentIDs {
		if _, err := fmt.Fprintf(w, "v2\t%d\t%x\n", at, id); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if !s.opts.NoSync {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	if !s.opts.NoSync {
		return syncDir(s.dir)
	}
	return nil
}

// --- verification ---

// VerifyReport summarizes a ledger integrity check.
type VerifyReport struct {
	OK       bool     `json:"ok"`
	Records  uint64   `json:"records"`
	Segments int      `json:"segments"`
	FirstSeq uint64   `json:"first_seq"`
	LastSeq  uint64   `json:"last_seq"`
	Head     string   `json:"head"`
	Errors   []string `json:"errors,omitempty"`
}

// Verify re-hashes every retained record and checks the chain links.
func (s *Spool) Verify() VerifyReport {
	committed, segs := s.snapshot()
	s.mu.Lock()
	prev := s.anchor
	s.mu.Unlock()

	rep := VerifyReport{Segments: len(segs)}
	expectSeq := uint64(0)
	fail := func(msg string) {
		if len(rep.Errors) < 20 {
			rep.Errors = append(rep.Errors, msg)
		}
	}
	for _, sg := range segs {
		f, err := os.Open(filepath.Join(s.dir, sg.Name))
		if err != nil {
			fail(err.Error())
			continue
		}
		r := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				break
			}
			rec, hash, perr := parseLine(line)
			if perr != nil {
				fail(fmt.Sprintf("%s: %v", sg.Name, perr))
				continue
			}
			if rec.Seq > committed {
				break
			}
			if expectSeq != 0 && rec.Seq != expectSeq {
				fail(fmt.Sprintf("seq gap: expected %d got %d", expectSeq, rec.Seq))
			}
			// The first retained record links to the anchor left behind by
			// retention; with no anchor (genesis) it must have no parent.
			if rec.Prev != prev {
				fail(fmt.Sprintf("chain break at seq %d", rec.Seq))
			}
			if rep.Records == 0 {
				rep.FirstSeq = rec.Seq
			}
			prev = hash
			expectSeq = rec.Seq + 1
			rep.Records++
			rep.LastSeq = rec.Seq
		}
		f.Close()
	}
	rep.Head = prev
	rep.OK = len(rep.Errors) == 0
	return rep
}

// --- dead letters ---

// DeadLetter is an event a destination permanently rejected.
type DeadLetter struct {
	At     time.Time   `json:"at"`
	Reason string      `json:"reason"`
	Event  usage.Event `json:"event"`
}

func (s *Spool) deadLetterPath(dest string) string {
	return filepath.Join(s.dir, "deadletter-"+dest+".log")
}

// AddDeadLetter parks a rejected event for operator review, once per event
// ID: a batch retried after a transient error rejects the same events
// again, and reconciliation subtracts each dead letter from the totals.
func (s *Spool) AddDeadLetter(dest string, ev usage.Event, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadIDs == nil {
		s.deadIDs = map[string]map[string]bool{}
	}
	ids := s.deadIDs[dest]
	if ids == nil {
		dls, err := s.deadLettersLocked(dest)
		if err != nil {
			return err
		}
		ids = map[string]bool{}
		for _, dl := range dls {
			ids[dl.Event.ID] = true
		}
		s.deadIDs[dest] = ids
	}
	if ids[ev.ID] {
		return nil
	}
	b, err := json.Marshal(DeadLetter{At: s.opts.Now().UTC(), Reason: reason, Event: ev})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.deadLetterPath(dest), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			// A crash may leave an incomplete final line. Keep the next
			// complete event separate so recovery never discards both.
			b = append([]byte{'\n'}, b...)
		}
	}
	line := append(b, '\n')
	if n, err := f.Write(line); err != nil {
		return err
	} else if n != len(line) {
		return io.ErrShortWrite
	}
	if !s.opts.NoSync {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !s.opts.NoSync {
		if err := syncDir(s.dir); err != nil {
			return err
		}
	}
	ids[ev.ID] = true
	return nil
}

// DeadLetters lists parked events for a destination, each event ID once
// (the first time it was parked).
func (s *Spool) DeadLetters(dest string) ([]DeadLetter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadLettersLocked(dest)
}

func (s *Spool) deadLettersLocked(dest string) ([]DeadLetter, error) {
	b, err := os.ReadFile(s.deadLetterPath(dest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []DeadLetter
	seen := map[string]bool{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var dl DeadLetter
		if err := json.Unmarshal(line, &dl); err == nil && !seen[dl.Event.ID] {
			seen[dl.Event.ID] = true
			out = append(out, dl)
		}
	}
	return out, nil
}

// ClearDeadLetters removes all parked events for a destination.
func (s *Spool) ClearDeadLetters(dest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.deadIDs, dest)
	err := os.Remove(s.deadLetterPath(dest))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && !s.opts.NoSync {
		return syncDir(s.dir)
	}
	return err
}

// RemoveDeadLetters removes only the IDs this replay delivered. Other
// pending letters, including ones parked concurrently, survive atomically.
func (s *Spool) RemoveDeadLetters(dest string, delivered []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dls, err := s.deadLettersLocked(dest)
	if err != nil {
		return err
	}
	drop := make(map[string]bool, len(delivered))
	for _, id := range delivered {
		drop[id] = true
	}
	var b bytes.Buffer
	ids := map[string]bool{}
	for _, dl := range dls {
		if drop[dl.Event.ID] {
			continue
		}
		if err := json.NewEncoder(&b).Encode(dl); err != nil {
			return err
		}
		ids[dl.Event.ID] = true
	}
	if err := writeFileAtomic(s.deadLetterPath(dest), b.Bytes(), s.opts.NoSync); err != nil {
		delete(s.deadIDs, dest) // a rename may already have succeeded
		return err
	}
	if s.deadIDs == nil {
		s.deadIDs = map[string]map[string]bool{}
	}
	s.deadIDs[dest] = ids
	return nil
}

// --- stats & lifecycle ---

// Stats is a point-in-time view of the spool.
type Stats struct {
	Committed  uint64               `json:"committed"`
	FirstSeq   uint64               `json:"first_seq"`
	Segments   int                  `json:"segments"`
	Bytes      int64                `json:"bytes"`
	Watermarks map[string]time.Time `json:"watermarks"`
	Cursors    map[string]uint64    `json:"cursors"`
	OldestAt   time.Time            `json:"oldest_at,omitempty"`
}

func (s *Spool) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Committed: s.committed, Segments: len(s.segs), Watermarks: map[string]time.Time{}, Cursors: map[string]uint64{}}
	for k, v := range s.watermarks {
		st.Watermarks[k] = v
	}
	for k, v := range s.cursors {
		st.Cursors[k] = v
	}
	for _, sg := range s.segs {
		st.Bytes += sg.Size
		if sg.Last != 0 && st.FirstSeq == 0 {
			st.FirstSeq = sg.First
			st.OldestAt = sg.FirstAt
		}
	}
	return st
}

// Close flushes the manifest and closes the active segment.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.writeManifestLocked()
	if cerr := s.active.Close(); err == nil {
		err = cerr
	}
	return err
}

func writeJSONAtomic(path string, v any, noSync bool) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, noSync)
}

func writeFileAtomic(path string, b []byte, noSync bool) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if !noSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
