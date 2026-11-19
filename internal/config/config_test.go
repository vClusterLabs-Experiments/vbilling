package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndLegacyAdapter(t *testing.T) {
	t.Setenv("ADAPTER", "noop")
	c := Load()
	if strings.Join(c.Adapters, ",") != "noop" || c.CollectionInterval != time.Minute || c.CPUMemoryBasis != "requests" || c.EnforcementMode != "observe" {
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

func TestLiteLLMPullSettings(t *testing.T) {
	t.Setenv("ADAPTER", "noop")
	t.Setenv("RECORDS_PULL_LITELLM_URL", "http://litellm.litellm.svc:4000")
	c := Load()
	if !c.RecordsLiteLLM || c.RecordsPullLiteLLMClient != "litellm" || c.RecordsPullLiteLLMInterval != 5*time.Minute || c.RecordsPullLiteLLMLookback != time.Hour {
		t.Fatalf("pull defaults: %+v", c)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "LITELLM_API_KEY") {
		t.Errorf("a pull without a key must be refused: %v", err)
	}
	t.Setenv("RECORDS_PULL_LITELLM_KEY", "sk-admin\n")
	c = Load()
	if err := c.Validate(); err != nil || c.RecordsPullLiteLLMKey != "sk-admin" {
		t.Fatalf("valid pull settings: %v %q", err, c.RecordsPullLiteLLMKey)
	}
	c.RecordsPullLiteLLMLookback = c.RecordsRetention // pulled records would be older than the ledger accepts
	if err := c.Validate(); err == nil {
		t.Error("a lookback beyond the records retention must be refused")
	}
}

func TestPriceCheckSettings(t *testing.T) {
	c := Load()
	if c.PriceCheck != "warn" || c.PriceCheckInterval != 10*time.Minute || c.PriceCheckLookback != 24*time.Hour || c.PriceCheckMaxHold != 28*24*time.Hour {
		t.Fatalf("price check defaults: %q %s %s %s", c.PriceCheck, c.PriceCheckInterval, c.PriceCheckLookback, c.PriceCheckMaxHold)
	}
	t.Setenv("PRICE_CHECK", "hold")
	if c = Load(); c.PriceCheck != "hold" || c.Validate() != nil {
		t.Fatalf("hold: %+v", c)
	}
	for name, mutate := range map[string]func(*Config){
		"bad mode":      func(c *Config) { c.PriceCheck = "block" },
		"tiny period":   func(c *Config) { c.PriceCheckInterval = time.Second },
		"tiny lookback": func(c *Config) { c.PriceCheckLookback = time.Minute },
	} {
		c := Load()
		mutate(c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Removed settings are ignored with a warning, never an error, so old Helm
// values and manifests keep starting; renamed ones keep their value.
func TestRemovedAndRenamedSettings(t *testing.T) {
	t.Setenv("COLLECTION_INTERVAL", "5m")
	t.Setenv("PRICE_CHECK_MAX_HOLD", "3d")
	t.Setenv("RECORDS_PULL_LITELLM_URL", "http://litellm:4000")
	t.Setenv("RECORDS_PULL_LITELLM_KEY", "sk-admin")
	t.Setenv("METRONOME_STRIPE_LINK", "true")
	t.Setenv("STRIPE_API_KEY", "sk_test_x")
	c := Load()
	if c.CollectionInterval != time.Minute || c.PriceCheckMaxHold != 28*24*time.Hour {
		t.Fatalf("removed settings must not change behavior: %s %s", c.CollectionInterval, c.PriceCheckMaxHold)
	}
	if c.RecordsPullLiteLLMURL != "http://litellm:4000" || c.RecordsPullLiteLLMKey != "sk-admin" {
		t.Fatalf("renamed settings lost their value: %q %q", c.RecordsPullLiteLLMURL, c.RecordsPullLiteLLMKey)
	}
	if !c.MetronomeProvision || !c.MetronomeStripeLink {
		t.Fatalf("METRONOME_STRIPE_LINK=true implied provisioning in v0.3.0: %+v", c)
	}
	joined := strings.Join(c.Warnings, "\n")
	for _, k := range []string{"COLLECTION_INTERVAL", "PRICE_CHECK_MAX_HOLD", "RECORDS_PULL_LITELLM_URL is now LITELLM_URL", "METRONOME_STRIPE_LINK"} {
		if !strings.Contains(joined, k) {
			t.Errorf("no warning for %s in %q", k, joined)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("old settings must still start: %v", err)
	}
}

// The archive is on by default (inside DATA_DIR) and "off" turns it off.
func TestArchiveDefault(t *testing.T) {
	t.Setenv("DATA_DIR", "/data")
	if c := Load(); c.ArchiveDir != "/data/archive" || !c.RecordsArchive {
		t.Fatalf("default archive: %q %t", c.ArchiveDir, c.RecordsArchive)
	}
	t.Setenv("ARCHIVE_DIR", "off")
	if c := Load(); c.ArchiveDir != "" || c.RecordsArchive {
		t.Fatalf("ARCHIVE_DIR=off: %q %t", c.ArchiveDir, c.RecordsArchive)
	}
}
