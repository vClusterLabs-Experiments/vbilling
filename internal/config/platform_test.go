package config

import (
	"testing"
	"time"
)

func TestPlatformTenantSource(t *testing.T) {
	t.Setenv("TENANT_SOURCE", "platform")
	if err := Load().Validate(); err == nil {
		t.Fatal("TENANT_SOURCE=platform without PLATFORM_URL and PLATFORM_ACCESS_KEY must be rejected")
	}
	t.Setenv("PLATFORM_URL", "https://platform.example.com")
	t.Setenv("PLATFORM_ACCESS_KEY", "key-from-a-secret\n")
	c := Load()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PlatformAccessKey != "key-from-a-secret" || c.PlatformRefresh != time.Minute || !c.PlatformProxy || c.PlatformOrphanGrace != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	for name, mutate := range map[string]func(*Config){
		"CA and insecure":  func(c *Config) { c.PlatformCAFile, c.PlatformInsecure = "/ca.crt", true },
		"refresh too fast": func(c *Config) { c.PlatformRefresh = time.Second },
		"negative grace":   func(c *Config) { c.PlatformOrphanGrace = -time.Minute },
	} {
		c := Load()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
