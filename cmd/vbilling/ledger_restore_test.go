package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/sourceid"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// lostVolume is a data directory as vBilling keeps it, with its mirror, at
// the moment its volume is lost.
type lostVolume struct {
	data, mirror string
	identity     string
	start        time.Time
	// What the mirror's last pass saw.
	head      uint64
	watermark time.Time
	records   []spool.Record // every record up to head
	segments  []byte         // the ledger's bytes up to head
	// After that pass, never mirrored.
	after []spool.Record
	// stateFiles counts what the data directory held besides its ledgers'
	// segments and manifests when the mirror made its pass.
	stateFiles int
}

// windowEvents are the events vBilling meters for one window: the same call
// gives the same event IDs, before the loss and when the window is metered
// again after a restore.
func windowEvents(identity string, ws time.Time) []usage.Event {
	var evs []usage.Event
	for _, tenant := range []string{"a", "b"} {
		e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: 8.0 / 60, WindowStart: ws, WindowEnd: ws.Add(time.Minute),
			Dimensions: map[string]string{usage.DimMeteringSource: identity}}
		e.Finalize()
		evs = append(evs, e)
	}
	return evs
}

func ledgerBytes(t *testing.T, dir string) []byte {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, name := range names { // zero-padded: in sequence order
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

// countStateFiles counts the files of a data directory that are not a
// ledger's segments or manifest: what a mirror keeps as state. The ledger
// directory holds some of them (cursors, dead letters, the destinations'
// start and pause marks).
func countStateFiles(t *testing.T, dir string) int {
	t.Helper()
	n, marks := 0, false
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") || name == "manifest.json" || (strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".log")) {
			return nil
		}
		marks = marks || name == "destinations.json"
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 6 || !marks {
		t.Fatalf("the data directory holds %d state file(s), the destinations' marks among them: %v", n, marks)
	}
	return n
}

// loseVolume builds a data directory with windows, cursors, held events,
// dead letters, counter baselines and per-request records, mirrors it, and
// then lets vBilling go on for two windows that never reach the mirror.
func loseVolume(t *testing.T) lostVolume {
	t.Helper()
	v := lostVolume{data: t.TempDir(), start: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)}
	v.mirror = spool.MirrorDir(filepath.Join(t.TempDir(), "archive", "vbilling-0"))
	now := v.start
	clock := func() time.Time { return now }
	var err error
	if v.identity, err = sourceid.Load(v.data); err != nil {
		t.Fatal(err)
	}
	ledger, err := spool.Open(filepath.Join(v.data, "ledger"), spool.Options{SegmentMaxRecords: 5, NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	records, err := spool.Open(filepath.Join(v.data, "records"), spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	held, err := pipeline.OpenHeld(filepath.Join(v.data, "held"), true)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Cursor("stripe")
	ledger.Cursor("metronome")
	window := func(w int) {
		ws := v.start.Add(time.Duration(w) * time.Minute)
		now = ws.Add(time.Minute)
		if err := ledger.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), windowEvents(v.identity, ws), ""); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(seq uint64) {
		recs, err := ledger.Read(seq-1, 1)
		if err != nil || len(recs) != 1 || recs[0].Event == nil {
			t.Fatalf("record %d: %v, %v", seq, recs, err)
		}
		if _, err := held.Add("metronome", []pipeline.HeldEntry{{Seq: seq, At: now, Series: "gpu_hours|" + recs[0].Event.Tenant, Reason: "no price", Event: *recs[0].Event}}); err != nil {
			t.Fatal(err)
		}
	}
	for w := 0; w < 5; w++ { // seq 1-15: two events and a commit per window
		window(w)
	}
	// Stripe was sent everything but the last window, and rejected one event.
	rejected, _ := ledger.Read(6, 1)
	if err := ledger.AddDeadLetter("stripe", *rejected[0].Event, "customer not found"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Ack("stripe", 12); err != nil {
		t.Fatal(err)
	}
	// Metronome has no price for tenant b's usage of window 4 yet: it holds
	// the event and moves on.
	hold(11)
	if err := ledger.Ack("metronome", 15); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"prom-counters.json": fmt.Sprintf(`{"generations":[{"window_end":%q},{"window_end":%q}]}`,
			v.start.Add(4*time.Minute).Format(time.RFC3339), v.start.Add(5*time.Minute).Format(time.RFC3339)),
		"billing-state.json": `{"a":"active","b":"suspended"}`,
	} {
		if err := os.WriteFile(filepath.Join(v.data, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := records.AppendBatch("gateway", windowEvents(v.identity, v.start)); err != nil {
		t.Fatal(err)
	}

	m := spool.NewMirror(spool.MirrorOptions{DataDir: v.data, Dir: v.mirror, Ledgers: []*spool.Spool{ledger, records}, Identity: v.identity, NoSync: true, Now: clock,
		Logf: func(string, ...any) {}})
	if err := m.Sync(); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	v.head = ledger.Committed()
	v.watermark, _ = ledger.Watermark(spool.SourceCollector)
	v.stateFiles = countStateFiles(t, v.data)
	if v.records, err = ledger.Read(0, 1000); err != nil {
		t.Fatal(err)
	}
	v.segments = ledgerBytes(t, filepath.Join(v.data, "ledger"))

	// vBilling goes on: two more windows, delivered to Stripe, one more
	// event held. Then the volume is gone before the mirror's next pass.
	window(5)
	window(6)
	hold(17)
	if err := ledger.Ack("stripe", ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	if v.after, err = ledger.Read(v.head, 1000); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestLedgerRestoreAfterLosingTheDataVolume is the whole way back: a ledger
// with windows, cursors, held events and dead letters is mirrored, goes on
// for two windows the mirror never sees, and is lost. The restored directory
// must open with the normal ledger code, verify, hold exactly what the mirror
// had, and let vBilling meter the lost windows again under the same event IDs.
func TestLedgerRestoreAfterLosingTheDataVolume(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	v := loseVolume(t)
	data := filepath.Join(t.TempDir(), "new-volume")

	code, out, errOut := runLedger("restore", "--from", v.mirror, "--to", data)
	if code != 0 {
		t.Fatalf("restore exited %d: %s", code, errOut)
	}
	for _, want := range []string{
		"Restored " + data + " from " + v.mirror,
		"recovery point   2026-11-26T10:05:00Z",
		"metering source  " + v.identity,
		"ledger           seq 1-15, 15 records in 3 segment(s), verified; last committed seq 15 written 2026-11-26T10:05:00Z",
		"watermark collector            2026-11-26T10:05:00Z",
		"cursor metronome               15 (in step)",
		"cursor stripe                  12 (3 record(s) to send)",
		"records          seq 1-3, 3 records in 1 segment(s), verified",
		"What vBilling does when it starts on " + data,
		"the next record links to seq 15",
		"up to 24 hours back",
		"deduplicated",
		"Not restored: usage pushed to vBilling after the recovery point",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report does not say %q:\n%s", want, out)
		}
	}

	// It opens with the normal ledger code and verifies.
	now := v.start.Add(30 * time.Minute)
	ledger, err := spool.Open(filepath.Join(data, "ledger"), spool.Options{SegmentMaxRecords: 5, NoSync: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open the restored ledger: %v", err)
	}
	defer ledger.Close()
	if rep := ledger.Verify(); !rep.OK {
		t.Fatalf("the restored ledger does not verify: %v", rep.Errors)
	}
	if rep := spool.VerifyChain(data, ""); !rep.OK || rep.LastSeq != v.head {
		t.Fatalf("offline verification of the restored ledger: %+v", rep)
	}
	// The watermark is the mirror's: the two windows after it are metered again.
	if got := ledger.Committed(); got != v.head {
		t.Fatalf("restored head %d, the mirror had %d", got, v.head)
	}
	if wm, ok := ledger.Watermark(spool.SourceCollector); !ok || !wm.Equal(v.watermark) {
		t.Fatalf("restored watermark %s, the mirror had %s", wm, v.watermark)
	}
	// Every cursor is at or below the restored head.
	cursors := ledger.Stats().Cursors
	if cursors["stripe"] != 12 || cursors["metronome"] != 15 {
		t.Fatalf("restored cursors %v, the mirror had stripe 12 and metronome 15", cursors)
	}
	for dest, c := range cursors {
		if c > v.head {
			t.Fatalf("cursor of %s is %d, beyond the restored head %d: it would skip what is metered again", dest, c, v.head)
		}
	}
	// The events the mirror had are there byte for byte, with the same IDs.
	if got := ledgerBytes(t, filepath.Join(data, "ledger")); !bytes.Equal(got, v.segments) {
		t.Fatalf("the restored segments are %d bytes, the ledger's were %d at the mirror's last pass", len(got), len(v.segments))
	}
	restored, err := ledger.Read(0, 1000)
	if err != nil || len(restored) != len(v.records) {
		t.Fatalf("%d restored records, want %d: %v", len(restored), len(v.records), err)
	}
	for i, rec := range restored {
		want := v.records[i]
		a, _ := json.Marshal(rec)
		b, _ := json.Marshal(want)
		if !bytes.Equal(a, b) || rec.Hash != want.Hash {
			t.Fatalf("restored record %d differs:\n%s\n%s", rec.Seq, a, b)
		}
		if rec.Event != nil && rec.Event.ID != want.Event.ID {
			t.Fatalf("restored event %d has ID %s, was %s", rec.Seq, rec.Event.ID, want.Event.ID)
		}
	}
	// The metering source identity came back with it.
	if id, err := sourceid.Load(data); err != nil || id != v.identity {
		t.Fatalf("restored metering source %q, was %q: %v", id, v.identity, err)
	}
	// Held events and dead letters are as of the mirror's last pass: what a
	// cursor passed without delivering is still parked, nothing newer is.
	held, err := pipeline.OpenHeld(filepath.Join(data, "held"), true)
	if err != nil {
		t.Fatal(err)
	}
	var heldSeqs []uint64
	if err := held.Each("metronome", func(e pipeline.HeldEntry) bool { heldSeqs = append(heldSeqs, e.Seq); return true }); err != nil {
		t.Fatal(err)
	}
	if len(heldSeqs) != 1 || heldSeqs[0] != 11 {
		t.Fatalf("restored held events %v, want the one of seq 11", heldSeqs)
	}
	dead, err := ledger.DeadLetters("stripe")
	if err != nil || len(dead) != 1 || dead[0].Event.ID != v.records[6].Event.ID {
		t.Fatalf("restored dead letters %v: %v", dead, err)
	}
	for name, want := range map[string]string{"billing-state.json": `{"a":"active","b":"suspended"}`} {
		if b, err := os.ReadFile(filepath.Join(data, name)); err != nil || string(b) != want {
			t.Fatalf("restored %s: %s, %v", name, b, err)
		}
	}
	records, err := spool.Open(filepath.Join(data, "records"), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	if records.Committed() != 3 || !records.Verify().OK {
		t.Fatalf("restored records ledger at seq %d", records.Committed())
	}

	// vBilling meters the windows since the watermark again. They take the
	// sequence numbers the lost records had, and the same event IDs, so a
	// billing platform that already has them counts them once.
	for w := 5; w < 7; w++ {
		ws := v.start.Add(time.Duration(w) * time.Minute)
		if err := ledger.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), windowEvents(v.identity, ws), ""); err != nil {
			t.Fatalf("meter window %d again: %v", w, err)
		}
	}
	again, err := ledger.Read(v.head, 1000)
	if err != nil || len(again) != len(v.after) {
		t.Fatalf("%d records metered again, %d were lost: %v", len(again), len(v.after), err)
	}
	for i, rec := range again {
		lost := v.after[i]
		if rec.Seq != lost.Seq || rec.Type != lost.Type {
			t.Fatalf("record %d metered again is %s seq %d, the lost one was %s seq %d", i, rec.Type, rec.Seq, lost.Type, lost.Seq)
		}
		if rec.Event != nil && rec.Event.ID != lost.Event.ID {
			t.Fatalf("event %d metered again has ID %s, the lost one had %s: the billing platform would count both", rec.Seq, rec.Event.ID, lost.Event.ID)
		}
	}
	// Stripe had been sent them before the loss; its restored cursor makes
	// vBilling send them again, with the window it had not been sent before.
	resend, err := ledger.Read(ledger.Cursor("stripe"), 1000)
	if err != nil || len(resend) != 9 || resend[0].Seq != 13 {
		t.Fatalf("stripe is sent %d records from seq %d again, want 9 from 13: %v", len(resend), resend[0].Seq, err)
	}
	if rep := ledger.Verify(); !rep.OK || rep.LastSeq != 21 {
		t.Fatalf("the continued ledger: %+v", rep)
	}

	// The mirror continues with the restored ledger: nothing is set aside.
	var logs []string
	m := spool.NewMirror(spool.MirrorOptions{DataDir: data, Dir: v.mirror, Ledgers: []*spool.Spool{ledger, records}, Identity: v.identity, NoSync: true,
		Logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }})
	if err := m.Sync(); err != nil {
		t.Fatalf("mirroring the restored ledger: %v", err)
	}
	if asides, _ := filepath.Glob(v.mirror + "-set-aside-*"); len(asides) != 0 || strings.Contains(strings.Join(logs, "\n"), "set aside") {
		t.Fatalf("the mirror of the restored ledger was set aside: %v\n%s", asides, strings.Join(logs, "\n"))
	}
	if rep := spool.VerifyMirror(v.mirror); !rep.OK || rep.Ledgers[0].LastSeq != 21 {
		t.Fatalf("the mirror after the restore: %+v", rep)
	}
}

func TestLedgerRestoreRefusesALedgerWithoutForce(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	v := loseVolume(t)

	// The lost volume turns out to be there after all: its ledger is newer
	// than the mirror, and a restore must not replace it by accident.
	code, out, errOut := runLedger("restore", "--from", v.mirror, "--to", v.data)
	if code != 1 || out != "" || !strings.Contains(errOut, "already holds a ledger") || !strings.Contains(errOut, "--force") {
		t.Fatalf("restore over a ledger: exit %d, %q, %q", code, out, errOut)
	}
	if rep := spool.VerifyChain(v.data, ""); !rep.OK || rep.LastSeq != 21 {
		t.Fatalf("a refused restore changed the ledger: %+v", rep)
	}
	code, out, errOut = runLedger("restore", "--from", v.mirror, "--to", v.data, "--force")
	if code != 0 || !strings.Contains(out, "replaced         the ledger that was in "+filepath.Join(v.data, "ledger")+" (last seq 21)") {
		t.Fatalf("forced restore: exit %d, %q, %q", code, out, errOut)
	}
	if rep := spool.VerifyChain(v.data, ""); !rep.OK || rep.LastSeq != v.head {
		t.Fatalf("the ledger after a forced restore: %+v", rep)
	}

	// Inside the pod the flags default to the mirror of ARCHIVE_DIR and to DATA_DIR.
	fresh := filepath.Join(t.TempDir(), "data")
	t.Setenv("DATA_DIR", fresh)
	t.Setenv("ARCHIVE_DIR", filepath.Dir(v.mirror))
	if code, _, errOut := runLedger("restore"); code != 0 {
		t.Fatalf("restore with the pod's defaults: exit %d, %s", code, errOut)
	}
	if rep := spool.VerifyChain(fresh, ""); !rep.OK || rep.LastSeq != v.head {
		t.Fatalf("the ledger restored with the pod's defaults: %+v", rep)
	}
	// With LEDGER_MIRROR_DIR, the mirror is there, not in ARCHIVE_DIR/mirror.
	fresh = filepath.Join(t.TempDir(), "data")
	t.Setenv("DATA_DIR", fresh)
	t.Setenv("ARCHIVE_DIR", t.TempDir())
	t.Setenv("LEDGER_MIRROR_DIR", v.mirror)
	if code, out, errOut := runLedger("restore"); code != 0 || !strings.Contains(out, "Restored "+fresh+" from "+v.mirror) {
		t.Fatalf("restore from LEDGER_MIRROR_DIR: exit %d, %s%s", code, out, errOut)
	}
	if rep := spool.VerifyChain(fresh, ""); !rep.OK || rep.LastSeq != v.head {
		t.Fatalf("the ledger restored from LEDGER_MIRROR_DIR: %+v", rep)
	}
}

func TestLedgerRestoreUsageErrors(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"restore"}, 2, "set --from"},
		{[]string{"restore", "--from", t.TempDir()}, 2, "set --from"},
		{[]string{"restore", "--from", t.TempDir(), "--to", t.TempDir(), "extra"}, 2, "unexpected argument"},
		{[]string{"restore", "--nope"}, 2, "flag provided but not defined"},
		{[]string{"restore", "--from", t.TempDir(), "--to", t.TempDir()}, 1, "ARCHIVE_DIR/mirror"},
	} {
		code, out, errOut := runLedger(tc.args...)
		if code != tc.code || out != "" || !strings.Contains(errOut, tc.want) {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", tc.args, code, out, errOut)
		}
	}
	if code, out, _ := runLedger("restore", "-h"); code != 0 || out != "" {
		t.Fatalf("restore -h: exit %d, stdout %q", code, out)
	}
	if code, out, _ := runLedger("help"); code != 0 || !strings.Contains(out, "restore  bring a lost data directory back from its mirror") {
		t.Fatalf("ledger help does not list restore: %d, %s", code, out)
	}
}

// "vbilling ledger verify" on a mirror directory verifies the mirrored
// ledger like any other, and says whether the recovery point is complete.
func TestLedgerVerifyMirror(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	t.Setenv("ARCHIVE_DIR", "")
	v := loseVolume(t)
	type report struct {
		spool.ChainReport
		Mirror *spool.MirrorReport `json:"mirror"`
	}
	verify := func(wantCode int) report {
		t.Helper()
		code, out, errOut := runLedger("verify", "--dir", v.mirror)
		var rep report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("report is not JSON: %v\n%s", err, out)
		}
		if code != wantCode {
			t.Fatalf("verify exited %d, want %d: %s\n%s", code, wantCode, out, errOut)
		}
		return rep
	}
	rep := verify(0)
	if !rep.OK || rep.Records != 15 || rep.LastSeq != v.head || rep.LiveSegments != 3 {
		t.Fatalf("chain report of the mirrored ledger: %+v", rep.ChainReport)
	}
	if rep.Mirror == nil || !rep.Mirror.OK || rep.Mirror.Identity != v.identity || len(rep.Mirror.Ledgers) != 2 ||
		rep.Mirror.Ledgers[0].LastSeq != v.head || rep.Mirror.StateFiles != v.stateFiles {
		t.Fatalf("mirror report: %+v", rep.Mirror)
	}
	// A data directory is not a mirror: its report has no mirror part.
	if code, out, _ := runLedger("verify", "--dir", v.data); code != 0 || strings.Contains(out, `"mirror"`) {
		t.Fatalf("verify of a data directory: exit %d, %s", code, out)
	}

	// A state file's copy is damaged: the ledger still verifies, the
	// recovery point does not, and the command fails.
	man, err := spool.ReadMirrorManifest(v.mirror)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range man.Files {
		if f.Path == "billing-state.json" {
			if err := os.WriteFile(filepath.Join(v.mirror, "state", f.File), []byte(`{"a":"active","b":"active!!!"}`), 0o640); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep = verify(1)
	if !rep.OK || rep.Mirror.OK || len(rep.Mirror.Errors) != 1 || !strings.Contains(rep.Mirror.Errors[0], "billing-state.json") {
		t.Fatalf("report with a damaged state file: chain ok %v, mirror %+v", rep.OK, rep.Mirror)
	}
	if code, _, errOut := runLedger("restore", "--from", v.mirror, "--to", filepath.Join(t.TempDir(), "data")); code != 1 || !strings.Contains(errOut, "does not verify, nothing was restored") {
		t.Fatalf("restore of a damaged mirror: exit %d, %s", code, errOut)
	}
}
