package spool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

var errKilled = errors.New("the copier was killed")

// change is one change a copier made on the mirror volume: units [start,
// start+cost) of its pass, one unit per byte written and one per other change.
type change struct {
	start, cost int
	op, path    string
}

// killer stops a copier the way a crash does: after budget units of change on
// the mirror volume nothing more reaches it, not even the rest of the write
// in progress.
type killer struct {
	budget    int
	spent     int
	killed    bool
	published bool     // MIRROR.json was replaced before the copier died
	at        string   // the change it died in
	inside    int      // bytes of that write that still reached the file
	segment   bool     // it died writing a segment
	trace     []change // every change it made
}

func (k *killer) before(op, path string, n int) (int, error) {
	if k.killed {
		return 0, errKilled
	}
	cost := 1
	if op == "write" {
		cost = n
	}
	if k.spent+cost > k.budget {
		left := k.budget - k.spent
		if op != "write" {
			left = 0
		}
		_, seg := parseSegName(filepath.Base(path))
		k.killed, k.spent, k.at, k.inside, k.segment = true, k.budget, op+" "+path, left, seg && op == "write"
		return left, errKilled
	}
	k.trace = append(k.trace, change{start: k.spent, cost: cost, op: op, path: path})
	k.spent += cost
	if op == "rename" && filepath.Base(path) == MirrorManifestFile {
		k.published = true
	}
	return n, nil
}

// killPoints picks where to kill a pass that made these changes: before every
// change and at every byte of every write. A temporary file is read by nobody
// until it is renamed, so a write to one is killed at a sample of its bytes.
func killPoints(trace []change, every int) []int {
	var out []int
	for _, c := range trace {
		step := every
		if c.op == "write" && strings.HasSuffix(c.path, ".tmp") {
			step = max(every, c.cost/8)
		}
		for i := 0; i < c.cost; i += step {
			out = append(out, c.start+i)
		}
		if c.cost > 1 && (c.cost-1)%step != 0 {
			out = append(out, c.start+c.cost-1)
		}
	}
	return out
}

// onlyAnchor reports whether every error is the copy's anchor being one step
// ahead of its oldest segment (see dropCompacted).
func onlyAnchor(errs []string) bool {
	for _, e := range errs {
		if !strings.Contains(e, "chain break") {
			return false
		}
	}
	return len(errs) > 0
}

// checkKilled asserts what must hold for a mirror whose copier died anywhere:
// its ledger copies verify, and open like a ledger after a crash, and its
// recovery point, if it has one, verifies and restores to one of the moments
// DATA_DIR was at. deep also does the slow parts where the kill did not hit a
// segment.
func checkKilled(t *testing.T, dir string, k *killer, hadPoint, deep bool, snaps ...dataSnapshot) {
	t.Helper()
	for _, name := range []string{"ledger", "records"} {
		copyDir := filepath.Join(dir, name)
		if segs, _ := listSegments(copyDir); len(segs) == 0 && !fileExists(filepath.Join(copyDir, ledgerManifest)) {
			continue // nothing of it was copied yet
		}
		// Offline, as "vbilling ledger verify" reads it.
		if rep := VerifyChain(copyDir, ""); !rep.OK && !(k.published && onlyAnchor(rep.Errors)) {
			t.Fatalf("killed in %q after %d units: the mirror's %s does not verify: %v", k.at, k.budget, name, rep.Errors)
		}
		if !deep && !k.segment {
			continue
		}
		// As vBilling opens a ledger: on a copy, because opening repairs.
		tmp := filepath.Join(dir+".open", name)
		copyTree(t, copyDir, tmp)
		sp, err := Open(tmp, Options{NoSync: true})
		if err != nil {
			t.Fatalf("killed in %q after %d units: the mirror's %s does not open: %v", k.at, k.budget, name, err)
		}
		v := sp.Verify()
		sp.Close()
		os.RemoveAll(dir + ".open")
		if !v.OK && !(k.published && onlyAnchor(v.Errors)) {
			t.Fatalf("killed in %q after %d units: the mirror's %s opens but does not verify: %v", k.at, k.budget, name, v.Errors)
		}
	}
	if _, err := ReadMirrorManifest(dir); err != nil {
		if hadPoint || !errors.Is(err, ErrNoRecoveryPoint) {
			t.Fatalf("killed in %q after %d units: the recovery point is gone or broken: %v", k.at, k.budget, err)
		}
		return
	}
	if rep := VerifyMirror(dir); !rep.OK {
		t.Fatalf("killed in %q after %d units: the recovery point does not verify: %v", k.at, k.budget, rep.Errors)
	}
	if deep {
		os.RemoveAll(filepath.Dir(restoreMatches(t, dir, snaps...)))
	}
}

