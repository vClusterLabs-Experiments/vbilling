package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// discoveredCluster is a cluster discovery lists for a tenant.
func discoveredCluster(tenant, namespace, name string, created time.Time) discovery.TenantCluster {
	return discovery.TenantCluster{Namespace: namespace, Name: name, CreatedAt: created, Ready: true,
		Annotations: map[string]string{discovery.AnnotationPrefix + "tenant": tenant}}
}

// Pushed usage that names a cluster names one of its tenant,
// for a window in which that cluster existed, on both push paths. Late
// usage for a window in which a deleted cluster still existed is accepted
// while the ledger remembers the cluster; a cluster vBilling never saw,
// another tenant's cluster, and a window before or after the cluster's
// time are refused with the reason.
func TestPushedUsageNamesOnlyClustersOfItsTenantWhileTheyExisted(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true, retention: 7 * 24 * time.Hour})
	e.ctrl.mu.Lock()
	e.ctrl.clusters = []discovery.TenantCluster{
		discoveredCluster("acme", "acme", "live", now.Add(-time.Hour)),
		discoveredCluster("globex", "globex", "prod", now.Add(-24*time.Hour)),
	}
	e.ctrl.mu.Unlock()
	// vcluster-acme-gone was metered for ten minutes, until twenty minutes
	// ago, and then deleted: discovery no longer lists it.
	first := now.Add(-30 * time.Minute)
	for i := 0; i < 10; i++ {
		e.meter(t, first.Add(time.Duration(i)*time.Minute), [2]string{"acme", "vcluster-acme-gone"}, [2]string{"globex", "vcluster-globex-prod"})
	}
	last := first.Add(10 * time.Minute) // the end of its last metered window
	const unknown, outside = "is not a cluster vBilling knows for tenant", "vBilling metered this cluster for tenant"
	cases := []struct {
		name, token, tenant, cluster string
		at                           time.Time // window start; zero: five minutes ago
		refused                      string    // part of the refusal; "": accepted
	}{
		{name: "no cluster", tenant: "acme"},
		{name: "a running cluster of the tenant", tenant: "acme", cluster: "vcluster-acme-live"},
		{name: "a running cluster, before it was created", tenant: "acme", cluster: "vcluster-acme-live", at: now.Add(-2 * time.Hour), refused: "was created at"},
		{name: "a cluster that never existed", tenant: "acme", cluster: "vcluster-acme-ghost", refused: unknown},
		{name: "another tenant's running cluster", tenant: "acme", cluster: "vcluster-globex-prod", refused: unknown},
		{name: "another tenant's cluster with a bound token", token: "tok-acme", cluster: "vcluster-globex-prod", refused: unknown},
		{name: "a deleted cluster, late usage for a window it existed in", tenant: "acme", cluster: "vcluster-acme-gone", at: first.Add(3 * time.Minute)},
		{name: "a deleted cluster, the window it was deleted in", tenant: "acme", cluster: "vcluster-acme-gone", at: last},
		{name: "a deleted cluster, a window after it was gone", tenant: "acme", cluster: "vcluster-acme-gone", at: last.Add(time.Minute), refused: outside},
		{name: "a deleted cluster, a window before it existed", tenant: "acme", cluster: "vcluster-acme-gone", at: first.Add(-2 * time.Minute), refused: outside},
		{name: "a deleted cluster named by another tenant", tenant: "globex", cluster: "vcluster-acme-gone", at: first.Add(3 * time.Minute), refused: unknown},
	}
	for i, c := range cases {
		token := c.token
		if token == "" {
			token = "ingest-secret"
		}
		id := fmt.Sprintf("c-%d", i)
		at := c.at
		if at.IsZero() {
			at = now.Add(-5 * time.Minute)
		}
		for path, a := range map[string]pushAnswer{
			"events":  e.events(t, token, pushedEvent{id: id, tenant: c.tenant, cluster: c.cluster, at: at}),
			"records": e.recs(t, token, pushedRecord{id: id, tenant: c.tenant, cluster: c.cluster, at: at.Add(30 * time.Second)}),
		} {
			reason, text := a.rejection(0)
			switch {
			case c.refused == "" && (a.code != http.StatusOK || a.accepted() != 1):
				t.Errorf("%s (%s): %v, want it accepted", c.name, path, a)
			case c.refused != "" && (a.code != http.StatusBadRequest || reason != "cluster" || !strings.Contains(text, c.refused)):
				t.Errorf("%s (%s): %v, want it refused with %q", c.name, path, a, c.refused)
			}
		}
	}
	// A record may name its cluster as a dimension too: the same rule.
	dim := map[string]any{"id": "dim-1", "tenant": "acme", "time": now.Add(-2 * time.Minute).Format(time.RFC3339),
		"usage": map[string]any{"inference_output_tokens": 1}, "dimensions": map[string]string{usage.DimTenantCluster: "vcluster-acme-ghost"}}
	if a := e.post(t, "/api/v1/records", "ingest-secret", dim); a.code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(a.out["rejected"]), unknown) {
		t.Errorf("a record naming an unknown cluster as a dimension: %v, want it refused", a)
	}
	// Nothing pushed is stored under a cluster that was refused.
	allowed := map[string]bool{"": true, "vcluster-acme-live": true, "vcluster-acme-gone": true}
	for _, ev := range append(e.stored(t), e.storedRecords(t)...) {
		if strings.HasPrefix(ev.Source, "ingest:") || strings.HasPrefix(ev.Source, "records:") {
			if c := ev.Dim(usage.DimTenantCluster); !allowed[c] {
				t.Errorf("pushed usage is stored for cluster %q: %+v", c, ev)
			}
		}
	}
}

