package spool

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// asBucket makes PlanMirror see a FUSE mount, or none, and records the paths
// it asked about.
func asBucket(t *testing.T, fuse *bool) *[]string {
	t.Helper()
	var asked []string
	old := bucketMount
	bucketMount = func(path string) bool {
		asked = append(asked, path)
		return *fuse
	}
	t.Cleanup(func() { bucketMount = old })
	return &asked
}

// LEDGER_MIRROR decided for an archive directory: kept on a disk or a file
// share, not kept on a bucket unless the operator says so, never without an
// archive directory.
func TestPlanMirror(t *testing.T) {
	fuse := false
	asked := asBucket(t, &fuse)
	root := t.TempDir()
	archive := filepath.Join(root, "archive", "vbilling-0") // created by its first use: not there yet

	for _, mode := range []string{"", MirrorAuto, MirrorOn, MirrorOff} {
		if p := PlanMirror(mode, "", ""); !reflect.DeepEqual(p, MirrorPlan{}) {
			t.Fatalf("mode %q without an archive directory: %+v", mode, p)
		}
	}
	for _, mode := range []string{"", MirrorAuto, MirrorOn} {
		if p := PlanMirror(mode, archive, ""); p.Dir != MirrorDir(archive) || p.Reason != "" || p.Hint != "" || len(p.Unused) != 0 {
			t.Fatalf("mode %q on a disk: %+v", mode, p)
		}
	}
	// The filesystem is asked about a directory that exists.
	if len(*asked) == 0 {
		t.Fatal("auto never looked at the archive's filesystem")
	}
	for _, path := range *asked {
		if path != root {
			t.Fatalf("asked about %s, want the nearest directory that exists, %s", path, root)
		}
	}

	off := PlanMirror(MirrorOff, archive, "")
	if off.Dir != "" || !strings.Contains(off.Reason, "LEDGER_MIRROR=off") || off.Hint == "" || len(off.Unused) != 0 {
		t.Fatalf("off: %+v", off)
	}

	// The archive is a bucket: no mirror by default, one when asked for.
	fuse = true
	bucket := PlanMirror(MirrorAuto, archive, "")
	if bucket.Dir != "" || !strings.Contains(bucket.Reason, "FUSE") || !strings.Contains(bucket.Reason, archive) || !strings.Contains(bucket.Hint, "LEDGER_MIRROR=on") ||
		!strings.Contains(bucket.Hint, "LEDGER_MIRROR_DIR (Helm: persistence.mirror)") {
		t.Fatalf("auto on a bucket: %+v", bucket)
	}
	if p := PlanMirror(MirrorOn, archive, ""); p.Dir != MirrorDir(archive) || p.Reason != "" {
		t.Fatalf("on, on a bucket: %+v", p)
	}

	// Source health says that no second copy is kept, as a setup and not as
	// a fault. A plan with a mirror reports nothing: the mirror does.
	h := health.New(telemetry.New())
	PlanMirror(MirrorOn, archive, "").Report(h, archive)
	MirrorPlan{}.Report(h, "")
	if got := h.List(); len(got) != 0 {
		t.Fatalf("sources of a plan with a mirror, or without an archive: %+v", got)
	}
	for _, p := range []MirrorPlan{off, bucket} {
		p.Report(h, archive)
		got := h.List()
		if len(got) != 1 || got[0].Name != mirrorHealthName || got[0].Kind != mirrorHealthKind || got[0].Status != health.Paused ||
			got[0].Subject != archive || got[0].Message != p.Reason || got[0].Hint != p.Hint {
			t.Fatalf("source of %+v: %+v", p, got)
		}
		if got := h.Summary()[health.Failing]; got != 0 {
			t.Fatalf("a mirror that is not kept counts as %d failing source(s)", got)
		}
	}
}

// A copy from when the mirror was kept is named, left alone, and never
// restored by itself: nothing says how old it is. It still restores by hand.
func TestMirrorThatIsNoLongerKeptIsNotRestoredByItself(t *testing.T) {
	fuse := false
	asBucket(t, &fuse)
	r := mirroredRig(t)
	archive := filepath.Dir(r.dir)
	for _, mode := range []string{MirrorOff, MirrorAuto} {
		fuse = mode == MirrorAuto // auto: the archive volume became a bucket
		p := PlanMirror(mode, archive, "")
		if p.Dir != "" || !reflect.DeepEqual(p.Unused, []string{r.dir}) {
			t.Fatalf("%s: %+v, want no mirror and the earlier copy named", mode, p)
		}
		// main hands the plan's directory to RestoreIfLost: there is none, so
		// an empty data directory starts as a new install.
		data := t.TempDir()
		if rep, err := RestoreIfLost(p.Dir, data, t.Logf); rep != nil || err != nil {
			t.Fatalf("%s: restored by itself: %+v, %v", mode, rep, err)
		}
		if entries, _ := os.ReadDir(data); len(entries) != 0 {
			t.Fatalf("%s: the data directory was touched: %v", mode, entries)
		}
		checkMirror(t, r.dir)
		if _, err := RestoreMirror(p.Unused[0], data, RestoreOptions{NoSync: true}); err != nil {
			t.Fatalf("%s: restore by hand: %v", mode, err)
		}
	}
}

