package spool

// Reading a mirror (mirror.go) back: verifying its recovery point and
// restoring DATA_DIR from it.
//
// A restore copies exactly what MIRROR.json names, the ledgers up to the
// recorded position and the recorded bytes of every state file, so the
// result is DATA_DIR as it was when the mirror's last complete pass took its
// cut. vBilling then starts as after a crash at that moment:
//
//   - it meters every window since the restored watermarks again, from the
//     lifetimes of the objects it finds, as far back as backfill reaches;
//   - each destination is sent everything after its restored cursor, and
//     what a billing platform already received is deduplicated there by the
//     event ID, which is derived from the restored metering source identity;
//   - Prometheus counters continue from the restored baselines, the
//     generation that matches the restored watermark;
//   - held events, dead letters, pause spans and billing states are as of the
//     recovery point.
//
// What it cannot bring back is what only reached DATA_DIR after that pass:
// usage pushed to vBilling (the ingest API, per-request records) and state
// changes of the last seconds. vbilling_ledger_mirror_lag_seconds is how many.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// restoreMarker is in a ledger directory from the first change a restore
// makes until its last. Open refuses a directory that holds it: a ledger
// restored halfway next to state restored halfway would lose or repeat usage.
const restoreMarker = "RESTORE-INCOMPLETE"

// staleRestore is the age of a recovery point from which an automatic restore
// says that it is old: Stripe deduplicates an event by its ID for a day, so
// what was in flight at the recovery point may be counted twice afterwards.
const staleRestore = 23 * time.Hour

// ErrNoRecoveryPoint means a directory holds no MIRROR.json: it is not a
// mirror, or its first pass never completed.
var ErrNoRecoveryPoint = errors.New("no " + MirrorManifestFile + ": not a mirror, or its first copy never completed")

// ErrTargetHoldsLedger is returned by RestoreMirror without Force when the
// target already holds a ledger.
var ErrTargetHoldsLedger = errors.New("the target already holds a ledger")

type restoreMark struct {
	From      string    `json:"from"`
	StartedAt time.Time `json:"started_at"`
}

// restoreInProgress is Open's check for a restore that did not finish.
func restoreInProgress(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, restoreMarker))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check %s for an unfinished restore: %w", dir, err)
	}
	var mark restoreMark
	_ = json.Unmarshal(b, &mark)
	from := mark.From
	if from == "" {
		from = "<mirror dir>"
	}
	return fmt.Errorf("%s holds a restore that did not finish: run \"vbilling ledger restore --from %s --to %s\" again before starting vBilling",
		dir, from, filepath.Dir(dir))
}

// recoveryPoint is the moment a restore of this recovery point brings back.
func (m *MirrorManifest) recoveryPoint() time.Time {
	if m.CutAt.IsZero() {
		return m.WrittenAt.UTC()
	}
	return m.CutAt.UTC()
}

// IsMirror reports whether dir holds a mirror's recovery point.
func IsMirror(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, MirrorManifestFile))
	return err == nil
}

// ReadMirrorManifest reads and checks the recovery point of the mirror in
// dir. It returns ErrNoRecoveryPoint when there is none.
func ReadMirrorManifest(dir string) (*MirrorManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, MirrorManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", dir, ErrNoRecoveryPoint)
	}
	if err != nil {
		return nil, err
	}
	var man MirrorManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, MirrorManifestFile), err)
	}
	if man.Version != mirrorFormat {
		return nil, fmt.Errorf("%s has format %d, this vBilling reads format %d", MirrorManifestFile, man.Version, mirrorFormat)
	}
	if len(man.Ledgers) == 0 {
		return nil, fmt.Errorf("%s names no ledger", MirrorManifestFile)
	}
	// Everything a restore writes is named here: nothing may point outside
	// the target or at a file another part of the restore writes.
	local := func(p string) bool { return p != "" && p != "." && filepath.IsLocal(filepath.FromSlash(p)) }
	ledgers := map[string]bool{}
	for _, e := range man.Ledgers {
		_, okFirst := parseSegName(e.FirstSegment)
		_, okLast := parseSegName(e.LastSegment)
		if !local(e.Dir) || ledgers[e.Dir] || !okFirst || !okLast || e.LastSize < 0 || e.FirstSegment > e.LastSegment {
			return nil, fmt.Errorf("%s: the entry of ledger %q is not valid", MirrorManifestFile, e.Dir)
		}
		ledgers[e.Dir] = true
	}
	seen := map[string]bool{}
	for _, f := range man.Files {
		dir, name := path.Split(f.Path)
		_, seg := parseSegName(name)
		inLedger := ledgers[strings.TrimSuffix(dir, "/")]
		if !local(f.Path) || !local(f.File) || f.Size < 0 || seen[f.Path] || ledgers[f.Path] ||
			(inLedger && (seg || name == ledgerManifest || name == restoreMarker)) {
			return nil, fmt.Errorf("%s: the entry of file %q is not valid", MirrorManifestFile, f.Path)
		}
		seen[f.Path] = true
	}
	return &man, nil
}

