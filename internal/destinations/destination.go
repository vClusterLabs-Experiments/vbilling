// Package destinations defines the billing-adapter contract.
//
// vBilling collects usage metrics from Tenant Clusters and ships them to a
// pluggable billing backend. Each backend (Lago, Metronome, Stripe Meters,
// OpenMeter, custom) implements Destination. The active backend is chosen via
// the ADAPTER env var; default is "lago".
package destinations

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
)

// Canonical metric codes emitted by the controller. Adapters translate these
// to whatever the destination expects.
const (
	MetricCPUCoreHours     = "vcluster_cpu_core_hours"
	MetricMemoryGBHours    = "vcluster_memory_gb_hours"
	MetricStorageGBHours   = "vcluster_storage_gb_hours"
	MetricInstanceHours    = "vcluster_instance_hours"
	MetricGPUHours         = "vcluster_gpu_hours"
	MetricGPUUtilization   = "vcluster_gpu_utilization"
	MetricNetworkEgressGB  = "vcluster_network_egress_gb"
	MetricLBHours          = "vcluster_lb_hours"
	MetricPrivateNodeHours = "vcluster_private_node_hours"
)

// Tenant is a billable entity (one Tenant Cluster).
type Tenant struct {
	ExternalID  string
	DisplayName string
	Currency    string
	Metadata    map[string]string
	CreatedAt   time.Time
}

// UsageEvent is one metered observation for a tenant. Adapters serialize this
// into the destination's wire format.
type UsageEvent struct {
	TenantExternalID string
	MetricCode       string
	Value            float64
	Unit             string
	Timestamp        time.Time
	TransactionID    string
	Properties       map[string]any
}

// Destination is the contract every billing adapter implements.
type Destination interface {
	Name() string
	Bootstrap(ctx context.Context) error
	EnsureTenant(ctx context.Context, t Tenant) error
	RemoveTenant(ctx context.Context, externalID string) error
	SendEvents(ctx context.Context, events []UsageEvent) error
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
		return nil, fmt.Errorf("unknown billing adapter %q (known: %v)", name, registered())
	}
	return f(cfg)
}

func registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	return names
}
