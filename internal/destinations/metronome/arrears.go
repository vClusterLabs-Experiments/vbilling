package metronome

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Settling billing holds. A webhook that pauses or suspends a tenant
// (a threshold notification mapped by a rule) names the notification, and
// a payment does not say whether it settled it: a top-up of $1 sends the
// same payment_gate.payment_status "paid" as one that refills the wallet.
// Before a payment lowers such a hold, vBilling reads the notification's
// status for the customer again:
//
//   - POST /v1/customer-alerts/get (alerts/get-a-threshold-notification):
//     "the real-time evaluation status for a specific threshold
//     notification-customer pair", customer_status ok, in_alarm,
//     evaluating, or null once the notification is archived.

// customerAlertRequest is the body of POST /v1/customer-alerts/get.
type customerAlertRequest struct {
	CustomerID string `json:"customer_id"`
	AlertID    string `json:"alert_id"`
}

// GetCustomerAlertStatus returns a threshold notification's status for a
// customer: "ok", "in_alarm", "evaluating", or "" when it is archived.
func (c *Client) GetCustomerAlertStatus(ctx context.Context, customerID, alertID string) (string, error) {
	var resp struct {
		Data struct {
			CustomerStatus *string `json:"customer_status"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/customer-alerts/get", nil, customerAlertRequest{CustomerID: customerID, AlertID: alertID}, &resp); err != nil {
		return "", err
	}
	if resp.Data.CustomerStatus == nil {
		return "", nil
	}
	return *resp.Data.CustomerStatus, nil
}

// Owed reads again what holds a tenant at Metronome. Of the causes of its
// billing hold, a threshold notification ("alert:<id>") still holds it
// unless Metronome evaluates it ok for the customer now: in alarm, still
// evaluating, archived (nothing watches the balance any more) or unknown,
// it holds. Anything else ("payment:<id>", an empty ID) cannot be read
// again here and holds too. A hold whose cause is not recorded (set by an
// operator, or by an older version) names no notification to read, so
// Metronome confirms nothing about it. An error means Metronome could not
// be read; the webhook that asked is then retried.
func (a *Adapter) Owed(ctx context.Context, tenant, customerID string, causes []string) ([]string, error) {
	if len(causes) == 0 {
		return []string{"no threshold notification to read again: the hold's cause is not recorded"}, nil
	}
	var owed []string
	for _, cause := range causes {
		kind, id, _ := strings.Cut(cause, ":")
		if kind != "alert" || id == "" || customerID == "" {
			owed = append(owed, cause)
			continue
		}
		status, err := a.c.GetCustomerAlertStatus(ctx, customerID, id)
		if IsNotFound(err) {
			owed = append(owed, cause) // gone: nothing confirms the balance
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("threshold notification %s of customer %s: %w", id, customerID, err)
		}
		if status != "ok" {
			owed = append(owed, cause)
		}
	}
	return owed, nil
}
