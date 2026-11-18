package crosscheck

import (
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

func TestCrossCheckFlagsDivergingTenants(t *testing.T) {
	for _, m := range []string{"cc_gateway_tokens", "cc_engine_tokens"} {
		_ = usage.RegisterCustom(usage.MetricDef{Code: m, Unit: "tokens", Billable: m == "cc_gateway_tokens"})
	}
	checks, err := Parse("cc_gateway_tokens=cc_engine_tokens:2%")
	if err != nil || len(checks) != 1 || checks[0].Tolerance != 0.02 {
		t.Fatalf("parse: %+v %v", checks, err)
	}
	if _, err := Parse("cc_gateway_tokens=nope"); err == nil {
		t.Error("unknown metric must fail")
	}
	hour := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	now := hour.Add(80 * time.Minute)
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	var evs []usage.Event
	add := func(tenant, metric string, minute int, q float64) {
		ws := hour.Add(time.Duration(minute) * time.Minute)
		e := usage.Event{Tenant: tenant, Metric: metric, Quantity: q, WindowStart: ws, WindowEnd: ws.Add(time.Minute), RecordedAt: now}
		e.Finalize()
		evs = append(evs, e)
	}
	for m := 0; m < 60; m++ {
		add("acme", "cc_gateway_tokens", m, 1000)
		add("acme", "cc_engine_tokens", m, 1005) // 0.5% apart: fine
		add("globex", "cc_gateway_tokens", m, 900)
		add("globex", "cc_engine_tokens", m, 1000) // a dropped gateway batch: 10%
	}
	if _, _, err := sp.AppendIngest("test", evs); err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	rows := (&Runner{Spool: sp, Checks: checks, Health: h, Now: func() time.Time { return now }}).Once()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		switch r.Tenant {
		case "acme":
			if !r.OK || r.Primary != 60000 || r.Secondary != 60300 {
				t.Errorf("acme = %+v", r)
			}
		case "globex":
			if r.OK || r.Divergence < 0.099 || r.Divergence > 0.101 {
				t.Errorf("globex = %+v", r)
			}
		}
	}
	problems := h.Problems()
	if len(problems) != 1 || problems[0].Subject != "globex" || problems[0].Status != health.Diverged {
		t.Errorf("health problems = %+v", problems)
	}
}
