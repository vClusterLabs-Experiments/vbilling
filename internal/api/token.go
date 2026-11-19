package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
)

// TokenFile is the file in DATA_DIR that holds the API token vBilling
// generates when API_TOKEN is not set.
const TokenFile = "api-token"

// PrepareTokens makes sure the API is never open. When API_TOKEN is empty
// it uses the token in DATA_DIR/api-token, creating the file with a random
// token (mode 0600) on the first start, and returns the file's path (log
// the path, never the token). INGEST_TOKEN defaults to that token, as it
// defaults to API_TOKEN.
//
// It refuses a tenant-bound ingest token (INGEST_TOKENS) that equals the
// API token or the shared ingest token: the tenant holding it would have
// the operator's access.
func PrepareTokens(cfg *config.Config) (generatedPath string, err error) {
	if cfg.APIToken == "" {
		path := filepath.Join(cfg.DataDir, TokenFile)
		tok, err := loadOrCreateToken(path)
		if err != nil {
			return "", fmt.Errorf("API_TOKEN is not set and %s cannot be used: %w", path, err)
		}
		cfg.APIToken, generatedPath = tok, path
		if cfg.IngestToken == "" {
			cfg.IngestToken = tok
		}
	}
	for tok, tenant := range cfg.IngestTokens {
		if tokenOK(tok, cfg.APIToken) || (cfg.IngestToken != "" && tokenOK(tok, cfg.IngestToken)) {
			return "", fmt.Errorf("the INGEST_TOKENS token of tenant %s is also the API or shared ingest token; give each tenant its own token", tenant)
		}
	}
	return generatedPath, nil
}

// loadOrCreateToken reads the token in path, or writes a new one there.
func loadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, keepPrivate(path)
		}
		// An empty file (a crash before the first write finished): replace it.
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".api-token-*") // created with mode 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return tok, keepPrivate(path)
}

// keepPrivate makes sure only vBilling's user can read the token file.
func keepPrivate(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0o600 {
		return os.Chmod(path, 0o600)
	}
	return nil
}
