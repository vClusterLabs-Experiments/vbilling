package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
)

// Without API_TOKEN, vBilling generates a token in DATA_DIR/api-token with
// mode 0600 and reuses it on the next start; without INGEST_TOKEN, a
// separate one in DATA_DIR/ingest-token. The ingest token is never the API
// token.
func TestPrepareTokensGeneratesAndKeepsAToken(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	files, err := PrepareTokens(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{files.API: TokenFile, files.Ingest: IngestTokenFile} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0o600 || path != filepath.Join(dir, want) {
			t.Fatalf("token file %s: %v %v", path, st, err)
		}
	}
	if len(cfg.APIToken) < 40 || len(cfg.IngestToken) < 40 || cfg.IngestToken == cfg.APIToken {
		t.Fatalf("token %d chars, ingest token %d chars and its own: %v", len(cfg.APIToken), len(cfg.IngestToken), cfg.IngestToken != cfg.APIToken)
	}
	again := &config.Config{DataDir: dir}
	if _, err := PrepareTokens(again); err != nil || again.APIToken != cfg.APIToken || again.IngestToken != cfg.IngestToken {
		t.Fatalf("restart: %v, same tokens %v %v", err, again.APIToken == cfg.APIToken, again.IngestToken == cfg.IngestToken)
	}
	own := &config.Config{DataDir: dir, IngestToken: "own-ingest"}
	if files, err := PrepareTokens(own); err != nil || own.APIToken != cfg.APIToken || own.IngestToken != "own-ingest" || files.Ingest != "" {
		t.Fatalf("INGEST_TOKEN set: %v, ingest %q, file %q", err, own.IngestToken, files.Ingest)
	}
	set := &config.Config{DataDir: t.TempDir(), APIToken: "set", IngestToken: "ingest"}
	if files, err := PrepareTokens(set); err != nil || files != (TokenFiles{}) || set.APIToken != "set" || set.IngestToken != "ingest" {
		t.Fatalf("both set: %+v %v", files, err)
	}
	for _, f := range []string{TokenFile, IngestTokenFile} {
		if _, err := os.Stat(filepath.Join(set.DataDir, f)); err == nil {
			t.Fatalf("no %s when its token is set", f)
		}
	}
	clash := &config.Config{DataDir: t.TempDir(), APIToken: "same", IngestTokens: map[string]string{"same": "acme"}}
	if _, err := PrepareTokens(clash); err == nil {
		t.Fatal("a tenant's token equal to the API token must be refused")
	}
}

// config.Load fills an unset INGEST_TOKEN with the API token, as earlier
// versions did. Gateways keep the ingest token in their configuration, so
// that default is replaced by a token of its own, and the API token is
// refused at ingest with a hint.
func TestIngestNeverTakesTheAPIToken(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir(), APIToken: adminToken, IngestToken: adminToken}
	files, err := PrepareTokens(cfg)
	if err != nil || files.Ingest == "" || cfg.IngestToken == adminToken || cfg.IngestToken == "" {
		t.Fatalf("ingest token %q from %q: %v", cfg.IngestToken, files.Ingest, err)
	}
	e := setup(t, func(c *config.Config) { c.IngestToken = cfg.IngestToken })
	code, out := postJSON(t, e.srv.URL+"/api/v1/events", adminToken, `[]`)
	if code != 401 || out["error"] == nil {
		t.Fatalf("ingest with the API token: %d %v", code, out)
	}
	if code, _ := postJSON(t, setupRecords(t).srv.URL+"/api/v1/records", adminToken, `{}`); code != 401 {
		t.Fatalf("records with the API token: %d", code)
	}
	if code, _ := postJSON(t, e.srv.URL+"/api/v1/events", cfg.IngestToken, `[]`); code != 400 {
		t.Fatalf("the ingest token is accepted (and an empty batch refused): %d", code)
	}
}
