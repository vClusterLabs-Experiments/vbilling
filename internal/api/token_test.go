package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
)

// Without API_TOKEN, vBilling generates a token in DATA_DIR/api-token with
// mode 0600 and reuses it on the next start; INGEST_TOKEN defaults to it.
func TestPrepareTokensGeneratesAndKeepsAToken(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	path, err := PrepareTokens(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 || path != filepath.Join(dir, TokenFile) {
		t.Fatalf("token file %s: %v %v", path, st, err)
	}
	if len(cfg.APIToken) < 40 || cfg.IngestToken != cfg.APIToken {
		t.Fatalf("token %d chars, ingest token defaults to it: %v", len(cfg.APIToken), cfg.IngestToken == cfg.APIToken)
	}
	again := &config.Config{DataDir: dir, IngestToken: "own-ingest"}
	if _, err := PrepareTokens(again); err != nil || again.APIToken != cfg.APIToken || again.IngestToken != "own-ingest" {
		t.Fatalf("restart: %v, same token %v, ingest %q", err, again.APIToken == cfg.APIToken, again.IngestToken)
	}
	set := &config.Config{DataDir: t.TempDir(), APIToken: "set"}
	if p, err := PrepareTokens(set); err != nil || p != "" || set.APIToken != "set" {
		t.Fatalf("API_TOKEN set: %q %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(set.DataDir, TokenFile)); err == nil {
		t.Fatal("no token file when API_TOKEN is set")
	}
	clash := &config.Config{DataDir: t.TempDir(), APIToken: "same", IngestTokens: map[string]string{"same": "acme"}}
	if _, err := PrepareTokens(clash); err == nil {
		t.Fatal("a tenant's token equal to the API token must be refused")
	}
}