// While vBilling is starting it cannot tell whether a cluster exists, so
// usage that names one waits (503 with Retry-After, nothing stored) instead
// of being refused, even with a token bound to a tenant. Usage the ledger
// already vouches for is accepted at once.
func TestUsageNamingAClusterWaitsUntilVBillingKnowsItsClusters(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true})
	// vBilling was down: its ledger last metered the cluster twelve minutes
	// ago, and it has not discovered clusters since it started.
	e.meter(t, now.Add(-12*time.Minute), [2]string{"acme", "vcluster-acme-live"})
	e.ctrl.mu.Lock()
	e.ctrl.ready = false
	e.ctrl.clusters = []discovery.TenantCluster{discoveredCluster("acme", "acme", "live", now.Add(-time.Hour))}
	e.ctrl.mu.Unlock()

	if a := e.events(t, "tok-acme", pushedEvent{id: "old", cluster: "vcluster-acme-live", at: now.Add(-12 * time.Minute)}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("usage for a window the ledger metered the cluster in: %v, want it accepted", a)
	}
	before := e.sp.Stats().Committed
	for path, a := range map[string]pushAnswer{
		"events":  e.events(t, "tok-acme", pushedEvent{id: "e-1", cluster: "vcluster-acme-live"}, pushedEvent{id: "e-2"}),
		"records": e.recs(t, "tok-acme", pushedRecord{id: "r-1", cluster: "vcluster-acme-live"}, pushedRecord{id: "r-2"}),
	} {
		if a.code != http.StatusServiceUnavailable || a.header.Get("Retry-After") == "" || !strings.Contains(fmt.Sprint(a.out["error"]), "vcluster-acme-live") {
			t.Errorf("%s naming a cluster before the first discovery: %v, want 503 with Retry-After", path, a)
		}
	}
	if e.sp.Stats().Committed != before || len(e.storedRecords(t)) != 0 {
		t.Fatal("a request that has to wait stored part of its usage")
	}
	e.ctrl.setReady(true)
	if a := e.events(t, "tok-acme", pushedEvent{id: "e-1", cluster: "vcluster-acme-live"}, pushedEvent{id: "e-2"}); a.code != http.StatusOK || a.accepted() != 2 {
		t.Fatalf("events after the first discovery: %v", a)
	}
	if a := e.recs(t, "tok-acme", pushedRecord{id: "r-1", cluster: "vcluster-acme-live"}, pushedRecord{id: "r-2"}); a.code != http.StatusOK || a.accepted() != 2 {
		t.Fatalf("records after the first discovery: %v", a)
	}
}

