// Package api serves vBilling's HTTP surface: health and Prometheus
// metrics, tenant-visible usage line items and exports, the ingest endpoint
// for custom usage emitters, reconciliation against billing backends,
// ledger verification, delivery status, and billing webhooks.
package api

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

//go:embed dashboard
var dashboardFS embed.FS

// Controller is the part of the controller the API reads.
type Controller interface {
	Ready() bool
	Clusters() []discovery.TenantCluster
	LastWindow() time.Time
}

// Server is vBilling's HTTP API.
type Server struct {
	cfg     *config.Config
	spool   *spool.Spool
	disp    *pipeline.Dispatcher
	tenants *pipeline.TenantRegistry
	ctrl    Controller
	enf     *enforcement.Enforcer // nil when no webhook secret is configured
	tel     *telemetry.Registry
	version string
	now     func() time.Time
}

func New(cfg *config.Config, sp *spool.Spool, disp *pipeline.Dispatcher, tenants *pipeline.TenantRegistry,
	ctrl Controller, enf *enforcement.Enforcer, tel *telemetry.Registry, version string) *Server {
	return &Server{cfg: cfg, spool: sp, disp: disp, tenants: tenants, ctrl: ctrl, enf: enf, tel: tel, version: version, now: time.Now}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.metrics)

	mux.HandleFunc("GET /api/v1/status", s.auth(s.status))
	mux.HandleFunc("GET /api/v1/catalog", s.auth(s.catalog))
	mux.HandleFunc("GET /api/v1/tenants", s.auth(s.listTenants))
	mux.HandleFunc("GET /api/v1/usage", s.auth(s.usage))
	mux.HandleFunc("GET /api/v1/events", s.auth(s.exportEvents))
	mux.HandleFunc("POST /api/v1/events", s.ingest)
	mux.HandleFunc("GET /api/v1/reconcile", s.auth(s.reconcile))
	mux.HandleFunc("GET /api/v1/ledger/verify", s.auth(s.verify))
	mux.HandleFunc("GET /api/v1/destinations", s.auth(s.listDestinations))
	mux.HandleFunc("GET /api/v1/destinations/{name}/dead-letters", s.auth(s.deadLetters))
	mux.HandleFunc("POST /api/v1/destinations/{name}/dead-letters/replay", s.auth(s.replayDeadLetters))
	mux.HandleFunc("POST /api/v1/destinations/{name}/cursor", s.auth(s.setCursor))
	mux.HandleFunc("GET /api/v1/billing-states", s.auth(s.billingStates))
	mux.HandleFunc("PUT /api/v1/billing-states/{tenant}", s.auth(s.setBillingState))

	mux.HandleFunc("POST /webhooks/stripe", s.stripeWebhook)
	mux.HandleFunc("POST /webhooks/metronome", s.metronomeWebhook)

	sub, _ := fs.Sub(dashboardFS, "dashboard")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func tokenOK(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// auth protects read and admin endpoints with API_TOKEN when it is set.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken != "" && !tokenOK(bearer(r), s.cfg.APIToken) {
			writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r)
	}
}

