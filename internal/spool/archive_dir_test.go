package spool

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The archive directory is created at start-up: by default it is a
// directory per pod under the mount, and nothing else creates it before the
// first compaction that has a segment to archive.
func TestEnsureArchiveDir(t *testing.T) {
	mount := t.TempDir()
	dir := filepath.Join(mount, "vbilling-0")

	for i := 0; i < 2; i++ { // creating it twice is fine
		if err := EnsureArchiveDir(dir); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("archive directory not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("archive directory mode %o is open to others", perm)
	}

	// An archive that is already there is left as it is.
	entries, skipped, err := ReadArchive(dir)
	if err != nil || len(entries) != 0 || len(skipped) != 0 {
		t.Fatalf("a new archive directory is an empty archive: %v %v %v", entries, skipped, err)
	}

	// A volume that is not there or not writable: the error says why.
	file := filepath.Join(mount, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureArchiveDir(filepath.Join(file, "vbilling-0")); err == nil {
		t.Fatal("creating the archive directory under a file must fail")
	}
	if err := EnsureArchiveDir(""); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("no directory configured: got %v, want ErrNoArchive", err)
	}
}
