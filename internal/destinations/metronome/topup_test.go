package metronome_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// walletRig is a portal-owned Metronome account (METRONOME_PROVISION off)
// with one customer, its identity and compute aliases and a prepaid
// contract, and, once started, vBilling's enforcer behind a Metronome
// webhook endpoint.
type walletRig struct {
	f        *metronometest.Server
	a        *metronome.Adapter
	enf      *enforcement.Enforcer
	url      string
	cus      string
	contract string
}

const (
	rigSecret = "whsec_topup"
	rigTenant = "tenant-acme"
)

func newWalletRig(t *testing.T) *walletRig {
	t.Helper()
	f := metronometest.New(t)
	cus, err := f.CreateCustomer("Acme AI", "org_1f2e", rigTenant)
	if err != nil {
		t.Fatal(err)
	}
	card := f.AddRateCard("AI Cloud USD")
	contract := f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: time.Now().AddDate(0, -1, 0), PrepaidCommit: 1000})
	cfg := &config.Config{Adapters: []string{"metronome"}, MetronomeAPIBase: f.URL, MetronomeAPIToken: metronometest.Token,
		MetronomeWebhookSecret: rigSecret, EnforcementMode: "observe"}
	a := metronome.New(metronome.NewClient(f.URL, metronometest.Token), cfg, kv.Memory(), nil)
	return &walletRig{f: f, a: a, cus: cus, contract: contract}
}

// start runs the enforcer with ENFORCEMENT_RULES rules behind a webhook
// endpoint that answers as vBilling's does: 200, or 500 for an error the
// provider retries.
func (r *walletRig) start(t *testing.T, rules string) {
	t.Helper()
	parsed, err := enforcement.ParseRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	r.enf = enforcement.New(nil, enforcement.Options{Mode: "observe", MetronomeWebhookSecret: rigSecret, Rules: parsed,
		Resolvers: map[string]destinations.CustomerResolver{"metronome": r.a},
		IsTenant:  func(id string) bool { return id == rigTenant }})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		_, err := r.enf.HandleMetronome(req.Context(), body, req.Header.Get("X-Metronome-Date"), req.Header.Get("Metronome-Webhook-Signature"))
		if err != nil && !errors.Is(err, enforcement.ErrIgnored) && !errors.Is(err, enforcement.ErrDuplicate) && !errors.Is(err, enforcement.ErrUnknownCustomer) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
}

func (r *walletRig) send(t *testing.T, n metronometest.Notification) int {
	t.Helper()
	status, body, err := r.f.SendWebhook(r.url, rigSecret, n)
	if err != nil {
		t.Fatalf("%s: %v", n.Type, err)
	}
	if status != http.StatusOK && status != http.StatusInternalServerError {
		t.Fatalf("%s: %d %s", n.Type, status, body)
	}
	return status
}

func (r *walletRig) state(t *testing.T, want enforcement.State, why string) {
	t.Helper()
	if got := r.enf.StateOf(rigTenant); got.State != want {
		t.Fatalf("%s: tenant is %s (%s via %s %s), want %s", why, got.State, got.Reason, got.Source, got.EventType, want)
	}
}

