// Package sourceid assigns a stable namespace to one collector's ledger.
// Keep this state with the ledger when restoring or failing over. Separate
// collectors need separate namespaces even when they share a region.
package sourceid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// Load reads or creates the source identity, after acquiring the writer guard.
func Load(dataDir string) (string, error) {
	s, err := kv.Open(filepath.Join(dataDir, "source.json"))
	if err != nil {
		return "", err
	}
	if id, ok := s.Get("id"); ok {
		b, err := hex.DecodeString(id)
		if err != nil || len(b) != 16 {
			return "", fmt.Errorf("invalid metering source identity in source.json")
		}
		return id, nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if err := s.Set("id", id); err != nil {
		return "", err
	}
	return id, nil
}
