package metronometest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
)

// The schemas in internal/destinations/metronome/testdata were extracted
// (with $refs resolved) from Metronome's published OpenAPI spec and from the
// OpenAPI blocks of its API reference pages:
//
//   - openapi-request-schemas.json: request bodies by endpoint name
//   - openapi-pricing-schemas.json: request, response and query parameters
//     of the read endpoints the price check uses, and of
//     POST /v1/customer-alerts/get (get_customer_alert), which a payment
//     reads before it lowers a billing hold
//
// Validate implements the subset of JSON Schema they use, strictly: an
// object schema that lists properties rejects unknown fields, so a misspelled
// field fails a test instead of being ignored by Metronome.

// Endpoint is one entry of openapi-pricing-schemas.json.
type Endpoint struct {
	Path     string         `json:"path"`
	Method   string         `json:"method"`
	Request  map[string]any `json:"request"`
	Response map[string]any `json:"response"`
	Query    map[string]any `json:"query"`
}

var (
	loadOnce sync.Once
	loadErr  error
	requests map[string]any
	pricing  map[string]Endpoint
)

func testdata(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata", name)
}

func load() error {
	loadOnce.Do(func() {
		b, err := os.ReadFile(testdata("openapi-request-schemas.json"))
		if err != nil {
			loadErr = err
			return
		}
		if err := json.Unmarshal(b, &requests); err != nil {
			loadErr = fmt.Errorf("openapi-request-schemas.json: %w", err)
			return
		}
		b, err = os.ReadFile(testdata("openapi-pricing-schemas.json"))
		if err != nil {
			loadErr = err
			return
		}
		if err := json.Unmarshal(b, &pricing); err != nil {
			loadErr = fmt.Errorf("openapi-pricing-schemas.json: %w", err)
		}
	})
	return loadErr
}

// RequestSchema returns the vendored request body schema of an endpoint
// (create_customer, ingest, usage, ...).
func RequestSchema(name string) (any, error) {
	if err := load(); err != nil {
		return nil, err
	}
	s, ok := requests[name]
	if !ok {
		return nil, fmt.Errorf("no request schema %q", name)
	}
	return s, nil
}

// PricingEndpoint returns the vendored schemas of a price check endpoint
// (list_billable_metrics, list_products, list_rate_cards, get_rates).
func PricingEndpoint(name string) (Endpoint, error) {
	if err := load(); err != nil {
		return Endpoint{}, err
	}
	e, ok := pricing[name]
	if !ok {
		return Endpoint{}, fmt.Errorf("no pricing schema %q", name)
	}
	return e, nil
}

// ValidateJSON validates a JSON document against schema.
func ValidateJSON(schema any, body []byte, path string) []string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return []string{path + ": invalid JSON: " + err.Error()}
	}
	return Validate(schema, v, path)
}

// ValidateValue validates a Go value as it would look on the wire.
func ValidateValue(schema any, v any, path string) []string {
	b, err := json.Marshal(v)
	if err != nil {
		return []string{path + ": " + err.Error()}
	}
	return ValidateJSON(schema, b, path)
}

var uuidFormat = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// Validate checks a decoded JSON value against a schema and returns every
// violation, each prefixed with its path.
func Validate(schema any, v any, path string) []string {
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
		errs = append(errs, Validate(sub, v, path)...)
	}
	if alts := asList(s["oneOf"]); len(alts) > 0 {
		matched := false
		for _, alt := range alts {
			if len(Validate(alt, v, path)) == 0 {
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
				errs = append(errs, Validate(ps, val, path+"."+k)...)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unknown field %q", k)
				}
			case map[string]any:
				errs = append(errs, Validate(ap, val, path+"."+k)...)
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
			errs = append(errs, Validate(s["items"], it, fmt.Sprintf("%s[%d]", path, i))...)
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