// A wallet Finance refills by adding a commit or credit to the contract
// sends no payment webhook: the pause its zero-balance notification raised
// lifts once Metronome shows the notification clear, at the next recheck.
// Holds a failed payment or an operator raised are not lifted that way.
func TestWalletRefilledWithoutAPaymentResumes(t *testing.T) {
	ctx := context.Background()
	r := newWalletRig(t)
	r.start(t, "alerts.low_remaining_commit_balance_reached=paused")

	empty := r.f.Alert("alerts.low_remaining_commit_balance_reached", r.cus, "Wallet empty", 0, 100)
	r.send(t, empty)
	r.state(t, enforcement.Paused, "the zero-balance notification")

	r.enf.RecheckHolds(ctx)
	r.state(t, enforcement.Paused, "a recheck while the notification is in alarm")

	// Finance adds a commit: Metronome evaluates the notification again and
	// it is ok, with no payment_gate webhook.
	r.f.SetAlertStatus(empty.Properties["alert_id"].(string), "ok")
	r.enf.RecheckHolds(ctx)
	r.state(t, enforcement.Active, "a recheck once the wallet is refilled")
	if got := r.enf.StateOf(rigTenant); got.Source != "metronome" || got.EventType != enforcement.RecheckEventType {
		t.Fatalf("lifted by %s %s, want the recheck", got.Source, got.EventType)
	}

	// A failed payment raises a hold that only a payment lowers.
	r.send(t, r.f.PaymentStatus(r.cus, r.contract, "failed"))
	r.state(t, enforcement.Delinquent, "a failed payment")
	r.enf.RecheckHolds(ctx)
	r.state(t, enforcement.Delinquent, "a recheck after a failed payment")
	r.send(t, r.f.PaymentStatus(r.cus, r.contract, "paid"))
	r.state(t, enforcement.Active, "the next successful payment")

	// An operator's hold (support, abuse) is never lifted by the wallet.
	if err := r.enf.SetHold(ctx, rigTenant, enforcement.HoldOperator, enforcement.Suspended, "abuse report", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	again := r.f.Alert("alerts.low_remaining_commit_balance_reached", r.cus, "Wallet empty", 0, 100)
	r.send(t, again)
	r.state(t, enforcement.Paused, "an empty wallet on top of the operator's hold")
	r.f.SetAlertStatus(again.Properties["alert_id"].(string), "ok")
	r.enf.RecheckHolds(ctx)
	r.state(t, enforcement.Suspended, "the refilled wallet with the operator's hold still set")
	if h, ok := r.enf.StateOf(rigTenant).Holds[enforcement.HoldBilling]; ok {
		t.Fatalf("billing hold still set after the refill: %+v", h)
	}
}

// A low-balance warning and the zero-balance pause are both
// alerts.low_remaining_commit_balance_reached. A rule on the warning's name
// or ID keeps it from pausing, and from keeping a pause once the wallet is
// above zero again.
func TestAWarningNotificationNeverPauses(t *testing.T) {
	for _, byID := range []bool{false, true} {
		name := "by name"
		if byID {
			name = "by ID"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r := newWalletRig(t)
			warn := r.f.Alert("alerts.low_remaining_commit_balance_reached", r.cus, "Wallet below $50", 4000, 5000)
			if byID {
				r.start(t, "alerts.low_remaining_commit_balance_reached=paused,alert:"+warn.Properties["alert_id"].(string)+"=warning")
			} else {
				r.start(t, "alerts.low_remaining_commit_balance_reached=paused,alert:Wallet below $50=warning")
			}
			r.send(t, warn)
			r.state(t, enforcement.Warning, "the low-balance warning")
			// The recheck lifts what a notification paused, not a warning:
			// it stays while the warning is in alarm.
			r.enf.RecheckHolds(ctx)
			r.state(t, enforcement.Warning, "a recheck while the warning is in alarm")

			empty := r.f.Alert("alerts.low_remaining_commit_balance_reached", r.cus, "Wallet empty", 0, 100)
			r.send(t, empty)
			r.state(t, enforcement.Paused, "the zero-balance notification")

			// A top-up clears the zero-balance notification; the wallet is
			// still under $50, so the warning stays in alarm.
			r.f.SetAlertStatus(empty.Properties["alert_id"].(string), "ok")
			r.send(t, r.f.PaymentStatus(r.cus, r.contract, "paid"))
			r.state(t, enforcement.Active, "a top-up with only the warning in alarm")

			// The same through the recheck, for a refill without a payment.
			empty = r.f.Alert("alerts.low_remaining_commit_balance_reached", r.cus, "Wallet empty", 0, 100)
			r.send(t, empty)
			r.state(t, enforcement.Paused, "the wallet empty again")
			r.f.SetAlertStatus(empty.Properties["alert_id"].(string), "ok")
			r.enf.RecheckHolds(ctx)
			r.state(t, enforcement.Active, "a recheck with only the warning in alarm")
		})
	}
}

func TestParseRulesNamesNotifications(t *testing.T) {
	rules, err := enforcement.ParseRules("alerts.low_remaining_commit_balance_reached=paused, alert:Balance = low=warning ,alert:5c1f9f2e-0d7b-4a51-9a0c-0f4f1b2b3c4d=suspended")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]enforcement.State{
		"alerts.low_remaining_commit_balance_reached": enforcement.Paused,
		"alert:Balance = low":                         enforcement.Warning,
		"alert:5c1f9f2e-0d7b-4a51-9a0c-0f4f1b2b3c4d":  enforcement.Suspended,
	}
	for k, v := range want {
		if rules[k] != v {
			t.Errorf("rule %q = %q, want %q (all: %v)", k, rules[k], v, rules)
		}
	}
	for _, bad := range []string{"alert:=paused", "alerts.x=stopped", "alerts.x"} {
		if _, err := enforcement.ParseRules(bad); err == nil {
			t.Errorf("ParseRules(%q) accepted", bad)
		}
	}
}

// Reconciliation names a tenant no Metronome customer holds yet instead of
// reporting it as a bare mismatch: Metronome keeps its usage under the alias
// and counts it for the customer that gets it.
func TestReconcileNamesTenantsWithoutACustomer(t *testing.T) {
	r := newWalletRig(t)
	day := time.Now().UTC().Truncate(24 * time.Hour)
	totals, err := r.a.RecordedTotals(context.Background(), destinations.TotalsQuery{From: day, To: day.AddDate(0, 0, 1),
		Tenants: []usage.Tenant{{ID: rigTenant}, {ID: "tenant-signed-up-later"}}, Metrics: []string{usage.MetricInstanceHours}})
	if err != nil {
		t.Fatal(err)
	}
	var note string
	for _, tot := range totals {
		if tot.Tenant == "tenant-signed-up-later" && tot.Metric == "" {
			note = tot.Note
		}
		if tot.Tenant == rigTenant && tot.Note != "" {
			t.Fatalf("a tenant its customer holds got a note: %+v", tot)
		}
	}
	if !strings.Contains(note, `no Metronome customer has the ingest alias "tenant-signed-up-later"`) {
		t.Fatalf("totals %+v: want a note for the tenant no customer holds", totals)
	}
}
