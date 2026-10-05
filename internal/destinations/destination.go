// Package destinations defines the billing-adapter contract.
//
// vBilling meters tenant clusters, writes canonical usage events to its
// durable ledger, and fans them out to one or more destinations: billing
// engines (Stripe, Metronome, Lago), data platforms (webhook), or a dry-run
// sink (noop). Destinations are selected with ADAPTERS=a,b,c.
package destinations

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Destination is the contract every adapter implements. All methods must be
// idempotent: vBilling retries them freely.
type Destination interface {
	Name() string
	// Bootstrap creates what the backend needs to accept the catalog
	// (meters, billable metrics, plan charges).
	Bootstrap(ctx context.Context, metrics []usage.MetricDef) error
	// EnsureTenant creates or updates the billing customer.
	EnsureTenant(ctx context.Context, t usage.Tenant) error
	// RemoveTenant runs once a tenant has had no tenant clusters for the
	// offboarding grace period. Adapters decide what that means; deleting
	// customers with open invoices is never the right answer.
	RemoveTenant(ctx context.Context, t usage.Tenant) error
	// SendEvents delivers a batch. Return Permanent(err) only when the
	// backend will never accept these events as sent (bad payload); every
	// other error is retried with backoff.
	SendEvents(ctx context.Context, events []usage.Event) error
}

// EventFilter is implemented by destinations that only take some metrics
// (billing engines skip informational metrics they have no meter for).
type EventFilter interface {
	Accepts(metric string) bool
}

// Reconciler is implemented by destinations that can report what they
// recorded, so vBilling can compare it with its own ledger.
type Reconciler interface {
	RecordedTotals(ctx context.Context, q TotalsQuery) ([]Total, error)
}

// TotalsQuery asks a backend for recorded usage.
type TotalsQuery struct {
	From, To time.Time
	Tenants  []usage.Tenant
	Metrics  []string
}

// Total is one tenant+metric aggregate.
type Total struct {
	Tenant   string  `json:"tenant"`
	Metric   string  `json:"metric"`
	Quantity float64 `json:"quantity"`
}

// CustomerResolver maps a backend customer ID (cus_..., a Metronome UUID)
// back to a vBilling tenant, so inbound billing webhooks can be acted on.
type CustomerResolver interface {
	TenantForCustomer(ctx context.Context, customerID string) (string, bool)
}

// PermanentError marks a payload the backend will never accept.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// PartialError reports events a backend permanently rejected inside an
// otherwise accepted batch. Every event not listed was accepted. Adapters
// return it only when nothing in the batch needs a retry.
type PartialError struct {
	Rejected map[string]string // event ID -> reason
}

func (e *PartialError) Error() string {
	return fmt.Sprintf("%d event(s) rejected", len(e.Rejected))
}

// Permanent wraps err as non-retryable.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err (or anything it wraps) is permanent.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// Accepts applies a destination's EventFilter, accepting everything when the
// destination has none.
func Accepts(d Destination, metric string) bool {
	if f, ok := d.(EventFilter); ok {
		return f.Accepts(metric)
	}
	return true
}

// BillableOnly is an EventFilter for billing engines: builtin billable
// metrics plus operator-declared custom metrics.
func BillableOnly(metric string) bool {
	def, ok := usage.Lookup(metric)
	return ok && def.Billable
}

// Factory builds a Destination from config.
type Factory func(cfg *config.Config) (Destination, error)

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register associates a factory with an adapter name. Adapter packages call
// this from init().
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = f
}

// New constructs the adapter selected by name.
func New(name string, cfg *config.Config) (Destination, error) {
	mu.RLock()
	f, ok := registry[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown billing adapter %q (known: %v)", name, Registered())
	}
	return f(cfg)
}

// NewAll constructs every adapter in names, failing on the first error.
func NewAll(names []string, cfg *config.Config) ([]Destination, error) {
	seen := map[string]bool{}
	var out []Destination
	for _, n := range names {
		if seen[n] {
			return nil, fmt.Errorf("adapter %q listed twice", n)
		}
		seen[n] = true
		d, err := New(n, cfg)
		if err != nil {
			return nil, fmt.Errorf("adapter %s: %w", n, err)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errors.New("no adapters configured")
	}
	return out, nil
}

// Registered lists known adapter names.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
