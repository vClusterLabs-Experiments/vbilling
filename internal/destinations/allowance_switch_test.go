package destinations

import (
	"context"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// openDest is a destination without an EventFilter (like webhook and noop).
type openDest struct{}

func (openDest) Name() string                                       { return "open" }
func (openDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (openDest) EnsureTenant(context.Context, usage.Tenant) error   { return nil }
func (openDest) SendEvents(context.Context, []usage.Event) error    { return nil }
func (openDest) RemoveTenant(context.Context, usage.Tenant) error   { return nil }

// billingDest filters like the billing adapters do.
type billingDest struct{ openDest }

func (billingDest) Accepts(metric string) bool { return BillableOnly(metric) }

// Switching storage allowances on or off changes the catalog at once, but
// events metered before the switch can still be on their way to a billing
// platform. Each storage event is billed the way it was metered: one
// metered without an allowance in full, one metered under an allowance
// through its overage event, which is billed after allowances are switched
// off too. Nothing metered before a switch is dropped or billed twice.
func TestStorageIsBilledTheWayItWasMetered(t *testing.T) {
	t.Cleanup(func() { usage.UseStorageAllowances(false) })
	plain := &usage.Event{Metric: usage.MetricStorageGBHours, Properties: map[string]any{}}
	underAllowance := &usage.Event{Metric: usage.MetricStorageGBHours, Properties: map[string]any{usage.PropOverageGiBHours: 0.5}}
	overage := &usage.Event{Metric: usage.MetricStorageOverageGBHours}
	gpu := &usage.Event{Metric: usage.MetricGPUHours}
	var d billingDest

	for _, on := range []bool{false, true, false} {
		usage.UseStorageAllowances(on)
		for _, tc := range []struct {
			name string
			e    *usage.Event
			want bool
		}{
			{"storage metered without an allowance", plain, true},
			{"storage metered under an allowance", underAllowance, false},
			{"overage", overage, true},
			{"GPU hours (the catalog decides)", gpu, true},
		} {
			if got := AcceptsEvent(d, tc.e); got != tc.want {
				t.Errorf("allowances on=%v: %s: billed=%v, want %v", on, tc.name, got, tc.want)
			}
		}
	}

	// A destination without a filter (webhook, noop) gets every event.
	usage.UseStorageAllowances(true)
	if !AcceptsEvent(openDest{}, underAllowance) {
		t.Error("a destination without a filter must get storage metered under an allowance too")
	}
}
