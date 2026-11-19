package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
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
}

// GetInvoice reads an invoice.
func (c *Client) GetInvoice(ctx context.Context, id string) (*Invoice, error) {
	var inv Invoice
	if err := c.do(ctx, http.MethodGet, "/v1/invoices/"+url.PathEscape(id), nil, "", &inv); err != nil {
		return nil, err
	}
	return &inv, nil
}

// inArrears are the statuses of a subscription that still owes: Stripe
// could not collect its latest invoice (past_due, unpaid) or its first one
// (incomplete).
func inArrears(status string) bool {
	return status == "past_due" || status == "unpaid" || status == "incomplete"
}

// stillOwed reports whether an invoice is still owed: finalized and not
// paid (open), or written off by the merchant but never paid
// (uncollectible).
func stillOwed(inv *Invoice) bool { return inv.Status == "open" || inv.Status == "uncollectible" }

// Owed reads again what holds a tenant at Stripe. A payment does not say
// whether it settled what suspended a tenant: paying a small, older
// invoice sends invoice.paid while the subscription it belongs to stays
// unpaid. So before a payment lowers a hold it did not settle itself,
// vBilling asks Stripe:
//
//   - every subscription of the tenant (by vBilling's metadata) and every
//     subscription that caused the hold ("subscription:<id>") that is
//     past_due, unpaid or incomplete still holds it; a subscription that
//     caused the hold and has ended still holds it while its latest invoice
//     is open or uncollectible;
//   - a one-off invoice that caused the hold ("invoice:<id>") holds it
//     while it is open or uncollectible;
//   - anything else (a billing alert, an empty ID) cannot be read again
//     and holds it.
//
// Causes come back by the same key; other subscriptions in arrears come
// back as "subscription:<id>". An error means Stripe could not be read.
func (a *Adapter) Owed(ctx context.Context, tenant, customerID string, causes []string) ([]string, error) {
	owed := map[string]bool{}
	wanted := map[string]bool{} // subscriptions that caused the hold
	var invoices []string
	for _, cause := range causes {
		kind, id, _ := strings.Cut(cause, ":")
		switch {
		case id == "":
			owed[cause] = true
		case kind == "subscription":
			wanted[id] = true
		case kind == "invoice":
			invoices = append(invoices, id)
		default:
			owed[cause] = true
		}
	}
	if customerID == "" {
		return nil, fmt.Errorf("no Stripe customer to read for tenant %s", tenant)
	}
	subs, err := a.c.ListSubscriptions(ctx, customerID)
	if err != nil {
		return nil, fmt.Errorf("subscriptions of customer %s: %w", customerID, err)
	}
	seen := map[string]bool{}
	for _, s := range subs {
		seen[s.ID] = true
		if !wanted[s.ID] && s.Metadata[metaTenant] != tenant {
			continue
		}
		switch {
		case inArrears(s.Status):
			owed["subscription:"+s.ID] = true
		case wanted[s.ID] && (s.Status == "canceled" || s.Status == "incomplete_expired") && s.LatestInvoice != "":
			// Ended, perhaps for non-payment: its last invoice says.
			inv, err := a.c.GetInvoice(ctx, string(s.LatestInvoice))
			if err != nil {
				return nil, fmt.Errorf("latest invoice %s of subscription %s: %w", s.LatestInvoice, s.ID, err)
			}
			if stillOwed(inv) {
				owed["subscription:"+s.ID] = true
			}
		}
	}
	for id := range wanted {
		if !seen[id] {
			owed["subscription:"+id] = true // not this customer's: nothing confirms it was paid
		}
	}
	for _, id := range invoices {
		inv, err := a.c.GetInvoice(ctx, id)
		switch {
		case IsCode(err, "resource_missing"):
			owed["invoice:"+id] = true
		case err != nil:
			return nil, fmt.Errorf("invoice %s: %w", id, err)
		case inv.Customer != "" && inv.Customer != customerID, stillOwed(inv):
			owed["invoice:"+id] = true
		}
	}
	out := make([]string, 0, len(owed))
	for k := range owed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
