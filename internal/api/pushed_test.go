package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// pushedMetric is a metric an operator declared for pushed usage, as
// CUSTOM_METRICS_FILE does. The ingest tests push under it: the metrics
// vBilling measures itself take no pushed usage.
const pushedMetric = "slurm_gpu_hours"

// recordsMetric is a metric a records format declared: the aggregator
// builds its events from per-request records.
const recordsMetric = "gateway_request_tokens"

func init() {
	if err := usage.RegisterCustom(usage.MetricDef{Code: pushedMetric, Name: "Slurm GPU Hours", Unit: "gpu-hours", Billable: true, GroupKeys: []string{"sku"}}); err != nil {
		panic(err)
	}
	if err := usage.RegisterCustom(usage.MetricDef{Code: recordsMetric, Unit: "tokens", Billable: true, Source: usage.SourceRecords}); err != nil {
		panic(err)
	}
}

// pushEnv is a server with both push paths, a destination that records the
// tenants it is told about, and a controller whose tenants, aliases and
// clusters the test sets.
type pushEnv struct {
	srv     *httptest.Server
	server  *Server
	sp      *spool.Spool
	records *spool.Spool
	ctrl    *pushCtrl
	dest    *noticeDest
	disp    *pipeline.Dispatcher
	health  *health.Registry
	tel     *telemetry.Registry
}

// pushCtrl is the controller as the ingest paths see it: whether discovery
// ran, the clusters it found, and the tenants and aliases it knows.
type pushCtrl struct {
	mu       sync.Mutex
	ready    bool
	clusters []discovery.TenantCluster
	tenants  map[string]bool
	aliases  map[string]string
}

func (c *pushCtrl) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

func (c *pushCtrl) Clusters() []discovery.TenantCluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]discovery.TenantCluster(nil), c.clusters...)
}

func (c *pushCtrl) LastWindow() time.Time { return time.Time{} }

// ResolveTenant is what the controller's ResolveTenant does: a tenant it
// knows, or an alias of one.
func (c *pushCtrl) ResolveTenant(v string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tenants[v] {
		return v, true
	}
	if t, ok := c.aliases[v]; ok {
		return t, true
	}
	return v, false
}

func (c *pushCtrl) setReady(ready bool) {
	c.mu.Lock()
	c.ready = ready
	c.mu.Unlock()
}

// noticeDest is a destination that records every tenant it is asked to
// create (the tenant notice a webhook destination would send) and every
// event it receives.
type noticeDest struct {
	name   string
	oldest time.Duration // the oldest usage it accepts; 0: no limit

	mu       sync.Mutex
	notified []string
	sent     []usage.Event
}

func (d *noticeDest) Name() string                                       { return d.name }
func (d *noticeDest) Bootstrap(context.Context, []usage.MetricDef) error { return nil }
func (d *noticeDest) RemoveTenant(context.Context, usage.Tenant) error   { return nil }
func (d *noticeDest) EnsureTenant(_ context.Context, t usage.Tenant) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.notified = append(d.notified, t.ID)
	return nil
}
func (d *noticeDest) SendEvents(_ context.Context, evs []usage.Event) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, evs...)
	return nil
}

// limitedDest is a noticeDest that declares the oldest usage it accepts.
type limitedDest struct{ *noticeDest }

func (d limitedDest) OldestUsage() time.Duration { return d.oldest }

type pushOptions struct {
	rejectUnknown bool                       // RECORDS_UNKNOWN_TENANTS=reject
	retention     time.Duration              // of the usage ledger (how long a deleted cluster is remembered)
	dests         []destinations.Destination // default: one noticeDest named "sink"
	mutate        func(*config.Config)
}

