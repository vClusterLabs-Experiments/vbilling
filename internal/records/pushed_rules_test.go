package records

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// rec is a native record, as JSON and as the raw object a pull hands over.
func rec(t *testing.T, fields map[string]any) (string, map[string]any) {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	return string(b), obj
}

// A record under a metric vBilling measures itself is refused,
// pushed or pulled, and says why; the metrics of records formats and those
// an operator declared keep working.
func TestRecordsUnderMeasuredMetricsAreRefused(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second).Format(time.RFC3339Nano)
	measured := map[string]bool{}
	for _, def := range usage.Catalog() {
		if def.Source == usage.SourceIngest || def.Source == usage.SourceRecords {
			continue
		}
		measured[def.Code] = true
		body, obj := rec(t, map[string]any{"id": "r-" + def.Code, "tenant": "acme", "time": at, "usage": map[string]any{def.Code: 3, "tokens_in": 5}})
		res := r.post(t, Native, body)
		if res.Accepted != 0 || res.RejectedCount != 1 || res.Rejected[0].Reason != ReasonReservedMetric ||
			!strings.Contains(res.Rejected[0].Error, def.Code+" is measured by vBilling itself") {
			t.Errorf("a record under %s: %+v, want it refused as a metric vBilling measures", def.Code, res)
		}
		if pres, err := r.in.ingestPulled(Native, "gw", "", []map[string]any{obj}, nil); err != nil || pres.Accepted != 0 || pres.RejectedCount != 1 {
			t.Errorf("a pulled row under %s: %+v %v, want it refused", def.Code, pres, err)
		}
	}
	if !measured[usage.MetricGPUHours] || !measured[usage.MetricPrivateNodeHours] || !measured[usage.MetricNetworkEgressGB] {
		t.Fatalf("the catalog does not mark GPU-hours, node-hours and egress as measured: %v", measured)
	}
	if st := r.records.Stats(); st.Committed != 0 {
		t.Fatalf("records under measured metrics were stored: %+v", st)
	}
	body, _ := rec(t, map[string]any{"id": "ok", "tenant": "acme", "time": at, "usage": map[string]any{"tokens_in": 5, "inference_output_tokens": 7}})
	if res := r.post(t, Native, body); res.Accepted != 1 || res.UsageRows != 2 {
		t.Fatalf("a record under an operator's metric and a format's metric: %+v", res)
	}
}

