package metronome

import (
	"context"
	"strings"
	"testing"
)

// Before a payment lowers a billing hold it did not settle itself, the
// threshold notification behind the hold is read again for the customer:
// only ok releases it. In alarm, still evaluating, archived or unknown, it
// holds, and so does anything that cannot be read again.
func TestOwedReadsTheAlertBehindTheHoldAgain(t *testing.T) {
	f := newFake(t)
	a := newReadOnly(t, f)
	ctx := context.Background()
	cus, _ := f.CreateCustomer("Acme AI", "org_1f2e", "tenant-acme")
	other, _ := f.CreateCustomer("Globex", "tenant-globex")
	n := f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 0, 50)
	alert := "alert:" + n.Properties["alert_id"].(string)
	owed := func(customer string, causes ...string) string {
		t.Helper()
		got, err := a.Owed(ctx, "tenant-acme", customer, causes)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}
	if got := owed(cus, alert); got != alert {
		t.Fatalf("in alarm: owed %q", got)
	}
	for status, want := range map[string]string{"ok": "", "evaluating": alert, "": alert, "in_alarm": alert} {
		f.SetAlertStatus(strings.TrimPrefix(alert, "alert:"), status)
		if got := owed(cus, alert); got != want {
			t.Errorf("customer status %q: owed %q, want %q", status, got, want)
		}
	}
	// Another customer's view of it, an unknown one, a failed top-up and
	// an empty ID are never confirmed paid.
	f.SetAlertStatus(strings.TrimPrefix(alert, "alert:"), "ok")
	if got := owed(other, alert); got != alert {
		t.Errorf("another customer's alert: owed %q", got)
	}
	unknown := "alert:00000000-0000-4000-8000-000000000000"
	if got := owed(cus, unknown, "payment:inv-1", "alert:"); got != unknown+",payment:inv-1,alert:" {
		t.Errorf("unreadable causes: owed %q", got)
	}
	if got := owed(cus); !strings.Contains(got, "not recorded") {
		t.Errorf("a hold without a recorded cause: owed %q, want it kept", got)
	}
	f.FailNext("POST /v1/customer-alerts/get", 1)
	if _, err := a.Owed(ctx, "tenant-acme", cus, []string{alert}); err == nil {
		t.Fatal("a failed read must be an error, so the webhook is retried")
	}
}
