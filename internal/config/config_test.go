package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndLegacyAdapter(t *testing.T) {
	t.Setenv("ADAPTER", "noop")
	c := Load()
	if strings.Join(c.Adapters, ",") != "noop" || c.CollectionInterval != time.Minute || c.CPUMemoryBasis != "usage" || c.EnforcementMode != "observe" {
		t.Fatalf("defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADAPTERS", "metronome, webhook")
	t.Setenv("API_TOKEN", "tok")
	t.Setenv("GPU_RESOURCES", "none")
	c = Load()
	if strings.Join(c.Adapters, ",") != "metronome,webhook" || c.IngestToken != "tok" || len(c.GPUResources) != 0 {
		t.Fatalf("overrides: %+v", c)
	}
}

func TestValidateRejectsUnsafeSettings(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"window too small":        func(c *Config) { c.CollectionInterval = 5 * time.Second },
		"window not hour-aligned": func(c *Config) { c.CollectionInterval = 7 * time.Minute },
		"bad basis":               func(c *Config) { c.CPUMemoryBasis = "limits" },
		"bad mode":                func(c *Config) { c.EnforcementMode = "delete-everything" },
		"bad tenant source":       func(c *Config) { c.TenantSource = "owner" },
	} {
		c := Load()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPrometheusAuthAndGPUQueries(t *testing.T) {
	t.Setenv("PROMETHEUS_HEADERS", "X-Scope-OrgID=tenant-1, Authorization=Basic dmI6cHc=")
	t.Setenv("PROMETHEUS_BEARER_TOKEN_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/token")
	t.Setenv("GPU_UTIL_QUERY", "none")
	c := Load()
	if c.PrometheusHeaders["X-Scope-OrgID"] != "tenant-1" || c.PrometheusHeaders["Authorization"] != "Basic dmI6cHc=" {
		t.Fatalf("headers: %v", c.PrometheusHeaders)
	}
	if c.PromTokenFile == "" || c.GPUUtilQuery != "none" || c.GPUCountQuery != "" {
		t.Fatalf("prometheus settings: %+v", c)
	}
}

// Secrets created with kubectl create secret --from-file keep the file's
// trailing newline; an API key with a newline breaks every request.
func TestSecretsFromFilesAreTrimmed(t *testing.T) {
	t.Setenv("STRIPE_API_KEY", "sk_test_123\n")
	t.Setenv("METRONOME_API_TOKEN", " tok \r\n")
	t.Setenv("WEBHOOK_SECRET", "whsec_abc\n")
	c := Load()
	if c.StripeAPIKey != "sk_test_123" || c.MetronomeAPIToken != "tok" || c.WebhookSecret != "whsec_abc" {
		t.Fatalf("secrets not trimmed: %q %q %q", c.StripeAPIKey, c.MetronomeAPIToken, c.WebhookSecret)
	}
}
