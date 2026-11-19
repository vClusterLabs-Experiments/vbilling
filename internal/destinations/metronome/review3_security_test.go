package metronome_test

import (
	"context"
	"encoding/json"
	"net/http"
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

// Third review. One Metronome customer, one prepaid commit, funds two
// tenants of the same company (its portal gave the customer both tenant
// IDs as ingest aliases, next to an identity alias). When the wallet runs
// dry, the commit alert must pause every tenant it funds. The adapter's
// TenantForCustomer maps the customer to the first alias it knows and the
// enforcer applies the signal to that one tenant only: the other keeps
// running on the empty wallet. The docs say a signal applies "to each alias
// that is a tenant vBilling knows ... never to the first alias alone".
func TestReview3EmptySharedWalletPausesEveryTenantItFunds(t *testing.T) {
	clock := time.Now().UTC().Truncate(time.Minute)
	f := metronometest.New(t)
	f.SetClock(func() time.Time { return clock })
	cus, err := f.CreateCustomer("Example Corp", "org_9a1b", "tenant-dev", "tenant-prod")
	if err != nil {
		t.Fatal(err)
	}
	a := metronome.New(metronome.NewClient(f.URL, metronometest.Token), &config.Config{MetronomeStripeCollect: "charge_automatically"}, kv.Memory(), nil)
	ctx := context.Background()
	var evs []usage.Event
	for i, tenant := range []string{"tenant-dev", "tenant-prod"} {
		ws := clock.Add(-time.Duration(i+2) * time.Minute)
		e := usage.Event{Tenant: tenant, Metric: usage.MetricGPUHours, Quantity: 1, WindowStart: ws, WindowEnd: ws.Add(time.Minute),
			Region: "ap-southeast-2", SKU: "NVIDIA-H100", ResourceID: "c-" + tenant}
		e.Finalize()
		evs = append(evs, e)
	}
	if err := a.SendEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}

	const secret = "mtr_secret"
	rules, err := enforcement.ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	if err != nil {
		t.Fatal(err)
	}
	enf := enforcement.New(nil, enforcement.Options{Mode: "observe", MetronomeWebhookSecret: secret, Rules: rules,
		Resolvers: map[string]destinations.CustomerResolver{"metronome": a}})
	body, _ := json.Marshal(f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 0, 50))
	date := time.Now().UTC().Format(http.TimeFormat)
	sig, err := enf.HandleMetronome(ctx, body, date, metronome.Sign(body, date, secret))
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"tenant-dev", "tenant-prod"} {
		if st := enf.StateOf(tenant); st.State != enforcement.Paused {
			t.Errorf("the wallet that funds %s is empty and %s is %s (the alert was applied to %q only)", tenant, tenant, st.State, sig.Tenant)
		}
	}
}
