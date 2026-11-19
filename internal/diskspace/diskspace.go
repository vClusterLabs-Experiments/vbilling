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

// Check reports each named volume (name -> path) to h and tel once.
func Check(volumes map[string]string, h *health.Registry, tel *telemetry.Registry) {
	for name, path := range volumes {
		if path == "" {
			continue
		}
		u, err := statfs(path)
		if err != nil {
			h.Fail(name, "storage", path, err, "check that the volume is mounted")
			continue
		}
		if tel != nil {
			tel.Set("vbilling_volume_free_bytes", "Free bytes on a volume vBilling writes to.", map[string]string{"volume": name}, float64(u.Free))
			tel.Set("vbilling_volume_size_bytes", "Size of a volume vBilling writes to.", map[string]string{"volume": name}, float64(u.Total))
		}
		if used := u.Used(); used >= FailAt {
			h.Set(name, "storage", path, health.Failing,
				fmt.Sprintf("%.0f%% of %s used", used*100, human(u.Total)),
				"grow the volume (or lower retention) before it fills: a full ledger volume stops every window commit and every record")
			continue
		}
		h.OK(name, "storage", path, 1)
	}
}

// Watch runs Check every minute until ctx ends.
func Watch(ctx context.Context, volumes map[string]string, h *health.Registry, tel *telemetry.Registry) {
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