// parseRange reads from/to (RFC 3339 or YYYY-MM-DD). Defaults: the
// current UTC month to now.
func (s *Server) parseRange(r *http.Request, defFrom, defTo time.Time) (time.Time, time.Time, error) {
	parse := func(v string, def time.Time) (time.Time, error) {
		if v == "" {
			return def, nil
		}
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC(), nil
		}
		if t, err := time.Parse("2006-01-02", v); err == nil {
			return t.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("invalid time %q (use RFC 3339 or YYYY-MM-DD)", v)
	}
	from, err := parse(r.URL.Query().Get("from"), defFrom)
	if err != nil {
		return from, from, err
	}
	to, err := parse(r.URL.Query().Get("to"), defTo)
	if err != nil {
		return from, to, err
	}
	if !to.After(from) {
		return from, to, errors.New("to must be after from")
	}
	return from, to, nil
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// field returns an event attribute by name for grouping and filtering.
func field(e *usage.Event, key string) string {
	switch key {
	case "tenant":
		return e.Tenant
	case "metric":
		return e.Metric
	case "unit":
		return e.Unit
	case "sku":
		return e.SKU
	case "region":
		return e.Region
	case "project":
		return e.Project
	case "resource_id":
		return e.ResourceID
	case "source":
		return e.Source
	case "day":
		return e.WindowStart.UTC().Format("2006-01-02")
	case "hour":
		return e.WindowStart.UTC().Format("2006-01-02T15:00Z")
	default:
		return e.Dim(key)
	}
}

// filter applies tenant/metric/project/region/sku/cluster query filters.
// filterKeys are the query parameters filter understands.
var filterKeys = []string{"tenant", "metric", "project", "region", "sku", "cluster"}

func hasFilter(r *http.Request) bool {
	for _, k := range filterKeys {
		if r.URL.Query().Get(k) != "" {
			return true
		}
	}
	return false
}

func filter(r *http.Request) func(*usage.Event) bool {
	q := r.URL.Query()
	checks := map[string]string{}
	for _, k := range []string{"tenant", "metric", "project", "region", "sku"} {
		if v := q.Get(k); v != "" {
			checks[k] = v
		}
	}
	if v := q.Get("cluster"); v != "" {
		checks[usage.DimTenantCluster] = v
	}
	return func(e *usage.Event) bool {
		for k, v := range checks {
			if field(e, k) != v {
				return false
			}
		}
		return true
	}
}

// --- health & metrics ---

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.ctrl != nil && !s.ctrl.Ready() {
		http.Error(w, "waiting for first successful discovery", http.StatusServiceUnavailable)
		return
	}
	io.WriteString(w, "ok\n")
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	st := s.spool.Stats()
	s.tel.Set("vbilling_ledger_committed_seq", "Highest committed ledger sequence number.", nil, float64(st.Committed))
	s.tel.Set("vbilling_ledger_bytes", "Bytes of retained ledger segments.", nil, float64(st.Bytes))
	s.tel.Set("vbilling_ledger_segments", "Retained ledger segments.", nil, float64(st.Segments))
	if s.disp != nil {
		for _, d := range s.disp.Statuses() {
			l := map[string]string{"destination": d.Name}
			s.tel.Set("vbilling_destination_lag_records", "Ledger records not yet acknowledged by a destination.", l, float64(d.LagRecords))
			ready := 0.0
			if d.Ready {
				ready = 1
			}
			s.tel.Set("vbilling_destination_ready", "1 once a destination is bootstrapped and receiving events.", l, ready)
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s.tel.WriteText(w)
}

// --- status, catalog, tenants ---

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"version":          s.version,
		"region":           s.cfg.Region,
		"cluster":          s.cfg.ClusterName,
		"adapters":         s.cfg.Adapters,
		"window":           s.cfg.CollectionInterval.String(),
		"enforcement_mode": s.cfg.EnforcementMode,
		"ledger":           s.spool.Stats(),
		"tenants":          len(s.tenants.All()),
	}
	if s.ctrl != nil {
		out["ready"] = s.ctrl.Ready()
		out["tenant_clusters"] = len(s.ctrl.Clusters())
		out["last_window_end"] = s.ctrl.LastWindow()
	}
	if s.disp != nil {
		out["destinations"] = s.disp.Statuses()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": usage.SchemaVersion, "metrics": usage.Catalog()})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	type row struct {
		usage.Tenant
		BillingState *enforcement.TenantState `json:"billing_state,omitempty"`
	}
	var out []row
	for _, t := range s.tenants.All() {
		rw := row{Tenant: t}
		if s.enf != nil {
			st := s.enf.StateOf(t.ID)
			rw.BillingState = &st
		}
		out = append(out, rw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": out})
}

// --- usage line items ---

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	now := s.now().UTC()
	from, to, err := s.parseRange(r, monthStart(now), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	groupBy := []string{"tenant"}
	if v := r.URL.Query().Get("group_by"); v != "" {
		groupBy = nil
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" && k != "metric" && k != "unit" {
				groupBy = append(groupBy, k)
			}
		}
	}
	keep := filter(r)
	type agg struct {
		group    []string
		metric   string
		unit     string
		quantity float64
		events   int
	}
	rows := map[string]*agg{}
	total := 0
	err = s.spool.Scan(from, to, func(e *usage.Event) error {
		if e.WindowStart.Before(from) || !e.WindowStart.Before(to) || !keep(e) {
			return nil
		}
		vals := make([]string, len(groupBy))
		for i, k := range groupBy {
			vals[i] = field(e, k)
		}
		key := strings.Join(append(vals, e.Metric), "\x00")
		a, ok := rows[key]
		if !ok {
			a = &agg{group: vals, metric: e.Metric, unit: e.Unit}
			rows[key] = a
		}
		a.quantity += e.Quantity
		a.events++
		total++
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan ledger: %v", err)
		return
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage-%s-%s.csv"`, from.Format("20060102"), to.Format("20060102")))
		cw := csv.NewWriter(w)
		cw.Write(append(append([]string{}, groupBy...), "metric", "unit", "quantity", "events"))
		for _, k := range keys {
			a := rows[k]
			cw.Write(append(append([]string{}, a.group...), a.metric, a.unit, strconv.FormatFloat(usage.Round(a.quantity), 'f', -1, 64), strconv.Itoa(a.events)))
		}
		cw.Flush()
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		a := rows[k]
		row := map[string]any{"metric": a.metric, "unit": a.unit, "quantity": usage.Round(a.quantity), "events": a.events}
		for i, g := range groupBy {
			row[g] = a.group[i]
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "group_by": groupBy, "events": total, "rows": out,
		"note": "Unrated usage from the vBilling ledger; prices and invoices come from your billing backend."})
}

// --- raw export ---

func (s *Server) exportEvents(w http.ResponseWriter, r *http.Request) {
	now := s.now().UTC()
	from, to, err := s.parseRange(r, monthStart(now), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	limit := math.MaxInt
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	keep := filter(r)
	format := r.URL.Query().Get("format")
	var cw *csv.Writer
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		cw = csv.NewWriter(w)
		cw.Write([]string{"id", "tenant", "metric", "quantity", "unit", "window_start", "window_end", "region", "project", "sku", "resource_id", "dimensions", "source"})
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
	}
	enc := json.NewEncoder(w)
	n := 0
	errStop := errors.New("limit reached")
	err = s.spool.Scan(from, to, func(e *usage.Event) error {
		if e.WindowStart.Before(from) || !e.WindowStart.Before(to) || !keep(e) {
			return nil
		}
		if n >= limit {
			return errStop
		}
		n++
		if cw != nil {
			dims := make([]string, 0, len(e.Dimensions))
			for k, v := range e.Dimensions {
				dims = append(dims, k+"="+v)
			}
			sort.Strings(dims)
			return cw.Write([]string{e.ID, e.Tenant, e.Metric, strconv.FormatFloat(e.Quantity, 'f', -1, 64), e.Unit,
				e.WindowStart.Format(time.RFC3339), e.WindowEnd.Format(time.RFC3339), e.Region, e.Project, e.SKU, e.ResourceID, strings.Join(dims, ";"), e.Source})
		}
		return enc.Encode(e)
	})
	if cw != nil {
		cw.Flush()
	}
	if err != nil && !errors.Is(err, errStop) {
		log.Printf("[api] export: %v", err)
	}
}

// --- ingest ---

const (
	maxIngestBody   = 10 << 20
	maxIngestEvents = 5000
	ingestMaxAge    = 34 * 24 * time.Hour // the strictest backend window (Metronome)
	ingestMaxSkew   = 5 * time.Minute
)

// ingest accepts usage from custom emitters: inference gateways (tokens),
// Slurm accounting, storage or network exporters. Events need a declared
// metric (builtin or CUSTOM_METRICS) and a closed window; an omitted ID is
// derived from the event identity, so resends deduplicate.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.IngestToken == "" {
		writeErr(w, http.StatusForbidden, "ingest is disabled: set INGEST_TOKEN (or API_TOKEN) to enable it")
		return
	}
	if !tokenOK(bearer(r), s.cfg.IngestToken) {
		writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBody+1))
	if err != nil || len(body) > maxIngestBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "body must be at most %d bytes", maxIngestBody)
		return
	}
	var events []usage.Event
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(body, &events)
	} else {
		var req struct {
			Events []usage.Event `json:"events"`
		}
		err = json.Unmarshal(body, &req)
		events = req.Events
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if len(events) == 0 || len(events) > maxIngestEvents {
		writeErr(w, http.StatusBadRequest, "send 1 to %d events", maxIngestEvents)
		return
	}
	client := r.Header.Get("X-VBilling-Client")
	if client == "" {
		client = "api"
	}
	source := "ingest:" + client
	now := s.now().UTC()
	type rejection struct {
		Index int    `json:"index"`
		ID    string `json:"id,omitempty"`
		Error string `json:"error"`
	}
	var valid []usage.Event
	var rejected []rejection
	for i := range events {
		e := events[i]
		e.Source = source
		e.RecordedAt = now
		if e.Region == "" {
			e.Region = s.cfg.Region
		}
		e.Finalize()
		var problems []string
		if _, ok := usage.Lookup(e.Metric); !ok {
			problems = append(problems, fmt.Sprintf("unknown metric %q (declare it in CUSTOM_METRICS)", e.Metric))
		}
		if e.WindowEnd.After(now.Add(ingestMaxSkew)) {
			problems = append(problems, "window_end is in the future")
		}
		if e.WindowStart.Before(now.Add(-ingestMaxAge)) {
			problems = append(problems, "window_start is older than 34 days")
		}
		if err := e.Validate(); err != nil {
			problems = append(problems, err.Error())
		}
		if len(problems) > 0 {
			rejected = append(rejected, rejection{Index: i, ID: e.ID, Error: strings.Join(problems, "; ")})
			continue
		}
		valid = append(valid, e)
	}
	var accepted, dups []string
	if len(valid) > 0 {
		if accepted, dups, err = s.spool.AppendIngest(source, valid); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "ledger write failed: %v", err)
			return
		}
	}
	status := http.StatusOK
	if len(valid) == 0 {
		status = http.StatusBadRequest
	}
	s.tel.Add("vbilling_ingested_events_total", "Events accepted through the ingest API.", map[string]string{"client": client}, float64(len(accepted)))
	writeJSON(w, status, map[string]any{"accepted": len(accepted), "duplicates": len(dups), "rejected": rejected, "ids": accepted})
}

