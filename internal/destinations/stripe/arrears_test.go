package stripe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Before a payment lowers a billing hold it did not settle itself, Stripe
// is read again: the tenant's subscriptions (by vBilling's metadata) and
// the subscription that caused the hold, past_due, unpaid or incomplete,
// still hold it; so does a subscription that caused it and ended with its
// last invoice unpaid, and a one-off invoice that is still open.
func TestOwedReadsSubscriptionsAndInvoicesAgain(t *testing.T) {
	f := newFakeStripe(t, func() time.Time { return now })
	a := newTestAdapter(t, f, "")
	ctx := context.Background()
	sub := func(id, status, tenant, latest string) {
		s := &Subscription{ID: id, Status: status, Customer: "cus_acme", Metadata: map[string]string{}, LatestInvoice: ObjectID(latest)}
		if tenant != "" {
			s.Metadata[metaTenant] = tenant
		}
		f.subs[id] = s
	}
	invoice := func(id, status string) { f.invoices[id] = &Invoice{ID: id, Customer: "cus_acme", Status: status} }
	owed := func(causes ...string) string {
		t.Helper()
		got, err := a.Owed(ctx, "acme", "cus_acme", causes)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(got, ",")
	}

	sub("sub_metered", "unpaid", "", "in_large") // the portal's, without vBilling's metadata
	sub("sub_vb", "active", "acme", "in_vb")
	sub("sub_neighbor", "past_due", "globex", "") // another tenant's, same customer
	if got := owed("subscription:sub_metered"); got != "subscription:sub_metered" {
		t.Fatalf("an unpaid subscription that caused the hold: owed %q", got)
	}
	f.subs["sub_metered"].Status = "active"
	if got := owed("subscription:sub_metered"); got != "" {
		t.Fatalf("active again: owed %q, want nothing", got)
	}
	f.subs["sub_vb"].Status = "past_due"
	if got := owed("subscription:sub_metered"); got != "subscription:sub_vb" {
		t.Fatalf("another subscription of the tenant past due: owed %q", got)
	}
	if got := owed(); got != "subscription:sub_vb" {
		t.Fatalf("a hold without a recorded cause: owed %q", got)
	}
	f.subs["sub_vb"].Status = "active"

	// Ended: its last invoice says whether it is still owed.
	sub("sub_gone", "canceled", "", "in_last")
	invoice("in_last", "open")
	if got := owed("subscription:sub_gone"); got != "subscription:sub_gone" {
		t.Fatalf("canceled with its last invoice open: owed %q", got)
	}
	invoice("in_last", "void")
	if got := owed("subscription:sub_gone"); got != "" {
		t.Fatalf("canceled with its last invoice void: owed %q", got)
	}

	invoice("in_oneoff", "open")
	invoice("in_written_off", "uncollectible")
	invoice("in_paid", "paid")
	if got := owed("invoice:in_oneoff", "invoice:in_written_off", "invoice:in_paid", "invoice:in_missing"); got !=
		"invoice:in_missing,invoice:in_oneoff,invoice:in_written_off" {
		t.Fatalf("one-off invoices: owed %q", got)
	}
	if got := owed("subscription:sub_elsewhere", "alert:alrt_1", "subscription:"); got != "alert:alrt_1,subscription:,subscription:sub_elsewhere" {
		t.Fatalf("causes Stripe cannot confirm: owed %q", got)
	}
	f.failNext["GET /v1/subscriptions"] = 1
	if _, err := a.Owed(ctx, "acme", "cus_acme", []string{"subscription:sub_metered"}); err == nil {
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