// killedPass runs one pass of a new copier in dir, on a copy of the mirror in
// base, and kills it after budget units.
func killedPass(t *testing.T, r *mirrorRig, base, dir string, budget int) (*killer, error) {
	t.Helper()
	if base != "" {
		copyTree(t, base, dir)
	}
	k := &killer{budget: budget}
	m := NewMirror(MirrorOptions{DataDir: r.data, Dir: dir, Ledgers: []*Spool{r.ledger, r.records}, Identity: r.identity,
		NoSync: true, Now: r.c.Now, Logf: func(string, ...any) {}})
	m.large = 256
	m.io.before = k.before
	err := m.Sync()
	for _, tf := range m.tracked {
		tf.unpin()
	}
	return k, err
}

// finishKilled lets a new copier complete the mirror in dir and checks that
// it then holds exactly what DATA_DIR holds.
func finishKilled(t *testing.T, r *mirrorRig, dir string, k *killer, final dataSnapshot) {
	t.Helper()
	logs := &logSink{}
	m := NewMirror(MirrorOptions{DataDir: r.data, Dir: dir, Ledgers: []*Spool{r.ledger, r.records}, Identity: r.identity,
		NoSync: true, Now: r.c.Now, Logf: logs.printf})
	m.large = 256
	err := m.Sync()
	for _, tf := range m.tracked {
		tf.unpin()
	}
	if err != nil {
		t.Fatalf("killed in %q after %d units: the next copier fails: %v", k.at, k.budget, err)
	}
	if logs.has("set aside") {
		t.Fatalf("killed in %q after %d units: the next copier set its own mirror aside:\n%s", k.at, k.budget, logs)
	}
	checkMirror(t, dir)
	for name, want := range map[string][]byte{"ledger": final.ledger, "records": final.records} {
		if !bytes.Equal(segmentBytes(t, filepath.Join(dir, name)), want) {
			t.Fatalf("killed in %q after %d units: the finished mirror does not hold the segments of %s", k.at, k.budget, name)
		}
	}
	os.RemoveAll(filepath.Dir(restoreMatches(t, dir, final)))
}