// --- verification ---

// MirrorReport is the result of verifying a mirror's recovery point.
type MirrorReport struct {
	OK       bool   `json:"ok"`
	Dir      string `json:"dir"`
	Identity string `json:"identity,omitempty"` // metering source identity of the mirrored ledger
	// RecoveryPoint is when the mirror's last complete pass took its cut: a restore
	// brings DATA_DIR back as it was then.
	RecoveryPoint *time.Time           `json:"recovery_point,omitempty"`
	Ledgers       []MirrorLedgerReport `json:"ledgers,omitempty"`
	StateFiles    int                  `json:"state_files"`
	StateBytes    int64                `json:"state_bytes"`
	Errors        []string             `json:"errors,omitempty"`
}

// MirrorLedgerReport is one ledger of a recovery point.
type MirrorLedgerReport struct {
	Dir        string               `json:"dir"`
	Segments   int                  `json:"segments"`
	Records    uint64               `json:"records"`
	FirstSeq   uint64               `json:"first_seq"`
	LastSeq    uint64               `json:"last_seq"` // the last committed record
	Head       string               `json:"head"`
	LastAt     *time.Time           `json:"last_at,omitempty"`
	Watermarks map[string]time.Time `json:"watermarks,omitempty"`
}

// VerifyMirror checks the recovery point of the mirror in dir without
// changing anything: every ledger must hold, from its anchor to the recorded
// head, records that match their hashes, carry the next sequence number and
// link to the record before, and every state file must hold the recorded
// bytes (sha256). Bytes after the recovery point, which a pass in progress
// adds, are not part of it and not read.
func VerifyMirror(dir string) MirrorReport {
	rep := MirrorReport{Dir: dir}
	man, err := ReadMirrorManifest(dir)
	if err != nil {
		rep.Errors = []string{err.Error()}
		return rep
	}
	at := man.recoveryPoint()
	rep.Identity, rep.RecoveryPoint = man.Identity, &at
	fail := func(format string, args ...any) {
		if len(rep.Errors) < maxReportItems {
			rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
		}
	}
	for i := range man.Ledgers {
		lr, err := readMirrorLedger(dir, &man.Ledgers[i], nil)
		rep.Ledgers = append(rep.Ledgers, lr)
		if err != nil {
			fail("%s: %v", man.Ledgers[i].Dir, err)
		}
	}
	for _, f := range man.Files {
		if err := readMirrorFile(dir, f, io.Discard); err != nil {
			fail("%s: %v", f.Path, err)
			continue
		}
		rep.StateFiles++
		rep.StateBytes += f.Size
	}
	rep.OK = len(rep.Errors) == 0
	return rep
}

