package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// mirrorVolume names the volume the mirror is on, for messages.
func (m *Mirror) mirrorVolume() string {
	if m.opts.OwnVolume {
		return "the mirror volume (LEDGER_MIRROR_DIR)"
	}
	return "the archive volume"
}

// Values of LEDGER_MIRROR.
const (
	// MirrorAuto keeps the mirror wherever it has a place (LEDGER_MIRROR_DIR,
	// or an archive directory), except on a bucket mounted through FUSE.
	MirrorAuto = "auto"
	// MirrorOn keeps it on any filesystem.
	MirrorOn = "on"
	// MirrorOff keeps none.
	MirrorOff = "off"
)

// mirrorReserve is the share of its volume the mirror leaves free. On the
// archive volume, the archive of compacted segments comes first: a segment
// that cannot be archived stays on the data volume, and a full data volume
// stops every commit. The archive volume's own health source fails at 85%
// used, so the mirror yields after that warning and before the volume is
// full. On a volume of its own (LEDGER_MIRROR_DIR) the same reserve keeps the
// volume from filling up with half a copy.
const mirrorReserve = 0.10

// MirrorPlan is LEDGER_MIRROR decided for the mirror's directory.
type MirrorPlan struct {
	// Dir is the mirror directory, "" when no mirror is kept.
	Dir string
	// Reason says why none is kept although it has a place, and Hint what
	// an operator can do about it. Both are empty when a mirror is kept, or
	// when there is no place for one at all.
	Reason, Hint string
	// Unused lists directories that hold an earlier mirror that is no longer
	// kept up to date: the mirror's own directory when none is kept now, and
	// ARCHIVE_DIR/mirror once the mirror has LEDGER_MIRROR_DIR. vBilling
	// never restores them by itself.
	Unused []string
}

// bucketMount reports whether path is on a FUSE mount. A variable so tests
// can replace it.
var bucketMount = onFUSE

// PlanMirror decides where the mirror goes. mode is LEDGER_MIRROR (empty is
// MirrorAuto). mirrorDir is LEDGER_MIRROR_DIR, a directory on a volume of the
// mirror's own; without it the mirror lives in ARCHIVE_DIR/mirror, on the
// archive volume.
//
// A bucket reached through a CSI driver (Cloud Storage FUSE, Mountpoint for
// Amazon S3, BlobFuse) is a FUSE mount. The archive suits one: it writes
// each file once. The mirror does not: it appends to its newest segment and
// replaces its recovery point after every committed window, which a bucket
// turns into uploading the whole object again, or refuses. So MirrorAuto
// keeps no mirror where its own directory is on a FUSE mount, and says so.
func PlanMirror(mode, archiveDir, mirrorDir string) MirrorPlan {
	dir, own := mirrorDir, mirrorDir != ""
	if !own {
		dir = MirrorDir(archiveDir)
	}
	if dir == "" {
		return MirrorPlan{}
	}
	var unused []string
	unusedIf := func(d string) {
		if d != "" && IsMirror(d) {
			unused = append(unused, d)
		}
	}
	// With a volume of its own, a mirror left on the archive volume is
	// no longer kept.
	archived := ""
	if own && MirrorDir(archiveDir) != "" && filepath.Clean(MirrorDir(archiveDir)) != filepath.Clean(dir) {
		archived = MirrorDir(archiveDir)
	}
	where, setting := "on the archive volume", "ARCHIVE_DIR"
	if own {
		where, setting = "in LEDGER_MIRROR_DIR", "LEDGER_MIRROR_DIR"
	}
	switch mode {
	case MirrorOff:
		unusedIf(dir)
		unusedIf(archived)
		return MirrorPlan{Reason: "LEDGER_MIRROR=off: no live second copy of the ledger is kept",
			Hint:   "losing the data volume then loses every record that is not archived or delivered yet. Remove LEDGER_MIRROR=off (Helm: persistence.archive.mirror) to keep the copy " + where,
			Unused: unused}
	case MirrorOn:
		unusedIf(archived)
		return MirrorPlan{Dir: dir, Unused: unused}
	}
	if bucketMount(existingAncestor(dir)) {
		unusedIf(dir)
		unusedIf(archived)
		hint := "the archive itself works there. Give the mirror a volume of its own with LEDGER_MIRROR_DIR (Helm: persistence.mirror), " +
			"a disk or a file share in another failure domain than the data volume, " +
			"or set LEDGER_MIRROR=on (Helm: persistence.archive.mirror=on) if this FUSE filesystem appends and renames in place"
		if own {
			hint = "put LEDGER_MIRROR_DIR (Helm: persistence.mirror) on a disk or a file share in another failure domain than the data volume, " +
				"or set LEDGER_MIRROR=on (Helm: persistence.archive.mirror=on) if this FUSE filesystem appends and renames in place"
		}
		place := "the archive directory " + archiveDir
		if own {
			place = setting + " " + dir
		}
		return MirrorPlan{Reason: fmt.Sprintf("no live second copy of the ledger is kept: %s is on a FUSE mount, which is how a bucket is mounted. "+
			"The mirror appends to its files after every window, and a bucket uploads the whole object again for that, or refuses", place),
			Hint: hint, Unused: unused}
	}
	unusedIf(archived)
	return MirrorPlan{Dir: dir, Unused: unused}
}