// busyRig builds a data directory, mirrors it, and then gives the next pass
// everything a pass can have to do: a window with an event that grows the
// newest segment, one that starts a new segment, compacted segments to drop,
// changed, new and removed files, an append to a large file and a moved
// cursor. It returns the two moments the mirror may restore to.
func busyRig(t *testing.T) (r *mirrorRig, before, after dataSnapshot) {
	t.Helper()
	r = newMirrorRig(t, Options{SegmentMaxRecords: 3, Retention: 10 * time.Minute})
	r.state("source.json", `{"id":"source-a"}`)
	r.state("billing-state.json", `{"tenant-0":"active"}`)
	r.state("gone.json", `{}`)
	r.appendState("held/stripe/big.jsonl", strings.Repeat("held line\n", 30))
	r.ledger.Cursor("stripe")
	for i := 0; i < 3; i++ {
		r.window(1) // two records each: a new segment every other window
	}
	if _, err := r.records.AppendBatch("gateway", nil); err != nil {
		t.Fatal(err)
	}
	r.sync()
	before = r.snapshot()

	r.c.Add(20 * time.Minute)
	if err := r.ledger.Ack("stripe", r.ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ledger.Compact([]string{"stripe"}); err != nil || n == 0 {
		t.Fatalf("compacted %d segments: %v", n, err)
	}
	r.window(1) // into the newest mirrored segment
	r.window(0) // a new segment
	r.state("billing-state.json", `{"tenant-0":"suspended"}`)
	r.state("new.json", `{}`)
	if err := os.Remove(filepath.Join(r.data, "gone.json")); err != nil {
		t.Fatal(err)
	}
	r.appendState("held/stripe/big.jsonl", "held line 31\n")
	return r, before, r.snapshot()
}

// killEverywhere kills a pass on a copy of base at every point killPoints
// picks, checks what is left, and lets a new copier finish a sample of them.
// The kills are independent, so they run on every core.
func (r *mirrorRig) killEverywhere(base string, every int, hadPoint bool, snaps ...dataSnapshot) {
	t := r.t
	t.Helper()
	root := t.TempDir()
	whole, err := killedPass(t, r, base, filepath.Join(root, "whole"), 1<<30)
	if err != nil {
		t.Fatalf("the pass fails without being killed: %v", err)
	}
	if testing.Short() {
		every *= 5
	}
	points := killPoints(whole.trace, every)
	final := snaps[len(snaps)-1]
	var mu sync.Mutex
	did := map[string]int{}
	killedBefore, killedAfter, inSegment := 0, 0, 0
	workers := runtime.GOMAXPROCS(0)
	t.Run("kills", func(t *testing.T) {
		for w := 0; w < workers; w++ {
			t.Run(fmt.Sprintf("worker-%d", w), func(t *testing.T) {
				t.Parallel()
				for n := w; n < len(points); n += workers {
					budget := points[n]
					dir := filepath.Join(root, fmt.Sprint(budget))
					k, err := killedPass(t, r, base, dir, budget)
					if !errors.Is(err, errKilled) {
						t.Fatalf("a pass killed after %d of %d units returned %v", budget, whole.spent, err)
					}
					mu.Lock()
					did[strings.Fields(k.at)[0]]++
					if k.published {
						killedAfter++
					} else {
						killedBefore++
					}
					if k.segment {
						inSegment++
					}
					mu.Unlock()
					// Between two changes everything is checked, and the next
					// copier finishes. Inside a write, where one byte more
					// changes little, that is done for a sample.
					between := k.inside == 0
					checkKilled(t, dir, k, hadPoint, between || budget%8 == 0, snaps...)
					if between || budget%16 == 0 {
						finishKilled(t, r, dir, k, final)
					}
					os.RemoveAll(dir)
				}
			})
		}
	})
	if hadPoint && (killedBefore == 0 || killedAfter == 0) {
		t.Fatalf("killed %d times before and %d times after the recovery point was replaced: both must be covered", killedBefore, killedAfter)
	}
	if !testing.Short() && inSegment < 600/every {
		t.Fatalf("only %d kills inside a segment write: this pass does not copy enough of the ledger", inSegment)
	}
	ops := []string{"mkdir", "open", "write", "sync", "syncdir", "rename"}
	if hadPoint {
		ops = append(ops, "remove")
	}
	for _, op := range ops {
		if did[op] == 0 {
			t.Fatalf("no kill in a %s: %v", op, did)
		}
	}
}

// TestMirrorKilledAnywhere kills the copier at every byte it writes and
// before every other change it makes, in a pass that does everything a pass
// can do. Whatever is left must open and verify as a ledger and restore to a
// moment DATA_DIR was at, and the next copier must finish the job.
func TestMirrorKilledAnywhere(t *testing.T) {
	r, before, after := busyRig(t)
	r.killEverywhere(r.dir, 1, true, before, after)
}

// The first copy of an existing ledger, killed anywhere: the mirror is a
// valid, shorter ledger all along and has no recovery point until the end.
// (Every third byte: every byte of a segment write is TestMirrorKilledAnywhere's.)
func TestMirrorFirstCopyKilledAnywhere(t *testing.T) {
	r, _, after := busyRig(t)
	r.killEverywhere("", 3, false, after)
}

// A copier that dies, and the one that picks up after it dies too.
func TestMirrorKilledTwice(t *testing.T) {
	r, before, after := busyRig(t)
	root := t.TempDir()
	whole, err := killedPass(t, r, r.dir, filepath.Join(root, "whole"), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	rounds := 60
	if testing.Short() {
		rounds = 15
	}
	for i := 0; i < rounds; i++ {
		first, second := filepath.Join(root, fmt.Sprint(i, "a")), filepath.Join(root, fmt.Sprint(i, "b"))
		k, err := killedPass(t, r, r.dir, first, rng.Intn(whole.spent))
		if !errors.Is(err, errKilled) {
			t.Fatal(err)
		}
		// The second copier starts on what the first left, and may need
		// about as much again.
		k2, err := killedPass(t, r, first, second, rng.Intn(whole.spent+200))
		if err != nil && !errors.Is(err, errKilled) {
			t.Fatalf("the copier after a kill in %q failed: %v", k.at, err)
		}
		k2.at = k.at + ", then " + k2.at
		k2.published = k2.published || k.published
		checkKilled(t, second, k2, true, true, before, after)
		finishKilled(t, r, second, k2, after)
		os.RemoveAll(first)
		os.RemoveAll(second)
	}
}

// Damage to the mirror is found when the copier starts, and repaired from
// the ledger: the copy is cut back to what is still good and copied again.
func TestMirrorRepairsDamage(t *testing.T) {
	seg := func(t *testing.T, r *mirrorRig, i int) string {
		t.Helper()
		segs, err := listSegments(filepath.Join(r.dir, "ledger"))
		if err != nil || len(segs) < 3 {
			t.Fatalf("segments %v: %v", segs, err)
		}
		if i < 0 {
			i += len(segs)
		}
		return filepath.Join(r.dir, "ledger", segs[i].name)
	}
	version := func(t *testing.T, r *mirrorRig, rel string) string {
		t.Helper()
		man, err := ReadMirrorManifest(r.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range man.Files {
			if f.Path == rel {
				return filepath.Join(r.dir, mirrorStateDir, filepath.FromSlash(f.File))
			}
		}
		t.Fatalf("%s is not in the recovery point", rel)
		return ""
	}
	chop := func(t *testing.T, path string, by int64) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, info.Size()-by); err != nil {
			t.Fatal(err)
		}
	}
	flip := func(t *testing.T, path string, at int64) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if at < 0 {
			at += int64(len(b))
		}
		b[at] ^= 0x01
		if err := os.WriteFile(path, b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		// breaks reports whether the damage makes the recovery point
		// unusable until it is repaired.
		breaks bool
		damage func(t *testing.T, r *mirrorRig)
	}{
		{"the newest segment cut in the middle of a record", true, func(t *testing.T, r *mirrorRig) { chop(t, seg(t, r, -1), 40) }},
		{"the newest segment cut to nothing", true, func(t *testing.T, r *mirrorRig) {
			if err := os.Truncate(seg(t, r, -1), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"a closed segment cut short", true, func(t *testing.T, r *mirrorRig) { chop(t, seg(t, r, 1), 40) }},
		{"a closed segment missing", true, func(t *testing.T, r *mirrorRig) {
			if err := os.Remove(seg(t, r, 1)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the oldest segment missing", true, func(t *testing.T, r *mirrorRig) {
			if err := os.Remove(seg(t, r, 0)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a byte changed at the end of the newest segment", true, func(t *testing.T, r *mirrorRig) { flip(t, seg(t, r, -1), -20) }},
		{"a byte changed at the end of a closed segment", true, func(t *testing.T, r *mirrorRig) { flip(t, seg(t, r, 1), -20) }},
		{"every segment gone", true, func(t *testing.T, r *mirrorRig) {
			if err := os.RemoveAll(filepath.Join(r.dir, "ledger")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a state file's version missing", true, func(t *testing.T, r *mirrorRig) {
			if err := os.Remove(version(t, r, "billing-state.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a state file's version changed", true, func(t *testing.T, r *mirrorRig) { flip(t, version(t, r, "billing-state.json"), 3) }},
		{"a large file's version cut short", true, func(t *testing.T, r *mirrorRig) { chop(t, version(t, r, "held/stripe/big.jsonl"), 10) }},
		{"a large file's version changed", true, func(t *testing.T, r *mirrorRig) { flip(t, version(t, r, "held/stripe/big.jsonl"), 5) }},
		{"the copy's ledger manifest missing", false, func(t *testing.T, r *mirrorRig) {
			if err := os.Remove(filepath.Join(r.dir, "ledger", ledgerManifest)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the copy's anchor one step ahead", false, func(t *testing.T, r *mirrorRig) {
			b, _ := json.Marshal(manifest{Anchor: strings.Repeat("ab", 32)})
			if err := os.WriteFile(filepath.Join(r.dir, "ledger", ledgerManifest), b, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"leftovers of a pass that never finished", false, func(t *testing.T, r *mirrorRig) {
			f, err := os.OpenFile(seg(t, r, -1), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("half a record")
			f.Close()
			f, err = os.OpenFile(version(t, r, "held/stripe/big.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("half a line")
			f.Close()
			if err := os.WriteFile(filepath.Join(r.dir, mirrorStateDir, "billing-state.json.v999"), []byte("unnamed"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.dir, MirrorManifestFile+".tmp"), []byte("{"), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"the recovery point missing", false, func(t *testing.T, r *mirrorRig) {
			if err := os.Remove(filepath.Join(r.dir, MirrorManifestFile)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMirrorRig(t, Options{SegmentMaxRecords: 3})
			r.state("billing-state.json", `{"tenant-0":"active"}`)
			r.appendState("held/stripe/big.jsonl", strings.Repeat("held line\n", 30))
			for i := 0; i < 5; i++ {
				r.window(1)
			}
			r.sync()
			tc.damage(t, r)
			if rep := VerifyMirror(r.dir); tc.breaks && rep.OK {
				t.Fatal("verification does not notice the damage")
			} else if tc.breaks {
				if _, err := RestoreMirror(r.dir, filepath.Join(t.TempDir(), "to"), RestoreOptions{NoSync: true}); err == nil {
					t.Fatal("a damaged recovery point was restored")
				}
			}
			r.window(1)
			r.newMirror() // found when the copier starts
			r.sync()
			if r.logs.has("set aside") {
				t.Fatalf("a damaged copy of this very ledger was set aside:\n%s", r.logs)
			}
			checkMirror(t, r.dir)
			if !bytes.Equal(segmentBytes(t, filepath.Join(r.dir, "ledger")), segmentBytes(t, filepath.Join(r.data, "ledger"))) {
				t.Fatal("the repaired mirror does not hold the ledger's segments")
			}
			r.restoreAndCompare()
			for rel := range tree(t, r.dir) {
				if strings.HasSuffix(rel, ".v999") {
					t.Fatalf("a version no recovery point names was kept: %s", rel)
				}
			}
		})
	}
}

// A mirror that holds records DATA_DIR's ledger does not have is another
// ledger, perhaps the only copy of one whose volume was lost. The copier
// never changes it: it moves it aside, starts a new mirror and keeps saying so.
func TestMirrorOfAnotherLedgerIsSetAside(t *testing.T) {
	type scene struct {
		r    *mirrorRig // the ledger now in DATA_DIR, mirrored into the old mirror directory
		lost dataSnapshot
	}
	// The mirror of a ledger with five windows and some state.
	lostLedger := func(t *testing.T) (*mirrorRig, dataSnapshot) {
		r := newMirrorRig(t, Options{SegmentMaxRecords: 3})
		r.state("source.json", `{"id":"source-a"}`)
		r.state("billing-state.json", `{"tenant-0":"suspended"}`)
		for i := 0; i < 5; i++ {
			r.window(2)
		}
		r.sync()
		return r, r.snapshot()
	}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) scene
		why   string
	}{
		{"a new install after the data volume was lost", func(t *testing.T) scene {
			old, lost := lostLedger(t)
			r := newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-b")
			r.window(1)
			return scene{r, lost}
		}, "metering source source-a"},
		{"the same identity with other records", func(t *testing.T) scene {
			old, lost := lostLedger(t)
			r := newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-a")
			r.state("source.json", `{"id":"source-a"}`)
			r.window(1)
			return scene{r, lost}
		}, "the ledger in DATA_DIR"},
		{"an empty ledger with the same identity", func(t *testing.T) scene {
			old, lost := lostLedger(t)
			return scene{newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-a"), lost}
		}, "the ledger in DATA_DIR"},
		{"an empty ledger with another identity", func(t *testing.T) scene {
			old, lost := lostLedger(t)
			return scene{newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-b"), lost}
		}, "metering source source-a"},
		{"a data volume that went back to an older copy", func(t *testing.T) scene {
			old := newMirrorRig(t, Options{SegmentMaxRecords: 3})
			old.window(2)
			old.window(2)
			earlier := filepath.Join(t.TempDir(), "earlier")
			copyTree(t, old.data, earlier)
			for i := 0; i < 3; i++ {
				old.window(2)
			}
			old.sync()
			lost := old.snapshot()
			// DATA_DIR is the earlier copy again and continues from there.
			r := &mirrorRig{t: t, c: old.c, data: earlier, dir: old.dir, identity: "source-a", opts: old.opts, logs: &logSink{}}
			r.openLedgers()
			t.Cleanup(r.closeLedgers)
			r.newMirror()
			r.window(1)
			return scene{r, lost}
		}, "the ledger in DATA_DIR"},
		{"another ledger and no recovery point", func(t *testing.T) scene {
			old, _ := lostLedger(t)
			if err := os.Remove(filepath.Join(old.dir, MirrorManifestFile)); err != nil {
				t.Fatal(err)
			}
			r := newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-b")
			r.window(1)
			return scene{r, dataSnapshot{}}
		}, "the ledger in DATA_DIR"},
		{"a recovery point that cannot be read", func(t *testing.T) scene {
			old, _ := lostLedger(t)
			if err := os.WriteFile(filepath.Join(old.dir, MirrorManifestFile), []byte("{not json"), 0o640); err != nil {
				t.Fatal(err)
			}
			r := newMirrorRigAt(t, Options{SegmentMaxRecords: 3}, old.dir, "source-a")
			return scene{r, dataSnapshot{}}
		}, "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			r := s.r
			was := tree(t, r.dir)
			r.sync()

			asides, _ := filepath.Glob(r.dir + mirrorSetAside + "*")
			if len(asides) != 1 {
				t.Fatalf("%d copies set aside, want 1:\n%s", len(asides), r.logs)
			}
			aside := asides[0]
			now := tree(t, aside)
			if len(now) != len(was) {
				t.Fatalf("the copy set aside holds %d files, the mirror held %d", len(now), len(was))
			}
			for rel, content := range was {
				if now[rel] != content {
					t.Fatalf("%s was changed while setting the mirror aside", rel)
				}
			}
			if !r.logs.has("WARNING") || !r.logs.has(tc.why) || !r.logs.has("vbilling ledger restore --from "+aside) {
				t.Fatalf("the log does not say why, or how to restore:\n%s", r.logs)
			}
			// The new mirror is this ledger's.
			checkMirror(t, r.dir)
			r.restoreAndCompare()
			// The copy that was set aside is still the ledger that was lost.
			if s.lost.committed != 0 {
				restoreMatches(t, aside, s.lost)
			}
			// It stays visible until an operator deals with it.
			r.c.Add(2 * time.Minute)
			r.m.report(false)
			src, ok := r.source(mirrorAsideName)
			if !ok || src.Status != health.Diverged || !strings.Contains(src.Hint, "--force") || src.Subject != aside {
				t.Fatalf("set-aside health source: %+v", src)
			}
			if got := r.mirrorHealth(); got.Status != health.OK {
				t.Fatalf("the new mirror is not healthy: %+v", got)
			}
			// A restart leaves it alone, and does not set anything else aside.
			r.window(1)
			r.newMirror()
			r.sync()
			if again, _ := filepath.Glob(r.dir + mirrorSetAside + "*"); len(again) != 1 {
				t.Fatalf("after a restart %d copies are set aside", len(again))
			}
		})
	}
}

// Two instances must never write one mirror. A mirror another host wrote a
// moment ago is left alone and reported; one nobody writes any more is set
// aside.
func TestMirrorOfAnotherInstance(t *testing.T) {
	other := newMirrorRig(t, Options{})
	other.window(2)
	other.sync()
	rewrite := func(edit func(*MirrorManifest)) {
		t.Helper()
		man, err := ReadMirrorManifest(other.dir)
		if err != nil {
			t.Fatal(err)
		}
		edit(man)
		b, _ := json.Marshal(man)
		if err := os.WriteFile(filepath.Join(other.dir, MirrorManifestFile), b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(func(man *MirrorManifest) { man.Writer = "vbilling-1/0badcafe" })

	r := newMirrorRigAt(t, Options{}, other.dir, "source-b")
	r.c = other.c
	r.newMirror()
	r.window(1)
	was := tree(t, r.dir)
	err := r.m.Sync()
	if err == nil || !strings.Contains(err.Error(), "vbilling-1/0badcafe") {
		t.Fatalf("mirroring into another instance's mirror: %v", err)
	}
	if src := r.mirrorHealth(); src.Status != health.Failing || !strings.Contains(src.Hint, "its own ARCHIVE_DIR") {
		t.Fatalf("health: %+v", src)
	}
	now := tree(t, r.dir)
	for rel, content := range was {
		if now[rel] != content {
			t.Fatalf("%s of the other instance's mirror was changed", rel)
		}
	}
	if asides, _ := filepath.Glob(r.dir + mirrorSetAside + "*"); len(asides) != 0 {
		t.Fatal("a mirror that may be in use was moved away")
	}
	// Nobody wrote it for a while: it is a leftover.
	r.c.Add(conflictWindow + time.Minute)
	r.sync()
	if asides, _ := filepath.Glob(r.dir + mirrorSetAside + "*"); len(asides) != 1 {
		t.Fatalf("the leftover was not set aside:\n%s", r.logs)
	}
	checkMirror(t, r.dir)

	// While this copier runs, something else replaces the recovery point.
	r.window(1)
	r.sync()
	b, err := os.ReadFile(filepath.Join(r.dir, MirrorManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var man MirrorManifest
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatal(err)
	}
	man.Writer, man.WrittenAt = "vbilling-1/0badcafe", r.c.Now()
	b, _ = json.Marshal(man)
	if err := os.WriteFile(filepath.Join(r.dir, MirrorManifestFile), b, 0o640); err != nil {
		t.Fatal(err)
	}
	r.window(1)
	for i := 0; i < 2; i++ { // found when publishing, and again when looking at the mirror anew
		if err := r.m.Sync(); err == nil || !strings.Contains(err.Error(), "vbilling-1/0badcafe") {
			t.Fatalf("pass %d over a recovery point somebody else wrote: %v", i, err)
		}
	}
	if src := r.mirrorHealth(); src.Status != health.Failing || !strings.Contains(src.Hint, "two vBilling instances") {
		t.Fatalf("health: %+v", src)
	}
	if got, _ := os.ReadFile(filepath.Join(r.dir, MirrorManifestFile)); !bytes.Equal(got, b) {
		t.Fatal("the other writer's recovery point was overwritten")
	}
}

// Whatever the mirror volume does, a window is committed as fast as without
// a mirror, and the mirror says what is wrong.
func TestMirrorVolumeErrorsNeverReachTheLedger(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	setMode := func(t *testing.T, root string, dirMode, fileMode os.FileMode) {
		t.Helper()
		var paths []string
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil {
				paths = append(paths, p)
			}
			return nil
		})
		for i := len(paths) - 1; i >= 0; i-- { // children before their directory
			info, err := os.Lstat(paths[i])
			if err != nil {
				continue
			}
			mode := fileMode
			if info.IsDir() {
				mode = dirMode
			}
			os.Chmod(paths[i], mode)
		}
	}
	failing := func(t *testing.T, r *mirrorRig, hint string) {
		t.Helper()
		before := r.tel.Get(mirrorErrorsMetric, nil)
		r.window(2) // fails the test if the ledger does not take it
		if err := r.m.Sync(); err == nil {
			t.Fatal("a pass on a broken mirror volume succeeded")
		}
		r.c.Add(30 * time.Second)
		r.m.report(true)
		src := r.mirrorHealth()
		if src.Status != health.Failing || !strings.Contains(src.Hint, hint) || !strings.Contains(src.Message, "behind the ledger") {
			t.Fatalf("health: %+v", src)
		}
		if got := r.tel.Get(mirrorErrorsMetric, nil); got != before+1 {
			t.Fatalf("%s went from %v to %v for one failed pass", mirrorErrorsMetric, before, got)
		}
		if lag := r.tel.Get(mirrorLagMetric, nil); lag < 30 {
			t.Fatalf("%s is %v with a window of 30s ago not mirrored", mirrorLagMetric, lag)
		}
	}
	recovered := func(t *testing.T, r *mirrorRig) {
		t.Helper()
		for i := 0; i < 3 && r.m.Sync() != nil; i++ {
		}
		r.sync()
		if src := r.mirrorHealth(); src.Status != health.OK {
			t.Fatalf("health after the volume came back: %+v", src)
		}
		if lag := r.tel.Get(mirrorLagMetric, nil); lag != 0 {
			t.Fatalf("%s is %v, in step", mirrorLagMetric, lag)
		}
		checkMirror(t, r.dir)
		r.restoreAndCompare()
	}

	t.Run("read-only", func(t *testing.T) {
		r := newMirrorRig(t, Options{SegmentMaxRecords: 4})
		r.window(2)
		r.sync()
		setMode(t, r.dir, 0o500, 0o400)
		t.Cleanup(func() { setMode(t, r.dir, 0o750, 0o640) })
		failing(t, r, "read-only or not writable")
		failing(t, r, "read-only or not writable")
		checkMirror(t, r.dir) // still the recovery point it had
		setMode(t, r.dir, 0o750, 0o640)
		recovered(t, r)
		if !r.logs.has("ledger-mirror recovered") {
			t.Fatalf("recovery is not logged:\n%s", r.logs)
		}
	})
	t.Run("removed", func(t *testing.T) {
		r := newMirrorRig(t, Options{SegmentMaxRecords: 4})
		r.window(2)
		r.sync()
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
		r.window(2)
		recovered(t, r) // it is created and filled again
	})
	t.Run("gone with its volume", func(t *testing.T) {
		r := newMirrorRig(t, Options{SegmentMaxRecords: 4})
		r.window(2)
		r.sync()
		parent := filepath.Dir(r.dir)
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(parent, 0o750) })
		failing(t, r, "read-only or not writable")
		os.Chmod(parent, 0o750)
		recovered(t, r)
	})
	t.Run("a file where the directory should be", func(t *testing.T) {
		r := newMirrorRig(t, Options{})
		if err := os.MkdirAll(filepath.Dir(r.dir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.dir, []byte("not a directory"), 0o640); err != nil {
			t.Fatal(err)
		}
		failing(t, r, "check that the archive volume is mounted")
	})
	t.Run("the ledger itself is damaged", func(t *testing.T) {
		r := newMirrorRig(t, Options{})
		r.window(2)
		r.sync()
		r.window(2)
		// A byte of a committed record changes on the data volume.
		segs, _ := listSegments(filepath.Join(r.data, "ledger"))
		path := filepath.Join(r.data, "ledger", segs[0].name)
		b, _ := os.ReadFile(path)
		b[len(b)-30] ^= 0x01
		if err := os.WriteFile(path, b, 0o640); err != nil {
			t.Fatal(err)
		}
		was := segmentBytes(t, filepath.Join(r.dir, "ledger"))
		if err := r.m.Sync(); err == nil || !strings.Contains(err.Error(), "does not verify") {
			t.Fatalf("copying a damaged record: %v", err)
		}
		if src := r.mirrorHealth(); src.Status != health.Failing || !strings.Contains(src.Hint, "vbilling ledger verify") {
			t.Fatalf("health: %+v", src)
		}
		if !bytes.Equal(segmentBytes(t, filepath.Join(r.dir, "ledger")), was) {
			t.Fatal("damaged bytes reached the mirror")
		}
		checkMirror(t, r.dir) // the copy from before the damage is intact
	})
}

func TestMirrorLagAndHealth(t *testing.T) {
	r := newMirrorRig(t, Options{})
	lag := func() float64 {
		r.m.report(true)
		return r.tel.Get(mirrorLagMetric, nil)
	}
	if got := lag(); got != 0 {
		t.Fatalf("lag of an empty ledger: %v", got)
	}
	r.window(1) // written at t0+1m
	r.c.Add(20 * time.Second)
	if got := lag(); got != 20 {
		t.Fatalf("lag before the first pass is %v, the oldest record is 20s old", got)
	}
	if msg := r.mirrorHealth().Message; !strings.Contains(msg, "no recovery point yet") {
		t.Fatalf("health before the first pass: %s", msg)
	}
	r.sync()
	if got := r.tel.Get(mirrorLagMetric, nil); got != 0 {
		t.Fatalf("lag in step: %v", got)
	}
	src := r.mirrorHealth()
	if src.Status != health.OK || src.Kind != "storage" || src.Subject != r.dir ||
		!strings.Contains(src.Message, "in step with the ledger, ledger seq 2, records seq 0, 0 state file(s)") {
		t.Fatalf("health in step: %+v", src)
	}
	// The mirror shares the ledger's filesystem here, as in a lab on one node.
	if !strings.Contains(src.Message, "same filesystem as the ledger") || !r.logs.has("does not survive losing that volume") {
		t.Fatalf("no warning that the mirror is on the ledger's own filesystem: %s\n%s", src.Message, r.logs)
	}

	// The lag is the age of the oldest record the mirror lacks, not of the
	// newest: it grows for as long as the mirror is behind.
	r.window(1)
	r.c.Add(30 * time.Second)
	if got := lag(); got != 30 {
		t.Fatalf("lag with one window not mirrored for 30s: %v", got)
	}
	r.window(1) // a minute later
	r.c.Add(10 * time.Second)
	if got := lag(); got != 100 {
		t.Fatalf("lag with windows of 100s and 10s ago not mirrored: %v, want 100", got)
	}
	if msg := r.mirrorHealth().Message; !strings.Contains(msg, "1m40s behind the ledger") {
		t.Fatalf("health behind: %s", msg)
	}
	// Per-request records count too.
	r.sync()
	if _, err := r.records.AppendBatch("gateway", nil); err != nil {
		t.Fatal(err)
	}
	r.c.Add(5 * time.Second)
	if got := lag(); got != 5 {
		t.Fatalf("lag with a record of 5s ago not mirrored: %v", got)
	}
	r.sync()
	if got := r.tel.Get(mirrorLagMetric, nil); got != 0 {
		t.Fatalf("lag in step: %v", got)
	}
}

// A pass that hangs on the mirror volume blocks nothing and is reported by
// the reporter that runs next to it.
func TestMirrorReportsAHangingVolume(t *testing.T) {
	r := newMirrorRig(t, Options{})
	r.window(1)
	r.sync()
	hang, entered := make(chan struct{}), make(chan struct{})
	first := true
	r.m.io.before = func(op, path string, n int) (int, error) {
		if first {
			first = false
			close(entered)
			<-hang
		}
		return n, nil
	}
	r.window(1)
	done := make(chan error, 1)
	go func() { done <- r.m.Sync() }()
	<-entered
	r.window(1) // the ledger does not wait for the mirror
	r.m.report(false)
	r.c.Add(2 * time.Minute)
	r.m.report(false)
	src := r.mirrorHealth()
	if src.Status != health.Failing || !strings.Contains(src.Message, "does not answer") {
		t.Fatalf("health while a pass hangs: %+v", src)
	}
	if lag := r.tel.Get(mirrorLagMetric, nil); lag < 120 {
		t.Fatalf("lag while a pass hangs: %v", lag)
	}
	close(hang)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.sync()
	if src := r.mirrorHealth(); src.Status != health.OK {
		t.Fatalf("health after the volume answered: %+v", src)
	}
}

// A mirror inside DATA_DIR, with the archive next to it, never copies itself.
func TestMirrorInsideDataDir(t *testing.T) {
	r := newMirrorRig(t, Options{})
	archive := filepath.Join(r.data, "archive")
	r.dir = MirrorDir(archive)
	if err := os.MkdirAll(archive, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archive, "seg-00000000000000000001.log.gz"), []byte("archived"), 0o640); err != nil {
		t.Fatal(err)
	}
	r.m = NewMirror(MirrorOptions{DataDir: r.data, Dir: r.dir, Ledgers: []*Spool{r.ledger, r.records}, Exclude: []string{archive},
		NoSync: true, Now: r.c.Now, Logf: r.logs.printf})
	r.state("billing-state.json", `{}`)
	r.window(1)
	r.sync()
	r.sync()
	man, err := ReadMirrorManifest(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != 1 || man.Files[0].Path != "billing-state.json" {
		t.Fatalf("mirrored %+v, want only billing-state.json", man.Files)
	}
	// A forced restore over the data directory keeps the archive and the mirror.
	if _, err := RestoreMirror(r.dir, r.data, RestoreOptions{Force: true, NoSync: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archive, "seg-00000000000000000001.log.gz")); err != nil {
		t.Fatalf("the archive did not survive a forced restore: %v", err)
	}
	checkMirror(t, r.dir)
}

func TestMirrorRefusesAnImpossibleSetup(t *testing.T) {
	c := &clock{t: t0}
	data := t.TempDir()
	elsewhere := open(t, filepath.Join(t.TempDir(), "ledger"), c, Options{})
	defer elsewhere.Close()
	named := open(t, filepath.Join(data, mirrorStateDir), c, Options{})
	defer named.Close()
	for name, ledgers := range map[string][]*Spool{"outside DATA_DIR": {elsewhere}, "named like the mirror's own directory": {named}, "none": nil} {
		t.Run(name, func(t *testing.T) {
			m := NewMirror(MirrorOptions{DataDir: data, Dir: filepath.Join(t.TempDir(), "mirror"), Ledgers: ledgers, NoSync: true, Logf: func(string, ...any) {}})
			if err := m.Sync(); err == nil {
				t.Fatal("a pass succeeded")
			}
		})
	}
}
