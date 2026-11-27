package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// GET /api/v1/billing-states shows a grace period an automation can act
// on: each tenant's next escalation (escalation.to and escalation.at),
// the rule that raised an escalated state (escalated_by), and the rules
// themselves. /metrics counts the escalations.
func TestBillingStatesShowTheNextEscalation(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	rules, err := enforcement.ParseEscalations("delinquent:72h:suspended,suspended:7d:paused")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Region: "us-central1", Adapters: []string{"fakebill"}, CollectionInterval: time.Minute,
		APIToken: adminToken, IngestToken: "ingest-secret", EnforcementMode: "observe"}
	tel := telemetry.New()
	enf := enforcement.New(nil, enforcement.Options{Mode: "observe", Telemetry: tel, Escalations: rules, Now: now})
	dest := &recDest{recorded: map[string]float64{}, reject: map[string]bool{}}
	s := New(cfg, sp, pipeline.New(sp, []destinations.Destination{dest}, pipeline.Options{}, nil), pipeline.NewTenantRegistry(), nil, enf, tel, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	type view struct {
		Escalations []struct{ From, After, To string } `json:"escalations"`
		States      []struct {
			Tenant      string    `json:"tenant"`
			State       string    `json:"state"`
			Since       time.Time `json:"since"`
			EscalatedBy string    `json:"escalated_by"`
			Escalation  *struct {
				To   string    `json:"to"`
				At   time.Time `json:"at"`
				Rule string    `json:"rule"`
			} `json:"escalation"`
		} `json:"states"`
	}
	get := func() view {
		t.Helper()
		resp := do(t, "GET", srv.URL+"/api/v1/billing-states", adminToken, nil)
		defer resp.Body.Close()
		var v view
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET /api/v1/billing-states: %d, %v", resp.StatusCode, err)
		}
		if len(v.States) != 1 || v.States[0].Tenant != "acme" {
			t.Fatalf("states %+v", v.States)
		}
		return v
	}

	// A portal sets the billing hold, as Lago or a custom billing platform does.
	resp := do(t, "PUT", srv.URL+"/api/v1/billing-states/acme", adminToken,
		strings.NewReader(`{"state": "delinquent", "hold": "billing", "reason": "invoice 42 overdue"}`))
	resp.Body.Close()
	v := get()
	if len(v.Escalations) != 2 || v.Escalations[0].From != "delinquent" || v.Escalations[0].After != "72h" || v.Escalations[1].After != "7d" {
		t.Fatalf("escalation rules %+v", v.Escalations)
	}
	st := v.States[0]
	if st.State != "delinquent" || st.Escalation == nil || st.Escalation.To != "suspended" || !st.Escalation.At.Equal(clock.Add(72*time.Hour)) ||
		st.Escalation.Rule != "delinquent:72h:suspended" {
		t.Fatalf("delinquent tenant: %+v, escalation %+v", st, st.Escalation)
	}

	mu.Lock()
	clock = clock.Add(72 * time.Hour)
	mu.Unlock()
	enf.Escalate(context.Background())
	st = get().States[0]
	if st.State != "suspended" || st.EscalatedBy != "delinquent:72h:suspended" || !st.Since.Equal(now()) ||
		st.Escalation == nil || st.Escalation.To != "paused" || !st.Escalation.At.Equal(now().Add(7*24*time.Hour)) {
		t.Fatalf("escalated tenant: %+v, escalation %+v", st, st.Escalation)
	}
	_, metrics := (&env{srv: srv, token: adminToken}).get(t, "/metrics")
	if !strings.Contains(metrics, `vbilling_enforcement_escalations_total{from="delinquent",to="suspended"} 1`) ||
		!strings.Contains(metrics, `vbilling_enforcement_escalations_total{from="suspended",to="paused"} 0`) {
		t.Fatalf("/metrics lacks the escalation counters:\n%s", metrics)
	}
}