// --- reconciliation ---

type reconcileRow struct {
	Tenant       string  `json:"tenant"`
	Metric       string  `json:"metric"`
	Ledger       float64 `json:"ledger"`
	DeadLettered float64 `json:"dead_lettered"`
	Expected     float64 `json:"expected"`
	Recorded     float64 `json:"recorded"`
	Difference   float64 `json:"difference"`
	Match        bool    `json:"match"`
}

type reconcileResult struct {
	Destination string         `json:"destination"`
	OK          bool           `json:"ok"`
	Error       string         `json:"error,omitempty"`
	Rows        []reconcileRow `json:"rows"`
}

// reconcile recomputes usage from the ledger and compares it with what each
// backend recorded. Default range: the previous full UTC day, which every
// backend can answer for (Metronome only reports whole UTC days).
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	today := s.now().UTC().Truncate(24 * time.Hour)
	from, to, err := s.parseRange(r, today.Add(-24*time.Hour), today)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	only := r.URL.Query().Get("destination")
	keep := filter(r)
	var results []reconcileResult
	for _, d := range s.disp.Destinations() {
		if only != "" && d.Name() != only {
			continue
		}
		rec, ok := d.(destinations.Reconciler)
		if !ok {
			if only != "" {
				results = append(results, reconcileResult{Destination: d.Name(), Error: "destination does not support reconciliation"})
			}
			continue
		}
		results = append(results, s.reconcileOne(r.Context(), d, rec, from, to, keep, hasFilter(r)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "results": results})
}

