//go:build linux

package spool

import "syscall"

// fuseSuperMagic is the filesystem type statfs reports for a FUSE mount.
const fuseSuperMagic = 0x65735546

// onFUSE reports whether path is on a FUSE filesystem. CSI drivers mount
// buckets that way (Cloud Storage FUSE, Mountpoint for Amazon S3, BlobFuse).
// A path that cannot be read is not taken for one.
func onFUSE(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return int64(st.Type) == fuseSuperMagic //nolint:unconvert // Type is int32 on 32-bit platforms
}
