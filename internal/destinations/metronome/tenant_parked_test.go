package metronome

import (
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// One tenant that cannot be set up, with the adapter itself: a Metronome account that has no
// room for another customer answers the create with an error that is not
// permanent. That used to stop delivery for every tenant: the batch was
// retried forever and the cursor never moved. Now only the tenant without a
// customer waits: its usage is parked, every other tenant's usage reaches
// Metronome, and once the customer can be created the parked usage arrives
// in order and Metronome counts all of it.
func TestACustomerMetronomeCannotCreateDoesNotStopTheOtherTenants(t *testing.T) {
	f := newRigFake(t)
	r := newHoldRig(t, t.TempDir(), rigOptions{base: f.URL, provision: true})
	waitFor(t, "the billable metrics created", func() bool {
		st := r.disp.Statuses()
		return len(st) == 1 && st[0].Ready
	})
	r.check() // the price check sees the billable metrics that count the usage
	at := rigNow()
	first := cpuEvent("tenant-a", at.Add(-9*time.Minute), 1)
	r.commit(t, first)
	waitFor(t, "tenant a's customer created and its usage sent", func() bool { return r.delivered() && sentIDs(f)[first.ID] })

	f.FailNext("POST /v1/customers", 1_000_000) // no room for another customer
	var a, b []usage.Event
	for m := 0; m < 4; m++ {
		ws := at.Add(time.Duration(m-8) * time.Minute)
		ea, eb := cpuEvent("tenant-a", ws, 1), cpuEvent("tenant-b", ws, 2)
		r.commit(t, ea, eb)
		a, b = append(a, ea), append(b, eb)
	}
	waitFor(t, "tenant a delivered, tenant b parked and the cursor at the head", func() bool {
		sent := sentIDs(f)
		for _, e := range a {
			if !sent[e.ID] {
				return false
			}
		}
		return r.delivered() && r.held.ParkedCount(rigDest) == len(b)
	})
	sent := sentIDs(f)
	for _, e := range b {
		if sent[e.ID] {
			t.Fatal("usage of a tenant without a customer reached Metronome")
		}
	}
	if n := r.held.Count(rigDest); n != 0 {
		t.Fatalf("%d parked event(s) are counted as held for a price", n)
	}
	rows := r.disp.Blocked(rigDest)
	if len(rows) != 1 || rows[0].Tenant != "tenant-b" || rows[0].Parked != len(b) || rows[0].Permanent || !strings.Contains(rows[0].Error, "create customer tenant-b") {
		t.Fatalf("tenants Metronome could not set up: %+v", rows)
	}
	if got := r.counted(t, "tenant-a", usage.MetricCPUCoreHours, at); got != 5 {
		t.Fatalf("Metronome counts %v CPU-hours for tenant a, want all 5", got)
	}

	f.FailNext("POST /v1/customers", 0) // the operator made room
	waitFor(t, "tenant b's customer created and its parked usage sent", func() bool {
		sent := sentIDs(f)
		for _, e := range b {
			if !sent[e.ID] {
				return false
			}
		}
		return r.held.ParkedCount(rigDest) == 0 && len(r.disp.Blocked(rigDest)) == 0
	})
	var order []string
	for _, e := range f.Events() {
		if e.Properties["value"] == "2" { // tenant b's
			order = append(order, e.TransactionID)
		}
	}
	want := make([]string, len(b))
	for i, e := range b {
		want[i] = e.ID
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("tenant b's usage arrived as %v, want ledger order %v", order, want)
	}
	if got := r.counted(t, "tenant-b", usage.MetricCPUCoreHours, at); got != 8 {
		t.Fatalf("Metronome counts %v CPU-hours for tenant b, want all 8 of the parked usage", got)
	}
}