func (s *Server) reconcileOne(ctx context.Context, d destinations.Destination, rec destinations.Reconciler, from, to time.Time, keep func(*usage.Event) bool, filtered bool) reconcileResult {
	res := reconcileResult{Destination: d.Name()}
	type key struct{ tenant, metric string }
	ledger, dead := map[key]float64{}, map[key]float64{}
	in := func(e *usage.Event) bool {
		return !e.WindowStart.Before(from) && e.WindowStart.Before(to) && destinations.Accepts(d, e.Metric)
	}
	// Billing platforms report totals per tenant and metric only, so filters
	// pick which (tenant, metric) pairs to compare, and each comparison covers
	// all of that tenant's usage of the metric. Summing only the filtered
	// events (one SKU, one tenant cluster) would mismatch by construction.
	selected := map[key]bool{}
	err := s.spool.Scan(from, to, func(e *usage.Event) error {
		if in(e) {
			k := key{e.Tenant, e.Metric}
			ledger[k] += e.Quantity
			if keep(e) {
				selected[k] = true
			}
		}
		return nil
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if dls, err := s.spool.DeadLetters(d.Name()); err == nil {
		for i := range dls {
			if e := &dls[i].Event; in(e) {
				k := key{e.Tenant, e.Metric}
				dead[k] += e.Quantity
				if keep(e) {
					selected[k] = true
				}
			}
		}
	}
	for k := range ledger {
		if !selected[k] {
			delete(ledger, k)
		}
	}
	for k := range dead {
		if !selected[k] {
			delete(dead, k)
		}
	}
	tenantSet, metricSet := map[string]bool{}, map[string]bool{}
	for k := range ledger {
		tenantSet[k.tenant], metricSet[k.metric] = true, true
	}
	q := destinations.TotalsQuery{From: from, To: to}
	for t := range tenantSet {
		tt, _ := s.tenants.Get(t)
		q.Tenants = append(q.Tenants, tt)
	}
	for m := range metricSet {
		q.Metrics = append(q.Metrics, m)
	}
	sort.Slice(q.Tenants, func(i, j int) bool { return q.Tenants[i].ID < q.Tenants[j].ID })
	sort.Strings(q.Metrics)
	recorded := map[key]float64{}
	if len(q.Tenants) > 0 {
		totals, err := rec.RecordedTotals(ctx, q)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		for _, t := range totals {
			recorded[key{t.Tenant, t.Metric}] += t.Quantity
		}
	}
	keys := map[key]bool{}
	for k := range ledger {
		keys[k] = true
	}
	for k := range recorded {
		if !filtered || selected[k] {
			keys[k] = true
		}
	}
	res.OK = true
	for k := range keys {
		expected := ledger[k] - dead[k]
		diff := recorded[k] - expected
		match := math.Abs(diff) <= math.Max(1e-6, 1e-3*math.Abs(expected))
		res.OK = res.OK && match
		res.Rows = append(res.Rows, reconcileRow{Tenant: k.tenant, Metric: k.metric, Ledger: usage.Round(ledger[k]), DeadLettered: usage.Round(dead[k]),
			Expected: usage.Round(expected), Recorded: usage.Round(recorded[k]), Difference: usage.Round(diff), Match: match})
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		if res.Rows[i].Tenant != res.Rows[j].Tenant {
			return res.Rows[i].Tenant < res.Rows[j].Tenant
		}
		return res.Rows[i].Metric < res.Rows[j].Metric
	})
	return res
}

// --- ledger, destinations, dead letters ---

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	rep := s.spool.Verify()
	status := http.StatusOK
	if !rep.OK {
		status = http.StatusConflict
	}
	writeJSON(w, status, rep)
}

