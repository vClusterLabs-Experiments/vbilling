package spool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mirroredRig is a ledger of five windows with a cursor and state, mirrored.
func mirroredRig(t *testing.T) *mirrorRig {
	t.Helper()
	r := newMirrorRig(t, Options{SegmentMaxRecords: 4})
	r.state("source.json", `{"id":"source-a"}`)
	r.state("billing-state.json", `{"tenant-0":"suspended"}`)
	r.ledger.Cursor("stripe")
	for i := 0; i < 5; i++ {
		r.window(2)
	}
	if err := r.ledger.Ack("stripe", 9); err != nil {
		t.Fatal(err)
	}
	r.sync()
	return r
}

// rewriteManifest edits the mirror's recovery point in place.
func rewriteManifest(t *testing.T, dir string, edit func(*MirrorManifest)) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, MirrorManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var man MirrorManifest
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatal(err)
	}
	edit(&man)
	if b, err = json.Marshal(man); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, MirrorManifestFile), b, 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRefusesATargetThatHoldsALedger(t *testing.T) {
	r := mirroredRig(t)
	lost := r.snapshot()

	// Another ledger, with state of its own, where the restore should go.
	other := newMirrorRig(t, Options{})
	other.state("source.json", `{"id":"source-b"}`)
	other.state("held/stripe/stale.jsonl", `{"seq":900}`+"\n")
	other.window(1)
	other.closeLedgers()
	was := tree(t, other.data)

	_, err := RestoreMirror(r.dir, other.data, RestoreOptions{NoSync: true})
	if !errors.Is(err, ErrTargetHoldsLedger) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("restore over a ledger: %v", err)
	}
	now := tree(t, other.data)
	if len(now) != len(was) {
		t.Fatalf("a refused restore changed the target: %d files, had %d", len(now), len(was))
	}
	for rel, content := range was {
		if now[rel] != content {
			t.Fatalf("a refused restore changed %s", rel)
		}
	}

	rep, err := RestoreMirror(r.dir, other.data, RestoreOptions{Force: true, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Replaced == nil || rep.Replaced.LastSeq != 2 || rep.Replaced.Identity != "source-b" || rep.Identity != "source-a" {
		t.Fatalf("report of the replaced ledger: %+v", rep.Replaced)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "another metering source identity") || !strings.Contains(rep.Notes[0], "billed twice") {
		t.Fatalf("no warning that the replaced ledger metered under another identity: %v", rep.Notes)
	}
	// Nothing of the other ledger's state stays next to the restored one.
	if _, err := os.Stat(filepath.Join(other.data, "held", "stripe", "stale.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a held event of the replaced ledger survived: %v", err)
	}
	got := stateTree(t, other.data)
	if len(got) != len(lost.state) {
		t.Fatalf("restored state %v, want %v", got, lost.state)
	}
	for rel, content := range lost.state {
		if got[rel] != content {
			t.Fatalf("restored %s is %q, want %q", rel, got[rel], content)
		}
	}
	sp, err := Open(filepath.Join(other.data, "ledger"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.Committed() != lost.committed || !sp.Verify().OK {
		t.Fatalf("restored ledger at seq %d, want %d", sp.Committed(), lost.committed)
	}

	for name, target := range map[string]string{"the target inside the mirror": filepath.Join(r.dir, "inside"), "the mirror itself": r.dir} {
		if _, err := RestoreMirror(r.dir, target, RestoreOptions{Force: true, NoSync: true}); err == nil || !strings.Contains(err.Error(), "inside the mirror") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := RestoreMirror(t.TempDir(), t.TempDir(), RestoreOptions{}); !errors.Is(err, ErrNoRecoveryPoint) {
		t.Fatalf("restore from a directory that is not a mirror: %v", err)
	}
}

// A cursor beyond the restored head would make its destination skip the
// records that take those sequence numbers after the restore. The mirror
// never holds one; a restore still lowers it, and so does the ledger itself.
func TestRestoreClampsCursorsToTheRestoredHead(t *testing.T) {
	r := mirroredRig(t)
	head := r.ledger.Committed()
	// Somebody edits the mirror: the cursors' version, and the recovery
	// point so that it still verifies.
	ahead := []byte(`{"lago": 4, "metronome": 4000, "stripe": 9999}`)
	rewriteManifest(t, r.dir, func(man *MirrorManifest) {
		for i, f := range man.Files {
			if f.Path != "ledger/"+cursorsFile {
				continue
			}
			if err := os.WriteFile(filepath.Join(r.dir, mirrorStateDir, f.File), ahead, 0o640); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(ahead)
			man.Files[i].Size, man.Files[i].SHA256 = int64(len(ahead)), hex.EncodeToString(sum[:])
		}
	})

	to := filepath.Join(t.TempDir(), "restored")
	rep, err := RestoreMirror(r.dir, to, RestoreOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	l := rep.Ledgers[0]
	if l.Cursors["stripe"] != head || l.Cursors["metronome"] != head || l.Cursors["lago"] != 4 {
		t.Fatalf("restored cursors %v, the head is %d", l.Cursors, head)
	}
	if l.Clamped["stripe"] != 9999 || l.Clamped["metronome"] != 4000 || len(l.Clamped) != 2 {
		t.Fatalf("cursors reported as lowered: %v", l.Clamped)
	}
	if len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "lowered") {
		t.Fatalf("notes: %v", rep.Notes)
	}
	onDisk := map[string]uint64{}
	b, _ := os.ReadFile(filepath.Join(to, "ledger", cursorsFile))
	if err := json.Unmarshal(b, &onDisk); err != nil || onDisk["stripe"] != head {
		t.Fatalf("cursors.json after the restore: %s (%v)", b, err)
	}
	sp, err := Open(filepath.Join(to, "ledger"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	for _, dest := range []string{"stripe", "metronome", "lago"} {
		if c := sp.Cursor(dest); c > sp.Committed() {
			t.Fatalf("cursor of %s is %d, beyond the restored head %d", dest, c, sp.Committed())
		}
	}
	// The windows after the head are metered again and take the next
	// sequence numbers: every destination must be sent them.
	if err := sp.AppendWindow(SourceCollector, t0.Add(time.Hour), nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{"stripe", "metronome"} {
		recs, err := sp.Read(sp.Cursor(dest), 10)
		if err != nil || len(recs) != 1 || recs[0].Seq != head+1 {
			t.Fatalf("%s would skip the window metered after the restore: %v, %v", dest, recs, err)
		}
	}

	// A copy made by hand, without the restore command: the ledger lowers
	// the cursor when it opens.
	byHand := filepath.Join(t.TempDir(), "ledger")
	copyTree(t, filepath.Join(r.dir, "ledger"), byHand)
	if err := os.WriteFile(filepath.Join(byHand, cursorsFile), ahead, 0o640); err != nil {
		t.Fatal(err)
	}
	sp2, err := Open(byHand, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp2.Close()
	if c := sp2.Cursor("stripe"); c != head {
		t.Fatalf("a ledger opened with a cursor at 9999 has it at %d, its head is %d", c, head)
	}
}

// A restore that did not finish leaves a data directory vBilling refuses to
// open, and running it again finishes it.
func TestRestoreInterrupted(t *testing.T) {
	r := mirroredRig(t)
	to := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreMirror(r.dir, to, RestoreOptions{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	// As it is in the middle of a restore: the markers are there, a segment
	// and a state file are not yet.
	mark, _ := json.Marshal(restoreMark{From: r.dir, StartedAt: t0})
	for _, name := range []string{"ledger", "records"} {
		if err := os.WriteFile(filepath.Join(to, name, restoreMarker), mark, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	segs, _ := listSegments(filepath.Join(to, "ledger"))
	if err := os.Remove(filepath.Join(to, "ledger", segs[len(segs)-1].name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(to, "billing-state.json")); err != nil {
		t.Fatal(err)
	}

	_, err := Open(filepath.Join(to, "ledger"), Options{NoSync: true})
	if err == nil || !strings.Contains(err.Error(), "restore that did not finish") || !strings.Contains(err.Error(), "vbilling ledger restore --from "+r.dir) {
		t.Fatalf("opening a ledger restored halfway: %v", err)
	}
	// Finishing it needs no --force: everything there is the restore's own.
	if _, err := RestoreMirror(r.dir, to, RestoreOptions{NoSync: true}); err != nil {
		t.Fatalf("running the restore again: %v", err)
	}
	restored := stateTree(t, to)
	for rel, content := range r.snapshot().state {
		if restored[rel] != content {
			t.Fatalf("%s after finishing the restore: %q", rel, restored[rel])
		}
	}
	sp, err := Open(filepath.Join(to, "ledger"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if sp.Committed() != r.ledger.Committed() || !sp.Verify().OK {
		t.Fatalf("the finished restore is at seq %d", sp.Committed())
	}
}

func TestRestoreIfLost(t *testing.T) {
	logs := &logSink{}

	t.Run("no mirror directory configured", func(t *testing.T) {
		if rep, err := RestoreIfLost("", t.TempDir(), logs.printf); rep != nil || err != nil {
			t.Fatalf("%v, %v", rep, err)
		}
	})
	t.Run("a new install: no mirror yet", func(t *testing.T) {
		data := t.TempDir()
		if rep, err := RestoreIfLost(filepath.Join(t.TempDir(), "archive", "mirror"), data, logs.printf); rep != nil || err != nil {
			t.Fatalf("%v, %v", rep, err)
		}
		if entries, _ := os.ReadDir(data); len(entries) != 0 {
			t.Fatalf("a new install's data directory was touched: %v", entries)
		}
	})
	t.Run("a mirror that never completed a pass", func(t *testing.T) {
		r := mirroredRig(t)
		if err := os.Remove(filepath.Join(r.dir, MirrorManifestFile)); err != nil {
			t.Fatal(err)
		}
		if rep, err := RestoreIfLost(r.dir, t.TempDir(), logs.printf); rep != nil || err != nil {
			t.Fatalf("%v, %v", rep, err)
		}
	})
	t.Run("the data directory holds its ledger", func(t *testing.T) {
		r := mirroredRig(t)
		r.window(1) // newer than the mirror
		was := r.snapshot()
		if rep, err := RestoreIfLost(r.dir, r.data, logs.printf); rep != nil || err != nil {
			t.Fatalf("%v, %v", rep, err)
		}
		if now := r.snapshot(); now.committed != was.committed || string(now.ledger) != string(was.ledger) {
			t.Fatal("a data directory that holds a ledger was changed")
		}
	})
	t.Run("the data volume was lost", func(t *testing.T) {
		r := mirroredRig(t)
		lost := r.snapshot()
		r.window(1) // never reaches the mirror
		logs := &logSink{}
		for _, data := range []string{t.TempDir(), filepath.Join(t.TempDir(), "not", "there", "yet")} {
			rep, err := RestoreIfLost(r.dir, data, logs.printf)
			if err != nil || rep == nil {
				t.Fatalf("%v, %v", rep, err)
			}
			// source.json, billing-state.json, and the ledger's cursors and destination marks.
			if rep.Identity != "source-a" || rep.Ledgers[0].LastSeq != lost.committed || rep.StateFiles != 4 {
				t.Fatalf("report %+v", rep)
			}
			sp, err := Open(filepath.Join(data, "ledger"), Options{NoSync: true})
			if err != nil {
				t.Fatal(err)
			}
			if sp.Committed() != lost.committed || !sp.Verify().OK {
				t.Fatalf("restored ledger at seq %d, want %d", sp.Committed(), lost.committed)
			}
			sp.Close()
			// The next start finds a ledger and leaves it alone.
			if rep, err := RestoreIfLost(r.dir, data, logs.printf); rep != nil || err != nil {
				t.Fatalf("second start: %v, %v", rep, err)
			}
		}
		for _, want := range []string{"holds no ledger, but the mirror", "restored", "To start as a new install instead"} {
			if !logs.has(want) {
				t.Fatalf("the log does not say %q:\n%s", want, logs)
			}
		}
	})
	t.Run("a restore that did not finish", func(t *testing.T) {
		r := mirroredRig(t)
		data := t.TempDir()
		mark, _ := json.Marshal(restoreMark{From: r.dir, StartedAt: t0})
		if err := os.MkdirAll(filepath.Join(data, "ledger"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "ledger", restoreMarker), mark, 0o640); err != nil {
			t.Fatal(err)
		}
		// The usage ledger's marker goes last: a restore that died right
		// before leaves a finished records ledger next to it.
		if _, err := RestoreMirror(r.dir, filepath.Join(t.TempDir(), "elsewhere"), RestoreOptions{NoSync: true}); err != nil {
			t.Fatal(err)
		}
		copyTree(t, filepath.Join(r.data, "records"), filepath.Join(data, "records"))
		rep, err := RestoreIfLost(filepath.Join(t.TempDir(), "another", "mirror"), data, logs.printf)
		if err != nil || rep == nil || rep.From != r.dir {
			t.Fatalf("finishing from the mirror the restore started with: %+v, %v", rep, err)
		}
		if _, err := Open(filepath.Join(data, "ledger"), Options{NoSync: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a restore that did not finish and no mirror", func(t *testing.T) {
		data := t.TempDir()
		if err := os.MkdirAll(filepath.Join(data, "ledger"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(data, "ledger", restoreMarker), []byte(`{"from":"/gone"}`), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := RestoreIfLost(filepath.Join(t.TempDir(), "mirror"), data, logs.printf); !errors.Is(err, ErrNoRecoveryPoint) {
			t.Fatalf("a data directory restored halfway and nothing to finish it from: %v", err)
		}
	})
	t.Run("a mirror that does not verify", func(t *testing.T) {
		r := mirroredRig(t)
		segs, _ := listSegments(filepath.Join(r.dir, "ledger"))
		if err := os.Truncate(filepath.Join(r.dir, "ledger", segs[0].name), 100); err != nil {
			t.Fatal(err)
		}
		data := t.TempDir()
		_, err := RestoreIfLost(r.dir, data, logs.printf)
		if err == nil || !strings.Contains(err.Error(), "does not verify, nothing was restored") {
			t.Fatalf("restoring a damaged mirror: %v", err)
		}
		if entries, _ := os.ReadDir(data); len(entries) != 0 {
			t.Fatalf("a restore that could not start changed the data directory: %v", entries)
		}
	})
	t.Run("a recovery point that is not readable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads everything")
		}
		r := mirroredRig(t)
		path := filepath.Join(r.dir, MirrorManifestFile)
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o640) })
		logs := &logSink{}
		// The volume cannot be read: nothing says a ledger was lost, and a
		// mirror volume never keeps vBilling from starting.
		if rep, err := RestoreIfLost(r.dir, t.TempDir(), logs.printf); rep != nil || err != nil {
			t.Fatalf("%v, %v", rep, err)
		}
		if !logs.has("WARNING") || !logs.has("cannot be read") {
			t.Fatalf("no warning:\n%s", logs)
		}
	})
}

func TestReadMirrorManifestRejectsWhatARestoreMustNotWrite(t *testing.T) {
	for name, edit := range map[string]func(*MirrorManifest){
		"a file outside the target":       func(m *MirrorManifest) { m.Files[0].Path = "../outside.json" },
		"an absolute file path":           func(m *MirrorManifest) { m.Files[0].Path = "/etc/passwd" },
		"a version outside the mirror":    func(m *MirrorManifest) { m.Files[0].File = "../../outside" },
		"a file that is a segment":        func(m *MirrorManifest) { m.Files[0].Path = "ledger/seg-00000000000000000001.log" },
		"a file that is a ledger's index": func(m *MirrorManifest) { m.Files[0].Path = "ledger/" + ledgerManifest },
		"a file that is a ledger":         func(m *MirrorManifest) { m.Files[0].Path = "ledger" },
		"the same file twice":             func(m *MirrorManifest) { m.Files = append(m.Files, m.Files[0]) },
		"a negative size":                 func(m *MirrorManifest) { m.Files[0].Size = -1 },
		"a ledger outside the target":     func(m *MirrorManifest) { m.Ledgers[0].Dir = "../ledger" },
		"the same ledger twice":           func(m *MirrorManifest) { m.Ledgers[1].Dir = m.Ledgers[0].Dir },
		"a segment name that is none":     func(m *MirrorManifest) { m.Ledgers[0].LastSegment = "cursors.json" },
		"segments in the wrong order":     func(m *MirrorManifest) { m.Ledgers[0].FirstSegment = "seg-99999999999999999999.log" },
		"another format":                  func(m *MirrorManifest) { m.Version = 2 },
		"no ledger":                       func(m *MirrorManifest) { m.Ledgers = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := mirroredRig(t)
			rewriteManifest(t, r.dir, edit)
			if _, err := ReadMirrorManifest(r.dir); err == nil {
				t.Fatal("accepted")
			}
			if rep := VerifyMirror(r.dir); rep.OK {
				t.Fatal("verified")
			}
			to := filepath.Join(t.TempDir(), "to")
			if _, err := RestoreMirror(r.dir, to, RestoreOptions{NoSync: true}); err == nil {
				t.Fatal("restored")
			}
			if _, err := os.Stat(to); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused restore created the target")
			}
		})
	}
}

// VerifyMirror reads only what the recovery point names, and notices every
// way in which that is not there any more.
func TestVerifyMirror(t *testing.T) {
	r := mirroredRig(t)
	rep := VerifyMirror(r.dir)
	if !rep.OK || rep.Identity != "source-a" || rep.RecoveryPoint == nil || rep.StateFiles != 4 || rep.StateBytes == 0 {
		t.Fatalf("report %+v", rep)
	}
	l := rep.Ledgers[0]
	wm, _ := r.ledger.Watermark(SourceCollector)
	if l.Dir != "ledger" || l.FirstSeq != 1 || l.LastSeq != r.ledger.Committed() || l.Records != 15 || l.Segments != 3 || !l.Watermarks[SourceCollector].Equal(wm) {
		t.Fatalf("ledger report %+v", l)
	}
	// A pass in progress adds bytes after the recovery point: not part of it.
	segs, _ := listSegments(filepath.Join(r.dir, "ledger"))
	f, err := os.OpenFile(filepath.Join(r.dir, "ledger", segs[len(segs)-1].name), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("half a record of the next window")
	f.Close()
	if rep := VerifyMirror(r.dir); !rep.OK {
		t.Fatalf("bytes after the recovery point fail it: %v", rep.Errors)
	}
	r.restoreAndCompare()

	rewriteManifest(t, r.dir, func(m *MirrorManifest) { m.Ledgers[0].Head = strings.Repeat("00", 32) })
	if rep := VerifyMirror(r.dir); rep.OK || !strings.Contains(strings.Join(rep.Errors, " "), "the recovery point says") {
		t.Fatalf("a head the records do not end at: %+v", rep.Errors)
	}
}

// A destination an operator paused, and one that waits for a start decision,
// are still paused and still waiting after a restore: their marks are state
// of the ledger directory and come back with the cursors. A restored ledger
// never sends a backlog nobody released.
func TestRestoreKeepsDestinationPausesAndStartDecisions(t *testing.T) {
	r := mirroredRig(t) // the cursor of "stripe" is at 9
	if _, err := r.ledger.PauseDestination("stripe", "sandbox cleanup"); err != nil {
		t.Fatal(err)
	}
	// "metronome" was removed from the configuration and added back with a
	// backlog: it waits for an operator.
	r.ledger.Cursor("metronome")
	if _, err := r.ledger.MarkDetached("metronome"); err != nil {
		t.Fatal(err)
	}
	r.window(2)
	if waiting, err := r.ledger.AwaitStart("metronome"); err != nil || !waiting {
		t.Fatalf("await start: %v, %v", waiting, err)
	}
	wantCursor, _ := r.ledger.CursorOf("metronome")
	r.sync()

	data := t.TempDir()
	if _, err := RestoreMirror(r.dir, data, RestoreOptions{NoSync: true}); err != nil {
		t.Fatal(err)
	}
	sp, err := Open(filepath.Join(data, "ledger"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	if st := sp.DestinationState("stripe"); st.Paused == nil || st.Paused.Reason != "sandbox cleanup" {
		t.Fatalf("the paused destination after the restore: %+v", st)
	}
	if c, ok := sp.CursorOf("stripe"); !ok || c != 9 {
		t.Fatalf("the paused destination's cursor after the restore: %d, %v", c, ok)
	}
	st := sp.DestinationState("metronome")
	if st.Detached == nil || !st.Detached.AwaitingStart {
		t.Fatalf("the destination that waited for a start decision after the restore: %+v", st)
	}
	if c, ok := sp.CursorOf("metronome"); !ok || c != wantCursor || c >= sp.Committed() {
		t.Fatalf("its cursor after the restore: %d, %v (was %d, head %d)", c, ok, wantCursor, sp.Committed())
	}
}

// An automatic restore says how old its recovery point is, and warns when it
// is older than a day: a billing platform that deduplicates by event ID for
// a day no longer recognizes what was in flight when the copy was made.
func TestRestoreIfLostSaysWhenTheRecoveryPointIsOld(t *testing.T) {
	const warning = "WARNING: that recovery point is"
	r := mirroredRig(t) // its clock, and so its recovery point, is long past
	old := &logSink{}
	if rep, err := RestoreIfLost(r.dir, t.TempDir(), old.printf); err != nil || rep == nil {
		t.Fatalf("%v, %v", rep, err)
	}
	if !old.has(warning) || !old.has("GET /api/v1/reconcile") {
		t.Fatalf("no warning about an old recovery point:\n%s", old.String())
	}
	// The usual case: the copy was made seconds before the volume was lost.
	rewriteManifest(t, r.dir, func(m *MirrorManifest) {
		m.CutAt = time.Now().UTC().Add(-3 * time.Second)
		m.WrittenAt = m.CutAt.Add(time.Second)
	})
	fresh := &logSink{}
	if rep, err := RestoreIfLost(r.dir, t.TempDir(), fresh.printf); err != nil || rep == nil {
		t.Fatalf("%v, %v", rep, err)
	}
	if fresh.has(warning) || !fresh.has(" ago (the mirror's last complete pass)") {
		t.Fatalf("a recovery point of seconds ago:\n%s", fresh.String())
	}
}
