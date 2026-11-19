package spool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestLegacyIngestIndexUpgradesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	oldID := "publisher-original-event-id"
	if err := os.WriteFile(filepath.Join(dir, "ingest-ids.log"), []byte(fmt.Sprintf("%d\t%s\n", t0.Unix(), oldID)), 0o640); err != nil {
		t.Fatal(err)
	}
	open := func() *Spool {
		s, err := Open(dir, Options{NoSync: true, Now: func() time.Time { return t0.Add(time.Hour) }})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	if !s.IngestedRecently(oldID) {
		t.Fatal("legacy retry identity lost")
	}
	s.Close()
	b, err := os.ReadFile(filepath.Join(dir, "ingest-ids.log"))
	if err != nil || strings.Contains(string(b), oldID) || !strings.HasPrefix(string(b), "v2\t") {
		t.Fatalf("index was not compacted: %q %v", b, err)
	}
	s = open()
	defer s.Close()
	accepted, duplicates, err := s.AppendIngest("ingest:test", []usage.Event{{ID: oldID}})
	if err != nil || len(accepted) != 0 || len(duplicates) != 1 {
		t.Fatalf("retry after restart: accepted %v, duplicates %v, error %v", accepted, duplicates, err)
	}
}
