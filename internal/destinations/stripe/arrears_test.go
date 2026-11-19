package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Before a payment clears a billing hold, Stripe is read again for the
// customer's whole standing: any subscription past_due, unpaid or
// incomplete (whoever created it), any open invoice past due or whose
// payment failed, and any uncollectible invoice keeps the hold. An open
// invoice not due yet and never charged does not, nor do paid, void and
// draft ones. Every page is read; the customer can be the tenant's own.
func TestOwedReadsTheCustomersWholeStanding(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	a.now = func() time.Time { return now }
	ctx := context.Background()
	sub := func(id, status string) {
		f.subs[id] = &Subscription{ID: id, Status: status, Customer: "cus_acme", Metadata: map[string]string{}}
	}
	invoice := func(id, status string, attempts int, due time.Time) {
		inv := &Invoice{ID: id, Customer: "cus_acme", Status: status, AttemptCount: &attempts}
		if !due.IsZero() {
			d := due.Unix()
			inv.DueDate = &d
		}
		f.invoices[id] = inv
	}
	owed := func(customer string) string {
		t.Helper()
		got, err := a.Owed(ctx, "acme", customer, nil)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	sub("sub_portal", "unpaid") // created outside vBilling, without its metadata
	sub("sub_vb", "active")
	sub("sub_old", "canceled")
	if got := owed("cus_acme"); got != "subscription sub_portal unpaid" {
		t.Fatalf("a subscription in arrears: owed %q", got)
	}
	f.subs["sub_portal"].Status = "active"
	if got := owed("cus_acme"); got != "" {
		t.Fatalf("every subscription active or ended, no invoice: owed %q, want nothing", got)
	}

	invoice("in_failed", "open", 1, time.Time{})                                                // charged automatically, declined
	invoice("in_overdue", "open", 0, now.Add(-24*time.Hour))                                    // sent, past its due date
	invoice("in_written_off", "uncollectible", 4, time.Time{})                                  // written off unpaid
	invoice("in_new", "open", 0, time.Time{})                                                   // finalized, first charge pending
	invoice("in_not_due", "open", 0, now.Add(7*24*time.Hour))                                   // sent, due next week
	invoice("in_paid", "paid", 2, time.Time{})                                                  // paid after a failure
	invoice("in_void", "void", 1, time.Time{})                                                  // voided
	f.invoices["in_unknown"] = &Invoice{ID: "in_unknown", Customer: "cus_acme", Status: "open"} // attempts not said
	f.invoices["in_globex"] = &Invoice{ID: "in_globex", Customer: "cus_globex", Status: "uncollectible"}
	want := "invoice in_failed open, payment failed (attempt 1),invoice in_overdue open, past due," +
		"invoice in_unknown open, its payment attempts unknown,invoice in_written_off uncollectible"
	if got := owed("cus_acme"); got != want {
		t.Fatalf("invoices: owed %q, want %q", got, want)
	}

	// Paged: more invoices than one page holds are all read.
	for _, id := range []string{"in_failed", "in_overdue", "in_written_off", "in_unknown"} {
		delete(f.invoices, id)
	}
	for i := 0; i < 150; i++ {
		invoice(fmt.Sprintf("in_%03d", i), "open", 0, time.Time{})
	}
	invoice("in_zzz", "open", 3, time.Time{}) // after the first page
	if got := owed("cus_acme"); got != "invoice in_zzz open, payment failed (attempt 3)" {
		t.Fatalf("an invoice past the first page: owed %q", got)
	}

	// The tenant's own customer, when the webhook came from elsewhere.
	if got := owed(""); got != "" {
		t.Fatalf("a tenant vBilling never mapped to a customer: owed %q, want nothing", got)
	}
	a.remember("acme", "cus_acme")
	if got := owed(""); got != "invoice in_zzz open, payment failed (attempt 3)" {
		t.Fatalf("the tenant's own customer: owed %q", got)
	}

	f.failNext["GET /v1/invoices"] = 1
	if _, err := a.Owed(ctx, "acme", "cus_acme", nil); err == nil {
		t.Fatal("a failed read must be an error, so the webhook is retried")
	}
}

// Subscriptions list latest_invoice as an ID, or as the invoice itself when
// expanded.
func TestObjectIDReadsAnIDOrAnExpandedObject(t *testing.T) {
	for raw, want := range map[string]string{`"in_1"`: "in_1", `{"id":"in_2","object":"invoice"}`: "in_2", `null`: ""} {
		var s struct {
			Latest ObjectID `json:"latest_invoice"`
		}
		if err := json.Unmarshal([]byte(`{"latest_invoice":`+raw+`}`), &s); err != nil || string(s.Latest) != want {
			t.Errorf("%s: %q, %v; want %q", raw, s.Latest, err, want)
		}
	}
}
