package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// The tests in this file run on a bucket mounted through FUSE, and only when
// VBILLING_BUCKET_ARCHIVE names a directory on such a mount (they are skipped
// otherwise). VBILLING_BUCKET_LEDGER, when set, keeps the ledger there, so
// that "vbilling ledger verify" and "export" can read the same ledger and
// archive afterwards. In Docker, in a privileged Linux container with
// /dev/fuse, mount-s3 and gcsfuse, next to MinIO and fake-gcs-server
// (-backend memory):
//
//	GOOS=linux go test -c -o spool.test ./internal/spool
//	mount-s3 archive /mnt/s3 --endpoint-url http://minio:9000 --force-path-style \
//	  --region us-east-1 --allow-other --uid 65532 --gid 65532 --allow-delete
//	gcsfuse --custom-endpoint http://fake-gcs:4443/storage/v1/ --anonymous-access \
//	  -o allow_other --uid 65532 --gid 65532 archive /mnt/gcs
//	VBILLING_BUCKET_ARCHIVE=/mnt/s3/vbilling-0 setpriv --reuid 65532 --regid 65532 \
//	  --clear-groups ./spool.test -test.run Bucket -test.v
//
// and the same with /mnt/gcs. Without --allow-delete, Mountpoint refuses the
// retention step, and the test says so.

func bucketDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("VBILLING_BUCKET_ARCHIVE")
	if root == "" {
		t.Skip("VBILLING_BUCKET_ARCHIVE is not set: no bucket mount to test on")
	}
	dir := filepath.Join(root, fmt.Sprintf("run-%d", time.Now().UnixNano()))
	t.Logf("archive directory on the bucket: %s", dir)
	return dir
}