// readMirrorLedger walks one ledger of a recovery point, from its first
// retained segment to the recorded size of its last, as one hash chain that
// must start at the recorded anchor and end at the recorded head. With emit
// set, each segment's bytes are handed to it after they were checked.
func readMirrorLedger(dir string, e *MirrorLedger, emit func(name string, b []byte) error) (MirrorLedgerReport, error) {
	rep := MirrorLedgerReport{Dir: e.Dir, Watermarks: e.Watermarks}
	src := filepath.Join(dir, filepath.FromSlash(e.Dir))
	first, _ := parseSegName(e.FirstSegment)
	last, _ := parseSegName(e.LastSegment)
	segs, err := listSegments(src)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}
	var w chainWalk
	w.start(e.Anchor, first)
	sawLast := false
	for _, s := range segs {
		if s.first < first || s.first > last {
			continue // compacted since, or added by a pass that did not finish
		}
		if s.first != w.next {
			return rep, fmt.Errorf("%s starts at seq %d where seq %d is expected: a segment is missing", s.name, s.first, w.next)
		}
		f, err := os.Open(filepath.Join(src, s.name))
		if err != nil {
			return rep, err
		}
		var r io.Reader = f
		var buf bytes.Buffer
		if s.first == last {
			r, sawLast = io.LimitReader(f, e.LastSize), true
		}
		if emit != nil {
			r = io.TeeReader(r, &buf)
		}
		_, err = w.segment(s.name, r, false, noBound)
		f.Close()
		if err != nil {
			return rep, fmt.Errorf("%s: %w", s.name, err)
		}
		if len(w.errs) > 0 {
			return rep, errors.New(w.errs[0])
		}
		if emit != nil {
			if err := emit(s.name, buf.Bytes()); err != nil {
				return rep, err
			}
		}
		rep.Segments++
	}
	if !sawLast {
		// The mirror creates no file for a segment without committed bytes.
		if e.LastSize != 0 {
			return rep, fmt.Errorf("%s is missing", e.LastSegment)
		}
		if w.next != last {
			return rep, fmt.Errorf("the segments end at seq %d, %s must start there: a segment is missing", w.next-1, e.LastSegment)
		}
		if emit != nil {
			if err := emit(e.LastSegment, nil); err != nil {
				return rep, err
			}
		}
		rep.Segments++
	}
	rep.Records, rep.FirstSeq, rep.LastSeq, rep.Head = w.records, w.first, w.last, w.prev
	if w.records > 0 {
		at := w.lastAt.UTC()
		rep.LastAt = &at
	}
	if w.next != e.Committed+1 || w.prev != e.Head {
		return rep, fmt.Errorf("the records end at seq %d (%s), the recovery point says seq %d (%s)", w.next-1, w.prev, e.Committed, e.Head)
	}
	rep.LastSeq = e.Committed // also when retention left no record
	return rep, nil
}

// readMirrorFile copies the recorded bytes of one state file to w and checks
// their sha256.
func readMirrorFile(dir string, f MirrorFile, w io.Writer) error {
	src, err := os.Open(filepath.Join(dir, mirrorStateDir, filepath.FromSlash(f.File)))
	if err != nil {
		return err
	}
	defer src.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(src, f.Size))
	if err != nil {
		return err
	}
	if n != f.Size {
		return fmt.Errorf("the mirror holds %d of its %d bytes", n, f.Size)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return errors.New("the mirror's copy does not match its sha256: it was changed or damaged")
	}
	return nil
}

// --- restore ---

// RestoreOptions tune RestoreMirror.
type RestoreOptions struct {
	// Force replaces a ledger the target already holds, with everything else
	// in the target directory.
	Force bool
	// NoSync skips fsync (tests only).
	NoSync bool
	Now    func() time.Time
}

// RestoreReport describes what RestoreMirror restored.
type RestoreReport struct {
	From, To string
	// Identity is the restored metering source identity.
	Identity string
	// RecoveryPoint is when the mirror's last complete pass took its cut.
	RecoveryPoint time.Time
	Ledgers       []RestoredLedger
	StateFiles    int
	StateBytes    int64
	// Replaced describes the ledger Force replaced, if the target held one.
	Replaced *ReplacedLedger
	Notes    []string
}

// RestoredLedger is one restored ledger.
type RestoredLedger struct {
	MirrorLedgerReport
	// Cursors are the destination cursors as restored. Clamped holds the
	// mirrored value of each one that was beyond the restored head and was
	// lowered to it.
	Cursors map[string]uint64
	Clamped map[string]uint64
}

// ReplacedLedger is what a forced restore found in the target.
type ReplacedLedger struct {
	Dir      string
	LastSeq  uint64
	LastAt   *time.Time
	Identity string // its metering source identity, when it could be read
}

