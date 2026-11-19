package config

import "testing"

// Third review: an operator gives two tenants the same ingest token (a
// copy-paste in api.tenantIngestTokens or INGEST_TOKENS). The map keeps the
// last entry and nothing complains: everything acme's gateway sends is
// billed to globex.
func TestReview3DuplicateIngestTokenIsRefused(t *testing.T) {
	t.Setenv("ADAPTER", "noop")
	t.Setenv("INGEST_TOKENS", "tok-shared=acme,tok-shared=globex")
	c := Load()
	if err := c.Validate(); err == nil {
		t.Fatalf("one token bound to two tenants was accepted and binds to %q only", c.IngestTokens["tok-shared"])
	}
}
