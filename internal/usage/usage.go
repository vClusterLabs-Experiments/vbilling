// Package usage defines vBilling's canonical usage schema.
//
// Every billable observation becomes an Event: a quantity of one metric,
// attributed to one tenant, over one closed time window, stamped with the
// dimensions a rating engine needs (region, project, SKU, capacity type, ...).
// Events carry a deterministic ID derived from their identity, so collecting
// or delivering the same window twice produces the same ID and every
// downstream system can deduplicate on it.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion identifies the wire format of Event. It changes only on
// breaking changes; new optional fields do not bump it.
const SchemaVersion = "vbilling.usage/v1"

// Well-known dimension keys. Dimensions are part of an event's identity and
// are what rating engines group and price on.
const (
	DimTenantCluster = "tenant_cluster" // tenant cluster external ID
	DimGPUType       = "gpu_type"       // normalized GPU model, e.g. NVIDIA-H100-80GB-HBM3
	DimGPUProfile    = "gpu_profile"    // full, mig-1g.10gb, timeslice-4, ...
	DimCapacityType  = "capacity_type"  // on-demand, spot, preemptible, reserved
	DimBillingMode   = "billing_mode"   // shared, dedicated_node, private_node
	DimZone          = "zone"
	DimTenantClass   = "tenant_class" // e.g. public, enterprise, government, dev
	DimNamespace     = "namespace"    // namespace inside the tenant cluster (optional)
	DimNode          = "node"
	DimInstanceType  = "instance_type"
)

// Capacity types.
const (
	CapacityOnDemand    = "on-demand"
	CapacitySpot        = "spot"
	CapacityPreemptible = "preemptible"
	CapacityReserved    = "reserved"
)

// Event is one metered quantity for one tenant over one window.
type Event struct {
	SchemaVersion string `json:"schema_version"`
	// ID is deterministic for collector events (see DeriveID). Ingested
	// events must supply their own stable ID.
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Metric string `json:"metric"`
	// Quantity is in Unit, accumulated over [WindowStart, WindowEnd).
	Quantity    float64   `json:"quantity"`
	Unit        string    `json:"unit"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	Region     string `json:"region,omitempty"`
	Project    string `json:"project,omitempty"`
	SKU        string `json:"sku,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`

	// Dimensions are identity-bearing attributes used for grouping/pricing.
	Dimensions map[string]string `json:"dimensions,omitempty"`
	// Properties are informational and never part of the identity.
	Properties map[string]any `json:"properties,omitempty"`

	// Source is "collector" for events vBilling measured itself, or
	// "ingest:<client>" for events pushed through the ingest API.
	Source     string    `json:"source,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Tenant is a billing customer. One tenant can own several tenant clusters.
type Tenant struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Email       string            `json:"email,omitempty"`
	Currency    string            `json:"currency,omitempty"`
	Region      string            `json:"region,omitempty"`
	Project     string            `json:"project,omitempty"`
	Class       string            `json:"class,omitempty"`
	Plan        string            `json:"plan,omitempty"`
	Clusters    []string          `json:"clusters,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	// ProviderIDs carries IDs the operator pinned for a provider, e.g.
	// {"stripe": "cus_123"} from a tenant-cluster annotation.
	ProviderIDs map[string]string `json:"provider_ids,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// DeriveID returns the deterministic ID for an event. The identity is the
// tenant, metric, window, region, project, SKU, resource ID and the sorted
// dimension set. Quantity and properties are deliberately excluded: the same
// observation re-measured after a restart must map to the same ID.
func DeriveID(e *Event) string {
	var b strings.Builder
	b.WriteString(SchemaVersion)
	for _, part := range []string{
		e.Tenant, e.Metric,
		e.WindowStart.UTC().Format(time.RFC3339Nano),
		e.WindowEnd.UTC().Format(time.RFC3339Nano),
		e.Region, e.Project, e.SKU, e.ResourceID,
	} {
		b.WriteByte(0)
		b.WriteString(part)
	}
	keys := make([]string, 0, len(e.Dimensions))
	for k := range e.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(e.Dimensions[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "vb1_" + hex.EncodeToString(sum[:16])
}

// Finalize fills schema version, unit, rounding and the derived ID. It is
// what the collector calls on every event it builds.
func (e *Event) Finalize() {
	e.SchemaVersion = SchemaVersion
	if e.Unit == "" {
		if def, ok := Lookup(e.Metric); ok {
			e.Unit = def.Unit
		}
	}
	e.Quantity = Round(e.Quantity)
	e.WindowStart = e.WindowStart.UTC()
	e.WindowEnd = e.WindowEnd.UTC()
	if e.ID == "" {
		e.ID = DeriveID(e)
	}
}

var (
	metricRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,99}$`)
	dimKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	idRe     = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,100}$`)
)

// Validate checks the invariants every destination relies on.
func (e *Event) Validate() error {
	var errs []error
	if !idRe.MatchString(e.ID) {
		errs = append(errs, fmt.Errorf("id %q must be 1-100 chars of [A-Za-z0-9._:-]", e.ID))
	}
	if e.Tenant == "" || len(e.Tenant) > 128 {
		errs = append(errs, errors.New("tenant must be 1-128 chars"))
	}
	if !metricRe.MatchString(e.Metric) {
		errs = append(errs, fmt.Errorf("metric %q must match %s", e.Metric, metricRe))
	}
	if math.IsNaN(e.Quantity) || math.IsInf(e.Quantity, 0) || e.Quantity < 0 {
		errs = append(errs, fmt.Errorf("quantity %v must be a finite, non-negative number", e.Quantity))
	}
	if e.WindowStart.IsZero() || e.WindowEnd.IsZero() || !e.WindowEnd.After(e.WindowStart) {
		errs = append(errs, errors.New("window_end must be after window_start"))
	}
	for k := range e.Dimensions {
		if !dimKeyRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("dimension key %q must match %s", k, dimKeyRe))
		}
	}
	return errors.Join(errs...)
}

// Dim returns a dimension value or "".
func (e *Event) Dim(key string) string {
	if e.Dimensions == nil {
		return ""
	}
	return e.Dimensions[key]
}

// Window returns the window of the given size that contains t, aligned to
// multiples of size since the Unix epoch.
func Window(t time.Time, size time.Duration) (start, end time.Time) {
	start = t.UTC().Truncate(size)
	return start, start.Add(size)
}

// Overlap returns how much of [start, end) falls inside [ws, we). A zero end
// means the interval is still open.
func Overlap(start, end, ws, we time.Time) time.Duration {
	if end.IsZero() || end.After(we) {
		end = we
	}
	if start.Before(ws) {
		start = ws
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// Round keeps nine decimal places, enough for per-second GPU-hours
// (1s = 0.000277778h) without float noise leaking into invoices.
func Round(f float64) float64 {
	return math.Round(f*1e9) / 1e9
}