// RestoreMirror restores the recovery point of the mirror in from into the
// data directory to. It verifies the mirror before it changes anything and
// the restored ledgers afterwards. A target that already holds a ledger is
// refused with ErrTargetHoldsLedger unless opts.Force is set. vBilling must
// not be running on the target.
func RestoreMirror(from, to string, opts RestoreOptions) (*RestoreReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	from, err := filepath.Abs(from)
	if err != nil {
		return nil, err
	}
	if to, err = filepath.Abs(to); err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(from, to); err == nil && (rel == "." || filepath.IsLocal(rel)) {
		return nil, fmt.Errorf("the target %s is inside the mirror %s", to, from)
	}
	man, err := ReadMirrorManifest(from)
	if err != nil {
		return nil, err
	}
	if check := VerifyMirror(from); !check.OK {
		return nil, fmt.Errorf("the mirror in %s does not verify, nothing was restored: %s", from, strings.Join(check.Errors, "; "))
	}
	rep := &RestoreReport{From: from, To: to, Identity: man.Identity, RecoveryPoint: man.recoveryPoint()}

	held, unfinished := "", false
	for _, dir := range ledgerDirsIn(to, man) {
		switch {
		case fileExists(filepath.Join(dir, restoreMarker)):
			unfinished = true
		case held == "":
			held = dir
		}
	}
	// A marker anywhere means everything in the target is what an earlier
	// restore put there: it is finished, not refused.
	if held != "" && !opts.Force && !unfinished {
		return nil, fmt.Errorf("%w in %s: stop vBilling, make sure this ledger is the one to replace, and pass --force", ErrTargetHoldsLedger, held)
	}
	if held != "" && !unfinished {
		rep.Replaced = describeLedger(to, held)
	}

	// From here on the target is changed. The markers come first and go last.
	mark, _ := json.Marshal(restoreMark{From: from, StartedAt: opts.Now().UTC()})
	for _, e := range man.Ledgers {
		dir := filepath.Join(to, filepath.FromSlash(e.Dir))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(filepath.Join(dir, restoreMarker), mark, opts.NoSync); err != nil {
			return nil, err
		}
		if err := syncDirUnless(opts.NoSync, dir); err != nil {
			return nil, err
		}
	}
	if err := clearTarget(to, from, man, opts.Force || unfinished); err != nil {
		return nil, err
	}

	dirs := map[string]bool{to: true}
	for i := range man.Ledgers {
		e := &man.Ledgers[i]
		dir := filepath.Join(to, filepath.FromSlash(e.Dir))
		dirs[dir] = true
		lr, err := readMirrorLedger(from, e, func(name string, b []byte) error {
			return writeFileSynced(filepath.Join(dir, name), b, 0o640, opts.NoSync)
		})
		if err != nil {
			return nil, fmt.Errorf("restore %s: %w", e.Dir, err)
		}
		// No segment metadata: the ledger reads and checks every record the
		// first time it opens.
		if err := writeJSONAtomic(filepath.Join(dir, ledgerManifest), manifest{Watermarks: e.Watermarks, Anchor: e.Anchor}, opts.NoSync); err != nil {
			return nil, err
		}
		rep.Ledgers = append(rep.Ledgers, RestoredLedger{MirrorLedgerReport: lr})
	}
	for _, f := range man.Files {
		dst := filepath.Join(to, filepath.FromSlash(f.Path))
		for dir := filepath.Dir(dst); len(dir) > len(to) && !dirs[dir]; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return nil, err
		}
		if err := restoreFile(from, f, dst, opts.NoSync); err != nil {
			return nil, fmt.Errorf("restore %s: %w", f.Path, err)
		}
		rep.StateFiles++
		rep.StateBytes += f.Size
	}
	// A cursor beyond the restored head would make its destination skip the
	// records that take those sequence numbers next. The recovery point never
	// holds one; this is for a mirror somebody edited.
	for i := range rep.Ledgers {
		l := &rep.Ledgers[i]
		note, err := clampCursors(filepath.Join(to, filepath.FromSlash(l.Dir), cursorsFile), man.Ledgers[i].Committed, l, opts.NoSync)
		if err != nil {
			return nil, err
		}
		if note != "" {
			rep.Notes = append(rep.Notes, note)
		}
	}
	for dir := range dirs {
		if err := syncDirUnless(opts.NoSync, dir); err != nil {
			return nil, err
		}
	}

	// What was written must read back as the recovery point.
	for i := range man.Ledgers {
		e := &man.Ledgers[i]
		dir := filepath.Join(to, filepath.FromSlash(e.Dir))
		check := VerifyChain(dir, "")
		if !check.OK {
			return nil, fmt.Errorf("the restored ledger in %s does not verify: %s", dir, strings.Join(check.Errors, "; "))
		}
		if check.Head != e.Head || (check.Records > 0 && check.LastSeq != e.Committed) {
			return nil, fmt.Errorf("the restored ledger in %s ends at seq %d (%s), the recovery point says seq %d (%s)", dir, check.LastSeq, check.Head, e.Committed, e.Head)
		}
	}
	// The usage ledger's marker goes last: it is what vBilling opens first.
	for i := len(man.Ledgers) - 1; i >= 0; i-- {
		dir := filepath.Join(to, filepath.FromSlash(man.Ledgers[i].Dir))
		if err := os.Remove(filepath.Join(dir, restoreMarker)); err != nil {
			return nil, err
		}
		if err := syncDirUnless(opts.NoSync, dir); err != nil {
			return nil, err
		}
	}
	if rep.Replaced != nil && rep.Replaced.Identity != "" && man.Identity != "" && rep.Replaced.Identity != man.Identity {
		rep.Notes = append(rep.Notes, fmt.Sprintf("the replaced ledger metered under another metering source identity (%s): the windows it delivered are metered again under the restored identity, with other event IDs, "+
			"and a billing platform counts both. Remove the usage it sent between its start and now there, or the tenants are billed twice for it", rep.Replaced.Identity))
	}
	return rep, nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func syncDirUnless(noSync bool, dir string) error {
	if noSync {
		return nil
	}
	return syncDir(dir)
}

