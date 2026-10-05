package metronome

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"testing"
)

// The request schemas in testdata were extracted (with $refs resolved) from
// Metronome's published OpenAPI spec (docs.metronome.com/openapi.json).
// validate implements the subset of JSON Schema those schemas use, strictly:
// an object schema that lists properties rejects unknown fields, so a
// misspelled field fails here instead of being ignored by Metronome.

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

var uuidFormat = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

func validate(schema any, v any, path string) []string {
	s, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, path+": "+fmt.Sprintf(format, args...)) }

	if v == nil {
		if s["nullable"] == true {
			return nil
		}
	}
	for _, sub := range asList(s["allOf"]) {
		errs = append(errs, validate(sub, v, path)...)
	}
	if alts := asList(s["oneOf"]); len(alts) > 0 {
		matched := false
		for _, alt := range alts {
			if len(validate(alt, v, path)) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			fail("matches none of the oneOf alternatives")
		}
	}
	if enum := asList(s["enum"]); len(enum) > 0 {
		found := false
		for _, e := range enum {
			if e == v {
				found = true
			}
		}
		if !found {
			fail("%v not in enum %v", v, enum)
		}
	}
	switch s["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			fail("expected object, got %T", v)
			return errs
		}
		props, _ := s["properties"].(map[string]any)
		for _, r := range asList(s["required"]) {
			if _, ok := obj[r.(string)]; !ok {
				fail("missing required field %q", r)
			}
		}
		for k, val := range obj {
			if ps, ok := props[k]; ok {
				errs = append(errs, validate(ps, val, path+"."+k)...)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unknown field %q", k)
				}
			case map[string]any:
				errs = append(errs, validate(ap, val, path+"."+k)...)
			default:
				if len(props) > 0 {
					fail("unknown field %q (not in Metronome's schema)", k)
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			fail("expected array, got %T", v)
			return errs
		}
		if min, ok := s["minItems"].(float64); ok && float64(len(arr)) < min {
			fail("%d items < minItems %v", len(arr), min)
		}
		if max, ok := s["maxItems"].(float64); ok && float64(len(arr)) > max {
			fail("%d items > maxItems %v", len(arr), max)
		}
		for i, it := range arr {
			errs = append(errs, validate(s["items"], it, fmt.Sprintf("%s[%d]", path, i))...)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			fail("expected string, got %T", v)
			return errs
		}
		if min, ok := s["minLength"].(float64); ok && float64(len(str)) < min {
			fail("length %d < minLength %v", len(str), min)
		}
		if max, ok := s["maxLength"].(float64); ok && float64(len(str)) > max {
			fail("length %d > maxLength %v", len(str), max)
		}
		if s["format"] == "uuid" && !uuidFormat.MatchString(str) {
			fail("%q is not a uuid", str)
		}
	case "number", "integer":
		if _, ok := v.(float64); !ok {
			fail("expected number, got %T", v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			fail("expected boolean, got %T", v)
		}
	}
	return errs
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
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
