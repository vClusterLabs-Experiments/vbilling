// Package diskspace watches the volumes vBilling writes to. A full ledger
// volume stops every window commit and every record, so it has to show up in
// source health long before it is full.
package diskspace

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

const (
	// FailAt is the share of a volume in use at which its source fails:
	// enough headroom left to grow the volume before commits stop.
	FailAt = 0.85
	// checkEvery is how often volumes are read.
	checkEvery = time.Minute
)

// Source names of the two volumes, also the "volume" label of their gauges
// (vbilling_volume_free_bytes, vbilling_volume_size_bytes).
const (
	LedgerVolumeName  = "ledger-volume"
	ArchiveVolumeName = "archive-volume"
)

// Volume is a directory vBilling writes to and the volume it is on.
type Volume struct {
	// Name is the health source and the "volume" label of the gauges.
	Name string
	// Path is a directory on the volume. A volume without one is not
	// configured and is not reported.
	Path string
	// Ensure, when set, creates Path before the volume is read. A directory
	// of vBilling's own under a mount does not exist on a new volume: the
	// volume is judged by whether vBilling can create and read it, not by
	// whether an earlier run already did.
	Ensure func(path string) error
	// EnsureHint is the hint when Ensure fails.
	EnsureHint string
	// FullHint is the hint once FailAt of the volume is used.
	FullHint string
}

// LedgerVolume is the volume of DATA_DIR.
func LedgerVolume(dataDir string) Volume {
	return Volume{
		Name:     LedgerVolumeName,
		Path:     dataDir,
		FullHint: "grow the volume (or lower retention) before it fills: a full ledger volume stops every window commit and every record",
	}
}

// ArchiveVolume is the volume of ARCHIVE_DIR ("" without an archive). ensure
// creates the archive directory (spool.EnsureArchiveDir), so that an empty,
// correctly mounted volume is healthy from the first check and one that is
// not mounted or not writable fails at once with the reason.
func ArchiveVolume(archiveDir string, ensure func(path string) error) Volume {
	return Volume{
		Name:   ArchiveVolumeName,
		Path:   archiveDir,
		Ensure: ensure,
		EnsureHint: "vBilling cannot create its archive directory: check that the archive volume is mounted and that vBilling's user " +
			"(65532 in the chart) may write to it. Until then compaction archives nothing and keeps every segment on the ledger volume",
		FullHint: "grow the archive volume: once it is full, compaction cannot archive, segments past retention stay on the ledger volume, " +
			"and a full ledger volume stops every window commit and every record",
	}
}

// Usage is a volume's size and free space, in bytes.
type Usage struct{ Total, Free uint64 }

// Used is the share of the volume in use.
func (u Usage) Used() float64 {
	if u.Total == 0 {
		return 0
	}
	return 1 - float64(u.Free)/float64(u.Total)
}

// statfs reads a volume's usage. A variable so tests can replace it.
var statfs = func(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert // Bsize is int64 on Linux, uint32 on macOS
	return Usage{Total: uint64(st.Blocks) * bs, Free: uint64(st.Bavail) * bs}, nil
}

// Check reports each volume to h and tel once.
func Check(volumes []Volume, h *health.Registry, tel *telemetry.Registry) {
	for _, v := range volumes {
		if v.Path == "" {
			continue
		}
		if v.Ensure != nil {
			if err := v.Ensure(v.Path); err != nil {
				h.Fail(v.Name, "storage", v.Path, err, v.EnsureHint)
				continue
			}
		}
		u, err := statfs(v.Path)
		if err != nil {
			h.Fail(v.Name, "storage", v.Path, err, "check that the volume is mounted")
			continue
		}
		if tel != nil {
			tel.Set("vbilling_volume_free_bytes", "Free bytes on a volume vBilling writes to.", map[string]string{"volume": v.Name}, float64(u.Free))
			tel.Set("vbilling_volume_size_bytes", "Size of a volume vBilling writes to.", map[string]string{"volume": v.Name}, float64(u.Total))
		}
		if used := u.Used(); used >= FailAt {
			h.Set(v.Name, "storage", v.Path, health.Failing, fmt.Sprintf("%.0f%% of %s used", used*100, human(u.Total)), v.FullHint)
			continue
		}
		h.OK(v.Name, "storage", v.Path, 1)
	}
}

// Watch runs Check now and then every minute until ctx ends.
func Watch(ctx context.Context, volumes []Volume, h *health.Registry, tel *telemetry.Registry) {
	Check(volumes, h, tel)
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			Check(volumes, h, tel)
		}
	}
}

func human(b uint64) string {
	const gib = 1 << 30
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}