// Report puts a plan without a mirror into source health, where an operator
// looks for the second copy. It is a setup, not a fault: the source is
// paused, never failing. Call it again from time to time, because sources
// nothing reports are forgotten.
func (p MirrorPlan) Report(h *health.Registry, archiveDir string) {
	if h == nil || p.Dir != "" || p.Reason == "" {
		return
	}
	h.Set(mirrorHealthName, mirrorHealthKind, archiveDir, health.Paused, p.Reason, p.Hint)
}

// existingAncestor is path, or the nearest directory above it that exists: a
// per-pod archive directory is created by its first use.
func existingAncestor(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// volumeSpace reads the free space and the size of the volume path is on, in
// bytes. ok is false when it cannot be read.
func volumeSpace(path string) (free, size uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(existingAncestor(path), &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize) //nolint:unconvert // Bsize is int64 on Linux, uint32 on macOS
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, true
}

// toCopy is how many bytes of the ledgers the mirror does not hold yet.
func (m *Mirror) toCopy(c *cut) int64 {
	var need int64
	for i, l := range m.ledgers {
		have := make(map[string]int64, len(l.segs))
		for _, sg := range l.segs {
			have[sg.name] = sg.size
		}
		for _, sg := range c.views[i].segs {
			if d := sg.Size - have[sg.Name]; d > 0 {
				need += d
			}
		}
	}
	return need
}

// room refuses a pass that would take the mirror's volume below its reserve.
// A pass that copies nothing (it only follows retention, which frees space)
// is never refused, and a first copy that does not fit is not started: half
// a copy would take the archive's space and restore nothing.
func (m *Mirror) room(need int64) error {
	if need <= 0 {
		return nil
	}
	free, size, ok := m.opts.Space(m.opts.Dir)
	if !ok || size == 0 {
		return nil
	}
	keep := uint64(mirrorReserve * float64(size))
	if free >= keep && free-keep >= uint64(need) {
		return nil
	}
	if m.opts.OwnVolume {
		return &mirrorError{
			msg: fmt.Sprintf("the mirror has %s to copy, and the volume of %s has %s free of %s: the mirror leaves %d%% of its volume free and has stopped copying",
				byteSize(uint64(need)), m.opts.Dir, byteSize(free), byteSize(size), int(mirrorReserve*100)),
			hint: "grow the mirror volume (LEDGER_MIRROR_DIR): the mirror continues by itself, and until then it falls behind. Metering, delivery and the archive are not affected. " +
				"The volume holds a copy of everything on the data volume",
		}
	}
	return &mirrorError{
		msg: fmt.Sprintf("the mirror has %s to copy, and the volume of %s has %s free of %s: the mirror leaves %d%% of the volume to the archive and has stopped copying",
			byteSize(uint64(need)), m.opts.Dir, byteSize(free), byteSize(size), int(mirrorReserve*100)),
		hint: "grow the archive volume: the mirror continues by itself, and until then it falls behind. Metering, delivery and the archive are not affected. " +
			"The volume holds the archive and a copy of everything on the data volume",
	}
}

// byteSize formats a number of bytes for a message.
func byteSize(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}
