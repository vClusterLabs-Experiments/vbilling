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

// IngestTokenFile is the file in DATA_DIR that holds the shared ingest
// token vBilling generates when INGEST_TOKEN is not set.
const IngestTokenFile = "ingest-token"

// TokenFiles are the token files PrepareTokens used: "" for a token that
// was set. Log the paths, never the tokens.
type TokenFiles struct {
	API    string
	Ingest string
}

// PrepareTokens makes sure the API is never open and that gateways never
// need the API token. When API_TOKEN is empty it uses the token in
// DATA_DIR/api-token, and when INGEST_TOKEN is empty the one in
// DATA_DIR/ingest-token, creating each file with a random token (mode
// 0600) on the first start.
//
// The shared ingest token is never the API token: gateways keep it in
// their configuration, where whoever reads it could otherwise pause tenants
// and export everyone's usage. config.Load still fills an unset
// INGEST_TOKEN with API_TOKEN, as earlier versions did; Validate refuses an
// INGEST_TOKEN set to the API token, so an equal value here is that
// default and is replaced by the generated token.
//
// It refuses a tenant-bound ingest token (INGEST_TOKENS) that equals the
// API token or the shared ingest token: the tenant holding it would have
// the operator's access.
func PrepareTokens(cfg *config.Config) (TokenFiles, error) {
	var files TokenFiles
	if cfg.IngestToken != "" && tokenOK(cfg.IngestToken, cfg.APIToken) {
		cfg.IngestToken = "" // the API token by default, never used for ingest
	}
	if cfg.APIToken == "" {
		path := filepath.Join(cfg.DataDir, TokenFile)
		tok, err := loadOrCreateToken(path)
		if err != nil {
			return TokenFiles{}, fmt.Errorf("API_TOKEN is not set and %s cannot be used: %w", path, err)
		}
		cfg.APIToken, files.API = tok, path
	}
	if cfg.IngestToken == "" {
		path := filepath.Join(cfg.DataDir, IngestTokenFile)
		tok, err := loadOrCreateToken(path)
		if err != nil {
			return TokenFiles{}, fmt.Errorf("INGEST_TOKEN is not set and %s cannot be used: %w", path, err)
		}
		if tokenOK(tok, cfg.APIToken) {
			return TokenFiles{}, fmt.Errorf("%s holds the API token; delete it so vBilling generates an ingest token of its own", path)
		}
		cfg.IngestToken, files.Ingest = tok, path
	}
	for tok, tenant := range cfg.IngestTokens {
		if tokenOK(tok, cfg.APIToken) || tokenOK(tok, cfg.IngestToken) {
			return TokenFiles{}, fmt.Errorf("the INGEST_TOKENS token of tenant %s is also the API or shared ingest token; give each tenant its own token", tenant)
		}
	}
	return files, nil
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
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*") // created with mode 0600
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
