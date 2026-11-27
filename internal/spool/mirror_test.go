package spool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// mirrorRig is a data directory with a usage ledger, a records ledger and a
// mirror of both.
type mirrorRig struct {
	t        *testing.T
	c        *clock
	data     string
	dir      string // the mirror
	identity string
	ledger   *Spool
	records  *Spool
	m        *Mirror
	health   *health.Registry
	tel      *telemetry.Registry
	logs     *logSink
	opts     Options
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) has(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newMirrorRig(t *testing.T, opts Options) *mirrorRig {
	t.Helper()
	return newMirrorRigAt(t, opts, MirrorDir(filepath.Join(t.TempDir(), "archive", "vbilling-0")), "source-a")
}

// newMirrorRigAt is a new, empty data directory mirrored into dir, which may
// hold the mirror of another one.
func newMirrorRigAt(t *testing.T, opts Options, dir, identity string) *mirrorRig {
	t.Helper()
	r := &mirrorRig{t: t, c: &clock{t: t0}, data: t.TempDir(), dir: dir, identity: identity, opts: opts, logs: &logSink{}}
	r.openLedgers()
	r.newMirror()
	t.Cleanup(r.closeLedgers)
	return r
}

func (r *mirrorRig) openLedgers() {
	r.t.Helper()
	r.ledger = open(r.t, filepath.Join(r.data, "ledger"), r.c, r.opts)
	r.records = open(r.t, filepath.Join(r.data, "records"), r.c, r.opts)
}

func (r *mirrorRig) closeLedgers() {
	r.ledger.Close()
	r.records.Close()
}

// newMirror starts a copier as a new process would: nothing in memory.
func (r *mirrorRig) newMirror() *Mirror {
	r.tel = telemetry.New()
	r.health = health.New(r.tel)
	r.health.SetClock(r.c.Now)
	r.health.SetLogger(r.logs.printf)
	r.m = NewMirror(MirrorOptions{DataDir: r.data, Dir: r.dir, Ledgers: []*Spool{r.ledger, r.records}, Identity: r.identity,
		Health: r.health, Tel: r.tel, NoSync: true, Now: r.c.Now, Logf: r.logs.printf})
	r.m.large = 256 // a few lines make a file "large": its appends are copied as deltas
	return r.m
}

func (r *mirrorRig) sync() {
	r.t.Helper()
	if err := r.m.Sync(); err != nil {
		r.t.Fatalf("mirror pass: %v", err)
	}
}

// window appends one window of n events to the usage ledger, a minute after
// the last one.
func (r *mirrorRig) window(n int) {
	r.t.Helper()
	wm, _ := r.ledger.Watermark(SourceCollector)
	if wm.IsZero() {
		wm = t0
	}
	var events []usage.Event
	for i := 0; i < n; i++ {
		events = append(events, ev(fmt.Sprintf("tenant-%d", i), wm, 0.25))
	}
	r.c.Add(time.Minute)
	if err := r.ledger.AppendWindow(SourceCollector, wm.Add(time.Minute), events, ""); err != nil {
		r.t.Fatalf("append window: %v", err)
	}
}

// state replaces a file of DATA_DIR the way vBilling's stores do.
func (r *mirrorRig) state(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte(content), true); err != nil {
		r.t.Fatal(err)
	}
}

// appendState appends to a file of DATA_DIR in place, like held events.
func (r *mirrorRig) appendState(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.data, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		r.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		r.t.Fatal(err)
	}
}

func (r *mirrorRig) source(name string) (health.Source, bool) {
	for _, s := range r.health.List() {
		if s.Name == name {
			return s, true
		}
	}
	return health.Source{}, false
}

// mirrorHealth returns the mirror's health source.
func (r *mirrorRig) mirrorHealth() health.Source {
	r.t.Helper()
	s, ok := r.source(mirrorHealthName)
	if !ok {
		r.t.Fatalf("no %s health source", mirrorHealthName)
	}
	return s
}

// checkMirror asserts that the mirror directory is a valid ledger and that
// its recovery point verifies.
func checkMirror(t *testing.T, dir string) MirrorReport {
	t.Helper()
	if rep := VerifyChain(dir, ""); !rep.OK {
		t.Fatalf("the mirror's ledger does not verify: %v", rep.Errors)
	}
	if rep := VerifyChain(filepath.Join(dir, "records"), ""); !rep.OK {
		t.Fatalf("the mirror's records ledger does not verify: %v", rep.Errors)
	}
	rep := VerifyMirror(dir)
	if !rep.OK {
		t.Fatalf("the mirror's recovery point does not verify: %v", rep.Errors)
	}
	return rep
}