// TestBucketMountSemantics records what the mount does with each operation
// an archive or a mirror might use. It fails nothing: the log is the result.
func TestBucketMountSemantics(t *testing.T) {
	dir := bucketDir(t)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := func(name string) string { return filepath.Join(dir, name) }
	report := func(op string, err error) {
		if err != nil {
			t.Logf("%-38s refused: %v", op, err)
		} else {
			t.Logf("%-38s works", op)
		}
	}
	report("create a new file (O_CREAT|O_EXCL)", os.WriteFile(path("a"), []byte("first\n"), 0o640))
	_, err := os.OpenFile(path("a"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if errors.Is(err, os.ErrExist) {
		t.Logf("%-38s refused with EEXIST, as O_EXCL must", "create an existing file (O_EXCL)")
	} else {
		t.Logf("%-38s answered %v", "create an existing file (O_EXCL)", err)
	}
	f, err := os.OpenFile(path("a"), os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = f.WriteString("more\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	report("append to a file (O_APPEND)", err)
	report("overwrite a file (O_TRUNC)", os.WriteFile(path("a"), []byte("other\n"), 0o640))
	report("fsync a file", fsyncNew(path("b")))
	report("fsync a directory", syncDir(dir))
	report("rename a file", os.Rename(path("a"), path("c")))
	if b, err := os.ReadFile(path("c")); err == nil {
		t.Logf("%-38s %d bytes at the new name: %q", "  after the rename", len(b), b)
	}
	if _, err := os.Stat(path("a")); err == nil {
		t.Logf("%-38s the old name still exists", "  after the rename")
	}
	report("delete a file", os.Remove(path("b")))
}

func fsyncNew(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("synced\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// TestBucketMirrorPlan: LEDGER_MIRROR=auto keeps no mirror in
// ARCHIVE_DIR/mirror on the bucket, keeps one in a LEDGER_MIRROR_DIR on a
// local disk, and none in a LEDGER_MIRROR_DIR on the bucket.
func TestBucketMirrorPlan(t *testing.T) {
	dir := bucketDir(t)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if !onFUSE(dir) {
		t.Fatalf("%s is not on a FUSE mount", dir)
	}
	local := filepath.Join(t.TempDir(), "vbilling-0")
	if p := PlanMirror(MirrorAuto, dir, ""); p.Dir != "" || !strings.Contains(p.Reason, "FUSE") {
		t.Fatalf("auto, archive on the bucket: %+v", p)
	}
	if p := PlanMirror(MirrorAuto, dir, local); p.Dir != local || p.Reason != "" {
		t.Fatalf("auto, archive on the bucket, LEDGER_MIRROR_DIR on a disk: %+v", p)
	}
	if p := PlanMirror(MirrorAuto, "", filepath.Join(dir, "mirror-0")); p.Dir != "" || !strings.Contains(p.Reason, "LEDGER_MIRROR_DIR") {
		t.Fatalf("auto, LEDGER_MIRROR_DIR on the bucket: %+v", p)
	}
	t.Logf("auto: no mirror in ARCHIVE_DIR/mirror or a LEDGER_MIRROR_DIR on the bucket, one in a LEDGER_MIRROR_DIR on a local disk")
}

// TestArchiveOnBucketMount archives two ledgers into a bucket the way
// vBilling does (the usage ledger into ARCHIVE_DIR, per-request records into
// ARCHIVE_DIR/records), with real fsyncs, then verifies and exports through
// the mount, leaves what a crash in the middle of a write leaves, archives
// again, and deletes the oldest segment as retention would.
func TestArchiveOnBucketMount(t *testing.T) {
	arch := bucketDir(t)
	data := os.Getenv("VBILLING_BUCKET_LEDGER")
	if data == "" {
		data = t.TempDir()
	}
	c := &clock{t: t0}
	type ledger struct {
		name string
		s    *Spool
		dir  string
		arch string
	}
	var ledgers []*ledger
	for _, l := range []struct{ name, sub string }{{"ledger", ""}, {"records", "records"}} {
		dir := filepath.Join(data, l.name)
		a := filepath.Join(arch, l.sub)
		s, err := Open(dir, Options{SegmentMaxRecords: 3, Retention: time.Hour, ArchiveDir: a, Now: c.Now})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		s.Cursor(consumer)
		ledgers = append(ledgers, &ledger{l.name, s, dir, a})
	}
	windows := 0
	appendWindows := func(n int) {
		for i := 0; i < n; i++ {
			c.Add(time.Minute)
			ws := t0.Add(time.Duration(windows) * time.Minute)
			for _, l := range ledgers {
				events := []usage.Event{ev("acme", ws, 8.0/60), ev("globex", ws, 2.0/60)}
				if err := l.s.AppendWindow(SourceCollector, ws.Add(time.Minute), events, ""); err != nil {
					t.Fatal(err)
				}
			}
			windows++
		}
	}
	compact := func(l *ledger) (int, time.Duration) {
		t.Helper()
		if err := l.s.Ack(consumer, l.s.Committed()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		n, err := l.s.Compact([]string{consumer})
		if err != nil {
			t.Fatalf("%s: compaction into the bucket: %v", l.name, err)
		}
		return n, time.Since(start)
	}
	verify := func(l *ledger, records uint64) ChainReport {
		t.Helper()
		rep := VerifyChain(l.dir, l.arch)
		if !rep.OK || rep.Records != records {
			t.Fatalf("%s: verify: %+v", l.name, rep)
		}
		return rep
	}

	// Six windows, five of them archived.
	appendWindows(6)
	c.Add(2 * time.Hour)
	for _, l := range ledgers {
		n, took := compact(l)
		if n != 5 {
			t.Fatalf("%s: compaction removed %d segments, want 5", l.name, n)
		}
		rep := verify(l, 18)
		t.Logf("%s: archived 5 segments in %s; verify: %d records, %d archived and %d live segments, %d warning(s)",
			l.name, took.Round(time.Millisecond), rep.Records, rep.ArchivedSegments, rep.LiveSegments, len(rep.Warnings))
	}
	usageLedger := ledgers[0]
	n := 0
	sum, err := ScanLedger(usageLedger.dir, usageLedger.arch, t0, t0.Add(time.Hour), func(e *usage.Event) error { n++; return nil })
	if err != nil || n != 12 || sum.FirstSeq != 1 || sum.LastSeq != 18 {
		t.Fatalf("export through the bucket: %d events, %+v, %v", n, sum, err)
	}
	t.Logf("export through the bucket: %d events, seq %d-%d", n, sum.FirstSeq, sum.LastSeq)
	inProcess, err := usageLedger.s.VerifyWithArchive()
	if err != nil || !inProcess.OK || inProcess.Records != 18 {
		t.Fatalf("in-process verify: %+v, %v", inProcess, err)
	}
	all := 0
	if err := usageLedger.s.ScanAll(t0, t0.Add(time.Hour), func(*usage.Event) error { all++; return nil }); err != nil || all != 12 {
		t.Fatalf("ScanAll through the bucket: %d, %v", all, err)
	}

	// What a crash in the middle of writing leaves on a bucket: an object
	// with no index entry, and an index object that is no entry. The next
	// compaction leaves both and archives under the next names.
	appendWindows(3)
	c.Add(2 * time.Hour)
	next := usageLedger.s.FirstSeq()
	leftovers := []string{archiveCopyName(next, 1, archiveDataExt), filepath.Join(ArchiveIndexDir, archiveCopyName(next, 1, archiveIndexExt))}
	if err := os.WriteFile(filepath.Join(usageLedger.arch, leftovers[0]), []byte("\x1f\x8b\x08\x00 a copy a crash left"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usageLedger.arch, leftovers[1]), []byte(`{"name":"seg-`), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, l := range ledgers {
		if n, _ := compact(l); n != 3 {
			t.Fatalf("%s: second compaction removed %d segments, want 3", l.name, n)
		}
	}
	rep := verify(usageLedger, 27)
	e := archiveEntry(t, usageLedger.arch, segName(next))
	if e.File() != archiveCopyName(next, 2, archiveDataExt) || e.source() != ArchiveIndexDir+"/"+archiveCopyName(next, 2, archiveIndexExt) {
		t.Fatalf("after the leftovers, seg %d archived as %s in %s", next, e.File(), e.source())
	}
	t.Logf("after leftovers of a crash: seg %d archived as %s in %s; verify ok with %d warning(s): %s",
		next, e.File(), e.source(), len(rep.Warnings), strings.Join(rep.Warnings, " | "))
	verify(ledgers[1], 27)

	// Retention removes the oldest segment: its entry, then its data file.
	first := filepath.Join(ArchiveIndexDir, archiveCopyName(1, 1, archiveIndexExt))
	if err := os.Remove(filepath.Join(usageLedger.arch, first)); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
			t.Logf("retention cannot delete through this mount: %v (Mountpoint needs --allow-delete)", err)
			return
		}
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(usageLedger.arch, archiveCopyName(1, 1, archiveDataExt))); err != nil {
		t.Fatal(err)
	}
	rep = VerifyChain(usageLedger.dir, usageLedger.arch)
	if !rep.OK || rep.FirstSeq != 4 || rep.Anchor == "" {
		t.Fatalf("verify after retention: %+v", rep)
	}
	all = 0
	if err := usageLedger.s.ScanAll(t0, t0.Add(time.Hour), func(*usage.Event) error { all++; return nil }); err != nil || all != 16 {
		t.Fatalf("ScanAll after retention: %d, %v", all, err)
	}
	t.Logf("retention deleted seg 1 (entry, then data file): verify ok from seq %d, anchor %s", rep.FirstSeq, rep.Anchor[:12])
}
