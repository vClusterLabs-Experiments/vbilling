package metronome_test

import (
	"context"
	"encoding/json"
	"errors"
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

// One commit funds two tenants. Its low-balance alert pauses both; a
// top-up resumes both, but only once Metronome evaluates the alert ok
// again: a top-up too small to lift the balance over the threshold leaves
// both paused.
func TestTopUpOfASharedWalletResumesEveryTenantOnceTheAlertIsOk(t *testing.T) {
	clock := time.Now().UTC().Truncate(time.Minute)
	f := metronometest.New(t)
	f.SetClock(func() time.Time { return clock })
	cus, err := f.CreateCustomer("Example Corp", "org_9a1b", "tenant-dev", "tenant-prod")
	if err != nil {
		t.Fatal(err)
	}
	contract := f.AddContract(metronometest.Contract{CustomerID: cus, StartingAt: clock.AddDate(0, -1, 0), PrepaidCommit: 1000})
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
	rules, _ := enforcement.ParseRules("alerts.low_remaining_commit_balance_reached=paused")
	enf := enforcement.New(nil, enforcement.Options{Mode: "observe", MetronomeWebhookSecret: secret, Rules: rules,
		Resolvers: map[string]destinations.CustomerResolver{"metronome": a}})
	send := func(n metronometest.Notification) error {
		body, _ := json.Marshal(n)
		date := time.Now().UTC().Format(http.TimeFormat)
		_, err := enf.HandleMetronome(ctx, body, date, metronome.Sign(body, date, secret))
		return err
	}
	both := func(want enforcement.State) {
		t.Helper()
		for _, tenant := range []string{"tenant-dev", "tenant-prod"} {
			if st := enf.StateOf(tenant); st.State != want {
				t.Fatalf("%s is %s (%s), want %s", tenant, st.State, st.Reason, want)
			}
		}
	}
	alert := f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 0, 50)
	if err := send(alert); err != nil {
		t.Fatal(err)
	}
	both(enforcement.Paused)
	small := f.PaymentStatus(cus, contract, "paid")
	if err := send(small); !errors.Is(err, enforcement.ErrNotSettled) {
		t.Fatalf("a top-up that leaves the alert in alarm: %v, want ErrNotSettled", err)
	}
	both(enforcement.Paused)
	f.SetAlertStatus(alert.Properties["alert_id"].(string), "ok")
	if err := send(small); err != nil {
		t.Fatalf("the top-up delivered again once the alert is ok: %v", err)
	}
	both(enforcement.Active)
}
