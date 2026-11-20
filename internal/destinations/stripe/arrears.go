package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ObjectID is a field Stripe sends as an object's ID, or as the object
// itself when it is expanded.
type ObjectID string

func (o *ObjectID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '{' {
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*o = ObjectID(v.ID)
		return nil
	}
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*o = ""
	if s != nil {
		*o = ObjectID(*s)
	}
	return nil
}

// Invoice is the part of an invoice that says whether it is still owed.
type Invoice struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	// Status: draft, open, paid, uncollectible or void.
	Status string `json:"status"`
	// AttemptCount is how many times Stripe tried to collect it: above
	// zero, a payment failed. Stripe always sends it; nil when an answer
	// did not.
	AttemptCount *int `json:"attempt_count"`
	// DueDate is when an invoice sent for payment is due (Unix seconds);
	// nil for one Stripe charges automatically.
	DueDate *int64 `json:"due_date"`
}

// inArrears are the statuses of a subscription that still owes: Stripe
// could not collect its latest invoice (past_due, unpaid) or its first one
// (incomplete).
func inArrears(status string) bool {
	return status == "past_due" || status == "unpaid" || status == "incomplete"
}

// invoiceOwed says why an invoice is still owed, or "" when it is not. An
// uncollectible invoice was written off unpaid. An open one is owed once
// it is past its due date, or Stripe tried to charge it and the payment
// failed; an open invoice that is neither (finalized, its first charge not
// attempted yet) owes nothing yet. One whose attempts the answer did not
// say is owed: nothing shows it was not tried.
func invoiceOwed(inv Invoice, now time.Time) string {
	switch {
	case inv.Status == "uncollectible":
		return "uncollectible"
	case inv.Status != "open":
		return ""
	case inv.AttemptCount == nil:
		return "open, its payment attempts unknown"
	case *inv.AttemptCount > 0:
		return fmt.Sprintf("open, payment failed (attempt %d)", *inv.AttemptCount)
	case inv.DueDate != nil && *inv.DueDate > 0 && now.After(time.Unix(*inv.DueDate, 0)):
		return "open, past due"
	}
	return ""
}

// Owed reads what a customer owes at Stripe now. A payment does not say
// whether it settled what held a tenant: paying a small invoice sends
// invoice.paid while an older one stays open, and Stripe marks a
// subscription active again once its latest invoice is paid, whatever older
// invoices still owe. So before a payment clears a billing hold, vBilling
// reads the customer's whole standing. It owes nothing only when:
//
//   - none of its subscriptions, of any status and whoever created them,
//     is past_due, unpaid or incomplete;
//   - none of its open invoices is past due or was charged and failed
//     (attempt_count above zero);
//   - none of its invoices is uncollectible.
//
// Every subscription (status=all) and every open and uncollectible invoice
// is read, page by page. With no customerID, the customer is the one
// vBilling bills the tenant as; a tenant it never mapped to a Stripe
// customer owes nothing here. counts is not used: Stripe's usage alerts
// have no standing to read. An error means Stripe could not be read, and
// the webhook that asked is retried.
func (a *Adapter) Owed(ctx context.Context, tenant, customerID string, _ func(...string) bool) ([]string, error) {
	if customerID == "" {
		id, ok := a.state.Get("cus/" + tenant)
		if !ok {
			return nil, nil
		}
		customerID = id
	}
	subs, err := a.c.EverySubscription(ctx, customerID)
	if err != nil {
		return nil, fmt.Errorf("subscriptions of customer %s: %w", customerID, err)
	}
	var owed []string
	for _, s := range subs {
		if inArrears(s.Status) {
			owed = append(owed, "subscription "+s.ID+" "+s.Status)
		}
	}
	now := a.now()
	for _, status := range []string{"open", "uncollectible"} {
		invs, err := a.c.EveryInvoice(ctx, customerID, status)
		if err != nil {
			return nil, fmt.Errorf("%s invoices of customer %s: %w", status, customerID, err)
		}
		for _, inv := range invs {
			if why := invoiceOwed(inv, now); why != "" {
				owed = append(owed, "invoice "+inv.ID+" "+why)
			}
		}
	}
	sort.Strings(owed)
	return owed, nil
}
