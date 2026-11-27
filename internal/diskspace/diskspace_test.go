package diskspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

func quietHealth(tel *telemetry.Registry) *health.Registry {
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	return h
}

func sources(h *health.Registry) map[string]health.Source {
	got := map[string]health.Source{}
	for _, s := range h.List() {
		got[s.Name] = s
	}
	return got
}

func TestFullVolumeFailsItsSource(t *testing.T) {
	h := quietHealth(nil)
	defer func(f func(string) (Usage, error)) { statfs = f }(statfs)
	statfs = func(path string) (Usage, error) {
		switch path {
		case "/data":
			return Usage{Total: 100, Free: 10}, nil // 90% used
		case "/archive":
			return Usage{Total: 100, Free: 60}, nil
		}
		return Usage{}, errors.New("no such volume")
	}
	Check([]Volume{{Name: "ledger", Path: "/data", FullHint: "grow it"}, {Name: "archive", Path: "/archive"},
		{Name: "gone", Path: "/missing"}, {Name: "unset", Path: ""}}, h, nil)
	got := sources(h)
	if got["ledger"].Status != health.Failing || got["archive"].Status != health.OK || got["gone"].Status != health.Failing {
		t.Fatalf("statuses %v", got)
	}
	if got["ledger"].Hint != "grow it" || !strings.Contains(got["ledger"].Message, "90% of") {
		t.Fatalf("a full volume reports its use and its own hint: %+v", got["ledger"])
	}
	if _, ok := got["unset"]; ok {
		t.Fatal("an unset volume must not be reported")
	}
}

func TestStatfsReadsARealVolume(t *testing.T) {
	u, err := statfs(t.TempDir())
	if err != nil || u.Total == 0 || u.Free > u.Total {
		t.Fatalf("statfs: %+v %v", u, err)
	}
}

// The live validation of v0.3.2: the chart sets
// ARCHIVE_DIR to <mount path>/<pod name>, a directory only the first
// compaction created, seven days after an install. Until then a correctly
// mounted, empty archive volume reported "no such file or directory" with
// the hint to check the mount, VBillingSourceUnhealthy fired, and the volume
// had no space gauges. The directory is created by the check itself now.
func TestEmptyArchiveVolumeIsHealthyFromTheFirstCheck(t *testing.T) {
	mount := t.TempDir()                      // the mounted, empty archive volume
	dir := filepath.Join(mount, "vbilling-0") // ARCHIVE_DIR: not there yet
	tel := telemetry.New()
	h := quietHealth(tel)

	Check([]Volume{LedgerVolume(t.TempDir()), ArchiveVolume(dir, spool.EnsureArchiveDir)}, h, tel)

	got := sources(h)
	if s := got[ArchiveVolumeName]; s.Status != health.OK || s.Kind != "storage" || s.Subject != dir {
		t.Fatalf("an empty, mounted archive volume must be ok from the first check: %+v", s)
	}
	if got[LedgerVolumeName].Status != health.OK {
		t.Fatalf("ledger volume: %+v", got[LedgerVolumeName])
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the check must create the archive directory: %v", err)
	}
	if n := tel.Get("vbilling_sources", map[string]string{"kind": "storage", "status": "failing"}); n != 0 {
		t.Fatalf("vbilling_sources{kind=storage,status=failing} = %v: VBillingSourceUnhealthy would fire on a healthy install", n)
	}
	// The archive volume has the same space gauges as the ledger volume.
	for _, volume := range []string{LedgerVolumeName, ArchiveVolumeName} {
		labels := map[string]string{"volume": volume}
		size, free := tel.Get("vbilling_volume_size_bytes", labels), tel.Get("vbilling_volume_free_bytes", labels)
		if size <= 0 || free <= 0 || free > size {
			t.Errorf("%s: vbilling_volume_size_bytes=%v vbilling_volume_free_bytes=%v", volume, size, free)
		}
	}
}

// What "not mounted or not writable" looks like: the directory cannot be
// created. That is reported with the reason and what to check, and it
// recovers by itself once the volume is there.
func TestArchiveVolumeThatCannotBeCreatedFailsWithTheReason(t *testing.T) {
	mount := t.TempDir()
	blocker := filepath.Join(mount, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "vbilling-0") // cannot be created under a file
	tel := telemetry.New()
	h := quietHealth(tel)
	vols := []Volume{ArchiveVolume(dir, spool.EnsureArchiveDir)}

	Check(vols, h, tel)
	s := sources(h)[ArchiveVolumeName]
	if s.Status != health.Failing || !strings.Contains(s.Message, "not a directory") {
		t.Fatalf("want failing with the reason the directory cannot be created: %+v", s)
	}
	if !strings.Contains(s.Hint, "mounted") || !strings.Contains(s.Hint, "write") {
		t.Fatalf("the hint must name both causes, not mounted and not writable: %q", s.Hint)
	}
	if size := tel.Get("vbilling_volume_size_bytes", map[string]string{"volume": ArchiveVolumeName}); size != 0 {
		t.Fatalf("no space gauge for a volume that cannot be read, got %v", size)
	}

	// The operator fixes the mount: the next check creates the directory.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocker, 0o750); err != nil {
		t.Fatal(err)
	}
	Check(vols, h, tel)
	if s := sources(h)[ArchiveVolumeName]; s.Status != health.OK {
		t.Fatalf("the source must recover without a restart: %+v", s)
	}
}

func TestNoArchiveNoArchiveSource(t *testing.T) {
	h := quietHealth(nil)
	Check([]Volume{LedgerVolume(t.TempDir()), ArchiveVolume("", spool.EnsureArchiveDir)}, h, nil)
	got := sources(h)
	if _, ok := got[ArchiveVolumeName]; ok || len(got) != 1 {
		t.Fatalf("without ARCHIVE_DIR only the ledger volume is reported: %v", got)
	}
}
