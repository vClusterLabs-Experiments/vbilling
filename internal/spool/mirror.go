package spool

// Live mirror: a second copy of everything in DATA_DIR on another volume.
//
// The ledger is the only place usage exists between metering and delivery,
// and the state files next to it (cursors, held events, counter baselines,
// the metering source identity) are what lets vBilling continue without
// billing twice or skipping. All of it sits on one volume. The mirror keeps a
// copy on the archive volume, in <ARCHIVE_DIR>/mirror, a few seconds behind,
// so losing the data volume or its node loses seconds, not the ledger.
// "vbilling ledger restore" (mirror_restore.go) puts it back.
//
// # What spool.go guarantees, and the mirror relies on
//
//   - appendLocked writes a window (its events, then the commit record) with
//     one write, fsyncs it, and only then advances seq, committed and the
//     active segment's Size, Last and LastHash, all under the spool lock. A
//     failed write or fsync truncates the file back to Size. So under the
//     lock, the first Size bytes of every segment are final: committed,
//     durable, never rewritten, and they end with a commit record.
//   - Only the newest segment grows. A segment is closed by rotation, inside
//     appendLocked and before a window is written, so a closed segment never
//     changes and is never empty.
//   - recover and scanSegment cut off only what follows the last commit
//     record of the newest segment, which is never part of Size.
//   - Compaction deletes whole segments from the old end and moves the anchor
//     to the last hash it deleted.
//   - Every reader (recover, scanSegment, VerifyChain) accepts a newest
//     segment that ends in a torn line or in events without their commit
//     record and leaves that tail out. A closed segment must be complete.
//
// # The copy
//
// The mirror never reads a segment past its Size, so it only ever holds whole
// committed windows of the live ledger, byte for byte:
//
//	<mirror>/ledger/seg-*.log    same names and bytes as DATA_DIR/ledger
//	<mirror>/records/seg-*.log   the same for the per-request records ledger
//	<mirror>/state/<path>.v<N>   every other file of DATA_DIR, one file per version
//	<mirror>/MIRROR.json         the recovery point: which bytes belong together
//
// A pass appends the committed bytes a segment gained since the last pass
// and fsyncs them. It verifies every line it copies (hash, sequence number,
// link to the line before), so damage on the data volume never reaches the
// mirror. A new segment file appears only after the one before it is complete
// and fsynced. At every instant, also when the copier dies in the middle of a
// write, the mirror's ledger directory is therefore what a live ledger
// directory is after a crash: complete closed segments in order and a newest
// segment that may end in an incomplete window, which every reader drops.
//
// # The recovery point
//
// State files must fit the ledger they are restored with, and "fit" differs
// per file: a cursor, a held event or an ingest ID that is newer than the
// ledger loses usage (the destination skips windows that are metered again,
// a retried event is refused as a duplicate), while counter baselines or the
// record of a window a cluster's API missed that are older than the ledger
// bill usage twice or never fill it. Every one of these files is safe in
// exactly one situation: the one vBilling is already built for, a crash.
//
// So a pass takes a cut: the committed position of each ledger and the files
// of DATA_DIR as they are while no window is committed. It first tries
// without holding anything (read the committed sequence numbers, open the
// files, read the sequence numbers again: unchanged means no commit happened
// in between). If the ledgers never stand still for that long, it holds the
// spool locks for one capture, which reads only the data volume and never the
// mirror volume. Files are opened in the cut and read afterwards: every
// writer either replaces its file by rename (the open file keeps the version
// of the cut) or appends to it (the bytes up to the size seen in the cut are
// final). cursors.json is opened first, because held events and dead letters
// are written before the cursor moves past them: an older cursor with newer
// held events sends an event twice, the opposite order would lose it.
//
// The pass then copies the ledger bytes up to the cut and the changed files
// (a new version file, or the appended bytes of a large append-only file),
// fsyncs them, and replaces MIRROR.json by rename. That rename is the commit
// of the pass. A restore takes exactly what MIRROR.json names: the ledgers up
// to the recorded sizes and the recorded bytes of each file. What it gets is
// DATA_DIR as it was at the cut, so vBilling continues as after a crash at
// that moment: the same code path every restart takes. Bytes a later,
// unfinished pass added are ignored, like a window without its commit record.
//
// What that means per kind of state:
//
//   - Destination cursors are never past the ledger head. Restore and the
//     ledger's own cursor load still clamp one that is (a damaged mirror).
//     Records between a cursor and the head are sent again and deduplicated by
//     the billing platform through the event ID.
//   - Held events and dead letters cover everything the cursors passed, and
//     hold no sequence number beyond the head, which would shadow the records
//     that take those numbers after the restore.
//   - The ingest ID index holds only IDs whose events are in the ledger: an
//     index ahead of the ledger would refuse the retry of an event the ledger
//     lost. An ID the index misses costs a duplicate, deduplicated downstream.
//   - Prometheus counter baselines are two generations, the one in force
//     chosen by the ledger watermark. At the cut one of them is the watermark,
//     so counters continue exactly once from it.
//   - Pause spans, owner history, adapter and billing states and tokens do not
//     depend on a ledger position: they are as of the cut.
//
// # Retention, restarts, other ledgers
//
// When the live ledger compacts a segment away (after archiving it, if it
// archives), the segment leaves the mirror with the next pass, once the
// recovery point no longer names it. The copy's own manifest.json gets the
// new anchor right before the segments go: two steps, as in the live
// ledger's compaction. A copier that dies between them leaves an anchor one
// step ahead, which the next start corrects; a restore does not read that
// file, it takes the anchor from MIRROR.json.
//
// A copier that starts looks at what the volume holds before it writes
// (reconcile). Bytes after the recovery point are a pass that never finished
// and are cut off; a segment that is shorter than the recovery point says,
// missing or changed is damage, and the copy is cut back to what still
// matches the ledger and copied again. A mirror that holds records the live
// ledger does not have (another metering source identity, a longer or a
// different ledger) may be the only copy of a ledger whose data volume was
// lost. It is never changed: the whole directory is renamed to
// <mirror>-set-aside-<time>, reported by the health source
// "ledger-mirror-set-aside" until an operator restores or deletes it, and a
// new mirror is started.
//
// # Never in the way
//
// The copier runs on its own goroutine. Nothing a ledger commit, a delivery
// or start-up does waits for the mirror volume: a slow, full, read-only or
// missing volume makes passes fail or take long, which shows as the health
// source "ledger-mirror", vbilling_ledger_mirror_lag_seconds and
// vbilling_ledger_mirror_errors_total. The one thing start-up does with the
// mirror is RestoreIfLost (mirror_restore.go), and only when DATA_DIR holds
// no ledger.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

const (
	// MirrorManifestFile is the mirror's recovery point.
	MirrorManifestFile = "MIRROR.json"

	mirrorSubdir     = "mirror"
	mirrorStateDir   = "state"
	mirrorSetAside   = "-set-aside-"
	mirrorFormat     = 1
	ledgerManifest   = "manifest.json"
	cursorsFile      = "cursors.json"
	mirrorHealthName = "ledger-mirror"
	mirrorAsideName  = "ledger-mirror-set-aside"
	mirrorHealthKind = "storage"

	// appendMin is the size from which a state file is copied as a stream and
	// kept open, so that an append to it is copied as a delta.
	appendMin = 64 << 10
	// maxPins bounds how many such files the copier keeps open.
	maxPins = 128
	// tailBytes of a large file are remembered to check that the source
	// still starts with what was mirrored.
	tailBytes = 256
	// compareWindow is how much of a closed segment is compared with the
	// live one when the copier starts.
	compareWindow = 64 << 10
	// racyWindow: a file modified this shortly before it was last looked at
	// is read again even if its size and time did not change, because a
	// second change inside one timestamp step would look the same.
	racyWindow = 2 * time.Second
	// optimisticCuts is how often a cut is tried without holding the ledgers.
	optimisticCuts = 2
	// conflictWindow: a mirror another host wrote this recently is in use.
	conflictWindow = 10 * time.Minute
)

// MirrorDir is where the mirror of the ledger archived in archiveDir lives:
// its own directory inside the archive's per-pod directory, so two pods never
// share one. It is empty when there is no archive directory.
func MirrorDir(archiveDir string) string {
	if archiveDir == "" {
		return ""
	}
	return filepath.Join(archiveDir, mirrorSubdir)
}

// MirrorOptions configure a Mirror. Zero durations pick defaults.
type MirrorOptions struct {
	// DataDir is DATA_DIR: every file under it is mirrored.
	DataDir string
	// Dir is the mirror directory (see MirrorDir).
	Dir string
	// Ledgers are the open ledgers under DataDir, the usage ledger first.
	Ledgers []*Spool
	// Identity is the metering source identity of DataDir's ledger. A mirror
	// that records another one belongs to another ledger.
	Identity string
	// Exclude lists directories under DataDir that are not state, such as an
	// archive directory inside it.
	Exclude []string
	Health  *health.Registry
	Tel     *telemetry.Registry

	// MinInterval is the shortest time between two passes.
	MinInterval time.Duration
	// StateInterval is how often DATA_DIR is checked for changed files when
	// no window was committed.
	StateInterval time.Duration
	// Space reads the free space and the size of the volume a path is on,
	// in bytes (ok false when it cannot). The default asks the filesystem.
	Space func(path string) (free, size uint64, ok bool)
	// NoSync skips fsync (tests only).
	NoSync bool
	Now    func() time.Time
	Logf   func(format string, args ...any)
}

