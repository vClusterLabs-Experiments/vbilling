package records

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Token factories price input served from a prompt cache separately from
// the rest of the input. LiteLLM's prompt_tokens include the cached ones
// (usage_object.prompt_tokens_details.cached_tokens), so the built-in format
// bills prompt - cached as input and cached as its own metric, the same way
// whether the request was pushed or read back from the spend logs.
func TestLiteLLMCachedInputAndServiceTier(t *testing.T) {
	q := request{call: "c-cache", resp: "chatcmpl-cache", team: "acme", end: t0.Add(30 * time.Second), in: 100, out: 20}
	push := q.payload(t)
	md := push["metadata"].(map[string]any)
	md["usage_object"] = map[string]any{"prompt_tokens": num(100), "completion_tokens": num(20),
		"prompt_tokens_details": map[string]any{"cached_tokens": num(60)}}
	push["cost_breakdown"] = map[string]any{"service_tier": "priority"}

	row := q.row(t)
	rmd := row["metadata"].(map[string]any)
	rmd["usage_object"] = md["usage_object"]
	rmd["cost_breakdown"] = map[string]any{"service_tier": "priority"}

	for name, obj := range map[string]map[string]any{"push": push, "pull": SpendLogPayload(row)} {
		rec, _, err := Map("litellm", LiteLLM, obj)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Usage["inference_input_tokens"] != 40 || rec.Usage["inference_cached_input_tokens"] != 60 || rec.Usage["inference_output_tokens"] != 20 {
			t.Errorf("%s: usage %v, want 40 input, 60 cached, 20 output", name, rec.Usage)
		}
		if rec.Dimensions["service_tier"] != "priority" {
			t.Errorf("%s: dimensions %v, want service_tier=priority", name, rec.Dimensions)
		}
	}

	// No cache details (most requests, and the fixtures as captured):
	// everything is input, and no service tier dimension appears.
	plain, _, err := Map("litellm", LiteLLM, request{call: "c-plain", resp: "r-plain", team: "acme", end: t0, in: 100, out: 20}.payload(t))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Usage["inference_input_tokens"] != 100 || plain.Usage["inference_cached_input_tokens"] != 0 || plain.Dimensions["service_tier"] != "" {
		t.Errorf("plain request: usage %v dimensions %v", plain.Usage, plain.Dimensions)
	}

	// A gateway reporting more cached than prompt tokens never bills
	// negative input.
	md["usage_object"].(map[string]any)["prompt_tokens_details"] = map[string]any{"cached_tokens": num(150)}
	odd, _, _ := Map("litellm", LiteLLM, push)
	if odd.Usage["inference_input_tokens"] != 0 {
		t.Errorf("input %v, want 0", odd.Usage["inference_input_tokens"])
	}
}

func TestSubtractMustNameUsageMetrics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.yaml")
	bad := "gw:\n  id: id\n  time: ts\n  usage: {in_tokens: prompt}\n  subtract: {in_tokens: [cached_tokens]}\n"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFormats(path, false); err == nil || !strings.Contains(err.Error(), "cached_tokens") {
		t.Fatalf("subtract of a metric the format does not read: %v", err)
	}
	good := "gw:\n  id: id\n  time: ts\n  usage: {in_tokens: prompt, cached_tokens: cached}\n  subtract: {in_tokens: [cached_tokens]}\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFormats(path, false)
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := Map("gw", f["gw"], map[string]any{"id": "r1", "ts": "2026-11-18T10:00:00Z", "prompt": 90.0, "cached": 30.0})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Usage["in_tokens"] != 60 || rec.Usage["cached_tokens"] != 30 {
		t.Fatalf("usage %v", rec.Usage)
	}
}