func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	type row struct {
		pipeline.Status
		DeadLetters int `json:"dead_letters"`
	}
	var out []row
	for _, st := range s.disp.Statuses() {
		dls, _ := s.spool.DeadLetters(st.Name)
		out = append(out, row{Status: st, DeadLetters: len(dls)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"destinations": out})
}

func (s *Server) destination(name string) destinations.Destination {
	for _, d := range s.disp.Destinations() {
		if d.Name() == name {
			return d
		}
	}
	return nil
}

func (s *Server) deadLetters(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.destination(name) == nil {
		writeErr(w, http.StatusNotFound, "no destination %q", name)
		return
	}
	dls, err := s.spool.DeadLetters(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"destination": name, "dead_letters": dls})
}

// replayDeadLetters resends parked events (after fixing the cause, for
// example creating a missing customer). Event IDs are stable, so a replay
// can never double-bill.
func (s *Server) replayDeadLetters(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d := s.destination(name)
	if d == nil {
		writeErr(w, http.StatusNotFound, "no destination %q", name)
		return
	}
	dls, err := s.spool.DeadLetters(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if len(dls) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"replayed": 0})
		return
	}
	events := make([]usage.Event, len(dls))
	for i := range dls {
		events[i] = dls[i].Event
	}
	err = d.SendEvents(r.Context(), events)
	var partial *destinations.PartialError
	switch {
	case err == nil:
		s.spool.ClearDeadLetters(name)
		writeJSON(w, http.StatusOK, map[string]any{"replayed": len(events)})
	case errors.As(err, &partial):
		s.spool.ClearDeadLetters(name)
		for _, ev := range events {
			if reason, bad := partial.Rejected[ev.ID]; bad {
				s.spool.AddDeadLetter(name, ev, reason)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"replayed": len(events) - len(partial.Rejected), "still_rejected": len(partial.Rejected)})
	default:
		writeErr(w, http.StatusBadGateway, "replay failed, dead letters kept: %v", err)
	}
}

