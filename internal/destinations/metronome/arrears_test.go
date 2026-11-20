package metronome

import (
	"context"
	"strings"
	"testing"
)

// Before a payment clears a billing hold, the customer's threshold
// notifications are read again: one a rule makes blocking that is not ok
// (in alarm, still evaluating) keeps the hold, under any name Metronome
// uses for its type. Notifications no rule makes blocking, ok ones, archived
// ones and other customers' never do. The customer can be found from the
// tenant's alias, and every page is read.
func TestOwedReadsTheCustomersThresholdNotifications(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f)
	ctx := context.Background()
	cus, _ := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	other, _ := f.CreateCustomer("Globex", "tenant-globex")
	rules := map[string]bool{"alerts.low_remaining_commit_balance_reached": true, "alerts.low_remaining_credit_balance_reached": true}
	counts := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := rules[k]; ok {
				return v
			}
		}
		return false
	}
	owed := func(tenant, customer string) string {
		t.Helper()
		got, err := a.Owed(ctx, tenant, customer, counts)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	wallet := f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 0, 50)
	id := wallet.Properties["alert_id"].(string)
	f.Alert("alerts.spend_threshold_reached", cus, "Spend above budget", 120, 100) // no rule: a warning
	f.Alert("alerts.low_remaining_commit_balance_reached", other, "Globex balance low", 0, 50)
	for status, want := range map[string]string{"in_alarm": "in alarm", "evaluating": "evaluating", "ok": "", "": ""} {
		f.SetAlertStatus(id, status)
		got := owed("tenant-acme", cus)
		if want == "" && got != "" || want != "" && (!strings.Contains(got, "Prepaid balance low") || !strings.HasSuffix(got, want)) {
			t.Errorf("wallet notification %q: owed %q, want %q", status, got, want)
		}
	}
	f.SetAlertStatus(id, "in_alarm")
	if got := owed("tenant-acme", ""); !strings.Contains(got, "Prepaid balance low") {
		t.Errorf("customer found by the tenant's alias: owed %q", got)
	}
	if got := owed("tenant-unknown", ""); got != "" {
		t.Errorf("a tenant no customer holds: owed %q, want nothing", got)
	}
	f.SetAlertStatus(id, "ok")

	// The credit balance notification is listed under its API type and
	// named another way in Metronome's webhook example: a rule on either
	// counts. Enough of them to need several pages.
	for i := 0; i < 4; i++ {
		f.Alert("alerts.low_remaining_contract_credit_balance_reached", cus, "Credits low", 0, 10)
	}
	if got := owed("tenant-acme", cus); strings.Count(got, "Credits low") != 4 {
		t.Errorf("credit notifications in alarm on several pages: owed %q", got)
	}

	if got := owed("tenant-acme", "00000000-0000-4000-8000-000000000000"); !strings.Contains(got, "not found") {
		t.Errorf("a customer Metronome does not know: owed %q, want it kept", got)
	}
	f.FailNext("POST /v1/customer-alerts/list", 1)
	if _, err := a.Owed(ctx, "tenant-acme", cus, counts); err == nil {
		t.Fatal("a failed read must be an error, so the webhook is retried")
	}
}
