package spool

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// bucketsUnder makes PlanMirror take every path under one of roots for a
// FUSE mount, and records the paths it asked about.
func bucketsUnder(t *testing.T, roots ...string) *[]string {
	t.Helper()
	var asked []string
	old := bucketMount
	bucketMount = func(path string) bool {
		asked = append(asked, path)
		for _, root := range roots {
			if rel, err := filepath.Rel(root, path); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { bucketMount = old })
	return &asked
}

// LEDGER_MIRROR_DIR gives the mirror a volume of its own. The archive can
// then stay in a bucket while the mirror is kept on a disk or a file share;
// auto still keeps no mirror where the mirror's own directory is a bucket.
func TestPlanMirrorOnAVolumeOfItsOwn(t *testing.T) {
	root := t.TempDir()
	bucket, disk := filepath.Join(root, "bucket"), filepath.Join(root, "disk")
	for _, d := range []string{bucket, disk} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	archive, own := filepath.Join(bucket, "vbilling-0"), filepath.Join(disk, "vbilling-0")
	asked := bucketsUnder(t, bucket)

	for _, mode := range []string{"", MirrorAuto, MirrorOn} {
		for _, arch := range []string{archive, ""} {
			if p := PlanMirror(mode, arch, own); p.Dir != own || p.Reason != "" || p.Hint != "" || len(p.Unused) != 0 {
				t.Fatalf("mode %q, archive %q, LEDGER_MIRROR_DIR on a disk: %+v", mode, arch, p)
			}
		}
	}
	// Auto looked at the mirror's own directory, not at the archive's.
	if len(*asked) == 0 {
		t.Fatal("auto never looked at the mirror's filesystem")
	}
	for _, path := range *asked {
		if path != disk {
			t.Fatalf("asked about %s, want the mirror's nearest directory that exists, %s", path, disk)
		}
	}

	onBucket := filepath.Join(bucket, "mirror-0")
	p := PlanMirror(MirrorAuto, archive, onBucket)
	if p.Dir != "" || !strings.Contains(p.Reason, "LEDGER_MIRROR_DIR "+onBucket+" is on a FUSE mount") ||
		!strings.Contains(p.Hint, "put LEDGER_MIRROR_DIR (Helm: persistence.mirror) on a disk or a file share") || !strings.Contains(p.Hint, "LEDGER_MIRROR=on") {
		t.Fatalf("auto, LEDGER_MIRROR_DIR on a bucket: %+v", p)
	}
	if p := PlanMirror(MirrorOn, archive, onBucket); p.Dir != onBucket || p.Reason != "" {
		t.Fatalf("on, LEDGER_MIRROR_DIR on a bucket: %+v", p)
	}
	off := PlanMirror(MirrorOff, archive, own)
	if off.Dir != "" || !strings.Contains(off.Reason, "LEDGER_MIRROR=off") || !strings.HasSuffix(off.Hint, "to keep the copy in LEDGER_MIRROR_DIR") {
		t.Fatalf("off with LEDGER_MIRROR_DIR: %+v", off)
	}
}

// Moving the mirror to a volume of its own: the copy on the archive volume is
// named as no longer kept, never changed, never deleted and never restored by
// itself. The new mirror is kept and restored from.
func TestMirrorMovesToAVolumeOfItsOwn(t *testing.T) {
	fuse := false
	asBucket(t, &fuse)
	r := mirroredRig(t) // the mirror in ARCHIVE_DIR/mirror
	archive, earlier := filepath.Dir(r.dir), r.dir
	before := treeSums(t, earlier)
	own := filepath.Join(t.TempDir(), "vbilling-0")

	for _, mode := range []string{MirrorAuto, MirrorOn} {
		p := PlanMirror(mode, archive, own)
		if p.Dir != own || !reflect.DeepEqual(p.Unused, []string{earlier}) {
			t.Fatalf("%s: %+v, want the mirror in LEDGER_MIRROR_DIR and the earlier copy named", mode, p)
		}
	}

	// The mirror on its own volume starts a copy of its own.
	r.dir = own
	r.newMirror()
	r.m.opts.OwnVolume = true
	r.window(2)
	r.sync()
	checkMirror(t, own)
	if !reflect.DeepEqual(treeSums(t, earlier), before) {
		t.Fatal("the mirror on the archive volume was changed")
	}

	// The data volume is lost: vBilling restores from LEDGER_MIRROR_DIR.
	snap := r.snapshot()
	data := t.TempDir()
	p := PlanMirror(MirrorAuto, archive, own)
	rep, err := RestoreIfLost(p.Dir, data, t.Logf)
	if err != nil || rep == nil || rep.From != own || rep.Ledgers[0].LastSeq != snap.committed {
		t.Fatalf("restore from LEDGER_MIRROR_DIR: %+v, %v (want seq %d)", rep, err, snap.committed)
	}
	if check := VerifyChain(filepath.Join(data, "ledger"), ""); !check.OK || check.LastSeq != snap.committed {
		t.Fatalf("the restored ledger: %+v", check)
	}

	// A new, empty mirror volume next to an empty data volume: the copy on
	// the archive volume is not restored by itself, and stays as it is.
	empty := filepath.Join(t.TempDir(), "vbilling-0")
	p = PlanMirror(MirrorAuto, archive, empty)
	if p.Dir != empty || !reflect.DeepEqual(p.Unused, []string{earlier}) {
		t.Fatalf("an empty mirror volume: %+v", p)
	}
	fresh := t.TempDir()
	if rep, err := RestoreIfLost(p.Dir, fresh, t.Logf); rep != nil || err != nil {
		t.Fatalf("restored by itself from %+v, %v", rep, err)
	}
	if entries, _ := os.ReadDir(fresh); len(entries) != 0 {
		t.Fatalf("the data directory was touched: %v", entries)
	}
	if !reflect.DeepEqual(treeSums(t, earlier), before) {
		t.Fatal("the mirror on the archive volume was changed")
	}
	// Off: both copies are named, neither is kept or touched.
	if p := PlanMirror(MirrorOff, archive, own); p.Dir != "" || !reflect.DeepEqual(p.Unused, []string{own, earlier}) {
		t.Fatalf("off: %+v", p)
	}
	// It still restores by hand.
	if _, err := RestoreMirror(earlier, t.TempDir(), RestoreOptions{NoSync: true}); err != nil {
		t.Fatalf("restore the earlier copy by hand: %v", err)
	}
}

// On a volume of its own, the mirror leaves 10% of it free as well, and its
// messages name that volume and its setting.
func TestMirrorOnItsOwnVolumeNamesIt(t *testing.T) {
	r := newMirrorRigAt(t, Options{}, filepath.Join(t.TempDir(), "vbilling-0"), "source-a")
	r.m.opts.OwnVolume = true
	r.window(3)
	asked := ""
	r.m.opts.Space = func(path string) (uint64, uint64, bool) {
		asked = path
		return 1, 1 << 30, true
	}
	err := r.m.Sync()
	if err == nil || !strings.Contains(err.Error(), "leaves 10% of its volume free") || asked != r.dir {
		t.Fatalf("a first copy that does not fit: %v (asked about %s)", err, asked)
	}
	if src := r.mirrorHealth(); src.Status != health.Failing || !strings.Contains(src.Hint, "grow the mirror volume (LEDGER_MIRROR_DIR)") {
		t.Fatalf("health: %+v", src)
	}
	for err, want := range map[error]string{
		syscall.ENOSPC:          "the mirror volume (LEDGER_MIRROR_DIR) is full",
		os.ErrPermission:        "the mirror volume (LEDGER_MIRROR_DIR) is read-only",
		errors.New("something"): "check that the mirror volume (LEDGER_MIRROR_DIR) is mounted",
		r.m.conflict(nil):       "give each its own LEDGER_MIRROR_DIR",
	} {
		if got := r.m.hint(err); !strings.Contains(got, want) {
			t.Fatalf("hint for %v: %q, want %q", err, got, want)
		}
	}
	r.m.opts.Space = func(string) (uint64, uint64, bool) { return 1 << 30, 1 << 30, true }
	r.sync()
	checkMirror(t, r.dir)
}
