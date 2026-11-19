package metronome_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/api"
	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations/metronome/metronometest"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestPrepaidLoopOnAnAccountThePortalOwns runs the loop of an AI Cloud whose
// portal and Finance own Metronome, with METRONOME_PROVISION off, against the
// fake Metronome:
//
//  1. the portal creates a customer with an identity alias and a compute
//     alias (the tenant ID), and a contract with a prepaid commit;
//  2. vBilling delivers the ledger's usage under the compute alias;
//  3. a commit low-balance alert pauses the tenant through an
//     ENFORCEMENT_RULES rule;
//  4. a payment-gate "paid" event (a top-up) resumes it;
//  5. reconciliation against /v1/usage matches the ledger.
//
// Webhooks go through vBilling's HTTP handler and the enforcer, which
// resolves the customer with TenantForCustomer (the compute alias, not the
// first one). The enforcer's own alias handling (AliasResolver) is not on
// this branch; CustomerAliases is checked at the adapter boundary.
func TestPrepaidLoopOnAnAccountThePortalOwns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const secret, apiToken, tenant = "whsec_loop", "api-token", "tenant-acme"
	f := metronometest.New(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)

	// Finance's catalog as configuration: the definitions vBilling prints,
	// a product on node-hours priced per profile, and a rate card.
	var nodeHours string
	for _, def := range usage.Billable() {
		b, _ := json.Marshal(metronome.BillableMetricFor(def))
		if id := f.AddBillableMetric(b); def.Code == usage.MetricPrivateNodeHours {
			nodeHours = id
		}
	}
	product := f.AddProduct("Compute Instance Hours", nodeHours, "sku")
	card := f.AddRateCard("AI Cloud AUD")
	f.AddRate(metronometest.Rate{RateCardID: card, ProductID: product, Price: 410, PricingGroupValues: map[string]string{"sku": "gpu-h100-8"}})

	// 1. Signup.
	cus, err := f.CreateCustomer("Acme AI", "org_1f2e", tenant)
	if err != nil {
		t.Fatal(err)
	}
	contract := f.AddContract(metronometest.Contract{CustomerID: cus, RateCardID: card, StartingAt: day.AddDate(0, -1, 0), PrepaidCommit: 5000})

	// vBilling, METRONOME_PROVISION off.
	cfg := &config.Config{Adapters: []string{"metronome"}, MetronomeAPIBase: f.URL, MetronomeAPIToken: metronometest.Token,
		MetronomeWebhookSecret: secret, APIToken: apiToken, EnforcementMode: "observe"}
	a := metronome.New(metronome.NewClient(f.URL, metronometest.Token), cfg, kv.Memory(), nil)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	tenants := pipeline.NewTenantRegistry()
	tenants.Upsert(usage.Tenant{ID: tenant, DisplayName: "Acme AI"})

	// 2. An hour of a private node, committed window by window, delivered.
	var sample usage.Event
	for m := 0; m < 60; m++ {
		ws := day.Add(10*time.Hour + time.Duration(m)*time.Minute)
		ev := usage.Event{Tenant: tenant, Metric: usage.MetricPrivateNodeHours, Quantity: 1.0 / 60, SKU: "gpu-h100-8", Region: "ap-southeast-2",
			ResourceID: "gpu-node-1", WindowStart: ws, WindowEnd: ws.Add(time.Minute), Source: "collector", RecordedAt: time.Now(),
			Dimensions: map[string]string{usage.DimTenantCluster: "acme-train", usage.DimCapacityType: usage.CapacityReserved,
				usage.DimBillingMode: "private_node", usage.DimVCPUs: "208", usage.DimMemoryGiB: "2014.8", usage.DimGPUCount: "8", usage.DimPublicIP: "true"}}
		ev.Finalize()
		if err := sp.AppendWindow(spool.SourceCollector, ev.WindowEnd, []usage.Event{ev}, ""); err != nil {
			t.Fatal(err)
		}
		sample = ev
	}
	if err := sp.SetCursor("metronome", 0); err != nil { // a new destination starts at the ledger's end
		t.Fatal(err)
	}
	disp := pipeline.New(sp, []destinations.Destination{a}, pipeline.Options{Tenants: tenants, IdlePoll: 10 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		Bootstrap: func(ctx context.Context, d destinations.Destination) error { return d.Bootstrap(ctx, usage.Catalog()) }}, nil)
	done := make(chan struct{})
	go func() { defer close(done); disp.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(10 * time.Second); len(f.Events()) < 60; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d of 60 events", len(f.Events()))
		}
	}
	for _, e := range f.Events() {
		if e.CustomerID != tenant || e.Customer != cus {
			t.Fatalf("event %s sent as %q, attributed to %q: want the compute alias on the portal's customer", e.TransactionID, e.CustomerID, e.Customer)
		}
		if e.Properties["public_ip"] != "true" || e.Properties["vcpus"] != "208" || e.Properties["gpu_count"] != "8" || e.Properties["memory_gib"] != "2014.8" {
			t.Fatalf("instance attributes missing: %v", e.Properties)
		}
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("vBilling changed the portal's Metronome: %v", w)
	}
	cov, err := a.CheckPrices(ctx, []destinations.PriceSeries{destinations.SeriesOf(a, a.PricePlan(usage.Tenant{ID: tenant}), &sample)})
	if err != nil || cov[0].Status != destinations.PriceCovered || !strings.Contains(cov[0].Detail, `"AI Cloud AUD"`) {
		t.Fatalf("price coverage from the customer's contract: %+v %v", cov, err)
	}
	if aliases, err := a.CustomerAliases(ctx, cus); err != nil || strings.Join(aliases, ",") != "org_1f2e,"+tenant {
		t.Fatalf("aliases %v %v", aliases, err)
	}

	// 3 and 4 through vBilling's webhook handler.
	rules, err := enforcement.ParseRules("alerts.low_remaining_commit_balance_reached=paused,alerts.low_remaining_contract_credit_and_commit_balance_reached=paused")
	if err != nil {
		t.Fatal(err)
	}
	enf := enforcement.New(nil, enforcement.Options{Mode: "observe", MetronomeWebhookSecret: secret, Rules: rules,
		Resolvers: map[string]destinations.CustomerResolver{"metronome": a}})
	srv := httptest.NewServer(api.New(cfg, sp, disp, tenants, nil, enf, telemetry.New(), "test").Handler())
	defer srv.Close()
	deliver := func(n metronometest.Notification, want enforcement.State) {
		t.Helper()
		status, body, err := f.SendWebhook(srv.URL+"/webhooks/metronome", secret, n)
		if err != nil || status != http.StatusOK {
			t.Fatalf("%s: %d %s %v", n.Type, status, body, err)
		}
		var res struct {
			Applied bool   `json:"applied"`
			Tenant  string `json:"tenant"`
			State   string `json:"state"`
		}
		json.Unmarshal(body, &res)
		if !res.Applied || res.Tenant != tenant || enf.StateOf(tenant).State != want {
			t.Fatalf("%s: %s, tenant state %s, want %s", n.Type, body, enf.StateOf(tenant).State, want)
		}
	}
	deliver(f.Alert("alerts.low_remaining_commit_balance_reached", cus, "Prepaid balance low", 12.5, 50), enforcement.Paused)
	deliver(f.PaymentStatus(cus, contract, "paid"), enforcement.Active)

	// 5. Reconciliation of the closed day.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/reconcile?destination=metronome&from="+day.Format("2006-01-02")+
		"&to="+day.AddDate(0, 0, 1).Format("2006-01-02"), nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var rec struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Rows  []struct {
				Tenant   string  `json:"tenant"`
				Metric   string  `json:"metric"`
				Expected float64 `json:"expected"`
				Recorded float64 `json:"recorded"`
				Match    bool    `json:"match"`
			} `json:"rows"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &rec); err != nil || len(rec.Results) != 1 {
		t.Fatalf("reconcile: %s", body)
	}
	r := rec.Results[0]
	if !r.OK || len(r.Rows) != 1 || r.Rows[0].Tenant != tenant || r.Rows[0].Metric != usage.MetricPrivateNodeHours ||
		!r.Rows[0].Match || r.Rows[0].Recorded != r.Rows[0].Expected || r.Rows[0].Expected < 0.99 {
		t.Fatalf("reconcile: %s", body)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("vBilling changed the portal's Metronome: %v", w)
	}
}