// writeFileSynced writes a new file and fsyncs it.
func writeFileSynced(path string, b []byte, perm os.FileMode, noSync bool) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
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
	return f.Close()
}

func restoreFile(from string, f MirrorFile, dst string, noSync bool) error {
	perm := os.FileMode(f.Mode & 0o777)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o600)
	if err != nil {
		return err
	}
	if err := readMirrorFile(from, f, out); err != nil {
		out.Close()
		return err
	}
	if !noSync {
		if err := out.Sync(); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm|0o600) // an existing file keeps its old mode otherwise
}

// ledgerDirsIn lists the directories under dir that hold a ledger or an
// unfinished restore of one: the ones the recovery point names, and any
// other directory directly under dir with segments or a ledger manifest.
func ledgerDirsIn(dir string, man *MirrorManifest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if seen[d] {
			return
		}
		seen[d] = true
		segs, _ := listSegments(d)
		if len(segs) > 0 || fileExists(filepath.Join(d, ledgerManifest)) || fileExists(filepath.Join(d, restoreMarker)) {
			out = append(out, d)
		}
	}
	if man != nil {
		for _, e := range man.Ledgers {
			add(filepath.Join(dir, filepath.FromSlash(e.Dir)))
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			add(filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// describeLedger reads what a forced restore is about to replace.
func describeLedger(dataDir, ledgerDir string) *ReplacedLedger {
	r := &ReplacedLedger{Dir: ledgerDir, Identity: readIdentity(dataDir)}
	check := VerifyChain(ledgerDir, "")
	r.LastSeq, r.LastAt = check.LastSeq, check.LastAt
	return r
}

// readIdentity reads the metering source identity next to a ledger
// (internal/sourceid keeps it in source.json), "" if there is none.
func readIdentity(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, "source.json"))
	if err != nil {
		return ""
	}
	var kv map[string]string
	if json.Unmarshal(b, &kv) != nil {
		return ""
	}
	return kv["id"]
}

// clearTarget removes what the restore replaces. With all set (a forced
// restore, or one that finishes an earlier attempt) the target is emptied, so
// nothing of another ledger's state stays next to the restored one: a held
// event or a counter baseline of another history would be applied to this
// one. Otherwise only the directories the ledgers are restored into are
// emptied. The markers, lost+found and a directory that holds the mirror (an
// archive directory inside the data directory) stay.
func clearTarget(to, from string, man *MirrorManifest, all bool) error {
	holdsMirror := func(p string) bool {
		rel, err := filepath.Rel(p, from)
		return err == nil && (rel == "." || filepath.IsLocal(rel))
	}
	var clear func(dir string, top bool) error
	clear = func(dir string, top bool) error {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			switch {
			case e.Name() == restoreMarker, top && e.Name() == "lost+found", e.IsDir() && holdsMirror(p):
			case e.IsDir() && holdsMarker(p):
				if err := clear(p, false); err != nil {
					return err
				}
			default:
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if all {
		return clear(to, true)
	}
	for _, e := range man.Ledgers {
		if err := clear(filepath.Join(to, filepath.FromSlash(e.Dir)), false); err != nil {
			return err
		}
	}
	return nil
}

// holdsMarker reports whether dir or a directory under it holds a restore
// marker, so clearing descends into it instead of removing it.
func holdsMarker(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == restoreMarker {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// clampCursors lowers every cursor beyond head to head and records the
// cursors in l.
func clampCursors(path string, head uint64, l *RestoredLedger, noSync bool) (note string, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cursors := map[string]uint64{}
	if err := json.Unmarshal(b, &cursors); err != nil {
		return fmt.Sprintf("%s could not be read to check its cursors (%v); the ledger lowers a cursor beyond its head when it opens", path, err), nil
	}
	l.Cursors = cursors
	for dest, c := range cursors {
		if c > head {
			if l.Clamped == nil {
				l.Clamped = map[string]uint64{}
			}
			l.Clamped[dest] = c
			cursors[dest] = head
		}
	}
	if len(l.Clamped) == 0 {
		return "", nil
	}
	if err := writeJSONAtomic(path, cursors, noSync); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d cursor(s) in %s pointed beyond the restored head and were lowered to seq %d", len(l.Clamped), path, head), nil
}

// RestoreIfLost restores dataDir from the mirror in mirrorDir when dataDir
// holds no ledger and the mirror holds a recovery point: the data volume was
// lost or replaced by an empty one. It also finishes a restore that was
// interrupted. In every other case it does nothing and returns nil, nil: a
// data directory that holds a ledger is never touched, and a mirror that is
// missing or has no recovery point leaves a new install to start.
//
// An error means dataDir holds no usable ledger although a mirror exists.
// vBilling must not start then: it would meter as a new install, under a new
// metering source identity, next to the copy of the ledger it lost.
func RestoreIfLost(mirrorDir, dataDir string, logf func(format string, args ...any)) (*RestoreReport, error) {
	if mirrorDir == "" || dataDir == "" {
		return nil, nil
	}
	from := mirrorDir
	ledgers := ledgerDirsIn(dataDir, nil)
	unfinished := false
	for _, dir := range ledgers {
		b, err := os.ReadFile(filepath.Join(dir, restoreMarker))
		if err != nil {
			continue
		}
		unfinished = true
		var mark restoreMark
		if json.Unmarshal(b, &mark) == nil && mark.From != "" && IsMirror(mark.From) {
			from = mark.From
		}
	}
	if len(ledgers) > 0 && !unfinished {
		return nil, nil // a ledger: nothing was lost
	}
	if _, err := os.ReadFile(filepath.Join(from, MirrorManifestFile)); err != nil && !unfinished {
		// No mirror, none that completed a pass, or a volume that cannot be
		// read: nothing here says a ledger was lost, so a new install starts.
		if !errors.Is(err, os.ErrNotExist) {
			logf("[mirror] WARNING: %s holds no ledger and the mirror in %s cannot be read (%v): starting as a new install. If the data volume was lost, stop vBilling now, make the mirror readable and run vbilling ledger restore --from %s --to %s --force",
				dataDir, from, err, from, dataDir)
		}
		return nil, nil
	}
	if _, err := ReadMirrorManifest(from); err != nil {
		return nil, err
	}
	if unfinished {
		logf("[mirror] %s holds a restore that did not finish: restoring it again from %s", dataDir, from)
	} else {
		logf("[mirror] %s holds no ledger, but the mirror in %s does: the data volume was lost or replaced. Restoring it", dataDir, from)
	}
	rep, err := RestoreMirror(from, dataDir, RestoreOptions{})
	if err != nil {
		return nil, err
	}
	for _, l := range rep.Ledgers {
		logf("[mirror] restored %s: seq %d-%d, %d record(s), verified", filepath.Join(dataDir, filepath.FromSlash(l.Dir)), l.FirstSeq, l.LastSeq, l.Records)
	}
	age := time.Since(rep.RecoveryPoint).Round(time.Second)
	logf("[mirror] restored %s as it was at %s, %s ago (the mirror's last complete pass), with %d state file(s). vBilling continues from there: it meters the windows since then again, as far back as backfill reaches, and billing platforms deduplicate what they already received by event ID. "+
		"To start as a new install instead, stop vBilling, empty %s and remove %s",
		dataDir, rep.RecoveryPoint.Format(time.RFC3339), age, rep.StateFiles, dataDir, from)
	if age > staleRestore {
		logf("[mirror] WARNING: that recovery point is %s old. Windows older than the backfill limit are not metered again, and a billing platform that deduplicates for a day no longer recognizes what was being delivered when the copy was made. "+
			"Compare GET /api/v1/reconcile with the billing platform for the hours around %s, and look at the windows since then", age, rep.RecoveryPoint.Format(time.RFC3339))
	}
	for _, n := range rep.Notes {
		logf("[mirror] %s", n)
	}
	return rep, nil
}