func (o *MirrorOptions) defaults() {
	if o.Space == nil {
		o.Space = volumeSpace
	}
	if o.MinInterval <= 0 {
		o.MinInterval = time.Second
	}
	if o.StateInterval <= 0 {
		o.StateInterval = 5 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
}

// MirrorManifest is MIRROR.json: the recovery point of a mirror.
type MirrorManifest struct {
	Version int `json:"version"`
	// Identity is the metering source identity of the mirrored ledger.
	Identity string `json:"identity,omitempty"`
	// Writer names the process that wrote this recovery point.
	Writer string `json:"writer"`
	Pass   uint64 `json:"pass"`
	// CutAt is the moment this recovery point is DATA_DIR of; WrittenAt is
	// when the pass that copied it ended.
	CutAt     time.Time      `json:"cut_at"`
	WrittenAt time.Time      `json:"written_at"`
	Ledgers   []MirrorLedger `json:"ledgers"`
	Files     []MirrorFile   `json:"files"`
}

// MirrorLedger is one ledger's position in a recovery point.
type MirrorLedger struct {
	Dir string `json:"dir"` // relative to DATA_DIR
	// FirstSegment to LastSegment are the retained segments; LastSize bytes
	// of the last one belong to the recovery point.
	FirstSegment string `json:"first_segment"`
	LastSegment  string `json:"last_segment"`
	LastSize     int64  `json:"last_size"`
	// Anchor is the hash the first retained record links to.
	Anchor     string               `json:"anchor"`
	Committed  uint64               `json:"committed"`
	Head       string               `json:"head"`
	LastAt     time.Time            `json:"last_at"`
	Watermarks map[string]time.Time `json:"watermarks,omitempty"`
}

// MirrorFile is one state file in a recovery point.
type MirrorFile struct {
	Path   string `json:"path"` // relative to DATA_DIR, with slashes
	File   string `json:"file"` // the version holding it, relative to <mirror>/state
	Size   int64  `json:"size"` // bytes of File that belong to the recovery point
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

func (m *MirrorManifest) ledger(dir string) *MirrorLedger {
	if m == nil {
		return nil
	}
	for i := range m.Ledgers {
		if m.Ledgers[i].Dir == dir {
			return &m.Ledgers[i]
		}
	}
	return nil
}

// Mirror keeps the copy. It is safe for concurrent use; a nil Mirror does
// nothing.
type Mirror struct {
	opts   MirrorOptions
	io     *mirrorIO
	host   string
	writer string
	setup  error // a configuration the mirror cannot work with

	passMu sync.Mutex // one pass at a time; guards everything below up to mu

	ledgers    []*mirrorLedger
	reconciled bool
	man        *MirrorManifest
	manBytes   []byte                  // MIRROR.json as this process last saw it
	tracked    map[string]*trackedFile // state files as mirrored, by path
	adoptable  map[string]MirrorFile   // recovery point entries not tracked yet
	gen        uint64                  // names the versions a pass writes
	warnedFS   bool
	announced  bool
	cuts       int           // optimistic tries per cut (tests lower it)
	racy       time.Duration // see racyWindow (tests lower it)
	large      int64         // see appendMin (tests lower it)

	heldCuts int // cuts taken while holding the ledgers

	// Test hooks.
	afterCapture func() // between capturing the files and reading the ledgers

	copied atomic.Int64 // bytes written to the mirror volume

	mu      sync.Mutex
	st      mirrorState
	started bool
	stopped chan struct{}
}

// mirrorState is what the reporter reads.
type mirrorState struct {
	running   time.Time // start of the pass in progress
	err       error
	hint      string
	point     bool     // a recovery point exists
	committed []uint64 // per ledger, in the recovery point
	files     int
	unread    []string        // files of DATA_DIR the last pass could not read
	sameFS    bool            // the mirror shares the ledger's filesystem
	pending   []pendingRecord // lag cache, per ledger
	reported  time.Time
	stalled   bool  // the last report found a pass hanging
	progress  int64 // copied at the last report
	progressT time.Time
	aside     time.Time // when set-aside copies were last looked for
}

type pendingRecord struct {
	after uint64
	at    time.Time
	known bool
}

// mirrorLedger is one ledger and its copy.
type mirrorLedger struct {
	sp  *Spool
	rel string // its directory relative to DATA_DIR
	dir string // its copy
	// segs are the mirrored segments, oldest first. All but the last are
	// complete; the last ends at a commit record.
	segs []mirrorSeg
	seq  uint64 // last mirrored record
	hash string // its hash
	// anchor is what the copy's manifest.json records.
	anchor      string
	hasManifest bool
}

type mirrorSeg struct {
	name  string
	first uint64
	size  int64
}

// trackedFile is a state file as the mirror holds it.
type trackedFile struct {
	file   string // version file, relative to <mirror>/state
	size   int64
	sum    string // sha256 of its first size bytes
	mode   uint32
	info   os.FileInfo // the source when it was captured
	seenAt time.Time

	// Large files: the source stays open, so that the same file grown is
	// recognized for certain and only what it gained is copied.
	pin  *os.File
	hash hash.Hash // running sha256 of the mirrored bytes
	tail []byte    // the last mirrored bytes
}

func (t *trackedFile) unpin() {
	if t != nil && t.pin != nil {
		t.pin.Close()
		t.pin, t.hash, t.tail = nil, nil, nil
	}
}

// ledgerView is a ledger at one instant.
type ledgerView struct {
	committed  uint64
	head       string
	anchor     string
	lastAt     time.Time
	watermarks map[string]time.Time
	segs       []segment // oldest first; the last is the active one
}

// mirrorViewLocked copies what the mirror needs. The caller holds s.mu.
func (s *Spool) mirrorViewLocked() ledgerView {
	v := ledgerView{committed: s.committed, head: s.head, anchor: s.anchor, lastAt: s.lastAt,
		watermarks: make(map[string]time.Time, len(s.watermarks)), segs: make([]segment, len(s.segs))}
	for k, t := range s.watermarks {
		v.watermarks[k] = t
	}
	for i, sg := range s.segs {
		v.segs[i] = *sg
		v.segs[i].index = nil
	}
	return v
}

func (s *Spool) mirrorView() ledgerView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mirrorViewLocked()
}

func (v *ledgerView) entry(dir string) MirrorLedger {
	last := v.segs[len(v.segs)-1]
	return MirrorLedger{Dir: dir, FirstSegment: v.segs[0].Name, LastSegment: last.Name, LastSize: last.Size,
		Anchor: v.anchor, Committed: v.committed, Head: v.head, LastAt: v.lastAt.UTC(), Watermarks: v.watermarks}
}

func sameLedgerEntry(a, b *MirrorLedger) bool {
	if a.Dir != b.Dir || a.FirstSegment != b.FirstSegment || a.LastSegment != b.LastSegment || a.LastSize != b.LastSize ||
		a.Anchor != b.Anchor || a.Committed != b.Committed || a.Head != b.Head || len(a.Watermarks) != len(b.Watermarks) {
		return false
	}
	for k, t := range a.Watermarks {
		if o, ok := b.Watermarks[k]; !ok || !o.Equal(t) {
			return false
		}
	}
	return true
}

// NewMirror returns a mirror of opts.DataDir in opts.Dir, or nil when there
// is no mirror directory. It touches no file: Run does, in the background.
func NewMirror(opts MirrorOptions) *Mirror {
	if opts.Dir == "" {
		return nil
	}
	opts.defaults()
	m := &Mirror{opts: opts, io: &mirrorIO{noSync: opts.NoSync}, tracked: map[string]*trackedFile{},
		adoptable: map[string]MirrorFile{}, cuts: optimisticCuts, racy: racyWindow, large: appendMin, stopped: make(chan struct{})}
	m.host, _ = os.Hostname()
	if m.host == "" {
		m.host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	m.writer = m.host + "/" + hex.EncodeToString(b[:])
	seen := map[string]bool{}
	for _, sp := range opts.Ledgers {
		rel, err := filepath.Rel(opts.DataDir, sp.dir)
		rel = filepath.ToSlash(rel)
		if err != nil || !filepath.IsLocal(filepath.FromSlash(rel)) || rel == "." || rel == mirrorStateDir || seen[rel] {
			m.setup = fmt.Errorf("the ledger in %s cannot be mirrored: it must be a directory of its own under %s, not named %q", sp.dir, opts.DataDir, mirrorStateDir)
			break
		}
		seen[rel] = true
		m.ledgers = append(m.ledgers, &mirrorLedger{sp: sp, rel: rel, dir: filepath.Join(opts.Dir, filepath.FromSlash(rel))})
	}
	if m.setup == nil && len(m.ledgers) == 0 {
		m.setup = errors.New("no ledger to mirror")
	}
	m.st.committed = make([]uint64, len(m.ledgers))
	m.st.pending = make([]pendingRecord, len(m.ledgers))
	return m
}

// --- changes on the mirror volume ---

// mirrorIO is every change the copier makes on the mirror volume, so that a
// test can stop it anywhere, the way a crash would.
type mirrorIO struct {
	noSync bool
	// before, when set, is asked before every change. For a write of n bytes
	// it returns how many of them still reach the file, then the change
	// fails with err.
	before func(op, path string, n int) (int, error)
}

func (o *mirrorIO) check(op, path string) error {
	if o.before == nil {
		return nil
	}
	_, err := o.before(op, path, 0)
	return err
}

func (o *mirrorIO) mkdirAll(dir string) error {
	if err := o.check("mkdir", dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o750)
}

func (o *mirrorIO) open(path string, flag int, perm os.FileMode) (*os.File, error) {
	if err := o.check("open", path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, flag, perm)
}

func (o *mirrorIO) write(f *os.File, b []byte) error {
	if o.before != nil {
		if n, err := o.before("write", f.Name(), len(b)); err != nil {
			if n > 0 && n <= len(b) {
				_, _ = f.Write(b[:n])
			}
			return err
		}
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (o *mirrorIO) sync(f *os.File) error {
	if err := o.check("sync", f.Name()); err != nil {
		return err
	}
	if o.noSync {
		return nil
	}
	return f.Sync()
}

func (o *mirrorIO) syncDir(dir string) error {
	if err := o.check("syncdir", dir); err != nil {
		return err
	}
	if o.noSync {
		return nil
	}
	return syncDir(dir)
}

func (o *mirrorIO) rename(from, to string) error {
	if err := o.check("rename", to); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func (o *mirrorIO) remove(path string) error {
	if err := o.check("remove", path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (o *mirrorIO) truncate(path string, size int64) error {
	if err := o.check("truncate", path); err != nil {
		return err
	}
	return os.Truncate(path, size)
}

// replace writes b to path through a temporary file: fsync, rename, fsync
// the directory.
func (o *mirrorIO) replace(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := o.open(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := o.write(f, b); err != nil {
		f.Close()
		return err
	}
	if err := o.sync(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := o.rename(tmp, path); err != nil {
		return err
	}
	return o.syncDir(filepath.Dir(path))
}

// --- running ---

// Run mirrors until ctx ends. It never returns an error: problems are
// reported to the health and telemetry registries.
func (m *Mirror) Run(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
	defer close(m.stopped)
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.loop(ctx)
	}()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// A pass in progress gets a moment. A mirror volume that does not
			// answer must not hold up a shutdown.
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return
		case <-t.C:
			m.report(false)
		}
	}
}

// Stop waits for Run to end and makes one last pass, so a clean stop leaves
// the mirror in step with the ledger. Call it after everything that writes to
// DATA_DIR has stopped. It gives up after a few seconds.
func (m *Mirror) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	started := m.started
	m.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-m.stopped:
	case <-time.After(10 * time.Second):
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if m.passMu.TryLock() { // not while a pass hangs on the volume
			m.passMu.Unlock()
			_ = m.Sync()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func (m *Mirror) loop(ctx context.Context) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		// Taken before the pass: a window committed while it runs wakes the
		// next one at once.
		wake := make([]<-chan struct{}, len(m.ledgers))
		for i, l := range m.ledgers {
			wake[i] = l.sp.Changed()
		}
		started := time.Now()
		err := m.Sync()
		wait := m.opts.MinInterval
		if err != nil {
			backoff = min(max(2*backoff, time.Second), time.Minute)
			wait = backoff
		} else {
			backoff = 0
			// A slow mirror volume gets fewer passes, not a busier copier.
			wait = max(wait, min(10*time.Since(started), time.Minute))
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		if err != nil {
			continue
		}
		tick := time.NewTimer(m.opts.StateInterval)
		woken := make(chan struct{}, 1)
		stop := make(chan struct{})
		for _, ch := range wake {
			go func() {
				select {
				case <-ch:
					select {
					case woken <- struct{}{}:
					default:
					}
				case <-stop:
				}
			}()
		}
		select {
		case <-ctx.Done():
		case <-woken:
		case <-tick.C:
		}
		close(stop)
		tick.Stop()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Sync makes one pass: it brings the mirror to the ledgers' committed
// position and DATA_DIR's current files, and records the recovery point.
func (m *Mirror) Sync() (err error) {
	if m == nil {
		return nil
	}
	m.passMu.Lock()
	defer m.passMu.Unlock()
	m.mu.Lock()
	m.st.running = m.opts.Now()
	m.mu.Unlock()
	defer func() { m.finish(err) }()
	if m.setup != nil {
		return m.setup
	}
	if err := m.pass(); err != nil {
		// Whatever the pass left on the volume is looked at again.
		m.reconciled = false
		return err
	}
	return nil
}

func (m *Mirror) pass() error {
	if !m.reconciled {
		if err := m.reconcile(); err != nil {
			return err
		}
		m.reconciled = true
	}
	c, err := m.cut()
	if err != nil {
		return err
	}
	defer c.close()
	m.mu.Lock()
	m.st.unread = c.unread
	m.mu.Unlock()
	// The archive shares this volume and comes first (see mirrorReserve).
	if err := m.room(m.toCopy(c)); err != nil {
		return err
	}
	m.gen++

	wrote := false
	dirs := map[string]bool{}
	for i, l := range m.ledgers {
		w, err := m.copyLedger(l, &c.views[i], dirs)
		if err != nil {
			return fmt.Errorf("copy the ledger in %s: %w", l.sp.dir, err)
		}
		wrote = wrote || w
	}
	files, obsolete, w, err := m.copyState(c, dirs)
	if err != nil {
		return err
	}
	wrote = wrote || w

	next := &MirrorManifest{Version: mirrorFormat, Identity: m.opts.Identity, Writer: m.writer, Pass: m.gen,
		CutAt: c.taken.UTC(), WrittenAt: m.opts.Now().UTC(), Files: files}
	same := m.man != nil && !wrote && len(m.man.Files) == len(files) && len(m.man.Ledgers) == len(m.ledgers)
	for i, l := range m.ledgers {
		e := c.views[i].entry(l.rel)
		next.Ledgers = append(next.Ledgers, e)
		if same {
			same = sameLedgerEntry(&m.man.Ledgers[i], &e)
		}
	}
	if !same {
		for dir := range dirs {
			if err := m.io.syncDir(dir); err != nil {
				return err
			}
		}
		if err := m.publish(next); err != nil {
			return err
		}
	}
	return m.cleanup(c, obsolete)
}

// publish replaces MIRROR.json. Everything it names is durable by now, so
// this rename is the commit of the pass.
func (m *Mirror) publish(next *MirrorManifest) error {
	path := filepath.Join(m.opts.Dir, MirrorManifestFile)
	onDisk, err := readOptional(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(onDisk, m.manBytes) {
		return m.conflict(onDisk)
	}
	b, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := m.io.replace(path, b, 0o640); err != nil {
		// The rename may have happened: look again before the next pass.
		return err
	}
	m.man, m.manBytes = next, b
	return nil
}

// conflict is the error for a MIRROR.json this process did not write.
func (m *Mirror) conflict(onDisk []byte) error {
	var other MirrorManifest
	who := "something else"
	if json.Unmarshal(onDisk, &other) == nil && other.Writer != "" {
		if other.Writer == m.writer {
			// This process's own rename, from a pass that failed after it.
			return fmt.Errorf("%s in %s is newer than the last pass that completed: looking at the mirror again", MirrorManifestFile, m.opts.Dir)
		}
		who = fmt.Sprintf("writer %s at %s", other.Writer, other.WrittenAt.Format(time.RFC3339))
	}
	return &mirrorError{msg: fmt.Sprintf("%s in %s was changed by %s while this vBilling mirrors into it", MirrorManifestFile, m.opts.Dir, who),
		hint: "two vBilling instances must never share one mirror directory: give each its own ARCHIVE_DIR"}
}

// mirrorError carries the hint shown with a failing health source.
type mirrorError struct {
	msg  string
	hint string
}

func (e *mirrorError) Error() string { return e.msg }

// cleanup removes what the new recovery point no longer needs: versions that
// were replaced and segments the live ledger compacted away.
func (m *Mirror) cleanup(c *cut, obsolete []string) error {
	for _, file := range obsolete {
		if err := m.io.remove(filepath.Join(m.opts.Dir, mirrorStateDir, filepath.FromSlash(file))); err != nil {
			return err
		}
	}
	for rel, t := range m.tracked {
		if !c.kept[rel] {
			t.unpin()
			delete(m.tracked, rel)
		}
	}
	for rel := range m.adoptable {
		if !c.kept[rel] {
			delete(m.adoptable, rel)
		}
	}
	for i, l := range m.ledgers {
		if err := m.dropCompacted(l, &c.views[i]); err != nil {
			return err
		}
	}
	return nil
}

// --- the cut ---

// cut is DATA_DIR at one instant between two commits.
type cut struct {
	at     time.Time // the system clock when the files were looked at
	taken  time.Time // opts.Now at that moment
	views  []ledgerView
	files  []fileCut
	unread []string        // files and directories the copier could not read
	kept   map[string]bool // paths in the recovery point this cut leads to
	locked bool            // taken while holding the ledgers
}

type fileCut struct {
	rel       string
	info      os.FileInfo
	size      int64
	f         *os.File // the source, open; nil when unchanged
	pinned    bool     // f is a tracked file's pin
	keep      bool     // f became a pin: do not close it
	unchanged bool
}

func (c *cut) close() {
	if c == nil {
		return
	}
	for i := range c.files {
		if fc := &c.files[i]; fc.f != nil && !fc.pinned && !fc.keep {
			fc.f.Close()
			fc.f = nil
		}
	}
}

// cut captures the files of DATA_DIR and the position of every ledger with no
// commit in between.
func (m *Mirror) cut() (*cut, error) {
	for attempt := 0; attempt < m.cuts; attempt++ {
		before := make([]uint64, len(m.ledgers))
		for i, l := range m.ledgers {
			before[i] = l.sp.Committed()
		}
		c, err := m.capture()
		if err != nil {
			return nil, err
		}
		if m.afterCapture != nil {
			m.afterCapture()
		}
		quiet := true
		for i, l := range m.ledgers {
			c.views = append(c.views, l.sp.mirrorView())
			quiet = quiet && c.views[i].committed == before[i]
		}
		if quiet {
			return c, nil
		}
		c.close()
	}
	// The ledgers did not stand still for one capture: hold them for one. It
	// reads the data volume only, never the mirror volume.
	for _, l := range m.ledgers {
		l.sp.mu.Lock()
	}
	c, err := m.capture()
	if err == nil {
		for _, l := range m.ledgers {
			c.views = append(c.views, l.sp.mirrorViewLocked())
		}
		c.locked = true
		m.heldCuts++
	}
	for i := len(m.ledgers) - 1; i >= 0; i-- {
		m.ledgers[i].sp.mu.Unlock()
	}
	return c, err
}

type stateEntry struct {
	rel  string
	info os.FileInfo
}

// capture lists the state files and opens the ones that may have changed.
func (m *Mirror) capture() (*cut, error) {
	// The system clock, whatever opts.Now is: it is compared with the times
	// the filesystem puts on files.
	c := &cut{at: time.Now(), taken: m.opts.Now()}
	entries, unread, err := m.stateFiles()
	if err != nil {
		return nil, err
	}
	c.unread = unread
	for _, e := range entries {
		fc, ok, err := m.captureFile(e)
		switch {
		case err != nil && errors.Is(err, os.ErrPermission):
			// Not readable right now. What the mirror holds of it stays.
			c.unread = append(c.unread, e.rel)
		case err != nil:
			c.close()
			return nil, err
		case ok:
			c.files = append(c.files, fc)
		}
	}
	return c, nil
}

// stateFiles lists every file of DATA_DIR that is mirrored as state, in the
// order it is captured: the ledgers' cursors first (see the package comment),
// then everything else by path. Segments and the ledgers' manifests are
// copied as ledgers; temporary files are left out.
func (m *Mirror) stateFiles() (entries []stateEntry, unread []string, err error) {
	ledgerDirs := map[string]bool{}
	for _, l := range m.ledgers {
		ledgerDirs[l.rel] = true
	}
	var walk func(rel string) error
	walk = func(rel string) error {
		dir := filepath.Join(m.opts.DataDir, filepath.FromSlash(rel))
		list, err := os.ReadDir(dir)
		if err != nil {
			if rel != "" && errors.Is(err, os.ErrNotExist) {
				return nil // removed meanwhile
			}
			if rel != "" && errors.Is(err, os.ErrPermission) {
				unread = append(unread, rel)
				return nil
			}
			return fmt.Errorf("list %s: %w", dir, err)
		}
		for _, e := range list {
			name := e.Name()
			if temporaryName(name) {
				continue
			}
			sub := path.Join(rel, name)
			if m.excluded(filepath.Join(dir, name)) {
				continue
			}
			if e.IsDir() {
				if name != "lost+found" {
					if err := walk(sub); err != nil {
						return err
					}
				}
				continue
			}
			if !e.Type().IsRegular() {
				continue
			}
			if ledgerDirs[rel] {
				if _, seg := parseSegName(name); seg || name == ledgerManifest || name == restoreMarker {
					continue
				}
			}
			info, err := e.Info()
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			entries = append(entries, stateEntry{rel: sub, info: info})
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, nil, err
	}
	cursors := func(rel string) bool {
		dir, name := path.Split(rel)
		return name == cursorsFile && ledgerDirs[strings.TrimSuffix(dir, "/")]
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ci, cj := cursors(entries[i].rel), cursors(entries[j].rel)
		if ci != cj {
			return ci
		}
		return entries[i].rel < entries[j].rel
	})
	return entries, unread, nil
}

// temporaryName reports files that are never state: what a writer renames
// into place (name.tmp, .name-random) and other hidden files.
func temporaryName(name string) bool {
	return strings.HasSuffix(name, ".tmp") || strings.HasPrefix(name, ".")
}

func (m *Mirror) excluded(full string) bool {
	within := func(dir string) bool {
		if dir == "" {
			return false
		}
		rel, err := filepath.Rel(dir, full)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	if within(m.opts.Dir) || strings.HasPrefix(full, m.opts.Dir+mirrorSetAside) {
		return true
	}
	for _, dir := range m.opts.Exclude {
		if within(dir) {
			return true
		}
	}
	return false
}

func sameStat(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}

// captureFile decides whether a file changed since it was mirrored, and opens
// it if it may have. ok is false for a file that is gone since it was listed:
// it is not in this cut.
func (m *Mirror) captureFile(e stateEntry) (fc fileCut, ok bool, err error) {
	fc = fileCut{rel: e.rel, info: e.info, size: e.info.Size()}
	t := m.tracked[e.rel]
	if t != nil && t.pin != nil {
		// The same open file, at least as long: an append, or nothing.
		if pi, perr := t.pin.Stat(); perr == nil && os.SameFile(pi, e.info) && pi.Size() >= t.size {
			fc.info, fc.size = pi, pi.Size()
			if pi.Size() == t.size && uint32(pi.Mode().Perm()) == t.mode {
				fc.unchanged = true
			} else {
				fc.f, fc.pinned = t.pin, true
			}
			return fc, true, nil
		}
	} else if t != nil && sameStat(t.info, e.info) && t.info.ModTime().Before(t.seenAt.Add(-m.racy)) {
		fc.unchanged, fc.size = true, t.size
		return fc, true, nil
	}
	f, err := os.Open(filepath.Join(m.opts.DataDir, filepath.FromSlash(e.rel)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fc, false, nil
		}
		return fc, false, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return fc, false, err
	}
	fc.f, fc.info, fc.size = f, info, info.Size()
	return fc, true, nil
}

// --- state files ---

func versionName(rel string, gen uint64) string { return fmt.Sprintf("%s.v%d", rel, gen) }

func (m *Mirror) statePath(file string) string {
	return filepath.Join(m.opts.Dir, mirrorStateDir, filepath.FromSlash(file))
}

// copyState brings the versions of every captured file up to the cut and
// returns the recovery point's file list, the versions it makes obsolete and
// whether anything was written.
func (m *Mirror) copyState(c *cut, dirs map[string]bool) (files []MirrorFile, obsolete []string, wrote bool, err error) {
	seen := map[string]bool{}
	for i := range c.files {
		fc := &c.files[i]
		seen[fc.rel] = true
		t, w, old, err := m.copyFile(fc, c.at, dirs)
		if err != nil {
			m.untrack(fc.rel)
			return nil, nil, false, fmt.Errorf("mirror %s: %w", fc.rel, err)
		}
		wrote = wrote || w
		if old != "" && old != t.file {
			obsolete = append(obsolete, old)
		}
		files = append(files, MirrorFile{Path: fc.rel, File: t.file, Size: t.size, SHA256: t.sum, Mode: t.mode})
	}
	// A file that left DATA_DIR leaves the recovery point. One that could
	// not be read this time stays in it as the mirror last copied it.
	unread := func(rel string) bool {
		for _, u := range c.unread {
			if rel == u || strings.HasPrefix(rel, u+"/") {
				return true
			}
		}
		return false
	}
	for rel, t := range m.tracked {
		switch {
		case seen[rel]:
		case unread(rel):
			seen[rel] = true
			files = append(files, MirrorFile{Path: rel, File: t.file, Size: t.size, SHA256: t.sum, Mode: t.mode})
		default:
			obsolete = append(obsolete, t.file)
			wrote = true
		}
	}
	for rel, e := range m.adoptable {
		switch {
		case seen[rel]:
		case unread(rel):
			seen[rel] = true
			files = append(files, e)
		default:
			obsolete = append(obsolete, e.File)
			wrote = true
		}
	}
	c.kept = seen
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, obsolete, wrote, nil
}

func (m *Mirror) untrack(rel string) {
	if t := m.tracked[rel]; t != nil {
		t.unpin()
		delete(m.tracked, rel)
	}
}

func (m *Mirror) pins() int {
	n := 0
	for _, t := range m.tracked {
		if t.pin != nil {
			n++
		}
	}
	return n
}

// copyFile makes sure the mirror holds a captured file as it is in the cut.
// old is a version this replaces.
func (m *Mirror) copyFile(fc *fileCut, at time.Time, dirs map[string]bool) (t *trackedFile, wrote bool, old string, err error) {
	t = m.tracked[fc.rel]
	if fc.unchanged {
		return t, false, "", nil
	}
	mode := uint32(fc.info.Mode().Perm())
	adopt, adoptable := m.adoptable[fc.rel]

	if fc.size < m.large {
		b := make([]byte, fc.size)
		if _, err := io.ReadFull(io.NewSectionReader(fc.f, 0, fc.size), b); err != nil {
			return nil, false, "", fmt.Errorf("read: %w", err)
		}
		digest := sha256.Sum256(b)
		sum := hex.EncodeToString(digest[:])
		switch {
		case t != nil && t.size == fc.size && t.sum == sum:
			// Rewritten with the same content, or only looked changed.
			wrote = t.mode != mode
			t.unpin()
			t.info, t.seenAt, t.mode = fc.info, at, mode
			return t, wrote, "", nil
		case t == nil && adoptable && adopt.Size == fc.size && adopt.SHA256 == sum && readMirrorFile(m.opts.Dir, adopt, io.Discard) == nil:
			// As the recovery point already holds it (after a restart), in a
			// version that is still intact.
			t = &trackedFile{file: adopt.File, size: fc.size, sum: sum, mode: mode, info: fc.info, seenAt: at}
			m.tracked[fc.rel] = t
			delete(m.adoptable, fc.rel)
			return t, adopt.Mode != mode, "", nil
		}
		if t != nil {
			old = t.file
		} else if adoptable {
			old = adopt.File
		}
		file := versionName(fc.rel, m.gen)
		if err := m.writeVersion(file, mode, bytes.NewReader(b), nil, dirs); err != nil {
			return nil, false, "", err
		}
		t.unpin()
		t = &trackedFile{file: file, size: fc.size, sum: sum, mode: mode, info: fc.info, seenAt: at}
		m.tracked[fc.rel] = t
		delete(m.adoptable, fc.rel)
		return t, true, old, nil
	}

	// A large file. The same open file, grown: copy what it gained.
	if t != nil && fc.pinned {
		if ok, err := m.sameStart(t, fc.f); err != nil {
			return nil, false, "", err
		} else if ok {
			grown := fc.size > t.size
			if err := m.appendVersion(t, fc.f, fc.size); err != nil {
				return nil, false, "", err
			}
			wrote = grown || t.mode != mode
			t.info, t.seenAt, t.mode = fc.info, at, mode
			return t, wrote, "", nil
		}
		// Rewritten in place: copy it whole, from a handle of its own.
		f, err := os.Open(filepath.Join(m.opts.DataDir, filepath.FromSlash(fc.rel)))
		if err != nil {
			return nil, false, "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, false, "", err
		}
		fc.f, fc.pinned, fc.info, fc.size = f, false, info, info.Size()
	}
	if t == nil && adoptable && adopt.Size >= m.large && fc.size >= adopt.Size {
		// After a restart: if the source still starts with what the recovery
		// point holds, continue that version instead of copying it again.
		if at2, err := m.adoptLarge(fc, adopt, mode, at); err != nil {
			return nil, false, "", err
		} else if at2 != nil {
			grown := fc.size > at2.size
			m.tracked[fc.rel] = at2
			delete(m.adoptable, fc.rel)
			if err := m.appendVersion(at2, fc.f, fc.size); err != nil {
				return nil, false, "", err
			}
			if at2.pin == fc.f {
				fc.keep = true
			}
			return at2, grown || adopt.Mode != mode, "", nil
		}
	}
	if t != nil {
		old = t.file
	} else if adoptable {
		old = adopt.File
	}
	file := versionName(fc.rel, m.gen)
	nt := &trackedFile{file: file, size: fc.size, mode: mode, info: fc.info, seenAt: at, hash: sha256.New()}
	if err := m.writeVersion(file, mode, io.NewSectionReader(fc.f, 0, fc.size), nt, dirs); err != nil {
		return nil, false, "", err
	}
	nt.sum = hex.EncodeToString(nt.hash.Sum(nil))
	t.unpin()
	if m.pins() < maxPins {
		nt.pin, fc.keep = fc.f, true
	} else {
		nt.hash, nt.tail = nil, nil
	}
	m.tracked[fc.rel] = nt
	delete(m.adoptable, fc.rel)
	return nt, true, old, nil
}

// feed adds mirrored bytes to a large file's running hash and tail.
func (t *trackedFile) feed(b []byte) {
	t.hash.Write(b)
	if len(b) >= tailBytes {
		t.tail = append(t.tail[:0], b[len(b)-tailBytes:]...)
		return
	}
	t.tail = append(t.tail, b...)
	if len(t.tail) > tailBytes {
		t.tail = append(t.tail[:0], t.tail[len(t.tail)-tailBytes:]...)
	}
}

// writeVersion writes a new version file and fsyncs it. Nothing names it
// until the next MIRROR.json does, so it needs no temporary name.
func (m *Mirror) writeVersion(file string, mode uint32, r io.Reader, t *trackedFile, dirs map[string]bool) error {
	dst := m.statePath(file)
	if err := m.io.mkdirAll(filepath.Dir(dst)); err != nil {
		return err
	}
	f, err := m.io.open(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode&0o777)|0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := m.stream(f, r, t); err != nil {
		return err
	}
	if err := m.io.sync(f); err != nil {
		return err
	}
	for dir := filepath.Dir(dst); len(dir) >= len(m.opts.Dir) && !dirs[dir]; dir = filepath.Dir(dir) {
		dirs[dir] = true
	}
	return f.Close()
}

func (m *Mirror) stream(dst *os.File, r io.Reader, t *trackedFile) error {
	buf := make([]byte, 1<<20)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := m.io.write(dst, buf[:n]); werr != nil {
				return werr
			}
			m.copied.Add(int64(n))
			if t != nil {
				t.feed(buf[:n])
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
	}
}

// sameStart reports whether the source still holds, right before the size
// that was mirrored, the bytes that were mirrored there.
func (m *Mirror) sameStart(t *trackedFile, src *os.File) (bool, error) {
	if len(t.tail) == 0 {
		return t.size == 0, nil
	}
	b := make([]byte, len(t.tail))
	if _, err := src.ReadAt(b, t.size-int64(len(b))); err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(b, t.tail), nil
}

// appendVersion copies the bytes a large file gained to its version. The
// recovery point names the version with its old size until the next one is
// published, so the bytes added here are invisible until then.
func (m *Mirror) appendVersion(t *trackedFile, src *os.File, upto int64) error {
	if upto == t.size {
		return nil
	}
	dst := m.statePath(t.file)
	f, err := m.io.open(dst, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return err
	} else if st.Size() != t.size {
		return fmt.Errorf("%s is %d bytes, the copier wrote %d: it was changed underneath", dst, st.Size(), t.size)
	}
	if err := m.stream(f, io.NewSectionReader(src, t.size, upto-t.size), t); err != nil {
		return err
	}
	if err := m.io.sync(f); err != nil {
		return err
	}
	t.size, t.sum = upto, hex.EncodeToString(t.hash.Sum(nil))
	return f.Close()
}

// adoptLarge checks, after a restart, that a large file still starts with
// the bytes of the version the recovery point names, and returns that
// version to continue. nil means it does not.
func (m *Mirror) adoptLarge(fc *fileCut, e MirrorFile, mode uint32, at time.Time) (*trackedFile, error) {
	mine, err := os.Open(m.statePath(e.File))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer mine.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(mine, 0, e.Size)); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return nil, nil // the mirror's copy is damaged: write a new one
	}
	t := &trackedFile{file: e.File, size: e.Size, sum: e.SHA256, mode: mode, info: fc.info, seenAt: at, hash: sha256.New()}
	buf := make([]byte, 1<<20)
	r := io.NewSectionReader(fc.f, 0, e.Size)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			t.feed(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if hex.EncodeToString(t.hash.Sum(nil)) != e.SHA256 {
		return nil, nil
	}
	if m.pins() < maxPins {
		t.pin = fc.f
	}
	return t, nil
}

// --- ledgers ---

// errBehindRetention: the live ledger compacted away segments the mirror had
// not finished. The mirror follows retention: its copy starts over at the
// oldest retained segment.
var errBehindRetention = errors.New("the ledger compacted past the mirror")

// copyLedger appends to the copy what the ledger committed up to the view.
func (m *Mirror) copyLedger(l *mirrorLedger, v *ledgerView, dirs map[string]bool) (wrote bool, err error) {
	wrote, err = m.appendLedger(l, v, dirs)
	if errors.Is(err, errBehindRetention) {
		// The recovery point named segments that are going: it goes first.
		if err := m.dropManifest(); err != nil {
			return false, err
		}
		for len(l.segs) > 0 {
			last := l.segs[len(l.segs)-1]
			if err := m.io.remove(filepath.Join(l.dir, last.name)); err != nil {
				return false, err
			}
			l.segs = l.segs[:len(l.segs)-1]
		}
		l.seq, l.hash = 0, ""
		m.opts.Logf("[mirror] the ledger in %s compacted segments the mirror had not copied: its copy starts again at the oldest retained segment", l.sp.dir)
		wrote, err = m.appendLedger(l, v, dirs)
		wrote = true
	}
	return wrote, err
}

func (m *Mirror) appendLedger(l *mirrorLedger, v *ledgerView, dirs map[string]bool) (wrote bool, err error) {
	live := v.segs
	index := map[string]int{}
	for i := range live {
		index[live[i].Name] = i
	}
	start := 0
	var w chainWalk
	switch {
	case len(l.segs) == 0:
		l.seq, l.hash = live[0].First-1, v.anchor
		if !l.hasManifest || l.anchor != v.anchor {
			if err := m.writeLedgerManifest(l, v.anchor, v.watermarks); err != nil {
				return false, err
			}
			wrote = true
		}
	default:
		last := l.segs[len(l.segs)-1]
		i, ok := index[last.name]
		switch {
		case ok:
			start = i
		case last.first < live[0].First && l.seq+1 == live[0].First && l.hash == v.anchor:
			// Everything mirrored was compacted away, and the oldest retained
			// segment follows it.
		case last.first < live[0].First:
			return false, errBehindRetention
		default:
			return false, fmt.Errorf("the mirror holds %s, which the ledger does not have", last.name)
		}
	}
	w.start(l.hash, l.seq+1)

	for i := start; i < len(live); i++ {
		sg := &live[i]
		have := int64(0)
		if n := len(l.segs); n > 0 && l.segs[n-1].name == sg.Name {
			have = l.segs[n-1].size
		}
		if have > sg.Size {
			return wrote, fmt.Errorf("the mirror holds %d bytes of %s, the ledger committed %d", have, sg.Name, sg.Size)
		}
		if have == sg.Size {
			continue
		}
		if err := m.appendSegment(l, &w, sg, have); err != nil {
			return true, err
		}
		wrote = true
		dirs[l.dir] = true
	}
	if l.seq != v.committed || l.hash != v.head {
		return wrote, fmt.Errorf("the copy ends at seq %d (%s), the ledger committed seq %d (%s)", l.seq, l.hash, v.committed, v.head)
	}
	return wrote, nil
}

// appendSegment appends the bytes [have, sg.Size) of a live segment to its
// copy, checking every line first, and fsyncs the copy. Only then may the
// next segment be created.
func (m *Mirror) appendSegment(l *mirrorLedger, w *chainWalk, sg *segment, have int64) error {
	src, err := os.Open(filepath.Join(l.sp.dir, sg.Name))
	if err != nil {
		return err
	}
	defer src.Close()
	if err := m.io.mkdirAll(l.dir); err != nil {
		return err
	}
	path := filepath.Join(l.dir, sg.Name)
	dst, err := m.io.open(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer dst.Close()
	if st, err := dst.Stat(); err != nil {
		return err
	} else if st.Size() != have {
		return fmt.Errorf("%s is %d bytes, the copier wrote %d: it was changed underneath", path, st.Size(), have)
	}

	var buf []byte
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		// Before the write: if it fails halfway, the file's length is not
		// known any more and the copy is looked at again.
		if n := len(l.segs); n == 0 || l.segs[n-1].name != sg.Name {
			l.segs = append(l.segs, mirrorSeg{name: sg.Name, first: sg.First, size: have})
		}
		if err := m.io.write(dst, buf); err != nil {
			return err
		}
		have += int64(len(buf))
		m.copied.Add(int64(len(buf)))
		l.segs[len(l.segs)-1].size = have
		l.seq, l.hash = w.last, w.prev
		buf = buf[:0]
		return nil
	}
	br := w.lines(io.NewSectionReader(src, have, sg.Size-have))
	for {
		line, err := readLine(br, &w.line)
		if err != nil && err != io.EOF {
			return fmt.Errorf("read %s: %w", sg.Name, err)
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' {
			return fmt.Errorf("%s: its committed bytes end in the middle of a line", sg.Name)
		}
		if _, ok := w.record(sg.Name, line); !ok || len(w.errs) > 0 {
			return &mirrorError{msg: fmt.Sprintf("the ledger itself does not verify, nothing of it was copied: %s", w.errs[0]),
				hint: "run vbilling ledger verify on DATA_DIR: the mirror only copies records that match their hash and link to the one before"}
		}
		buf = append(buf, line...)
		if len(buf) >= 4<<20 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	// The copy must end where the ledger says this segment ends, with the
	// commit record that closes its last window.
	if w.last != sg.Last || w.prev != sg.LastHash {
		return fmt.Errorf("%s: its committed bytes end at seq %d (%s), the ledger says %d (%s)", sg.Name, w.last, w.prev, sg.Last, sg.LastHash)
	}
	if err := flush(); err != nil {
		return err
	}
	if err := m.io.sync(dst); err != nil {
		return err
	}
	return dst.Close()
}

// writeLedgerManifest writes the copy's manifest.json: the anchor of its
// oldest segment, which a verification of the copy starts from.
func (m *Mirror) writeLedgerManifest(l *mirrorLedger, anchor string, watermarks map[string]time.Time) error {
	if err := m.io.mkdirAll(l.dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(manifest{Watermarks: watermarks, Anchor: anchor}, "", "  ")
	if err != nil {
		return err
	}
	if err := m.io.replace(filepath.Join(l.dir, ledgerManifest), b, 0o640); err != nil {
		l.hasManifest = false
		return err
	}
	l.anchor, l.hasManifest = anchor, true
	return nil
}

// dropCompacted removes from the copy the segments the live ledger compacted
// away (after archiving them, when it archives), once the recovery point no
// longer names them. The anchor moves first and the segments follow in the
// same breath, as in the live ledger's own compaction.
func (m *Mirror) dropCompacted(l *mirrorLedger, v *ledgerView) error {
	first := v.segs[0].First
	n := 0
	for n < len(l.segs) && l.segs[n].first < first {
		n++
	}
	if n == 0 {
		return nil
	}
	if err := m.writeLedgerManifest(l, v.anchor, v.watermarks); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err := m.io.remove(filepath.Join(l.dir, l.segs[0].name)); err != nil {
			return err
		}
		l.segs = l.segs[1:]
	}
	return m.io.syncDir(l.dir)
}

// dropManifest removes the recovery point before the copier takes away
// something it names, so that a restore finds no recovery point rather than
// a broken one.
func (m *Mirror) dropManifest() error {
	if m.man == nil && m.manBytes == nil {
		return nil
	}
	if err := m.io.remove(filepath.Join(m.opts.Dir, MirrorManifestFile)); err != nil {
		return err
	}
	if err := m.io.syncDir(m.opts.Dir); err != nil {
		return err
	}
	m.man, m.manBytes = nil, nil
	m.mu.Lock()
	m.st.point = false
	m.mu.Unlock()
	return nil
}

// --- starting on what the volume holds ---

// reconcile looks at what the mirror volume holds before the first pass and
// after a failed one. A copy that is part of the live ledger is continued,
// after cutting off what a crash left unfinished or damage made unusable. A
// copy that holds records the live ledger does not have is another ledger,
// perhaps the only copy of one whose data volume was lost: it is never
// changed, only set aside.
func (m *Mirror) reconcile() error {
	if err := m.io.mkdirAll(m.opts.Dir); err != nil {
		return err
	}
	m.checkSameFilesystem()

	path := filepath.Join(m.opts.Dir, MirrorManifestFile)
	raw, err := readOptional(path)
	if err != nil {
		return err
	}
	var man *MirrorManifest
	if len(raw) > 0 {
		man = &MirrorManifest{}
		if err := json.Unmarshal(raw, man); err != nil || man.Version != mirrorFormat {
			// Not a recovery point this version can continue.
			return m.setAside(nil, fmt.Sprintf("its %s cannot be read", MirrorManifestFile))
		}
		// A recovery point that changed since this process last saw it, and
		// not by this process (whose own rename may have outlived a failed
		// pass): somebody else is writing here right now.
		if m.manBytes != nil && !bytes.Equal(raw, m.manBytes) && man.Writer != m.writer &&
			m.opts.Now().Sub(man.WrittenAt) < conflictWindow {
			return m.conflict(raw)
		}
	}

	views := make([]ledgerView, len(m.ledgers))
	for i, l := range m.ledgers {
		views[i] = l.sp.mirrorView()
	}
	reason := ""
	if man != nil && man.Identity != "" && m.opts.Identity != "" && man.Identity != m.opts.Identity {
		reason = fmt.Sprintf("it mirrors the ledger of metering source %s, DATA_DIR holds %s", man.Identity, m.opts.Identity)
	}
	for i, l := range m.ledgers {
		if reason != "" {
			break
		}
		if reason, err = m.foreign(l, man.ledger(l.rel), &views[i], man != nil); err != nil {
			return err
		}
	}
	if reason != "" {
		return m.setAside(man, reason)
	}

	m.man, m.manBytes = man, raw
	if man != nil && man.Pass > m.gen {
		m.gen = man.Pass
	}
	for i, l := range m.ledgers {
		if err := m.repairLedger(l, man.ledger(l.rel), &views[i]); err != nil {
			return err
		}
	}
	if err := m.reconcileState(); err != nil {
		return err
	}
	m.mu.Lock()
	m.st.point = m.man != nil
	for i, l := range m.ledgers {
		m.st.committed[i] = 0
		if e := m.man.ledger(l.rel); e != nil {
			m.st.committed[i] = e.Committed
		}
	}
	if m.man != nil {
		m.st.files = len(m.man.Files)
	}
	m.mu.Unlock()
	return nil
}

// checkSameFilesystem warns once when the mirror is on the ledger's own
// filesystem: it then protects against a damaged file, not a lost volume.
func (m *Mirror) checkSameFilesystem() {
	a, errA := os.Stat(m.opts.DataDir)
	b, errB := os.Stat(m.opts.Dir)
	if errA != nil || errB != nil {
		return
	}
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	same := okA && okB && uint64(sa.Dev) == uint64(sb.Dev) //nolint:unconvert // Dev is int32 on macOS
	m.mu.Lock()
	m.st.sameFS = same
	m.mu.Unlock()
	if same && !m.warnedFS {
		m.warnedFS = true
		m.opts.Logf("[mirror] WARNING: the ledger mirror %s is on the same filesystem as the ledger in %s: it does not survive losing that volume. Mount a volume of its own at ARCHIVE_DIR, on other storage than the data volume",
			m.opts.Dir, m.opts.DataDir)
	}
}

// mirrored lists the copy's segments with their sizes, limited to what the
// recovery point names: bytes after it belong to a pass that never finished.
func mirrored(dir string, e *MirrorLedger) ([]mirrorSeg, error) {
	list, err := listSegments(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lastFirst, limited := uint64(0), false
	if e != nil {
		lastFirst, limited = parseSegName(e.LastSegment)
	}
	var out []mirrorSeg
	for _, s := range list {
		if limited && s.first > lastFirst {
			break
		}
		info, err := os.Stat(filepath.Join(dir, s.name))
		if err != nil {
			return nil, err
		}
		size := info.Size()
		if limited && s.first == lastFirst && size > e.LastSize {
			size = e.LastSize
		}
		out = append(out, mirrorSeg{name: s.name, first: s.first, size: size})
	}
	return out, nil
}

// foreign reports why a copy is not part of the live ledger, "" if it is.
// It changes nothing.
func (m *Mirror) foreign(l *mirrorLedger, e *MirrorLedger, v *ledgerView, hasPoint bool) (string, error) {
	segs, err := mirrored(l.dir, e)
	if err != nil {
		return "", err
	}
	live := map[string]*segment{}
	for i := range v.segs {
		live[v.segs[i].Name] = &v.segs[i]
	}
	liveFirst := v.segs[0].First
	var older *mirrorSeg // the newest mirrored segment the ledger compacted away
	for i := range segs {
		s := &segs[i]
		lv, ok := live[s.name]
		if !ok {
			if s.first < liveFirst {
				older = s
				continue
			}
			return fmt.Sprintf("it holds %s/%s, which the ledger in DATA_DIR does not have", l.rel, s.name), nil
		}
		if s.size > lv.Size {
			return fmt.Sprintf("it holds %d bytes of %s/%s, the ledger in DATA_DIR committed only %d", s.size, l.rel, s.name, lv.Size), nil
		}
		from := max(0, s.size-compareWindow)
		same, err := sameRange(filepath.Join(l.dir, s.name), filepath.Join(l.sp.dir, s.name), from, s.size)
		if err != nil {
			return "", err
		}
		if same {
			continue
		}
		// Other bytes at the same place: a record of another history if they
		// verify on their own, damage if they do not (repaired later).
		sc, err := scanSegmentFile(filepath.Join(l.dir, s.name), s.first, s.size)
		if err != nil {
			return "", err
		}
		if sc.bad == "" && sc.lines > 0 {
			return fmt.Sprintf("its %s/%s holds other records than the ledger in DATA_DIR", l.rel, s.name), nil
		}
	}
	if older != nil {
		sc, err := scanSegmentFile(filepath.Join(l.dir, older.name), older.first, older.size)
		if err != nil {
			return "", err
		}
		switch {
		case sc.bad != "" || sc.lines == 0:
			// Damaged or empty: nothing to keep.
		case sc.lastSeq+1 == liveFirst && sc.lastHash != v.anchor:
			return fmt.Sprintf("its %s/%s does not lead to the first record the ledger in DATA_DIR retains", l.rel, older.name), nil
		case sc.lastSeq+1 > liveFirst:
			return fmt.Sprintf("its %s/%s overlaps the ledger in DATA_DIR with other segments", l.rel, older.name), nil
		case sc.lastSeq+1 < liveFirst && !hasPoint:
			// Old records, a gap, and no recovery point that says whose.
			return fmt.Sprintf("it holds %s/%s from before what the ledger in DATA_DIR retains and no %s to tell whose it is", l.rel, older.name, MirrorManifestFile), nil
		}
	}
	return "", nil
}

// setAside moves the whole mirror directory away, untouched, and starts an
// empty one. It refuses while another host may still be writing it.
func (m *Mirror) setAside(man *MirrorManifest, reason string) error {
	if man != nil && man.Writer != "" && !strings.HasPrefix(man.Writer, m.host+"/") {
		if age := m.opts.Now().Sub(man.WrittenAt); age < conflictWindow {
			return &mirrorError{
				msg:  fmt.Sprintf("the mirror in %s is not this ledger's (%s) and %s wrote it %s ago: it may be in use", m.opts.Dir, reason, man.Writer, age.Round(time.Second)),
				hint: fmt.Sprintf("two vBilling instances must never share one mirror directory: give each its own ARCHIVE_DIR. If no other instance writes it, it is set aside %s after its last write", conflictWindow),
			}
		}
	}
	aside := m.opts.Dir + mirrorSetAside + m.opts.Now().UTC().Format("20060102T150405Z")
	for n := 2; ; n++ {
		if _, err := os.Lstat(aside); errors.Is(err, os.ErrNotExist) {
			break
		}
		aside = fmt.Sprintf("%s%s%s-%d", m.opts.Dir, mirrorSetAside, m.opts.Now().UTC().Format("20060102T150405Z"), n)
	}
	if err := m.io.rename(m.opts.Dir, aside); err != nil {
		return err
	}
	_ = m.io.syncDir(filepath.Dir(m.opts.Dir))
	m.opts.Logf("[mirror] WARNING: the mirror in %s is not a copy of the ledger in %s (%s). It was set aside, unchanged, in %s and a new mirror is started. "+
		"If the data volume was lost and that copy is the ledger to keep, stop vBilling and run: vbilling ledger restore --from %s --to %s --force. Otherwise delete it",
		m.opts.Dir, m.opts.DataDir, reason, aside, aside, m.opts.DataDir)
	for rel := range m.tracked {
		m.untrack(rel)
	}
	m.adoptable = map[string]MirrorFile{}
	m.man, m.manBytes = nil, nil
	for _, l := range m.ledgers {
		l.segs, l.seq, l.hash, l.anchor, l.hasManifest = nil, 0, "", "", false
	}
	m.mu.Lock()
	m.st.point, m.st.aside = false, time.Time{}
	m.mu.Unlock()
	return m.reconcile()
}

// repairLedger brings one copy to a state the copier can continue: nothing
// after the recovery point, no segment that is not a prefix of the live one,
// and a newest segment that ends at a commit record. Everything it removes is
// in the live ledger (foreign said so) and is copied again.
func (m *Mirror) repairLedger(l *mirrorLedger, e *MirrorLedger, v *ledgerView) error {
	l.segs, l.seq, l.hash, l.anchor, l.hasManifest = nil, 0, "", "", false
	if err := m.io.mkdirAll(l.dir); err != nil {
		return err
	}
	if raw, err := readOptional(filepath.Join(l.dir, ledgerManifest)); err != nil {
		return err
	} else if len(raw) > 0 {
		var man manifest
		if json.Unmarshal(raw, &man) == nil {
			l.anchor, l.hasManifest = man.Anchor, true
		}
	}
	all, err := mirrored(l.dir, nil)
	if err != nil {
		return err
	}
	// 1. What a pass that never finished added after the recovery point.
	if e != nil {
		if lastFirst, ok := parseSegName(e.LastSegment); ok {
			for len(all) > 0 && all[len(all)-1].first > lastFirst {
				if err := m.io.remove(filepath.Join(l.dir, all[len(all)-1].name)); err != nil {
					return err
				}
				all = all[:len(all)-1]
			}
			if n := len(all); n > 0 && all[n-1].first == lastFirst && all[n-1].size > e.LastSize {
				if err := m.io.truncate(filepath.Join(l.dir, all[n-1].name), e.LastSize); err != nil {
					return err
				}
				all[n-1].size = e.LastSize
			}
		}
	}
	// 2. The first segment that is not what the live ledger holds, and
	// everything after it.
	live := map[string]int{}
	for i := range v.segs {
		live[v.segs[i].Name] = i
	}
	// The copy must hold the retained segments from the oldest one on, in
	// order, each complete except the last.
	keep, expect := len(all), 0
	for i := range all {
		s := &all[i]
		li, ok := live[s.name]
		if !ok {
			continue // compacted away since; dropped after the next recovery point
		}
		lv := &v.segs[li]
		ok = li == expect && s.size <= lv.Size && (i == len(all)-1 || s.size == lv.Size)
		if ok {
			ok, err = sameRange(filepath.Join(l.dir, s.name), filepath.Join(l.sp.dir, s.name), max(0, s.size-compareWindow), s.size)
			if err != nil {
				return err
			}
		}
		if !ok {
			keep = i
			break
		}
		expect++
	}
	// 3. A newest segment that does not end at a commit record.
	damaged := keep < len(all)
	var tail segScan
	for keep > 0 {
		s := &all[keep-1]
		if tail, err = scanSegmentFile(filepath.Join(l.dir, s.name), s.first, s.size); err != nil {
			return err
		}
		if s.size > 0 && tail.goodEnd == s.size {
			break
		}
		// An empty file is what cutting back to a recovery point at the very
		// start of a segment leaves: no damage. Anything else is.
		damaged = damaged || s.size > 0
		if tail.goodEnd > 0 {
			break
		}
		keep--
	}
	if damaged && e != nil {
		m.opts.Logf("[mirror] the copy of %s in %s is shorter than its recovery point or damaged: it is cut back to its last intact window and copied again from the ledger", l.sp.dir, l.dir)
		if err := m.dropManifest(); err != nil {
			return err
		}
	}
	changed := false
	for len(all) > keep {
		if err := m.io.remove(filepath.Join(l.dir, all[len(all)-1].name)); err != nil {
			return err
		}
		all, changed = all[:len(all)-1], true
	}
	if keep > 0 && tail.goodEnd < all[keep-1].size {
		if err := m.io.truncate(filepath.Join(l.dir, all[keep-1].name), tail.goodEnd); err != nil {
			return err
		}
		all[keep-1].size, changed = tail.goodEnd, true
	}
	if changed {
		if err := m.io.syncDir(l.dir); err != nil {
			return err
		}
	}
	l.segs = all
	if keep == 0 {
		return nil
	}
	l.seq, l.hash = tail.lastSeq, tail.lastHash
	// The copy's manifest must name what its oldest record links to. A crash
	// between moving the anchor and removing compacted segments leaves it one
	// step ahead: put it back.
	prev, err := firstPrev(filepath.Join(l.dir, all[0].name))
	if err != nil {
		return err
	}
	if !l.hasManifest || l.anchor != prev {
		return m.writeLedgerManifest(l, prev, v.watermarks)
	}
	return nil
}

// firstPrev returns the hash the first record of a segment links to.
func firstPrev(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf []byte
	line, err := readLine(bufio.NewReaderSize(f, 1<<16), &buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	hdr, _, err := checkLine(line)
	if err != nil {
		return "", fmt.Errorf("%s: first record: %w", path, err)
	}
	return hdr.Prev, nil
}

// reconcileState removes versions no recovery point names and cuts versions
// back to the size it names: both are leftovers of a pass that did not finish.
func (m *Mirror) reconcileState() error {
	named := map[string]int64{}
	m.adoptable = map[string]MirrorFile{}
	if m.man != nil {
		for _, e := range m.man.Files {
			named[e.File] = e.Size
			if m.tracked[e.Path] == nil {
				m.adoptable[e.Path] = e
			}
		}
	}
	// Versions written since, by this process, that the next recovery point
	// will name.
	for rel, t := range m.tracked {
		info, err := os.Stat(m.statePath(t.file))
		if err != nil || info.Size() < t.size {
			m.untrack(rel)
			if m.man != nil {
				for _, e := range m.man.Files {
					if e.Path == rel {
						m.adoptable[rel] = e
					}
				}
			}
			continue
		}
		if t.size > named[t.file] {
			named[t.file] = t.size
		}
	}
	root := filepath.Join(m.opts.Dir, mirrorStateDir)
	var walk func(rel string) error
	walk = func(rel string) error {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		list, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range list {
			sub := path.Join(rel, e.Name())
			full := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if err := walk(sub); err != nil {
					return err
				}
				continue
			}
			size, ok := named[sub]
			if !ok {
				if err := m.io.remove(full); err != nil {
					return err
				}
				continue
			}
			info, err := e.Info()
			if err != nil {
				return err
			}
			if info.Size() > size {
				if err := m.io.truncate(full, size); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("")
}

// segScan is what reading one segment file of the copy found.
type segScan struct {
	goodEnd  int64  // offset after the last commit record
	lastSeq  uint64 // that record
	lastHash string
	lines    int    // complete lines that verify
	bad      string // the first line that does not, "" if none
}

// scanSegmentFile reads the first limit bytes of a segment and checks every
// complete line: its hash, its sequence number and its link to the line
// before. A partial last line is not an error.
func scanSegmentFile(path string, first uint64, limit int64) (segScan, error) {
	var sc segScan
	f, err := os.Open(path)
	if err != nil {
		return sc, err
	}
	defer f.Close()
	var w chainWalk
	w.next = first
	br := w.lines(io.LimitReader(f, limit))
	var off int64
	for {
		line, err := readLine(br, &w.line)
		if err != nil && err != io.EOF {
			return sc, err
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			return sc, nil
		}
		hdr, ok := w.record(filepath.Base(path), line)
		if !ok || len(w.errs) > 0 {
			sc.bad = w.errs[0]
			return sc, nil
		}
		off += int64(len(line))
		sc.lines++
		if hdr.Type == TypeCommit {
			sc.goodEnd, sc.lastSeq, sc.lastHash = off, hdr.Seq, w.prev
		}
	}
}

// sameRange reports whether two files hold the same bytes in [from, to).
func sameRange(a, b string, from, to int64) (bool, error) {
	if to <= from {
		return true, nil
	}
	read := func(path string) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, to-from)
		n, err := f.ReadAt(buf, from)
		if err != nil && err != io.EOF {
			return nil, err
		}
		return buf[:n], nil
	}
	x, err := read(a)
	if err != nil {
		return false, err
	}
	y, err := read(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

// --- reporting ---

const (
	mirrorLagMetric    = "vbilling_ledger_mirror_lag_seconds"
	mirrorLagHelp      = "Age of the oldest committed ledger record that is not in the mirror yet (0 when in step)."
	mirrorErrorsMetric = "vbilling_ledger_mirror_errors_total"
	mirrorErrorsHelp   = "Mirror passes that failed. Metering and delivery are not affected: the mirror falls behind."
	mirrorDefaultHint  = "check that the archive volume is mounted, writable by vBilling's user and not full. Metering and delivery continue; only the second copy of the ledger falls behind"
)

// finish records the outcome of a pass and reports it.
func (m *Mirror) finish(err error) {
	m.mu.Lock()
	m.st.running = time.Time{}
	m.st.err, m.st.hint = err, ""
	if err == nil {
		m.st.point = m.man != nil
		for i, l := range m.ledgers {
			if e := m.man.ledger(l.rel); e != nil {
				m.st.committed[i] = e.Committed
			}
		}
		if m.man != nil {
			m.st.files = len(m.man.Files)
		}
	} else {
		m.st.hint = mirrorHint(err)
	}
	first := err == nil && m.man != nil && !m.announced
	if first {
		m.announced = true
	}
	m.mu.Unlock()
	if err != nil && m.opts.Tel != nil {
		m.opts.Tel.Add(mirrorErrorsMetric, mirrorErrorsHelp, nil, 1)
	}
	if first {
		m.opts.Logf("[mirror] the ledger mirror in %s is in step: %s, %d state file(s)", m.opts.Dir, m.positions(), len(m.man.Files))
	}
	m.report(true)
}

func (m *Mirror) positions() string {
	var parts []string
	for _, e := range m.man.Ledgers {
		parts = append(parts, fmt.Sprintf("%s seq %d", e.Dir, e.Committed))
	}
	return strings.Join(parts, ", ")
}

func mirrorHint(err error) string {
	var me *mirrorError
	switch {
	case errors.As(err, &me):
		return me.hint
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return "the archive volume is full: grow it. Metering and delivery continue; only the second copy of the ledger falls behind"
	case errors.Is(err, syscall.EROFS), errors.Is(err, os.ErrPermission):
		return "the archive volume is read-only or not writable by vBilling's user. Metering and delivery continue; only the second copy of the ledger falls behind"
	}
	return mirrorDefaultHint
}

// lag is the age of the oldest committed record the recovery point does not
// hold, over all ledgers. It reads the data volume only.
func (m *Mirror) lag(now time.Time) time.Duration {
	var worst time.Duration
	for i, l := range m.ledgers {
		m.mu.Lock()
		point, at := m.st.point, m.st.committed[i]
		cached := m.st.pending[i]
		m.mu.Unlock()
		st := l.sp.Stats()
		if st.FirstSeq == 0 || (point && st.Committed <= at) {
			continue // nothing retained, or in step
		}
		if !point {
			at = st.FirstSeq - 1
		}
		if !cached.known || cached.after != at {
			cached = pendingRecord{after: at, at: st.OldestAt, known: true}
			if recs, err := l.sp.Read(at, 1); err == nil && len(recs) == 1 {
				cached.at = recs[0].At
			}
			m.mu.Lock()
			m.st.pending[i] = cached
			m.mu.Unlock()
		}
		if d := now.Sub(cached.at); d > worst {
			worst = d
		}
	}
	return worst
}

// report publishes the lag and, after a pass or once a minute, the health of
// the mirror. It runs next to the passes, so a pass that hangs on the mirror
// volume still shows.
func (m *Mirror) report(afterPass bool) {
	now := m.opts.Now()
	lag := m.lag(now)
	if m.opts.Tel != nil {
		m.opts.Tel.Set(mirrorLagMetric, mirrorLagHelp, nil, lag.Seconds())
	}
	if m.opts.Health == nil {
		return
	}
	m.mu.Lock()
	if copied := m.copied.Load(); copied != m.st.progress || m.st.progressT.IsZero() {
		m.st.progress, m.st.progressT = copied, now
	}
	stalled := !m.st.running.IsZero() && now.Sub(m.st.running) > time.Minute && now.Sub(m.st.progressT) > time.Minute
	due := afterPass || now.Sub(m.st.reported) >= time.Minute || stalled != m.st.stalled
	if due {
		m.st.reported, m.st.stalled = now, stalled
	}
	asideDue := now.Sub(m.st.aside) >= time.Minute
	if asideDue {
		m.st.aside = now
	}
	st := m.st // the slices are copied: a pass replaces their contents
	st.committed = append([]uint64(nil), m.st.committed...)
	st.unread = append([]string(nil), m.st.unread...)
	m.mu.Unlock()
	if asideDue {
		m.reportSetAside()
	}
	if !due {
		return
	}
	behind := ""
	if lag > 0 {
		behind = fmt.Sprintf(": the mirror is %s behind the ledger", lag.Round(100*time.Millisecond))
	}
	switch {
	case stalled:
		m.opts.Health.Set(mirrorHealthName, mirrorHealthKind, m.opts.Dir, health.Failing,
			fmt.Sprintf("a pass has been running for %s without writing anything: the mirror volume does not answer%s", now.Sub(st.running).Round(time.Second), behind),
			mirrorDefaultHint)
	case st.err != nil:
		m.opts.Health.Set(mirrorHealthName, mirrorHealthKind, m.opts.Dir, health.Failing, st.err.Error()+behind, st.hint)
	default:
		m.opts.Health.Set(mirrorHealthName, mirrorHealthKind, m.opts.Dir, health.OK, m.okMessage(st, lag), "")
	}
}

func (m *Mirror) okMessage(st mirrorState, lag time.Duration) string {
	var b strings.Builder
	switch {
	case !st.point:
		b.WriteString("first copy in progress, no recovery point yet")
	case lag == 0:
		b.WriteString("in step with the ledger")
	default:
		fmt.Fprintf(&b, "%s behind the ledger", lag.Round(100*time.Millisecond))
	}
	if st.point {
		for i, l := range m.ledgers {
			fmt.Fprintf(&b, ", %s seq %d", l.rel, st.committed[i])
		}
		fmt.Fprintf(&b, ", %d state file(s)", st.files)
	}
	if n := len(st.unread); n > 0 {
		fmt.Fprintf(&b, "; %d file(s) of DATA_DIR could not be read and are not in the mirror (%s)", n, st.unread[0])
	}
	if st.sameFS {
		b.WriteString("; on the same filesystem as the ledger, so it does not survive losing that volume")
	}
	return b.String()
}

// reportSetAside keeps copies that were set aside visible until an operator
// restores or deletes them.
func (m *Mirror) reportSetAside() {
	parent := filepath.Dir(m.opts.Dir)
	list, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	prefix := filepath.Base(m.opts.Dir) + mirrorSetAside
	var found []string
	for _, e := range list {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			found = append(found, filepath.Join(parent, e.Name()))
		}
	}
	if len(found) == 0 {
		return
	}
	sort.Strings(found)
	newest := found[len(found)-1]
	m.opts.Health.Set(mirrorAsideName, mirrorHealthKind, newest, health.Diverged,
		fmt.Sprintf("%d earlier mirror copy(ies) set aside, the newest in %s: a copy of a ledger that is not the one in DATA_DIR", len(found), newest),
		fmt.Sprintf("if the data volume was lost and that copy is the ledger to keep, stop vBilling and run vbilling ledger restore --from %s --to %s --force; otherwise delete the directory", newest, m.opts.DataDir))
}