func setupPush(t *testing.T, o pushOptions) *pushEnv {
	t.Helper()
	cfg := &config.Config{Region: "eu-west-1", ClusterName: "cp-1", CollectionInterval: time.Minute, Retention: o.retention,
		APIToken: adminToken, IngestToken: "ingest-secret", IngestTokens: map[string]string{"tok-acme": "acme", "tok-globex": "globex"},
		RecordsRejectUnknown: o.rejectUnknown}
	if o.mutate != nil {
		o.mutate(cfg)
	}
	dir := t.TempDir()
	clock := func() time.Time { return now }
	sp, err := spool.Open(filepath.Join(dir, "ledger"), spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := spool.Open(filepath.Join(dir, "records"), spool.Options{NoSync: true, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close(); rs.Close() })
	formats, _ := records.LoadFormats("", true)
	if err := formats.Declare(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.New()
	h := health.New(tel)
	h.SetLogger(func(string, ...any) {})
	ctrl := &pushCtrl{ready: true, tenants: map[string]bool{"acme": true, "globex": true}, aliases: map[string]string{"team-acme": "acme"}}
	reg := pipeline.NewTenantRegistry()
	reg.Upsert(usage.Tenant{ID: "acme", DisplayName: "Acme"})
	reg.Upsert(usage.Tenant{ID: "globex", DisplayName: "Globex"})
	dest := &noticeDest{name: "sink"}
	dests := o.dests
	if dests == nil {
		dests = []destinations.Destination{dest}
	}
	for _, d := range dests {
		sp.Cursor(d.Name()) // a destination receives what is committed after it is first seen
	}
	disp := pipeline.New(sp, dests, pipeline.Options{Tenants: reg, IdlePoll: 5 * time.Millisecond, MinBackoff: time.Millisecond}, nil)
	in := records.NewIngester(rs, formats, records.Options{Window: time.Minute, Region: cfg.Region, Now: clock, RejectUnknownTenants: o.rejectUnknown},
		ctrl.ResolveTenant, h, tel)
	s := New(cfg, sp, disp, reg, ctrl, nil, tel, "test").WithHealth(h).WithRecords(in)
	s.now = clock
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &pushEnv{srv: srv, server: s, sp: sp, records: rs, ctrl: ctrl, dest: dest, disp: disp, health: h, tel: tel}
}

// deliver runs the dispatcher until the destinations have everything in
// the ledger.
func (e *pushEnv) deliver(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { e.disp.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()
	for _, d := range e.disp.Destinations() {
		for e.sp.Cursor(d.Name()) < e.sp.Committed() {
			if ctx.Err() != nil {
				t.Fatal("delivery timed out")
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// stored returns the events in the usage ledger.
func (e *pushEnv) stored(t *testing.T) []usage.Event {
	t.Helper()
	var out []usage.Event
	if err := e.sp.ScanAll(now.Add(-60*24*time.Hour), now.Add(time.Hour), func(ev *usage.Event) error {
		out = append(out, *ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// storedRecords returns the usage rows in the records ledger.
func (e *pushEnv) storedRecords(t *testing.T) []usage.Event {
	t.Helper()
	var out []usage.Event
	if err := e.records.ScanRecords(now.Add(-60*24*time.Hour), now.Add(time.Hour), func(ev *usage.Event, _ time.Time) error {
		out = append(out, *ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *pushEnv) metrics(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := e.tel.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// pushedEvent is one event of the request body of POST /api/v1/events,
// for the minute that ended five minutes before the test clock unless at
// says otherwise.
type pushedEvent struct {
	id, tenant, metric, cluster string
	at                          time.Time // window start
	props                       map[string]any
}

func (p pushedEvent) body() map[string]any {
	ws := p.at
	if ws.IsZero() {
		ws = now.Add(-5 * time.Minute)
	}
	metric := p.metric
	if metric == "" {
		metric = pushedMetric
	}
	ev := map[string]any{"metric": metric, "quantity": 1, "window_start": ws, "window_end": ws.Add(time.Minute)}
	if p.id != "" {
		ev["id"] = p.id
	}
	if p.tenant != "" {
		ev["tenant"] = p.tenant
	}
	if p.cluster != "" {
		ev["dimensions"] = map[string]string{usage.DimTenantCluster: p.cluster}
	}
	if p.props != nil {
		ev["properties"] = p.props
	}
	return ev
}

// pushAnswer is what an ingest endpoint answered.
type pushAnswer struct {
	code   int
	header http.Header
	out    map[string]any
}

// rejection returns the reason and text of the n-th rejected event or record.
func (a pushAnswer) rejection(n int) (reason, text string) {
	list, _ := a.out["rejected"].([]any)
	if n >= len(list) {
		return "", ""
	}
	m, _ := list[n].(map[string]any)
	reason, _ = m["reason"].(string)
	text, _ = m["error"].(string)
	return reason, text
}

func (a pushAnswer) accepted() float64 {
	n, _ := a.out["accepted"].(float64)
	return n
}

func (a pushAnswer) String() string { return fmt.Sprintf("HTTP %d %v", a.code, a.out) }

// post sends a request to an ingest endpoint. headers are name, value pairs.
func (e *pushEnv) post(t *testing.T, path, token string, body any, headers ...string) pushAnswer {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	case []byte:
		raw = b
	default:
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	a := pushAnswer{code: resp.StatusCode, header: resp.Header}
	_ = json.NewDecoder(resp.Body).Decode(&a.out)
	return a
}

// events posts events with a token.
func (e *pushEnv) events(t *testing.T, token string, evs ...pushedEvent) pushAnswer {
	t.Helper()
	list := make([]map[string]any, len(evs))
	for i, ev := range evs {
		list[i] = ev.body()
	}
	return e.post(t, "/api/v1/events", token, list)
}

// pushedRecord is a native record for POST /api/v1/records, timed two
// minutes before the test clock unless at says otherwise.
type pushedRecord struct {
	id, tenant, metric, cluster string
	at                          time.Time
}

func (r pushedRecord) body() map[string]any {
	at := r.at
	if at.IsZero() {
		at = now.Add(-2 * time.Minute)
	}
	metric := r.metric
	if metric == "" {
		metric = "inference_output_tokens"
	}
	out := map[string]any{"id": r.id, "time": at.Format(time.RFC3339), "usage": map[string]any{metric: 7}}
	if r.tenant != "" {
		out["tenant"] = r.tenant
	}
	if r.cluster != "" {
		out["tenant_cluster"] = r.cluster
	}
	return out
}

func (e *pushEnv) recs(t *testing.T, token string, recs ...pushedRecord) pushAnswer {
	t.Helper()
	list := make([]map[string]any, len(recs))
	for i, r := range recs {
		list[i] = r.body()
	}
	return e.post(t, "/api/v1/records", token, list)
}

// meter commits what the collector measures in the minute starting at ws
// for each cluster (tenant, cluster ID): its control plane hour and a GPU.
func (e *pushEnv) meter(t *testing.T, ws time.Time, clusters ...[2]string) {
	t.Helper()
	var evs []usage.Event
	for _, c := range clusters {
		for _, metric := range []string{usage.MetricInstanceHours, usage.MetricGPUHours} {
			ev := usage.Event{Tenant: c[0], Metric: metric, Quantity: 1.0 / 60, Region: "eu-west-1", ResourceID: c[1],
				WindowStart: ws, WindowEnd: ws.Add(time.Minute), Dimensions: map[string]string{usage.DimTenantCluster: c[1]},
				Source: spool.SourceCollector, RecordedAt: now}
			ev.Finalize()
			evs = append(evs, ev)
		}
	}
	if err := e.sp.AppendWindow(spool.SourceCollector, ws.Add(time.Minute), evs, ""); err != nil {
		t.Fatal(err)
	}
}

// With RECORDS_UNKNOWN_TENANTS=reject, an event pushed for a tenant
// vBilling does not know is refused exactly as a record for it is: with an
// explained error, nothing stored, and no tenant announced to a destination.
func TestPushedEventsForUnknownTenantsAreRefusedLikeRecords(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true})
	rec := e.recs(t, "ingest-secret", pushedRecord{id: "r-1", tenant: "tenant-phantom"})
	ev := e.events(t, "ingest-secret", pushedEvent{id: "e-1", tenant: "tenant-phantom"})
	for path, a := range map[string]pushAnswer{"records": rec, "events": ev} {
		reason, text := a.rejection(0)
		if a.code != http.StatusBadRequest || a.accepted() != 0 || reason != "tenant" || !strings.Contains(text, `unknown tenant "tenant-phantom"`) ||
			!strings.Contains(text, "vbilling.vcluster.com/aliases") {
			t.Errorf("%s for an unknown tenant: %v, want 400 with the unknown tenant explained", path, a)
		}
	}
	if st := e.sp.Stats(); st.Committed != 0 {
		t.Fatalf("usage for a tenant that does not exist was stored: %+v", e.stored(t))
	}
	if rows := e.storedRecords(t); len(rows) != 0 {
		t.Fatalf("records for a tenant that does not exist were stored: %+v", rows)
	}
	// A known tenant's event in the same request is stored, and it is the
	// only tenant a destination hears about.
	a := e.events(t, "ingest-secret", pushedEvent{id: "e-2", tenant: "tenant-phantom"}, pushedEvent{id: "e-3", tenant: "acme"})
	if a.code != http.StatusOK || a.accepted() != 1 || a.out["rejected_count"] != float64(1) {
		t.Fatalf("a batch with one known tenant: %v", a)
	}
	e.deliver(t)
	if got := fmt.Sprint(e.dest.notified); got != "[acme]" {
		t.Fatalf("destinations were told about tenants %s, want acme only", got)
	}
	for _, ev := range e.dest.sent {
		if ev.Tenant != "acme" {
			t.Fatalf("a destination received usage for %q", ev.Tenant)
		}
	}
}

// With RECORDS_UNKNOWN_TENANTS=accept (the default), both push paths accept
// usage for a tenant vBilling does not know and report it the same way: in
// the answer and in source health.
func TestPushedEventsForUnknownTenantsAreAcceptedAndReportedLikeRecords(t *testing.T) {
	e := setupPush(t, pushOptions{})
	rec := e.recs(t, "ingest-secret", pushedRecord{id: "r-1", tenant: "tenant-new"})
	ev := e.events(t, "ingest-secret", pushedEvent{id: "e-1", tenant: "tenant-new"}, pushedEvent{id: "e-2", tenant: "acme"})
	for path, a := range map[string]pushAnswer{"records": rec, "events": ev} {
		if a.code != http.StatusOK || fmt.Sprint(a.out["unknown_tenants"]) != "[tenant-new]" || !strings.Contains(fmt.Sprint(a.out["warnings"]), "billed to tenants vBilling does not know (tenant-new)") {
			t.Errorf("%s for an unknown tenant: %v, want it accepted and reported", path, a)
		}
	}
	sources := map[string]health.Status{}
	for _, s := range e.health.List() {
		sources[s.Name] = s.Status
	}
	if sources["records/vbilling/unknown-tenants"] != health.Empty || sources["ingest/api/unknown-tenants"] != health.Empty {
		t.Fatalf("source health %v: want both push paths to report the unknown tenant", sources)
	}
}

// A pushed event resolves its tenant as a record does: an alias bills to
// its tenant, and the event keeps the ID of the value as sent, so a retry
// counts once whatever the alias maps to by then. A tenant-bound token
// still wins over the payload.
func TestPushedEventsResolveTenantAliasesLikeRecords(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true})
	if a := e.recs(t, "ingest-secret", pushedRecord{id: "r-1", tenant: "team-acme"}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("a record under an alias: %v", a)
	}
	first := e.events(t, "ingest-secret", pushedEvent{tenant: "team-acme"})
	if first.code != http.StatusOK || first.accepted() != 1 {
		t.Fatalf("an event under an alias: %v", first)
	}
	for _, ev := range append(e.stored(t), e.storedRecords(t)...) {
		if ev.Tenant != "acme" {
			t.Fatalf("usage sent as team-acme is billed to %q, want acme", ev.Tenant)
		}
	}
	// The alias moves to another tenant: a retry of the same event is still
	// the same event.
	e.ctrl.mu.Lock()
	e.ctrl.aliases["team-acme"] = "globex"
	e.ctrl.mu.Unlock()
	if retry := e.events(t, "ingest-secret", pushedEvent{tenant: "team-acme"}); retry.accepted() != 0 || retry.out["duplicates"] != float64(1) {
		t.Fatalf("a retry after the alias changed: %v, want a duplicate", retry)
	}
	// A token bound to a tenant bills that tenant, whatever the payload names.
	if a := e.events(t, "tok-globex", pushedEvent{id: "b-1", tenant: "acme"}); a.code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(a.out["rejected"]), "bound to") {
		t.Fatalf("another tenant with a bound token: %v", a)
	}
	if a := e.events(t, "tok-globex", pushedEvent{id: "b-2"}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("a bound token without a tenant in the payload: %v", a)
	}
}

// Until the first discovery vBilling cannot tell which tenant a value
// means, so pushed events get 503 with Retry-After, as records do, and
// nothing is stored. A token bound to a tenant needs no discovery, and
// the wait never outlasts the startup grace.
func TestPushedEventsWaitForTheFirstDiscovery(t *testing.T) {
	e := setupPush(t, pushOptions{rejectUnknown: true})
	e.ctrl.setReady(false)
	for path, a := range map[string]pushAnswer{
		"events":  e.events(t, "ingest-secret", pushedEvent{id: "e-1", tenant: "team-acme"}),
		"records": e.recs(t, "ingest-secret", pushedRecord{id: "r-1", tenant: "team-acme"}),
	} {
		if a.code != http.StatusServiceUnavailable || a.header.Get("Retry-After") == "" {
			t.Errorf("%s before the first discovery: %v (Retry-After %q), want 503 with Retry-After", path, a, a.header.Get("Retry-After"))
		}
	}
	if st := e.sp.Stats(); st.Committed != 0 {
		t.Fatal("an event was stored before vBilling knew its tenant")
	}
	if a := e.events(t, "tok-acme", pushedEvent{id: "b-1"}); a.code != http.StatusOK {
		t.Fatalf("a bound token before the first discovery: %v", a)
	}
	e.ctrl.setReady(true)
	if a := e.events(t, "ingest-secret", pushedEvent{id: "e-1", tenant: "team-acme"}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("after the first discovery: %v", a)
	}
	e.ctrl.setReady(false)
	e.server.started = time.Now().Add(-ingestStartupGrace - time.Second)
	if a := e.events(t, "ingest-secret", pushedEvent{id: "e-2", tenant: "acme"}); a.code != http.StatusOK {
		t.Fatalf("discovery that never succeeds must not hold ingest for good: %v", a)
	}
}

// Every metric the catalog says vBilling measures itself
// (whatever is added to it later) takes no pushed usage, on either push
// path and with either kind of ingest token: the refusal says so and points
// at the custom metrics file.
func TestMeasuredMetricsTakeNoPushedUsage(t *testing.T) {
	e := setupPush(t, pushOptions{})
	refused := map[string]bool{}
	for _, def := range usage.Catalog() {
		if def.Source == usage.SourceIngest || def.Source == usage.SourceRecords {
			continue
		}
		refused[def.Code] = true
		for _, token := range []string{"ingest-secret", "tok-acme"} {
			id := def.Code + "-" + token
			for path, a := range map[string]pushAnswer{
				"events":  e.events(t, token, pushedEvent{id: id, tenant: "acme", metric: def.Code}),
				"records": e.recs(t, token, pushedRecord{id: id, tenant: "acme", metric: def.Code}),
			} {
				reason, text := a.rejection(0)
				if a.code != http.StatusBadRequest || reason != "reserved_metric" || !strings.Contains(text, def.Code+" is measured by vBilling itself") ||
					!strings.Contains(text, "CUSTOM_METRICS_FILE") {
					t.Errorf("%s under %s with %s: %v, want it refused as measured by vBilling", path, def.Code, token, a)
				}
			}
		}
	}
	// The metrics the collector and Prometheus measure were all among them.
	for _, code := range []string{usage.MetricGPUHours, usage.MetricCPUCoreHours, usage.MetricMemoryGBHours, usage.MetricPrivateNodeHours,
		usage.MetricInstanceHours, usage.MetricStorageGBHours, usage.MetricLBHours, usage.MetricNetworkEgressGB,
		usage.MetricGPUDowntimeHours, usage.MetricGPUUtilization} {
		if !refused[code] {
			t.Errorf("%s is not marked as measured in the catalog, so it takes pushed usage", code)
		}
	}
	if st := e.sp.Stats(); st.Committed != 0 || len(e.storedRecords(t)) != 0 {
		t.Fatalf("usage pushed under a measured metric was stored: %+v %+v", e.stored(t), e.storedRecords(t))
	}
	// A ghost node with five node-hours in one minute, the case the validation pushed.
	ghost := map[string]any{"id": "ghost", "tenant": "acme", "metric": usage.MetricPrivateNodeHours, "quantity": 5, "resource_id": "ghost-node",
		"window_start": now.Add(-5 * time.Minute), "window_end": now.Add(-4 * time.Minute), "dimensions": map[string]string{usage.DimNode: "ghost-node"}}
	if a := e.post(t, "/api/v1/events", "ingest-secret", []any{ghost}); a.code != http.StatusBadRequest {
		t.Fatalf("five node-hours of a made-up node in a one-minute window: %v, want it refused", a)
	}
}

// A metric a records format declares is built from per-request records by
// the aggregator: records keep writing it, and window totals pushed as
// events do not. A metric the operator declared for pushed usage takes both.
func TestRecordsMetricsTakeRecordsOnly(t *testing.T) {
	e := setupPush(t, pushOptions{})
	if a := e.recs(t, "ingest-secret", pushedRecord{id: "r-1", tenant: "acme", metric: recordsMetric}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("a record under a records metric: %v", a)
	}
	a := e.events(t, "ingest-secret", pushedEvent{id: "e-1", tenant: "acme", metric: recordsMetric})
	if reason, text := a.rejection(0); a.code != http.StatusBadRequest || reason != "reserved_metric" || !strings.Contains(text, "POST /api/v1/records") {
		t.Fatalf("an event under a records metric: %v, want it refused with a pointer to the records API", a)
	}
	if a := e.recs(t, "ingest-secret", pushedRecord{id: "r-2", tenant: "acme", metric: pushedMetric}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("a record under an operator's own metric: %v", a)
	}
	if a := e.events(t, "ingest-secret", pushedEvent{id: "e-2", tenant: "acme", metric: pushedMetric}); a.code != http.StatusOK || a.accepted() != 1 {
		t.Fatalf("an event under an operator's own metric: %v", a)
	}
}

// The operator corrects usage under a measured metric with the API token
// and operator_adjustment=true. Every event of such a request is marked
// with a property no emitter can set, keeps an ingest source, and gets an
// ID of its own. Without the parameter the API token stays refused at
// ingest, and an ingest token cannot ask for an adjustment.
func TestOperatorAdjustmentsUnderMeasuredMetricsAreMarked(t *testing.T) {
	e := setupPush(t, pushOptions{})
	ws := now.Add(-5 * time.Minute)
	e.meter(t, ws, [2]string{"acme", "vcluster-acme-train"})
	// The correction has the identity of the GPU-hours vBilling measured for
	// that minute: same tenant, metric, window, region, resource and cluster.
	correction := pushedEvent{tenant: "acme", metric: usage.MetricGPUHours, cluster: "vcluster-acme-train"}.body()
	correction["resource_id"] = "vcluster-acme-train"
	fix := []map[string]any{correction}

	if a := e.post(t, "/api/v1/events", adminToken, fix); a.code != http.StatusUnauthorized || !strings.Contains(fmt.Sprint(a.out["error"]), "operator_adjustment=true") {
		t.Fatalf("the API token without operator_adjustment: %v, want 401 that names the parameter", a)
	}
	if a := e.post(t, "/api/v1/events?operator_adjustment=true", "ingest-secret", fix); a.code != http.StatusForbidden {
		t.Fatalf("an ingest token asking for an operator adjustment: %v, want 403", a)
	}
	if a := e.post(t, "/api/v1/events?operator_adjustment=true", "wrong", fix); a.code != http.StatusUnauthorized {
		t.Fatalf("a wrong token asking for an operator adjustment: %v, want 401", a)
	}
	if a := e.post(t, "/api/v1/events?operator_adjustment=maybe", adminToken, fix); a.code != http.StatusBadRequest {
		t.Fatalf("operator_adjustment=maybe: %v, want 400", a)
	}
	if a := e.post(t, "/api/v1/records?operator_adjustment=true", adminToken, []any{pushedRecord{id: "r-1", tenant: "acme"}.body()}); a.code != http.StatusUnauthorized {
		t.Fatalf("records with the API token: %v, want 401 (an adjustment is an event)", a)
	}
	forged := []map[string]any{pushedEvent{id: "forged", tenant: "acme", props: map[string]any{"operator_adjustment": true}}.body()}
	if a := e.post(t, "/api/v1/events", "ingest-secret", forged); a.code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(a.out["rejected"]), "vBilling sets itself") {
		t.Fatalf("an emitter that sets operator_adjustment itself: %v, want it refused", a)
	}
	before := len(e.stored(t))

	a := e.post(t, "/api/v1/events?operator_adjustment=true", adminToken, fix)
	if a.code != http.StatusOK || a.accepted() != 1 || a.out["operator_adjustment"] != true {
		t.Fatalf("an operator adjustment under %s: %v", usage.MetricGPUHours, a)
	}
	all := e.stored(t)
	if len(all) != before+1 {
		t.Fatalf("%d events stored, want the adjustment next to the %d measured ones", len(all), before)
	}
	ids := map[string]int{}
	for _, ev := range all {
		ids[ev.ID]++
		if ev.Source == spool.SourceCollector {
			if ev.Properties["operator_adjustment"] != nil {
				t.Fatalf("a measured event is marked as an adjustment: %+v", ev)
			}
			continue
		}
		if ev.Source != "ingest:operator" || ev.Properties["operator_adjustment"] != true || !strings.HasPrefix(ev.ID, "ie1_") {
			t.Fatalf("the adjustment is stored as %+v: want source ingest:operator, operator_adjustment true and an ingest ID", ev)
		}
	}
	for id, n := range ids {
		if n > 1 {
			t.Fatalf("the adjustment took the ID %s of a measured event", id)
		}
	}
	// The same adjustment again is the same event.
	if again := e.post(t, "/api/v1/events?operator_adjustment=true", adminToken, fix); again.accepted() != 0 || again.out["duplicates"] != float64(1) {
		t.Fatalf("the adjustment sent again: %v, want a duplicate", again)
	}
	// Its tenant and cluster are checked like anyone's.
	bad := []map[string]any{pushedEvent{id: "x", tenant: "acme", metric: usage.MetricGPUHours, cluster: "vcluster-acme-ghost"}.body()}
	if a := e.post(t, "/api/v1/events?operator_adjustment=true", adminToken, bad); a.code != http.StatusBadRequest {
		t.Fatalf("an adjustment for a cluster vBilling does not know: %v, want it refused", a)
	}
}
