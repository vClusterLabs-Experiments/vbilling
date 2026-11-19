package kv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStorePersistsAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "s.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("deleted key came back")
	}
	if v, _ := r.Get("b"); v != "2" {
		t.Fatalf("b = %q after reopen", v)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

// A change that cannot be written is not kept in memory either: memory and
// disk never disagree.
func TestFailedWriteLeavesTheStoreAsItWas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("hold", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o750)
	if err := s.Set("hold", "active"); err == nil {
		t.Skip("the directory is still writable (running as root?)")
	}
	if v, _ := s.Get("hold"); v != "paused" {
		t.Fatalf("hold = %q in memory after a failed write, want the stored paused", v)
	}
	if err := s.Set("new", "x"); err == nil {
		t.Fatal("write into a read-only directory succeeded")
	}
	if _, ok := s.Get("new"); ok {
		t.Fatal("a key whose write failed is visible")
	}
	if err := s.Delete("hold"); err == nil {
		t.Fatal("delete in a read-only directory succeeded")
	}
	if v, _ := s.Get("hold"); v != "paused" {
		t.Fatalf("hold = %q after a failed delete", v)
	}
}

// Once the rename succeeded the file holds the change. If only the
// directory sync fails, the change reports the error but is not rolled back
// in memory: memory never disagrees with the file, and reopening the store
// reads the same value.
func TestChangeOnDiskIsKeptWhenOnlyTheDirectorySyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("hold", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("gone", "x"); err != nil {
		t.Fatal(err)
	}
	defer func(f func(string) error) { syncDir = f }(syncDir)
	syncDir = func(string) error { return errors.New("input/output error") }
	if err := s.Set("hold", "active"); err == nil {
		t.Fatal("a change whose directory sync failed reported success")
	}
	if err := s.Delete("gone"); err == nil {
		t.Fatal("a delete whose directory sync failed reported success")
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"hold", "gone"} {
		mem, inMem := s.Get(k)
		disk, onDisk := r.Get(k)
		if mem != disk || inMem != onDisk {
			t.Errorf("%s: memory has %q (%t), the file has %q (%t)", k, mem, inMem, disk, onDisk)
		}
	}
	if v, _ := s.Get("hold"); v != "active" {
		t.Fatalf("hold = %q in memory, want the active the file holds", v)
	}
}
