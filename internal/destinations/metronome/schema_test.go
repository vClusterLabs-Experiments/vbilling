package metronome

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
)

// The request schemas in testdata were extracted (with $refs resolved) from
// Metronome's published OpenAPI spec (docs.metronome.com/openapi.json).
// metronometest.Validate implements the subset of JSON Schema those schemas
// use, strictly: an object schema that lists properties rejects unknown
// fields, so a misspelled field fails here instead of being ignored by
// Metronome. The fake Metronome in metronometest validates with it too.

var requestSchemas map[string]any

func loadSchemas(t *testing.T) map[string]any {
	t.Helper()
	if requestSchemas == nil {
		b, err := os.ReadFile("testdata/openapi-request-schemas.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &requestSchemas); err != nil {
			t.Fatal(err)
		}
	}
	return requestSchemas
}

func validate(schema any, v any, path string) []string {
	return metronometest.Validate(schema, v, path)
}

// assertMatchesSchema validates a captured request body.
func assertMatchesSchema(t *testing.T, name string, body []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%s: invalid JSON: %v", name, err)
	}
	schema, ok := loadSchemas(t)[name]
	if !ok {
		t.Fatalf("no schema %q", name)
	}
	for _, e := range validate(schema, v, name) {
		t.Errorf("schema violation: %s", e)
	}
}

func TestValidatorCatchesTypos(t *testing.T) {
	bad := []byte(`{"name":"x","ingest_alias":["typo"]}`)
	var v any
	json.Unmarshal(bad, &v)
	if errs := validate(loadSchemas(t)["create_customer"], v, "create_customer"); len(errs) == 0 {
		t.Fatal("validator accepted an unknown field")
	}
}