// setCursor rewinds or advances a destination, e.g. {"seq": 0} replays the
// retained ledger into a newly added backend.
func (s *Server) setCursor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.destination(name) == nil {
		writeErr(w, http.StatusNotFound, "no destination %q", name)
		return
	}
	var req struct {
		Seq *uint64 `json:"seq"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Seq == nil {
		writeErr(w, http.StatusBadRequest, `body must be {"seq": <number>}`)
		return
	}
	if err := s.spool.SetCursor(name, *req.Seq); err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	log.Printf("[api] cursor for %s set to %d", name, *req.Seq)
	writeJSON(w, http.StatusOK, map[string]any{"destination": name, "cursor": s.spool.Cursor(name)})
}

// --- billing state ---

func (s *Server) billingStates(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enforcement": "disabled", "states": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": s.cfg.EnforcementMode, "states": s.enf.States()})
}

func (s *Server) setBillingState(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil {
		writeErr(w, http.StatusNotFound, "enforcement is disabled")
		return
	}
	var req struct {
		State  enforcement.State `json:"state"`
		Reason string            `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Reason == "" {
		req.Reason = "set by operator"
	}
	tenant := r.PathValue("tenant")
	if err := s.enf.Set(r.Context(), tenant, req.State, req.Reason, "api", "manual"); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, s.enf.StateOf(tenant))
}

// --- webhooks ---

func (s *Server) webhookResult(w http.ResponseWriter, sig enforcement.Signal, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"applied": true, "tenant": sig.Tenant, "state": sig.State})
	case errors.Is(err, enforcement.ErrSignature):
		writeErr(w, http.StatusBadRequest, "%v", err)
	case errors.Is(err, enforcement.ErrIgnored), errors.Is(err, enforcement.ErrUnknownCustomer), errors.Is(err, enforcement.ErrDuplicate):
		writeJSON(w, http.StatusOK, map[string]any{"applied": false, "reason": err.Error()})
	default:
		log.Printf("[api] webhook %s: %v", sig.Type, err)
		writeErr(w, http.StatusInternalServerError, "%v", err) // the provider retries
	}
}

func (s *Server) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil || s.cfg.StripeWebhookSecret == "" {
		writeErr(w, http.StatusNotFound, "stripe webhooks are not enabled (set STRIPE_WEBHOOK_SECRET)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	sig, err := s.enf.HandleStripe(r.Context(), body, r.Header.Get("Stripe-Signature"))
	s.webhookResult(w, sig, err)
}

func (s *Server) metronomeWebhook(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil || s.cfg.MetronomeWebhookSecret == "" {
		writeErr(w, http.StatusNotFound, "metronome webhooks are not enabled (set METRONOME_WEBHOOK_SECRET)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	date := r.Header.Get("X-Metronome-Date")
	if date == "" {
		date = r.Header.Get("Date")
	}
	sig, err := s.enf.HandleMetronome(r.Context(), body, date, r.Header.Get("Metronome-Webhook-Signature"))
	s.webhookResult(w, sig, err)
}