// A pushed record that names a cluster vBilling does not know for
// its tenant is refused. A pulled row was served, so it keeps its usage,
// billed to the tenant without a cluster, and its rows keep the name the
// gateway logged.
func TestPulledRowsNamingAnUnknownClusterKeepTheirUsage(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	at := t0.Add(-30 * time.Second)
	// Without any knowledge of clusters, a record cannot name one.
	body, _ := rec(t, map[string]any{"id": "r-0", "tenant": "acme", "time": at.Format(time.RFC3339Nano), "usage": map[string]any{"tokens_in": 1}, "tenant_cluster": "vcluster-acme-train"})
	if res := r.post(t, Native, body); res.Accepted != 0 || res.Rejected[0].Reason != ReasonCluster {
		t.Fatalf("a record naming a cluster without knowledge of clusters: %+v, want it refused", res)
	}
	r.in.UseClusters(safeguard.NewClusters(r.ledger, safeguard.ClusterOptions{Now: r.clk.now, LiveFor: -1, Live: func() ([]safeguard.LiveCluster, bool) {
		return []safeguard.LiveCluster{{ID: "vcluster-acme-train", Tenant: "acme", Created: t0.Add(-time.Hour)}}, true
	}}))
	fields := func(id, cluster string) map[string]any {
		return map[string]any{"id": id, "tenant": "acme", "time": at.Format(time.RFC3339Nano), "usage": map[string]any{"tokens_in": 4, "tokens_out": 2}, "tenant_cluster": cluster}
	}
	body, _ = rec(t, fields("r-1", "vcluster-acme-train"))
	if res := r.post(t, Native, body); res.Accepted != 1 {
		t.Fatalf("a record naming a cluster of its tenant: %+v", res)
	}
	body, ghost := rec(t, fields("r-2", "vcluster-acme-ghost"))
	if res := r.post(t, Native, body); res.Accepted != 0 || res.Rejected[0].Reason != ReasonCluster || !strings.Contains(res.Rejected[0].Error, "is not a cluster vBilling knows for tenant") {
		t.Fatalf("a pushed record naming an unknown cluster: %+v, want it refused", res)
	}
	pres, err := r.in.ingestPulled(Native, Native, "", []map[string]any{ghost}, nil)
	if err != nil || pres.Accepted != 1 || pres.RejectedCount != 0 || len(pres.Warnings) != 1 {
		t.Fatalf("the same record pulled from the gateway's log: %+v %v, want it stored with a warning", pres, err)
	}
	clusters := map[string]string{}
	if err := r.records.ScanRecords(t0.Add(-time.Hour), t0.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		id, _ := ev.Properties["request_id"].(string)
		clusters[id] = ev.Dim(usage.DimTenantCluster)
		if id == "r-2" && ev.Properties[propUnknownCluster] != "vcluster-acme-ghost" {
			t.Errorf("the pulled row does not keep the name the gateway logged: %+v", ev.Properties)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := clusters["r-2"]; !ok || got != "" || clusters["r-1"] != "vcluster-acme-train" {
		t.Fatalf("stored clusters by request %v: want r-1 under its cluster and r-2 under none", clusters)
	}
	// The aggregated usage bills the tenant, and never names the cluster.
	r.clk.set(t0.Add(2 * time.Minute))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range r.ledgerEvents(t) {
		if ev.Tenant != "acme" || ev.Dim(usage.DimTenantCluster) == "vcluster-acme-ghost" {
			t.Errorf("billed %+v", ev)
		}
	}
	if got := total(r.ledgerEvents(t), "tokens_in"); got != 8 {
		t.Fatalf("billed %v tokens_in, want both requests (8)", got)
	}
}

// While vBilling cannot check a cluster yet, a push that names one stores
// nothing and asks for a retry.
func TestRecordsWaitWhileAClusterCannotBeChecked(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	settled := false
	r.in.UseClusters(safeguard.NewClusters(r.ledger, safeguard.ClusterOptions{Now: r.clk.now, LiveFor: -1, Live: func() ([]safeguard.LiveCluster, bool) {
		if !settled {
			return nil, false
		}
		return []safeguard.LiveCluster{{ID: "vcluster-acme-train", Tenant: "acme"}}, true
	}}))
	at := t0.Add(-30 * time.Second).Format(time.RFC3339Nano)
	one, _ := rec(t, map[string]any{"id": "r-1", "tenant": "acme", "time": at, "usage": map[string]any{"tokens_in": 1}})
	two, _ := rec(t, map[string]any{"id": "r-2", "tenant": "acme", "time": at, "usage": map[string]any{"tokens_in": 1}, "tenant_cluster": "vcluster-acme-train"})
	body := "[" + one + "," + two + "]"
	if _, err := r.in.Ingest(Native, Native, "", []byte(body)); !errors.Is(err, safeguard.ErrNotReady) {
		t.Fatalf("a push naming a cluster before vBilling knows its clusters: %v, want ErrNotReady", err)
	}
	if st := r.records.Stats(); st.Committed != 0 {
		t.Fatalf("part of a push that has to wait was stored: %+v", st)
	}
	settled = true
	if res := r.post(t, Native, body); res.Accepted != 2 {
		t.Fatalf("the same push once vBilling knows its clusters: %+v", res)
	}
}

// Records older than the oldest usage a destination accepts are
// refused with the destination's name, and a pull does not read further
// back than that.
func TestRecordsOlderThanADestinationAcceptsAreRefused(t *testing.T) {
	r := newRig(t, Options{MaxAge: 6 * time.Hour})
	defer r.close()
	if got := r.in.maxAge(); got != 6*time.Hour {
		t.Fatalf("max age %s without a destination limit", got)
	}
	r.in.LimitAge(8*time.Hour, "loosebill") // looser than the records' own limit: no effect
	if got := r.in.maxAge(); got != 6*time.Hour {
		t.Fatalf("max age %s with a looser destination limit", got)
	}
	r.in.LimitAge(90*time.Minute, "strictbill")
	if got := r.in.maxAge(); got != 90*time.Minute {
		t.Fatalf("max age %s, want the destination's 90 minutes", got)
	}
	res := r.post(t, Native, "["+native("kept", "acme", t0.Add(-time.Hour), 1, 0)+","+native("lost", "acme", t0.Add(-2*time.Hour), 1, 0)+","+native("older", "acme", t0.Add(-7*time.Hour), 1, 0)+"]")
	if res.Accepted != 1 || res.RejectedCount != 2 {
		t.Fatalf("records an hour, two hours and seven hours old: %+v", res)
	}
	for _, rej := range res.Rejected {
		want := "the oldest usage destination strictbill accepts"
		if rej.ID == "older" {
			want = "the oldest usage vBilling accepts"
		}
		if rej.Reason != ReasonTooOld || !strings.Contains(rej.Error, want) {
			t.Errorf("%s refused with %q (%s), want %q", rej.ID, rej.Error, rej.Reason, want)
		}
	}
}

// Every refused record is counted by reason. A client that never
// had a push accepted is counted under one fixed label, so refusals never
// add a series of a name the sender chose.
func TestRefusedRecordsAreCountedByReasonForEveryClient(t *testing.T) {
	r := &rig{clk: &clock{t: t0}, dir: t.TempDir(), opts: Options{RejectUnknownTenants: true},
		resolve: func(v string) (string, bool) { return v, v == "acme" }}
	r.open(t)
	defer r.close()
	at := t0.Add(-30 * time.Second)
	push := func(client string, known bool, body string) Result {
		t.Helper()
		res, err := r.in.IngestPush(Native, Push{Client: client, Known: known}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, client := range []string{"made-up-1", "made-up-2"} {
		if res := push(client, false, native("r-"+client, "nobody", at, 1, 0)); res.RejectedCount != 1 || res.Rejected[0].Reason != ReasonTenant {
			t.Fatalf("%s: %+v", client, res)
		}
	}
	if res := push("gateway", false, "["+native("ok", "acme", at, 1, 0)+","+native("bad", "nobody", at, 1, 0)+"]"); res.Accepted != 1 || res.RejectedCount != 1 {
		t.Fatalf("gateway: %+v", res)
	}
	var b strings.Builder
	r.in.tel.WriteText(&b)
	text := b.String()
	for _, want := range []string{
		`vbilling_records_received_total{client="_unaccepted"} 2`,
		`vbilling_records_rejected_total{client="_unaccepted",reason="tenant"} 2`,
		`vbilling_records_received_total{client="gateway"} 2`,
		`vbilling_records_accepted_total{client="gateway"} 1`,
		`vbilling_records_rejected_total{client="gateway",reason="tenant"} 1`,
	} {
		if !strings.Contains(text, "\n"+want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(text, "made-up") {
		t.Errorf("a client that never had a push accepted names a series:\n%s", text)
	}
	if UnacceptedClient == "" || clientNameLike(UnacceptedClient) {
		t.Errorf("%q could be a client's own name", UnacceptedClient)
	}
}

// clientNameLike reports whether s could be an X-VBilling-Client value: the
// API takes letters, digits, dots, dashes and underscores, starting with a
// letter or a digit.
func clientNameLike(s string) bool {
	first := s[0]
	return first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9'
}