// segmentBytes reads every segment of a ledger directory, in order.
func segmentBytes(t *testing.T, dir string) []byte {
	t.Helper()
	segs, err := listSegments(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []byte
	for _, s := range segs {
		b, err := os.ReadFile(filepath.Join(dir, s.name))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

// tree reads every regular file under dir, by relative path.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stateTree is tree without what a data directory holds besides state:
// segments, the ledgers' manifests and temporary files.
func stateTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := tree(t, dir)
	for rel := range out {
		name := filepath.Base(rel)
		if _, seg := parseSegName(name); seg || name == ledgerManifest || temporaryName(name) {
			delete(out, rel)
		}
	}
	return out
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dataSnapshot is what a data directory held at one moment.
type dataSnapshot struct {
	committed uint64
	ledger    []byte
	records   []byte
	state     map[string]string
}

func (r *mirrorRig) snapshot() dataSnapshot {
	r.t.Helper()
	return dataSnapshot{committed: r.ledger.Committed(), ledger: segmentBytes(r.t, filepath.Join(r.data, "ledger")),
		records: segmentBytes(r.t, filepath.Join(r.data, "records")), state: stateTree(r.t, r.data)}
}

// restoreMatches restores the mirror in dir into a new directory and checks
// that the result is exactly one of the snapshots: the ledgers byte for byte
// and every state file. It returns the restored directory.
func restoreMatches(t *testing.T, dir string, snaps ...dataSnapshot) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), "restored")
	rep, err := RestoreMirror(dir, to, RestoreOptions{NoSync: true})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	head := rep.Ledgers[0].LastSeq
	for _, snap := range snaps {
		if snap.committed != head {
			continue
		}
		if got := segmentBytes(t, filepath.Join(to, "ledger")); !bytes.Equal(got, snap.ledger) {
			t.Fatalf("the restored ledger holds %d bytes, DATA_DIR held %d at seq %d: not the same records", len(got), len(snap.ledger), head)
		}
		if got := segmentBytes(t, filepath.Join(to, "records")); !bytes.Equal(got, snap.records) {
			t.Fatalf("the restored records ledger holds %d bytes, DATA_DIR held %d", len(got), len(snap.records))
		}
		got := stateTree(t, to)
		for rel, content := range snap.state {
			if got[rel] != content {
				t.Fatalf("restored %s is %q, DATA_DIR held %q", rel, got[rel], content)
			}
		}
		for rel := range got {
			if _, ok := snap.state[rel]; !ok {
				t.Fatalf("restored %s, which DATA_DIR did not hold", rel)
			}
		}
		// It must open the way vBilling opens it, and verify.
		sp, err := Open(filepath.Join(to, "ledger"), Options{NoSync: true})
		if err != nil {
			t.Fatalf("open the restored ledger: %v", err)
		}
		defer sp.Close()
		if v := sp.Verify(); !v.OK || sp.Committed() != head {
			t.Fatalf("the restored ledger is at seq %d (want %d), verify: %v", sp.Committed(), head, v.Errors)
		}
		return to
	}
	t.Fatalf("the restored ledger is at seq %d, which is none of the moments DATA_DIR was at", head)
	return ""
}

func (r *mirrorRig) restoreAndCompare() string {
	r.t.Helper()
	return restoreMatches(r.t, r.dir, r.snapshot())
}

func TestMirrorDir(t *testing.T) {
	if got := MirrorDir(""); got != "" {
		t.Fatalf("no archive directory, mirror directory %q", got)
	}
	if got, want := MirrorDir("/archive/vbilling-0"), filepath.Join("/archive/vbilling-0", "mirror"); got != want {
		t.Fatalf("mirror directory %q, want %q", got, want)
	}
	if m := NewMirror(MirrorOptions{}); m != nil {
		t.Fatal("a mirror without a directory")
	}
	var none *Mirror
	none.Run(t.Context())
	none.Stop()
	if err := none.Sync(); err != nil {
		t.Fatal(err)
	}
}