// The archive of compacted segments shares the mirror's volume and comes
// first: a segment that cannot be archived stays on the data volume, and a
// full data volume stops every commit. The mirror leaves a tenth of the
// volume free, never starts a first copy that does not fit, and says what to
// do. A pass that only follows retention frees space and is always made.
func TestMirrorLeavesRoomForTheArchive(t *testing.T) {
	r := newMirrorRig(t, Options{SegmentMaxRecords: 2, Retention: 10 * time.Minute})
	r.ledger.Cursor("stripe")
	for i := 0; i < 6; i++ {
		r.window(1) // one segment per window
	}
	var need uint64
	for _, dir := range []string{"ledger", "records"} {
		entries, err := os.ReadDir(filepath.Join(r.data, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), "seg-") {
				need += uint64(info.Size())
			}
		}
	}
	if need == 0 {
		t.Fatal("the ledger holds nothing to copy")
	}
	size := 10 * need // the reserve is exactly what the first copy needs
	free, asked := uint64(0), ""
	r.m.opts.Space = func(path string) (uint64, uint64, bool) {
		asked = path
		return free, size, true
	}
	mirrored := func() int {
		segs, err := listSegments(filepath.Join(r.dir, "ledger"))
		if err != nil {
			t.Fatal(err)
		}
		return len(segs)
	}

	// One byte short of fitting above the reserve: nothing is copied.
	free = 2*need - 1
	err := r.m.Sync()
	if err == nil || !strings.Contains(err.Error(), "leaves 10% of the volume to the archive") {
		t.Fatalf("a first copy that does not fit: %v", err)
	}
	if asked != r.dir {
		t.Fatalf("asked about the volume of %s, want the mirror's, %s", asked, r.dir)
	}
	if n := mirrored(); n != 0 || IsMirror(r.dir) {
		t.Fatalf("%d segment(s) copied, recovery point %v: a copy that does not fit must not be started", n, IsMirror(r.dir))
	}
	src := r.mirrorHealth()
	if src.Status != health.Failing || !strings.Contains(src.Hint, "grow the archive volume") || !strings.Contains(src.Message, "behind the ledger") {
		t.Fatalf("health of a mirror without room: %+v", src)
	}
	if lag := r.tel.Get(mirrorLagMetric, nil); lag <= 0 {
		t.Fatalf("lag %v while nothing is mirrored", lag)
	}

	// It fits: the copy is made, by the next pass, without anyone's help.
	free = 2 * need
	r.sync()
	if n := mirrored(); n != 6 {
		t.Fatalf("%d segments mirrored, want 6", n)
	}
	checkMirror(t, r.dir)
	if src := r.mirrorHealth(); src.Status != health.OK {
		t.Fatalf("health once there is room: %+v", src)
	}

	// The volume fills up (the archive grew): new records are not copied...
	free = size / 20
	r.window(1)
	if err := r.m.Sync(); err == nil {
		t.Fatal("copied into the reserve")
	}
	free = size / 2
	r.sync()
	// ...but a pass that only follows retention is made although the volume
	// is nearly full: it gives space back.
	free = size / 20
	r.c.Add(time.Hour)
	if err := r.ledger.Ack("stripe", r.ledger.Committed()); err != nil {
		t.Fatal(err)
	}
	if n, err := r.ledger.Compact([]string{"stripe"}); err != nil || n == 0 {
		t.Fatalf("compacted %d segments: %v", n, err)
	}
	before := mirrored()
	r.sync()
	if after := mirrored(); after >= before {
		t.Fatalf("%d segments in the mirror after compaction, %d before", after, before)
	}
	checkMirror(t, r.dir)
	r.restoreAndCompare()

	// A volume whose size cannot be read is never taken for a full one.
	r.m.opts.Space = func(string) (uint64, uint64, bool) { return 0, 0, false }
	r.window(1)
	r.sync()
	checkMirror(t, r.dir)
}

func TestByteSize(t *testing.T) {
	for b, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 41 << 30: "41.0 GiB", 3 << 40: "3.0 TiB"} {
		if got := byteSize(b); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", b, got, want)
		}
	}
}