// Every refusal of the two ingest endpoints is counted. Requests
// refused as a whole count in vbilling_ingest_requests_refused_total by
// endpoint and reason; events and records refused one by one count by
// reason, under the client's name once a push of it was accepted and under
// one fixed label before.
func TestEveryIngestRefusalIsCounted(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true})
	ok := pushedEvent{id: "ok", tenant: "acme"}
	want := func(a pushAnswer, code int, what string) {
		t.Helper()
		if a.code != code {
			t.Fatalf("%s: %v, want HTTP %d", what, a, code)
		}
	}
	for _, path := range []string{"/api/v1/events", "/api/v1/records"} {
		want(e.post(t, path, "", "[]"), http.StatusUnauthorized, path+" without a token")
		want(e.post(t, path, adminToken, "[]"), http.StatusUnauthorized, path+" with the API token")
		want(e.post(t, path, "ingest-secret", "[]", "X-VBilling-Client", "bad name"), http.StatusBadRequest, path+" with a malformed client")
		want(e.post(t, path, "ingest-secret", "not json"), http.StatusBadRequest, path+" with a body that is not JSON")
	}
	want(e.post(t, "/api/v1/events", "ingest-secret", "[]"), http.StatusBadRequest, "no events")
	want(e.post(t, "/api/v1/records?format=nope", "ingest-secret", "{}"), http.StatusBadRequest, "an unknown format")
	want(e.post(t, "/api/v1/events", "ingest-secret", bytes.Repeat([]byte("x"), maxIngestBody+1)), http.StatusRequestEntityTooLarge, "events over the body limit")
	want(e.post(t, "/api/v1/records", "ingest-secret", bytes.Repeat([]byte("x"), maxRecordsBody+1)), http.StatusRequestEntityTooLarge, "records over the body limit")

	// The first push of a client is refused: the refusal that was
	// not counted.
	want(e.recs(t, "ingest-secret", pushedRecord{id: "r-0", tenant: "tenant-phantom"}), http.StatusBadRequest, "a record for an unknown tenant")
	want(e.events(t, "ingest-secret", pushedEvent{id: "e-0", tenant: "tenant-phantom"}), http.StatusBadRequest, "an event for an unknown tenant")

	// One refusal of every kind next to one accepted event or record.
	old, future := now.Add(-35*24*time.Hour), now.Add(time.Hour)
	unsafe := pushedEvent{id: "i", tenant: "acme"}.body()
	unsafe["sku"] = "<b>"
	evs := []map[string]any{ok.body(),
		pushedEvent{id: "t", tenant: "tenant-phantom"}.body(),
		pushedEvent{id: "m", tenant: "acme", metric: usage.MetricGPUHours}.body(),
		pushedEvent{id: "c", tenant: "acme", cluster: "vcluster-acme-ghost"}.body(),
		pushedEvent{id: "u", tenant: "acme", metric: "no_such_metric"}.body(),
		pushedEvent{id: "o", tenant: "acme", at: old}.body(),
		pushedEvent{id: "f", tenant: "acme", at: future}.body(),
		pushedEvent{tenant: "acme"}.body(), pushedEvent{tenant: "acme"}.body(), // no id, one identity: the second is refused
		unsafe}
	if a := e.post(t, "/api/v1/events", "ingest-secret", evs); a.code != http.StatusOK || a.accepted() != 2 || a.out["rejected_count"] != float64(8) {
		t.Fatalf("events: %v, want 2 accepted and 8 refused", a)
	}
	noID := pushedRecord{tenant: "acme"}.body()
	delete(noID, "id")
	recs := []map[string]any{pushedRecord{id: "ok", tenant: "acme"}.body(),
		pushedRecord{id: "t", tenant: "tenant-phantom"}.body(),
		pushedRecord{id: "m", tenant: "acme", metric: usage.MetricGPUHours}.body(),
		pushedRecord{id: "c", tenant: "acme", cluster: "vcluster-acme-ghost"}.body(),
		pushedRecord{id: "u", tenant: "acme", metric: "no_such_metric"}.body(),
		pushedRecord{id: "o", tenant: "acme", at: old}.body(),
		pushedRecord{id: "f", tenant: "acme", at: future}.body(),
		noID}
	if a := e.post(t, "/api/v1/records", "ingest-secret", recs); a.code != http.StatusOK || a.accepted() != 1 || a.out["rejected_count"] != float64(7) {
		t.Fatalf("records: %v, want 1 accepted and 7 refused", a)
	}

	// vBilling starting, a token over its rate, a ledger that cannot be written.
	e.ctrl.setReady(false)
	want(e.events(t, "ingest-secret", ok), http.StatusServiceUnavailable, "events before the first discovery")
	want(e.recs(t, "ingest-secret", pushedRecord{id: "r-9", tenant: "acme"}), http.StatusServiceUnavailable, "records before the first discovery")
	e.ctrl.setReady(true)
	e.server.limiter = newRateLimiter(func() time.Time { return now }) // no refill while the test runs
	for i := 0; i < boundTokenBurst; i++ {
		e.server.limiter.allow(limitKey("tok-globex"), boundTokenRate, boundTokenBurst)
	}
	want(e.events(t, "tok-globex", pushedEvent{id: "x"}), http.StatusTooManyRequests, "events over the token's rate")
	want(e.recs(t, "tok-globex", pushedRecord{id: "x"}), http.StatusTooManyRequests, "records over the token's rate")
	e.sp.Close()
	e.records.Close()
	want(e.events(t, "ingest-secret", pushedEvent{id: "late", tenant: "acme"}), http.StatusServiceUnavailable, "events with the ledger closed")
	want(e.recs(t, "ingest-secret", pushedRecord{id: "late", tenant: "acme"}), http.StatusServiceUnavailable, "records with the ledger closed")

	// Ingest that is not enabled.
	off := setupPush(t, pushOptions{mutate: func(c *config.Config) { c.IngestToken, c.IngestTokens = "", nil }})
	want(off.events(t, "anything", ok), http.StatusForbidden, "events without any ingest token configured")
	plain := setup(t)
	if code, _ := postJSON(t, plain.srv.URL+"/api/v1/records", "ingest-secret", "{}"); code != http.StatusNotFound {
		t.Fatalf("records where records ingest is not enabled: %d, want 404", code)
	}
	var plainTel bytes.Buffer
	plain.server.tel.WriteText(&plainTel)

	text := e.metrics(t)
	expect := func(text, line string) {
		t.Helper()
		if !strings.Contains(text, "\n"+line+"\n") {
			t.Errorf("missing %s", line)
		}
	}
	for reason, n := range map[string][2]int{ // events, records
		refusedUnauthorized: {2, 2}, refusedClient: {1, 1}, refusedBadRequest: {2, 1}, refusedFormat: {0, 1}, refusedTooLarge: {1, 1},
		refusedStarting: {1, 1}, refusedRateLimited: {1, 1}, refusedLedger: {1, 1},
	} {
		for i, endpoint := range []string{endpointEvents, endpointRecords} {
			if n[i] > 0 {
				expect(text, fmt.Sprintf(`vbilling_ingest_requests_refused_total{endpoint=%q,reason=%q} %d`, endpoint, reason, n[i]))
			}
		}
	}
	expect(off.metrics(t), `vbilling_ingest_requests_refused_total{endpoint="events",reason="disabled"} 1`)
	expect(plainTel.String(), `vbilling_ingest_requests_refused_total{endpoint="records",reason="disabled"} 1`)

	expect(text, `vbilling_records_received_total{client="_unaccepted"} 1`)
	expect(text, `vbilling_records_rejected_total{client="_unaccepted",reason="tenant"} 1`)
	expect(text, `vbilling_ingest_events_received_total{client="_unaccepted"} 1`)
	expect(text, `vbilling_ingest_events_rejected_total{client="_unaccepted",reason="tenant"} 1`)
	expect(text, `vbilling_ingest_events_received_total{client="api"} 10`)
	expect(text, `vbilling_ingested_events_total{client="api"} 2`)
	expect(text, `vbilling_records_received_total{client="vbilling"} 8`)
	expect(text, `vbilling_records_accepted_total{client="vbilling"} 1`)
	for _, reason := range []string{"tenant", "reserved_metric", "cluster", "unknown_metric", "too_old", "in_future", "duplicate", "invalid"} {
		expect(text, fmt.Sprintf(`vbilling_ingest_events_rejected_total{client="api",reason=%q} 1`, reason))
		if reason != "duplicate" { // a repeated request ID in one batch of records is not refused, it is counted once
			expect(text, fmt.Sprintf(`vbilling_records_rejected_total{client="vbilling",reason=%q} 1`, reason))
		}
	}
	if t.Failed() {
		t.Logf("metrics:\n%s", text)
	}
}