func TestMirrorFollowsLedgerAndState(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 4})
	// Setting the mirror up is no work on its volume: start-up never waits for it.
	if _, err := os.Stat(filepath.Dir(r.dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creating the mirror touched its volume: %v", err)
	}
	r.state("source.json", `{"id":"source-a"}`)
	r.state("held/stripe/abc.jsonl", `{"seq":3}`+"\n")
	r.state("prom-counters.json", `{"generations":[]}`)
	r.state("ledger/skipped.tmp", "half written")
	r.state(".api-token-123", "a temporary token file")
	if err := os.WriteFile(filepath.Join(r.data, "api-token"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r.window(2) // rotates: 3 records per window, 4 per segment
	}
	r.ledger.Cursor("stripe")
	if err := r.ledger.Ack("stripe", 6); err != nil {
		t.Fatal(err)
	}
	if err := r.ledger.AddDeadLetter("stripe", ev("tenant-0", t0, 1), "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.records.AppendBatch("gateway", []usage.Event{ev("tenant-0", t0, 3)}); err != nil {
		t.Fatal(err)
	}
	r.sync()

	rep := checkMirror(t, r.dir)
	if rep.Identity != "source-a" || len(rep.Ledgers) != 2 || rep.Ledgers[0].LastSeq != r.ledger.Committed() || rep.Ledgers[1].LastSeq != r.records.Committed() {
		t.Fatalf("recovery point %+v", rep)
	}
	man, err := ReadMirrorManifest(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range man.Files {
		paths = append(paths, f.Path)
	}
	// The ledger directory's own state is among them: cursors, dead letters
	// and the destinations' start, pause and detach marks.
	want := []string{"api-token", "held/stripe/abc.jsonl", "ledger/cursors.json", "ledger/deadletter-stripe.log", "ledger/destinations.json", "prom-counters.json", "source.json"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("mirrored files %v, want %v", paths, want)
	}
	if !bytes.Equal(segmentBytes(t, filepath.Join(r.dir, "ledger")), segmentBytes(t, filepath.Join(r.data, "ledger"))) {
		t.Fatal("the mirror's segments are not the ledger's bytes")
	}
	to := r.restoreAndCompare()
	if info, err := os.Stat(filepath.Join(to, "api-token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the restored token file must stay private: %v, %v", info.Mode(), err)
	}
	if !r.logs.has("in step") {
		t.Fatalf("the first complete pass is not announced:\n%s", r.logs)
	}

	// Nothing changed: a pass writes nothing.
	before := tree(t, r.dir)
	r.sync()
	r.sync()
	if after := tree(t, r.dir); len(after) != len(before) || after[MirrorManifestFile] != before[MirrorManifestFile] {
		t.Fatal("a pass with nothing to copy changed the mirror")
	}

	// A changed file gets a new version and the old one goes; a removed file
	// leaves the recovery point.
	r.state("prom-counters.json", `{"generations":[{"window_end":"2026-10-05T10:05:00Z"}]}`)
	if err := os.Remove(filepath.Join(r.data, "held", "stripe", "abc.jsonl")); err != nil {
		t.Fatal(err)
	}
	r.window(1)
	r.sync()
	checkMirror(t, r.dir)
	to = r.restoreAndCompare()
	if _, err := os.Stat(filepath.Join(to, "held", "stripe", "abc.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file removed from DATA_DIR was restored: %v", err)
	}
	versions := 0
	for rel := range tree(t, filepath.Join(r.dir, mirrorStateDir)) {
		if strings.HasPrefix(rel, "prom-counters.json.v") {
			versions++
		}
		if strings.HasPrefix(rel, "held/") {
			t.Fatalf("version %s of a removed file was kept", rel)
		}
	}
	if versions != 1 {
		t.Fatalf("%d versions of prom-counters.json kept, want 1", versions)
	}
}

// TestMirrorRandomized appends, rotates, compacts, acknowledges and changes
// state at random. After every pass the mirror must verify, hold exactly the
// ledger's bytes and restore to what DATA_DIR holds.
func TestMirrorRandomized(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			r := newMirrorRig(t, Options{SegmentMaxRecords: 7, Retention: 10 * time.Minute})
			r.state("source.json", `{"id":"source-a"}`)
			r.ledger.Cursor("stripe")
			r.state("held/stripe/big.jsonl", strings.Repeat("held line\n", 40)) // a large append-only file
			for step := 0; step < 150; step++ {
				switch op := rng.Intn(10); {
				case op < 5:
					r.window(rng.Intn(4))
				case op == 5:
					if _, err := r.records.AppendBatch("gateway", []usage.Event{ev("tenant-r", t0, float64(step))}); err != nil {
						t.Fatal(err)
					}
				case op == 6:
					if err := r.ledger.Ack("stripe", r.ledger.Committed()-uint64(rng.Intn(3))); err != nil {
						t.Fatal(err)
					}
				case op == 7:
					r.c.Add(time.Duration(rng.Intn(20)) * time.Minute)
					if _, err := r.ledger.Compact([]string{"stripe"}); err != nil {
						t.Fatal(err)
					}
				case op == 8:
					r.state(fmt.Sprintf("state-%d.json", rng.Intn(3)), fmt.Sprintf(`{"step":%d}`, step))
				default:
					r.appendState("held/stripe/big.jsonl", fmt.Sprintf("held line %d\n", step))
				}
				if rng.Intn(4) == 0 {
					continue // the copier runs less often than the ledger commits
				}
				if rng.Intn(15) == 0 {
					r.newMirror() // a restart
				}
				r.sync()
				checkMirror(t, r.dir)
				for _, name := range []string{"ledger", "records"} {
					if !bytes.Equal(segmentBytes(t, filepath.Join(r.dir, name)), segmentBytes(t, filepath.Join(r.data, name))) {
						t.Fatalf("step %d: the mirror does not hold exactly the segments of %s", step, name)
					}
				}
				if step%10 == 0 {
					r.restoreAndCompare()
				}
			}
			r.sync()
			r.restoreAndCompare()
			if r.logs.has("set aside") || r.logs.has("damaged") || r.logs.has("starts again") {
				t.Fatalf("the copier repaired a mirror nothing had touched:\n%s", r.logs)
			}
		})
	}
}

