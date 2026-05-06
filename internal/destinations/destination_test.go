package destinations

import (
	"context"
	"strings"
	"testing"

	"github.com/loft-sh/vbilling/internal/config"
)

type fakeDest struct{ name string }

func (f *fakeDest) Name() string                                              { return f.name }
func (f *fakeDest) Bootstrap(context.Context) error                           { return nil }
func (f *fakeDest) EnsureTenant(context.Context, Tenant) error                { return nil }
func (f *fakeDest) RemoveTenant(context.Context, string) error                { return nil }
func (f *fakeDest) SendEvents(context.Context, []UsageEvent) error            { return nil }

func TestRegistryReturnsRegisteredAdapter(t *testing.T) {
	Register("test-fake", func(*config.Config) (Destination, error) {
		return &fakeDest{name: "test-fake"}, nil
	})

	d, err := New("test-fake", &config.Config{})
	if err != nil {
		t.Fatalf("New(test-fake) returned error: %v", err)
	}
	if d.Name() != "test-fake" {
		t.Errorf("got Name=%q, want %q", d.Name(), "test-fake")
	}
}

func TestRegistryReturnsErrorForUnknownAdapter(t *testing.T) {
	_, err := New("does-not-exist", &config.Config{})
	if err == nil {
		t.Fatal("expected error for unknown adapter, got nil")
	}
	if !strings.Contains(err.Error(), "unknown billing adapter") {
		t.Errorf("error %q does not mention unknown adapter", err.Error())
	}
}
