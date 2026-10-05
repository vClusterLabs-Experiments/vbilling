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
	if old, ok := s.data[k]; ok && old == v {
		return nil
	}
	s.data[k] = v
	return s.flushLocked()
}

func (s *Store) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[k]; !ok {
		return nil
	}
	delete(s.data, k)
	return s.flushLocked()
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
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