// A file that only grows is copied by what it gained, into the same version,
// also across a restart. Rewritten, it gets a new version.
func TestMirrorCopiesAppendsAsDeltas(t *testing.T) {
	r := newMirrorRig(t, Options{})
	r.window(1)
	r.appendState("held/stripe/big.jsonl", strings.Repeat("held line\n", 40))
	r.sync()
	version := func() (string, int64) {
		t.Helper()
		man, err := ReadMirrorManifest(r.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range man.Files {
			if f.Path == "held/stripe/big.jsonl" {
				return f.File, f.Size
			}
		}
		t.Fatal("the large file is not in the recovery point")
		return "", 0
	}
	first, size := version()
	written := r.m.copied.Load()

	r.appendState("held/stripe/big.jsonl", "one more line\n")
	r.sync()
	if file, grown := version(); file != first || grown != size+int64(len("one more line\n")) {
		t.Fatalf("after an append the recovery point names %s with %d bytes, want %s with %d", file, grown, first, size+14)
	}
	if delta := r.m.copied.Load() - written; delta != int64(len("one more line\n")) {
		t.Fatalf("an append of 14 bytes copied %d bytes", delta)
	}
	r.restoreAndCompare()

	// A restart finds the version by its content and continues it.
	r.newMirror()
	r.appendState("held/stripe/big.jsonl", "after the restart\n")
	r.sync()
	if file, _ := version(); file != first {
		t.Fatalf("after a restart the file was copied again as %s", file)
	}
	if got := r.m.copied.Load(); got != int64(len("after the restart\n")) {
		t.Fatalf("the restarted copier wrote %d bytes for an append of 18", got)
	}
	r.restoreAndCompare()

	// Rewritten in place with other content of the same length and more: it
	// does not start with what was mirrored any more.
	path := filepath.Join(r.data, "held", "stripe", "big.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.ToUpper(b), "more\n"...), 0o640); err != nil {
		t.Fatal(err)
	}
	r.sync()
	if file, _ := version(); file == first {
		t.Fatal("a rewritten file was continued as if it had only grown")
	}
	r.restoreAndCompare()

	// Replaced by rename, shorter.
	r.state("held/stripe/big.jsonl", strings.Repeat("new\n", 80))
	r.sync()
	r.restoreAndCompare()
	if n := len(tree(t, filepath.Join(r.dir, mirrorStateDir, "held"))); n != 1 {
		t.Fatalf("%d versions of the file kept, want 1", n)
	}
}

// With no recently changed file, a pass looks at sizes and times only, and
// still notices every change.
func TestMirrorNoticesChangesByStat(t *testing.T) {
	r := newMirrorRig(t, Options{})
	r.m.racy = -time.Hour // nothing counts as recently changed
	r.window(1)
	r.state("billing-state.json", `{"tenant-0":"active"}`)
	r.sync()
	r.sync()
	r.state("billing-state.json", `{"tenant-0":"suspended"}`)
	r.sync()
	r.restoreAndCompare()
	if err := os.Chmod(filepath.Join(r.data, "billing-state.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.sync()
	to := r.restoreAndCompare()
	if info, _ := os.Stat(filepath.Join(to, "billing-state.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("a changed mode was not mirrored: %v", info.Mode())
	}
}

// A file the copier cannot read stays in the recovery point as it was last
// copied, and the mirror says so.
func TestMirrorKeepsWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	r := newMirrorRig(t, Options{})
	r.window(1)
	r.state("billing-state.json", `{"tenant-0":"active"}`)
	r.sync()
	path := filepath.Join(r.data, "billing-state.json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o640) })
	r.window(1)
	r.sync()
	man, err := ReadMirrorManifest(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != 1 || man.Files[0].Path != "billing-state.json" {
		t.Fatalf("the unreadable file left the recovery point: %+v", man.Files)
	}
	if msg := r.mirrorHealth().Message; !strings.Contains(msg, "could not be read") {
		t.Fatalf("health does not name the unreadable file: %s", msg)
	}
	checkMirror(t, r.dir)
}

// The mirror follows retention: what compaction removed from the ledger
// leaves the mirror, also when the mirror was not running meanwhile.
func TestMirrorFollowsRetention(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 2, Retention: 10 * time.Minute})
	r.ledger.Cursor("stripe")
	for i := 0; i < 6; i++ {
		r.window(1) // one segment per window
	}
	r.sync()
	mirrored := func() int {
		segs, err := listSegments(filepath.Join(r.dir, "ledger"))
		if err != nil {
			t.Fatal(err)
		}
		return len(segs)
	}
	if got := mirrored(); got != 6 {
		t.Fatalf("%d segments mirrored, want 6", got)
	}
	r.c.Add(time.Hour)
	if err := r.ledger.Ack("stripe", r.ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ledger.Compact([]string{"stripe"}); err != nil || n != 5 {
		t.Fatalf("compacted %d segments: %v", n, err)
	}
	r.sync()
	if got := mirrored(); got != 1 {
		t.Fatalf("%d segments in the mirror after compaction, want 1", got)
	}
	checkMirror(t, r.dir)
	r.restoreAndCompare()

	// The copier stops. The ledger moves on and compacts everything the
	// mirror holds, and more that it never copied.
	for i := 0; i < 4; i++ {
		r.window(1)
	}
	r.c.Add(time.Hour)
	if err := r.ledger.Ack("stripe", r.ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ledger.Compact([]string{"stripe"}); err != nil || n != 4 {
		t.Fatalf("compacted %d segments: %v", n, err)
	}
	r.window(1)
	r.newMirror()
	r.sync()
	if !r.logs.has("starts again at the oldest retained segment") {
		t.Fatalf("the copier did not say it started over:\n%s", r.logs)
	}
	if r.logs.has("set aside") {
		t.Fatalf("an old copy of this very ledger was set aside:\n%s", r.logs)
	}
	checkMirror(t, r.dir)
	if !bytes.Equal(segmentBytes(t, filepath.Join(r.dir, "ledger")), segmentBytes(t, filepath.Join(r.data, "ledger"))) {
		t.Fatal("the mirror does not hold exactly the retained segments")
	}
	r.restoreAndCompare()
}

// A ledger whose retained records are all gone still restores: the next
// record must take the next sequence number and link to the last hash.
func TestMirrorOfLedgerWithoutRetainedRecords(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 2, Retention: time.Minute})
	r.ledger.Cursor("stripe")
	r.window(1)
	r.window(1)
	r.c.Add(time.Hour)
	// A failed append after rotation leaves an empty active segment behind
	// two closed ones; here the same shape comes from compacting everything
	// that is closed after one more rotation.
	r.window(1)
	if err := r.ledger.Ack("stripe", r.ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	r.sync()
	r.c.Add(time.Hour)
	if _, err := r.ledger.Compact([]string{"stripe"}); err != nil {
		t.Fatal(err)
	}
	r.sync()
	checkMirror(t, r.dir)
	to := r.restoreAndCompare()
	sp, err := Open(filepath.Join(to, "ledger"), Options{NoSync: true, Now: r.c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	head := sp.Committed()
	if err := sp.AppendWindow(SourceCollector, t0.Add(24*time.Hour), nil, ""); err != nil {
		t.Fatal(err)
	}
	if sp.Committed() != head+1 {
		t.Fatalf("the restored ledger continued at seq %d after %d", sp.Committed(), head)
	}
	if v := sp.Verify(); !v.OK {
		t.Fatalf("the continued ledger does not verify: %v", v.Errors)
	}
}

// The cut pairs every file with the ledger position it was written for, also
// when windows are committed while the files are being looked at.
func TestMirrorCutIsBetweenCommits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cuts     int
		disturb  int // commits land during this many captures
		wantHeld bool
	}{
		{"quiet", 2, 0, false},
		{"one capture disturbed", 2, 1, false},
		{"every capture disturbed", 2, 1 << 30, true},
		{"always holding the ledgers", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMirrorRig(t, Options{})
			r.m.cuts = tc.cuts
			// A store like the counter baselines: it writes the position the
			// next window will have, then commits the window. Between two
			// commits the file names the ledger's position or the next one,
			// never an older one.
			step := func() {
				r.state("ahead.json", fmt.Sprintf("%d", r.ledger.Committed()+1))
				r.window(0)
			}
			step()
			disturbed := 0
			r.m.afterCapture = func() {
				if disturbed < tc.disturb {
					disturbed++
					step()
					step()
				}
			}
			for i := 0; i < 5; i++ {
				disturbed = 0
				step()
				r.sync()
				man, err := ReadMirrorManifest(r.dir)
				if err != nil {
					t.Fatal(err)
				}
				to := restoreMatches(t, r.dir, r.snapshot())
				b, err := os.ReadFile(filepath.Join(to, "ahead.json"))
				if err != nil {
					t.Fatal(err)
				}
				var wrote uint64
				fmt.Sscan(string(b), &wrote)
				if head := man.Ledgers[0].Committed; wrote != head && wrote != head+1 {
					t.Fatalf("the recovery point pairs a file written for seq %d with a ledger at seq %d", wrote, head)
				}
				if tc.disturb > 0 && tc.disturb < 1<<30 && disturbed != tc.disturb {
					t.Fatalf("%d captures were disturbed, want %d", disturbed, tc.disturb)
				}
			}
			// The ledgers are only held when they never stood still.
			if held := r.m.heldCuts > 0; held != tc.wantHeld {
				t.Fatalf("%d cuts were taken while holding the ledgers, want any: %v", r.m.heldCuts, tc.wantHeld)
			}
		})
	}
}

// Passes that hold the ledgers for their capture run next to appends without
// blocking them for long, and without a deadlock.
func TestMirrorNextToAppends(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 50})
	r.m.cuts = 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			r.window(1)
			if i%10 == 0 {
				r.state("billing-state.json", fmt.Sprintf(`{"n":%d}`, i))
			}
		}
	}()
	for {
		select {
		case <-done:
			r.sync()
			checkMirror(t, r.dir)
			r.restoreAndCompare()
			return
		default:
			r.sync()
		}
	}
}

