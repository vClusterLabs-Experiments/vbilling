// Package kv is a tiny persistent string map adapters use to remember
// backend IDs (tenant -> Stripe customer, metric -> meter) across restarts.
package kv

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store is safe for concurrent use. A Store with an empty path is memory-only.
//
// Every change is written to disk before it is visible: the whole map goes
// to a temporary file, which is fsynced and renamed over the store, and the
// directory is fsynced, so a change that returned nil survives a crash and a
// change that failed leaves the store as it was, in memory and on disk.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]string
}

// Open loads path if it exists.
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]string{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, os.MkdirAll(filepath.Dir(path), 0o750)
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Memory returns a memory-only store.
func Memory() *Store { s, _ := Open(""); return s }

func (s *Store) Get(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[k]
	return v, ok
}

func (s *Store) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.data[k]
	if had && old == v {
		return nil
	}
	s.data[k] = v
	if err := s.flushLocked(); err != nil {
		if had {
			s.data[k] = old
		} else {
			delete(s.data, k)
		}
		return err
	}
	return nil
}

func (s *Store) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.data[k]
	if !ok {
		return nil
	}
	delete(s.data, k)
	if err := s.flushLocked(); err != nil {
		s.data[k] = old
		return err
	}
	return nil
}

// WithPrefix returns all entries whose key starts with prefix, prefix trimmed.
func (s *Store) WithPrefix(prefix string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.data {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}
	return out
}

func (s *Store) flushLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// The rename is durable once the directory entry is.
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		err = d.Sync()
		d.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
