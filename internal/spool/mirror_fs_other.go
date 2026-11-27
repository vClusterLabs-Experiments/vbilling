//go:build !linux

package spool

// onFUSE reports whether path is on a FUSE filesystem. vBilling runs on
// Linux; elsewhere (a developer's machine) nothing is taken for a bucket.
func onFUSE(string) bool { return false }
