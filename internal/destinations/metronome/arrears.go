package metronome

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Reading what a customer owes. A payment does not say whether it refilled
// the wallet: a top-up of $1 sends the same payment_gate.payment_status
// "paid" as one that refills it. So before a payment clears a billing hold,
// vBilling reads the customer's threshold notifications again:
//
//   - POST /v1/customer-alerts/list (alerts/get-all-threshold-notifications):
//     every enabled threshold notification of a customer with its
//     real-time status for the customer, customer_status ok, in_alarm or
//     evaluating, paged with next_page.
//
// Metronome keeps no payment-gate state to read: a payment-gated top-up
// whose payment fails voids its invoice and adds no commit, so it leaves
// nothing owed.

// alertNames groups the names Metronome uses for one kind of threshold
// notification: its type in the API and the webhook types it is documented
// with. The contract credit balance notification is listed as
// low_remaining_contract_credit_balance_reached, while Metronome's webhook
// example reads alerts.low_remaining_credit_balance_reached and an older
// one low_credit_balance_reached: a rule on any of the names counts for all.
var alertNames = [][]string{
	{"low_remaining_contract_credit_balance_reached", "low_remaining_credit_balance_reached", "low_credit_balance_reached"},
	{"low_remaining_contract_credit_percentage_reached", "low_remaining_credit_percentage_reached"},
}

// webhookTypes returns the webhook event types a threshold notification of
// a type can arrive as.
func webhookTypes(alertType string) []string {
	names := []string{alertType}
	for _, g := range alertNames {
		if slices.Contains(g, alertType) {
			names = g
			break
		}
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "alerts." + n
	}
	return out
}

// alertRulePrefix starts a rule key that names one threshold notification
// (enforcement.AlertRulePrefix, which this package cannot import).
const alertRulePrefix = "alert:"

// ruleKeys lists a threshold notification's rule keys, most specific
// first: its ID, its name, then each webhook type it can arrive as.
func ruleKeys(al CustomerAlert) []string {
	var keys []string
	if al.Alert.ID != "" {
		keys = append(keys, alertRulePrefix+al.Alert.ID)
	}
	if al.Alert.Name != "" {
		keys = append(keys, alertRulePrefix+al.Alert.Name)
	}
	return append(keys, webhookTypes(al.Alert.Type)...)
}

// Owed reads what a customer owes at Metronome now: each of its threshold
// notifications that counts and is not ok for the customer (in alarm, still
// evaluating, or not evaluated). A notification counts when counts reports
// it, by its rule keys (alert:<ID>, alert:<name>, then its webhook types:
// alerts.<type> and the other names of the low-balance credit types), as
// mapped by a rule to a state above warning: those are what pause or
// suspend a tenant, the prepaid wallet's low-balance commit and credit
// notifications above all, while a warning rule on one notification (a
// low-balance warning above the zero-balance pause) keeps it from counting.
// With no customerID, the customer is the one that holds the tenant ID as
// an ingest alias; a tenant no customer holds owes nothing here. A customer
// Metronome does not know confirms nothing, and is owed. An error means
// Metronome could not be read, and the webhook that asked is retried.
func (a *Adapter) Owed(ctx context.Context, tenant, customerID string, counts func(keys ...string) bool) ([]string, error) {
	if customerID == "" {
		cus, err := a.c.FindCustomerByAlias(ctx, tenant)
		if err != nil {
			return nil, fmt.Errorf("customer of tenant %s: %w", tenant, err)
		}
		if cus == nil {
			return nil, nil
		}
		customerID = cus.ID
	}
	alerts, err := a.c.ListCustomerAlerts(ctx, customerID)
	if IsNotFound(err) {
		return []string{"customer " + customerID + " not found, so nothing confirms its balance"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("threshold notifications of customer %s: %w", customerID, err)
	}
	var owed []string
	for _, al := range alerts {
		status := "not evaluated"
		if al.CustomerStatus != nil {
			status = strings.ReplaceAll(*al.CustomerStatus, "_", " ")
		}
		if status == "ok" || counts == nil || !counts(ruleKeys(al)...) {
			continue
		}
		owed = append(owed, fmt.Sprintf("threshold notification %q (%s) %s", al.Alert.Name, al.Alert.Type, status))
	}
	sort.Strings(owed)
	return owed, nil
}
