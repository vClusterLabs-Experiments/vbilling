package destinations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

type fakeDest struct{ name string }

func (f *fakeDest) Name() string                                       { return f.name }
func (f *fakeDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (f *fakeDest) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (f *fakeDest) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (f *fakeDest) SendEvents(context.Context, []usage.Event) error    { return nil }

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
	if _, err := NewAll([]string{"test-fake", "test-fake"}, &config.Config{}); err == nil {
		t.Error("duplicate adapters must be rejected")
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

func TestStatusErrorClassification(t *testing.T) {
	for status, permanent := range map[int]bool{400: true, 413: true, 422: true, 401: false, 403: false, 404: false, 409: false, 429: false, 500: false, 503: false} {
		err := StatusError("POST", "/x", status, []byte("body"))
		if IsPermanent(err) != permanent {
			t.Errorf("HTTP %d permanent=%v, want %v", status, IsPermanent(err), permanent)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != status {
			t.Errorf("HTTP %d not unwrappable to HTTPError", status)
		}
	}
	if !IsPermanent(fmt.Errorf("wrapped: %w", Permanent(errors.New("x")))) {
		t.Error("wrapped permanent error not detected")
	}
}

func TestBillableOnly(t *testing.T) {
	if !BillableOnly(usage.MetricGPUHours) || BillableOnly(usage.MetricGPUUtilization) || BillableOnly("unknown_metric") {
		t.Error("BillableOnly classification wrong")
	}
}

func TestSentCacheEvictsOldest(t *testing.T) {
	c := NewSentCache(2)
	c.Add("a")
	c.Add("b")
	c.Add("c")
	if c.Has("a") || !c.Has("b") || !c.Has("c") {
		t.Error("cache did not evict the oldest entry")
	}
}