// Run copies after each commit, picks up files that change without one, and
// Stop leaves the mirror in step.
func TestMirrorRun(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 10})
	r.m = NewMirror(MirrorOptions{DataDir: r.data, Dir: r.dir, Ledgers: []*Spool{r.ledger, r.records}, Identity: r.identity,
		Health: r.health, Tel: r.tel, NoSync: true, Now: r.c.Now, Logf: r.logs.printf,
		MinInterval: time.Millisecond, StateInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		r.m.Run(ctx)
	}()
	inStep := func() bool {
		man, err := ReadMirrorManifest(r.dir)
		if err != nil || man.Ledgers[0].Committed != r.ledger.Committed() {
			return false
		}
		for _, f := range man.Files {
			if f.Path == "billing-state.json" {
				b, _ := os.ReadFile(filepath.Join(r.dir, mirrorStateDir, f.File))
				live, _ := os.ReadFile(filepath.Join(r.data, "billing-state.json"))
				return bytes.Equal(b, live)
			}
		}
		return false
	}
	wait := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !inStep() {
			if time.Now().After(deadline) {
				t.Fatalf("the mirror did not follow %s:\n%s", what, r.logs)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	r.state("billing-state.json", `{"tenant-0":"active"}`)
	for i := 0; i < 20; i++ {
		r.window(1)
	}
	wait("the commits")
	r.state("billing-state.json", `{"tenant-0":"suspended"}`) // no commit: the timer finds it
	wait("a state change without a commit")

	cancel()
	<-ran
	r.window(1) // after the copier stopped
	r.state("billing-state.json", `{"tenant-0":"paused"}`)
	r.m.Stop()
	if !inStep() {
		t.Fatal("Stop did not leave the mirror in step")
	}
	checkMirror(t, r.dir)
	r.restoreAndCompare()
}
