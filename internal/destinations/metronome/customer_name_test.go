package metronome

import (
	"context"
	"strings"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func provisioning(t *testing.T, f *metronometest.Server, state *kv.Store) *Adapter {
	t.Helper()
	return New(NewClient(f.URL, metronometest.Token), &config.Config{MetronomeProvision: true}, state, nil)
}

func ensure(t *testing.T, a *Adapter, tn usage.Tenant) {
	t.Helper()
	if err := a.EnsureTenant(context.Background(), tn); err != nil {
		t.Fatalf("EnsureTenant(%s): %v", tn.ID, err)
	}
}

// A customer vBilling created follows its tenant's display name, so
// invoices show the name the tenant has now. The request matches
// Metronome's schema (the fake validates it), and an unchanged name costs
// no request.
func TestCreatedCustomerFollowsTheTenantsName(t *testing.T) {
	f := metronometest.New(t)
	state := kv.Memory()
	a := provisioning(t, f, state)
	tn := usage.Tenant{ID: "tenant-acme", DisplayName: "Acme"}
	ensure(t, a, tn)
	id, _ := state.Get("cus/tenant-acme")
	if c, _ := f.Customer(id); c.Name != "Acme" {
		t.Fatalf("created customer is named %q, want Acme", c.Name)
	}

	ensure(t, a, tn) // set up again, same name
	if n := f.Calls("POST /v1/customers/{id}/setName"); n != 0 {
		t.Fatalf("renamed %d time(s) although the name did not change", n)
	}

	tn.DisplayName = "Acme Renamed"
	ensure(t, a, tn)
	if c, _ := f.Customer(id); c.Name != "Acme Renamed" {
		t.Fatalf("customer is named %q after the tenant was renamed, want Acme Renamed", c.Name)
	}
	// Restarted with the same state: nothing to do.
	ensure(t, provisioning(t, f, state), tn)
	if n := f.Calls("POST /v1/customers/{id}/setName"); n != 1 {
		t.Fatalf("setName called %d times, want once", n)
	}

	// A tenant without a display name is named after its ID, as at creation.
	tn.DisplayName = ""
	ensure(t, a, tn)
	if c, _ := f.Customer(id); c.Name != "tenant-acme" {
		t.Fatalf("customer is named %q, want the tenant ID", c.Name)
	}
}

// Customers vBilling did not create keep their names: one it found by the
// tenant's ingest alias (made by a portal), one it was given with the
// metronome-customer-id annotation, and one set up before vBilling recorded
// which customers it created.
func TestCustomersVBillingDidNotCreateKeepTheirNames(t *testing.T) {
	f := metronometest.New(t)
	state := kv.Memory()
	a := provisioning(t, f, state)

	portal, err := f.CreateCustomer("Globex Corporation", "tenant-globex")
	if err != nil {
		t.Fatal(err)
	}
	ensure(t, a, usage.Tenant{ID: "tenant-globex", DisplayName: "Globex"})
	ensure(t, a, usage.Tenant{ID: "tenant-globex", DisplayName: "Globex Renamed"})
	if c, _ := f.Customer(portal); c.Name != "Globex Corporation" {
		t.Fatalf("a customer vBilling found by its alias was renamed to %q", c.Name)
	}

	pinned, err := f.CreateCustomer("Initech Ltd", "initech-billing")
	if err != nil {
		t.Fatal(err)
	}
	tn := usage.Tenant{ID: "tenant-initech", DisplayName: "Initech", ProviderIDs: map[string]string{"metronome": pinned}}
	ensure(t, a, tn)
	tn.DisplayName = "Initech Renamed"
	ensure(t, a, tn)
	if c, _ := f.Customer(pinned); c.Name != "Initech Ltd" {
		t.Fatalf("a customer the tenant pins was renamed to %q", c.Name)
	}

	// Set up by an earlier version: remembered, but not as created.
	older, err := f.CreateCustomer("Umbrella", "tenant-umbrella")
	if err != nil {
		t.Fatal(err)
	}
	_ = state.Set("cus/tenant-umbrella", older)
	_ = state.Set("tenant/"+older, "tenant-umbrella")
	ensure(t, a, usage.Tenant{ID: "tenant-umbrella", DisplayName: "Umbrella Renamed"})
	if c, _ := f.Customer(older); c.Name != "Umbrella" {
		t.Fatalf("a customer from before v0.3.3 was renamed to %q", c.Name)
	}
	if n := f.Calls("POST /v1/customers/{id}/setName"); n != 0 {
		t.Fatalf("setName called %d times for customers vBilling did not create", n)
	}
}

// A rename that fails never holds up the tenant's usage: EnsureTenant
// succeeds, and the rename is tried again the next time the tenant is set
// up.
func TestFailedRenameDoesNotHoldUpUsage(t *testing.T) {
	f := metronometest.New(t)
	state := kv.Memory()
	a := provisioning(t, f, state)
	tn := usage.Tenant{ID: "tenant-acme", DisplayName: "Acme"}
	ensure(t, a, tn)
	id, _ := state.Get("cus/tenant-acme")

	tn.DisplayName = "Acme Renamed"
	f.FailNext("POST /v1/customers/{id}/setName", 1)
	ensure(t, a, tn)
	if c, _ := f.Customer(id); c.Name != "Acme" {
		t.Fatalf("customer renamed to %q although Metronome failed the request", c.Name)
	}
	if got, _ := state.Get("named/" + id); got != "Acme" {
		t.Fatalf("state says the customer is named %q after a failed rename", got)
	}
	ensure(t, a, tn)
	if c, _ := f.Customer(id); c.Name != "Acme Renamed" {
		t.Fatalf("customer is named %q after the retry, want Acme Renamed", c.Name)
	}
}

// Metronome keeps at most 160 characters of a name. vBilling remembers the
// name it asked for, so a long name is not sent again on every setup.
func TestLongNameIsSentOnce(t *testing.T) {
	f := metronometest.New(t)
	state := kv.Memory()
	a := provisioning(t, f, state)
	tn := usage.Tenant{ID: "tenant-acme", DisplayName: "Acme"}
	ensure(t, a, tn)
	tn.DisplayName = strings.Repeat("A", 200)
	ensure(t, a, tn)
	ensure(t, a, tn)
	if n := f.Calls("POST /v1/customers/{id}/setName"); n != 1 {
		t.Fatalf("setName called %d times, want once", n)
	}
}
