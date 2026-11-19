// Package api serves vBilling's HTTP surface: health and Prometheus
// metrics, tenant-visible usage line items and exports, the ingest endpoint
// for custom usage emitters, reconciliation against billing backends,
// ledger verification, delivery status, and billing webhooks.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/crosscheck"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/enforcement"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/pricing"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
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

	health  *health.Registry  // nil: no source health
	records *records.Ingester // nil: POST /api/v1/records disabled
	pauses  *pause.Store      // nil: pause spans not exposed
	started time.Time         // records wait for the first discovery after this
	checks  []crosscheck.Check
	pricing *pricing.Service // nil: PRICE_CHECK=off

	limiter *rateLimiter // requests per ingest token
	clients *clientNames // client names per ingest token
	skus    *skuQuota    // new SKUs per source, tenant and day
	priced  pricedSKUs   // SKUs the price check found priced

	timeouts serverTimeouts // see HTTPServer
}

// WithCrossChecks enables GET /api/v1/crosscheck.
func (s *Server) WithCrossChecks(c []crosscheck.Check) *Server { s.checks = c; return s }

// WithHealth exposes source health in the status API and /api/v1/sources,
// and reports usage stored past the SKU cap there.
func (s *Server) WithHealth(h *health.Registry) *Server { s.health = h; s.skus.health = h; return s }

// WithPauses exposes when clusters were paused in GET /api/v1/billing-states.
func (s *Server) WithPauses(p *pause.Store) *Server { s.pauses = p; return s }

// WithRecords enables POST /api/v1/records. Records (pushed or pulled)
// count towards the same SKU caps as ingested events.
func (s *Server) WithRecords(in *records.Ingester) *Server {
	s.records = in
	in.UseSKUGate(s.skus)
	return s
}

// WithPricing enables GET /api/v1/pricing and POST /api/v1/pricing/check.
// SKUs it finds priced never count against the SKU cap (see Run).
func (s *Server) WithPricing(p *pricing.Service) *Server { s.pricing = p; return s }

func New(cfg *config.Config, sp *spool.Spool, disp *pipeline.Dispatcher, tenants *pipeline.TenantRegistry,
	ctrl Controller, enf *enforcement.Enforcer, tel *telemetry.Registry, version string) *Server {
	s := &Server{cfg: cfg, spool: sp, disp: disp, tenants: tenants, ctrl: ctrl, enf: enf, tel: tel, version: version, now: time.Now, started: time.Now(),
		limiter: newRateLimiter(time.Now), clients: newClientNames(), timeouts: defaultTimeouts}
	s.skus = newSKUQuota(func() time.Time { return s.now() })
	s.skus.priced = s.priced.has
	return s
}

// Handler returns the routed handler. Everything but /healthz, /readyz,
// the billing webhooks (signed by the billing platform), ingest (ingest
// tokens) and the dashboard's static page needs the API token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", s.auth(s.metrics))

	mux.HandleFunc("GET /api/v1/status", s.auth(s.status))
	mux.HandleFunc("GET /api/v1/catalog", s.auth(s.catalog))
	mux.HandleFunc("GET /api/v1/tenants", s.auth(s.listTenants))
	mux.HandleFunc("GET /api/v1/usage", s.auth(s.longRoute(s.usage)))
	mux.HandleFunc("GET /api/v1/events", s.auth(s.exportEvents)) // streams: rollingDeadline
	mux.HandleFunc("POST /api/v1/events", s.ingest)
	mux.HandleFunc("POST /api/v1/records", s.ingestRecords)
	mux.HandleFunc("GET /api/v1/sources", s.auth(s.sources))
	mux.HandleFunc("GET /api/v1/crosscheck", s.auth(s.longRoute(s.crossCheck)))
	mux.HandleFunc("GET /api/v1/pricing", s.auth(s.pricingReport))
	mux.HandleFunc("POST /api/v1/pricing/check", s.auth(s.longRoute(s.pricingReport)))
	mux.HandleFunc("GET /api/v1/reconcile", s.auth(s.longRoute(s.reconcile)))
	mux.HandleFunc("GET /api/v1/ledger/verify", s.auth(s.longRoute(s.verify)))
	mux.HandleFunc("GET /api/v1/destinations", s.auth(s.listDestinations))
	mux.HandleFunc("GET /api/v1/destinations/{name}/dead-letters", s.auth(s.deadLetters))
	mux.HandleFunc("POST /api/v1/destinations/{name}/dead-letters/replay", s.auth(s.longRoute(s.replayDeadLetters)))
	mux.HandleFunc("POST /api/v1/destinations/{name}/cursor", s.auth(s.setCursor))
	mux.HandleFunc("GET /api/v1/billing-states", s.auth(s.billingStates))
	mux.HandleFunc("PUT /api/v1/billing-states/{tenant}", s.auth(s.setBillingState))

	mux.HandleFunc("POST /webhooks/stripe", s.stripeWebhook)
	mux.HandleFunc("POST /webhooks/metronome", s.metronomeWebhook)

	sub, _ := fs.Sub(dashboardFS, "dashboard")
	mux.Handle("GET /", dashboardHeaders(http.FileServer(http.FS(sub))))
	return noSniff(mux)
}

// noSniff stops browsers from reading a response as anything but its
// declared type.
func noSniff(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
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

// auth requires the API token on read and admin endpoints. Without a
// configured token nothing passes: the API is never open (PrepareTokens
// generates one at startup when API_TOKEN is not set).
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIToken == "" || !tokenOK(bearer(r), s.cfg.APIToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vbilling"`)
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
	if s.pricing != nil {
		rep := s.pricing.Report()
		out["pricing"] = map[string]any{"mode": rep.Mode, "unpriced": rep.Unpriced, "held_events": rep.HeldEvents}
	}
	if s.health != nil {
		problems := s.health.Problems()
		out["sources"] = s.health.Summary()
		out["healthy"] = len(problems) == 0
		var warnings []string
		for _, p := range problems {
			if len(warnings) == 25 {
				warnings = append(warnings, fmt.Sprintf("... and %d more, see GET /api/v1/sources", len(problems)-25))
				break
			}
			msg := p.Name + ": " + string(p.Status)
			if p.Message != "" {
				msg += ": " + p.Message
			}
			if p.Hint != "" {
				msg += " (hint: " + p.Hint + ")"
			}
			warnings = append(warnings, msg)
		}
		out["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, out)
}

// sources lists every usage source with its last success and last error.
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	if s.health == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sources": []any{}})
		return
	}
	list := s.health.List()
	if kind := r.URL.Query().Get("kind"); kind != "" {
		kept := list[:0]
		for _, src := range list {
			if src.Kind == kind {
				kept = append(kept, src)
			}
		}
		list = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"summary": s.health.Summary(), "sources": list})
}

// crossCheck compares two sources of the same usage (CROSS_CHECKS) per
// tenant over [from, to), by hour (default) or day. Default range: the last
// 24 complete hours.
func (s *Server) crossCheck(w http.ResponseWriter, r *http.Request) {
	end := s.now().UTC().Truncate(time.Hour)
	from, to, err := s.parseRange(r, end.Add(-24*time.Hour), end)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	bucket := time.Hour
	if r.URL.Query().Get("bucket") == "day" {
		bucket = 24 * time.Hour
	}
	rows, err := crosscheck.Evaluate(s.spool, s.checks, from, to, bucket)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if t := r.URL.Query().Get("tenant"); t != "" {
		kept := rows[:0]
		for _, row := range rows {
			if row.Tenant == t {
				kept = append(kept, row)
			}
		}
		rows = kept
	}
	ok := true
	for _, row := range rows {
		ok = ok && row.OK
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": s.checks, "ok": ok, "rows": rows})
}

// pricingReport returns price coverage per destination: whether each billing
// platform prices every billable metric and every recent series (metric,
// SKU, plan). POST runs the checks first. Filters: destination, status.
func (s *Server) pricingReport(w http.ResponseWriter, r *http.Request) {
	if s.pricing == nil {
		writeJSON(w, http.StatusOK, pricing.Report{Mode: pricing.ModeOff, OK: true, Destinations: []pricing.DestReport{}})
		return
	}
	var rep pricing.Report
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		rep = s.pricing.CheckNow(ctx)
	} else {
		rep = s.pricing.Report()
	}
	q := r.URL.Query()
	if dest, status := q.Get("destination"), q.Get("status"); dest != "" || status != "" {
		kept := rep.Destinations[:0]
		for _, d := range rep.Destinations {
			if dest != "" && d.Destination != dest {
				continue
			}
			if status != "" {
				rows := d.Series[:0]
				for _, row := range d.Series {
					if row.Status == status {
						rows = append(rows, row)
					}
				}
				d.Series = rows
			}
			kept = append(kept, d)
		}
		rep.Destinations = kept
	}
	if rep.Destinations == nil {
		rep.Destinations = []pricing.DestReport{}
	}
	writeJSON(w, http.StatusOK, rep)
}

// ingestCaller is who sent an ingest request.
type ingestCaller struct {
	bound string // the tenant a tenant-bound token is bound to; "" for the shared token
	key   string // the token's ID (records.TokenID): rate limiter, client names, SKU caps, record IDs
}

// skuSource is the SKU cap the caller's usage counts against: each
// tenant-bound token has its own, so a tenant's own token never uses up
// what the operator's gateway reports with another token bound to it.
func (c ingestCaller) skuSource() string {
	if c.bound != "" {
		return records.SKUSourceToken(c.key)
	}
	return records.SKUSourceShared
}

// ingestAuth checks an ingest token and its request rate. A token from
// INGEST_TOKENS is bound to a tenant: everything sent with it is billed to
// that tenant. The API token is never an ingest token (PrepareTokens gives
// ingest a token of its own).
func (s *Server) ingestAuth(w http.ResponseWriter, r *http.Request) (ingestCaller, bool) {
	if s.cfg.IngestToken == "" && len(s.cfg.IngestTokens) == 0 {
		writeErr(w, http.StatusForbidden, "ingest is disabled: set INGEST_TOKEN, or INGEST_TOKENS for tokens bound to a tenant")
		return ingestCaller{}, false
	}
	got := bearer(r)
	if s.cfg.IngestToken != "" && tokenOK(got, s.cfg.IngestToken) {
		return ingestCaller{key: limitKey(got)}, s.limitIngest(w, got, false)
	}
	for tok, tenant := range s.cfg.IngestTokens {
		if tokenOK(got, tok) {
			return ingestCaller{bound: tenant, key: limitKey(tok)}, s.limitIngest(w, tok, true)
		}
	}
	if s.cfg.APIToken != "" && tokenOK(got, s.cfg.APIToken) {
		writeErr(w, http.StatusUnauthorized, "the API token is not an ingest token: send the ingest token "+
			"(INGEST_TOKEN, DATA_DIR/ingest-token, or the chart Secret's ingest-token key)")
		return ingestCaller{}, false
	}
	writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
	return ingestCaller{}, false
}

// ingestRecords accepts per-request usage (one record per request) from
// gateways and inference servers. Records are acknowledged only once they
// are on disk; any non-2xx answer means nothing was stored and the emitter
// should retry. ?format= (or X-VBilling-Format) picks the payload format:
// vbilling (default), litellm, or a name from RECORDS_MAPPINGS_FILE.
// recordsStartupGrace bounds how long records wait for the first discovery.
const recordsStartupGrace = 2 * time.Minute

func (s *Server) ingestRecords(w http.ResponseWriter, r *http.Request) {
	if s.records == nil {
		writeErr(w, http.StatusNotFound, "records ingest is not enabled")
		return
	}
	caller, ok := s.ingestAuth(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = r.Header.Get("X-VBilling-Format")
	}
	if format == "" {
		format = records.Native
	}
	if !s.records.HasFormat(format) {
		writeErr(w, http.StatusBadRequest, "%v", records.ErrUnknownFormat{Name: format})
		return
	}
	client, known, done, ok := s.ingestClient(w, r, caller, format)
	if !ok {
		return
	}
	accepted := false
	defer func() { done(accepted) }()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRecordsBody+1))
	if err != nil || len(body) > maxRecordsBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "body must be at most %d bytes", maxRecordsBody)
		return
	}
	// Until the first discovery, gateway identities cannot be mapped to
	// tenants (vbilling.vcluster.com/aliases): ask the emitter to retry
	// rather than bill a raw identity. Tokens bound to a tenant need no
	// mapping. Never wait for longer than a short startup grace.
	if caller.bound == "" && s.ctrl != nil && !s.ctrl.Ready() && time.Since(s.started) < recordsStartupGrace {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusServiceUnavailable, "vBilling is starting and has not discovered tenants yet; retry in a few seconds")
		return
	}
	push := records.Push{Client: client, Bound: caller.bound, Known: known}
	if caller.bound != "" {
		push.Token = caller.key // records sent with a bound token get IDs of the token's own
	}
	res, err := s.records.IngestPush(format, push, body)
	var unknown records.ErrUnknownFormat
	var tooLarge records.ErrTooLarge
	switch {
	case errors.As(err, &unknown):
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	case errors.As(err, &tooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, "%v", err)
		return
	case err != nil && res.Received == 0:
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	case err != nil:
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	accepted = res.Accepted > 0 || res.AlreadyStored > 0
	status := http.StatusOK
	if res.Received > 0 && res.Accepted == 0 && res.AlreadyStored == 0 && res.Skipped == 0 && res.DuplicatesInBatch == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, res)
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
	groupKey := func(e *usage.Event) string {
		vals := make([]string, len(groupBy))
		for i, k := range groupBy {
			vals[i] = field(e, k)
		}
		return strings.Join(append(vals, e.Metric), "\x00")
	}
	repeated, err := s.scanUsage(from, to, func(e *usage.Event) error {
		if e.WindowStart.Before(from) || !e.WindowStart.Before(to) || !keep(e) {
			return nil
		}
		key := groupKey(e)
		a, ok := rows[key]
		if !ok {
			vals := make([]string, len(groupBy))
			for i, k := range groupBy {
				vals[i] = field(e, k)
			}
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
	// Parts of those totals a destination refused for good or holds until
	// priced, by destination: not billed there (yet).
	und := s.undeliveredUsage(from, to, keep, groupKey)
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="usage-%s-%s.csv"`, from.Format("20060102"), to.Format("20060102")))
		cw := csv.NewWriter(w)
		cw.Write(csvRow(append(append([]string{}, groupBy...), "metric", "unit", "quantity", "events", "dead_lettered", "held")...))
		for _, k := range keys {
			a := rows[k]
			cw.Write(csvRow(append(append([]string{}, a.group...), a.metric, a.unit, strconv.FormatFloat(usage.Round(a.quantity), 'f', -1, 64), strconv.Itoa(a.events),
				destinationsCSV(und.dead[k]), destinationsCSV(und.held[k]))...))
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
		if d := byDestination(und.dead[k]); d != nil {
			row["dead_lettered"] = d
		}
		if h := byDestination(und.held[k]); h != nil {
			row["held"] = h
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "group_by": groupBy, "events": total, "repeated_ids": repeated, "rows": out,
		"note": "Unrated usage from the vBilling ledger (and its archive), each event ID counted once. dead_lettered and held are the parts of a row's quantity " +
			"a destination refused for good or holds until priced, by destination. Prices and invoices come from your billing backend."})
}

// csvRow escapes every cell that a spreadsheet would read as a formula
// (usage.CSVCell): tenants, IDs, SKUs and dimensions come from tenants and
// gateways.
func csvRow(cells ...string) []string {
	for i, c := range cells {
		cells[i] = usage.CSVCell(c)
	}
	return cells
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
	extend := s.rollingDeadline(w) // an export streams for as long as it takes
	err = s.spool.ScanAll(from, to, func(e *usage.Event) error {
		extend()
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
			return cw.Write(csvRow(e.ID, e.Tenant, e.Metric, strconv.FormatFloat(e.Quantity, 'f', -1, 64), e.Unit,
				e.WindowStart.Format(time.RFC3339), e.WindowEnd.Format(time.RFC3339), e.Region, e.Project, e.SKU, e.ResourceID, strings.Join(dims, ";"), e.Source))
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
	ingestMaxAge  = 34 * 24 * time.Hour // the strictest backend window (Metronome)
	ingestMaxSkew = 5 * time.Minute
)

// reservedIDPrefixes are the ID namespaces vBilling derives itself:
// usage.DeriveID (vb1_, every event vBilling measures: collector,
// Prometheus, records aggregator, fills), TenantEventID (te1_) and
// IngestEventID (ie1_). An id sent with the shared ingest token is kept as
// is, so it must not start with one of them.
var reservedIDPrefixes = []string{"vb1_", "te1_", "ie1_"}

func reservedID(id string) bool {
	for _, p := range reservedIDPrefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// valueProblems checks the values and dimensions of an ingested event.
// They reach dashboards, exports and billing platforms.
func valueProblems(e *usage.Event) []string {
	var out []string
	for _, f := range []struct{ name, v string }{{"sku", e.SKU}, {"region", e.Region}, {"project", e.Project}, {"resource_id", e.ResourceID},
		{"unit", e.Unit}} {
		if err := usage.CheckValue(f.v); err != nil {
			out = append(out, f.name+" "+err.Error())
		}
	}
	if len(e.Dimensions) > usage.MaxDimensions {
		return append(out, fmt.Sprintf("%d dimensions, more than %d", len(e.Dimensions), usage.MaxDimensions))
	}
	keys := make([]string, 0, len(e.Dimensions))
	for k := range e.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if usage.ReservedDimension(k) {
			out = append(out, fmt.Sprintf("dimension %s is the name of a property of the event itself; rename it", k))
		}
		if err := usage.CheckValue(e.Dimensions[k]); err != nil {
			out = append(out, fmt.Sprintf("dimension %s %v", k, err))
		}
	}
	return out
}

// propertyKeyRe is what the name of an ingested event's property must
// match, like a dimension key.
var propertyKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ownProperties are the properties vBilling sets on ingested events itself.
var ownProperties = map[string]bool{"client_event_id": true}

// propertyProblems checks an ingested event's properties, as sent. They are
// informational, but exports show them and billing platforms receive them
// next to the event's own values: Lago takes them as event properties,
// where one named sku would price usage the SKU cap and the price check
// never saw. A property is a string checked like any value, a number or a
// boolean, under a name like a dimension key that is not one of the
// usage's own properties.
func propertyProblems(e *usage.Event) []string {
	if len(e.Properties) > usage.MaxDimensions {
		return []string{fmt.Sprintf("%d properties, more than %d", len(e.Properties), usage.MaxDimensions)}
	}
	keys := make([]string, 0, len(e.Properties))
	for k := range e.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		switch {
		case !propertyKeyRe.MatchString(k):
			out = append(out, fmt.Sprintf("property name %.64q must match %s", k, propertyKeyRe))
			continue
		case usage.ReservedDimension(k) || ownProperties[k]:
			out = append(out, fmt.Sprintf("property %s is the name of a property vBilling sets itself; rename it", k))
			continue
		}
		switch v := e.Properties[k].(type) {
		case string:
			if err := usage.CheckValue(v); err != nil {
				out = append(out, fmt.Sprintf("property %s %v", k, err))
			}
		case bool, float64:
		default:
			out = append(out, fmt.Sprintf("property %s must be a string, a number or a boolean", k))
		}
	}
	return out
}

// maxRejections bounds the refused events an ingest answer lists;
// rejected_count counts them all.
const maxRejections = records.MaxRejections

// ingest accepts usage from custom emitters: inference gateways (tokens),
// Slurm accounting, storage or network exporters. Events need a declared
// metric (builtin or CUSTOM_METRICS) and a closed window; an omitted ID is
// derived from the event identity, so resends deduplicate.
//
// Ingested events never take an ID vBilling's own sources use: an event's
// own id is scoped to the token (TenantEventID) when the token is bound to a
// tenant, and must not use a reserved prefix otherwise; an event without an
// id gets IngestEventID.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.ingestAuth(w, r)
	if !ok {
		return
	}
	bound := caller.bound
	client, _, done, ok := s.ingestClient(w, r, caller, "api")
	if !ok {
		return
	}
	var accepted []string
	defer func() { done(len(accepted) > 0) }()
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
	source := "ingest:" + client
	boundToken := "" // the bound token's ID (records.TokenID); "" for the shared token
	if bound != "" {
		boundToken = caller.key
	}
	now := s.now().UTC()
	type rejection struct {
		Index int    `json:"index"`
		ID    string `json:"id,omitempty"`
		Error string `json:"error"`
	}
	var valid []usage.Event
	var rejected []rejection
	rejectedCount := 0
	var warnings []string
	derived := map[string]bool{}  // IDs vBilling derived because the event had none
	legacy := map[string]string{} // derived ID -> the ID v0.3.0 gave the same event
	firstAt := map[string]int{}
	overflow := 0
	for i := range events {
		e := events[i]
		e.Source = source
		e.RecordedAt = now
		if e.Region == "" {
			e.Region = s.cfg.Region
		}
		var problems []string
		if bound != "" {
			if e.Tenant != "" && e.Tenant != bound {
				problems = append(problems, fmt.Sprintf("tenant %q does not match the tenant this token is bound to", e.Tenant))
			}
			e.Tenant = bound
		}
		problems = append(problems, propertyProblems(&e)...)
		noID := e.ID == ""
		switch {
		case noID:
		case bound != "":
			// A tenant chooses its own IDs, and so does every other token
			// bound to it, such as the operator's gateway: scope the id to the
			// token and client, as record IDs are, so no other tenant or token
			// can produce it. The id as sent is kept, so it is checked like
			// any value.
			if err := usage.CheckValue(e.ID); err != nil {
				problems = append(problems, "id "+err.Error())
			} else {
				if e.Properties == nil {
					e.Properties = map[string]any{}
				}
				e.Properties["client_event_id"] = e.ID
			}
			id := TenantEventID(bound, caller.key, client, e.ID)
			legacy[id] = v030TenantEventID(bound, client, e.ID)
			e.ID = id
		case reservedID(e.ID):
			problems = append(problems, fmt.Sprintf("id %q starts with a prefix vBilling reserves for the IDs it derives (%s)",
				e.ID, strings.Join(reservedIDPrefixes, ", ")))
		}
		problems = append(problems, valueProblems(&e)...)
		e.Finalize()
		if noID {
			// e.ID is now usage.DeriveID: the ID of the same usage measured by
			// vBilling itself. Move it into the ingest namespace.
			id := IngestEventID(source, bound, boundToken, client, e.ID)
			legacy[id] = e.ID
			e.ID = id
		}
		if j, dup := firstAt[e.ID]; dup && (noID || derived[e.ID]) {
			problems = append(problems, fmt.Sprintf("same tenant, metric, window and dimensions as event %d and no id: "+
				"events without an id are deduplicated by those fields, so separate requests would be counted once. "+
				"Give every event a unique id, or send per-request usage to POST /api/v1/records, which sums it", j))
		}
		if _, seen := firstAt[e.ID]; !seen {
			firstAt[e.ID] = i
		}
		if noID {
			derived[e.ID] = true
		}
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
			if rejectedCount++; len(rejected) < maxRejections {
				rejected = append(rejected, rejection{Index: i, ID: e.ID, Error: strings.Join(problems, "; ")})
			}
			continue
		}
		valid = append(valid, e)
	}
	// An event v0.3.0 stored under another ID (one without an id under its
	// derived ID, one a tenant-bound token sent with an id under the ID
	// scoped to its tenant) is a retry across the upgrade: count it once.
	fresh := valid[:0:0]
	var retried []string
	for _, e := range valid {
		if old := legacy[e.ID]; old != "" && s.spool.IngestedRecently(old) {
			retried = append(retried, e.ID)
			continue
		}
		fresh = append(fresh, e)
	}
	// The SKU cap counts what each kind of token brings in for a tenant on
	// its own: a tenant's bound token never uses up what the operator's
	// gateways report for it.
	capped := map[string]string{} // ID -> tenant, of events stored under OverflowSKU
	for i := range fresh {
		var over bool
		if fresh[i].SKU, over = s.skus.Admit(caller.skuSource(), fresh[i].Tenant, fresh[i].SKU); over {
			capped[fresh[i].ID] = fresh[i].Tenant
		}
	}
	var dups []string
	if len(fresh) > 0 {
		if accepted, dups, err = s.spool.AppendIngest(source, fresh); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "ledger write failed: %v", err)
			return
		}
	}
	overTenants := map[string]int{}
	for _, id := range accepted {
		if t, ok := capped[id]; ok {
			overflow++
			overTenants[t]++
		}
	}
	for t, n := range overTenants {
		s.skus.Overflowed(caller.skuSource(), t, n)
	}
	dups = append(dups, retried...)
	status := http.StatusOK
	if len(valid) == 0 {
		status = http.StatusBadRequest
	}
	if len(accepted) > 0 { // a series per client only once it stored usage
		s.tel.Add("vbilling_ingested_events_total", "Events accepted through the ingest API.", map[string]string{"client": client}, float64(len(accepted)))
	}
	// rejected lists the first maxRejections refused events.
	out := map[string]any{"accepted": len(accepted), "duplicates": len(dups), "rejected": rejected, "rejected_count": rejectedCount, "ids": accepted}
	silent := 0
	for _, id := range dups {
		if derived[id] {
			silent++
		}
	}
	if silent > 0 {
		warnings = append(warnings, fmt.Sprintf("%d event(s) without an id matched usage already in the ledger and were not counted again. "+
			"If they are separate requests rather than retries, give each one an id, or send per-request usage to POST /api/v1/records", silent))
	}
	if overflow > 0 {
		warnings = append(warnings, fmt.Sprintf("%d event(s) are billed under SKU %q: this kind of ingest token brought in more than %d new SKUs for their tenant today",
			overflow, OverflowSKU, maxSKUsPerTenantPerDay))
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	writeJSON(w, status, out)
}

// TenantEventID is the ledger ID of an event a tenant-bound token sent with
// its own id. token is the token's ID (records.TokenID): the same token,
// client and id always give the same ID, so retries deduplicate, while no
// other tenant, collector or token bound to the same tenant can produce it.
// A tenant chooses its client names and can learn the IDs of the events a
// gateway that serves it sends, and billing keeps the first copy of an ID.
func TenantEventID(tenant, token, client, id string) string {
	sum := sha256.Sum256([]byte("token\x00" + token + "\x00" + tenant + "\x00" + client + "\x00" + id))
	return "te1_" + hex.EncodeToString(sum[:16])
}

// v030TenantEventID is the ID v0.3.0 gave an event a tenant-bound token
// sent with its own id, scoped to the tenant and client only. A retry of
// such an event after the upgrade is recognized by it.
func v030TenantEventID(tenant, client, id string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + client + "\x00" + id))
	return "te1_" + hex.EncodeToString(sum[:16])
}

// IngestEventID is the ledger ID of an event sent without an id. derived is
// the event's usage.DeriveID, which every event vBilling measures itself
// (collector, Prometheus, records aggregator, fills) uses as its ID. The ingest
// source, the tenant the token is bound to and the token's ID ("" and "" for
// the shared ingest token) and the client are hashed with it, so a retry gets
// the same ID while an ingested event can never take the ID of vBilling's
// own usage, nor of another tenant's, token's or client's.
func IngestEventID(source, bound, token, client, derived string) string {
	sum := sha256.Sum256([]byte("ingest\x00" + source + "\x00" + bound + "\x00" + token + "\x00" + client + "\x00" + derived))
	return "ie1_" + hex.EncodeToString(sum[:16])
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
	// Each event ID once, as billing platforms keep one copy; dead letters
	// are parked once per event ID.
	_, err := s.scanUsage(from, to, func(e *usage.Event) error {
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

// verify checks the retained ledger's hash chain. With ?archive=true it starts
// from the oldest archived segment (ARCHIVE_DIR) and continues into the
// retained ledger as one chain.
func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	if v := r.URL.Query().Get("archive"); v != "" {
		withArchive, err := strconv.ParseBool(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "archive must be true or false")
			return
		}
		if withArchive {
			rep, err := s.spool.VerifyWithArchive()
			if err != nil {
				writeErr(w, http.StatusBadRequest, "%v", err)
				return
			}
			status := http.StatusOK
			if !rep.OK {
				status = http.StatusConflict
			}
			writeJSON(w, status, rep)
			return
		}
	}
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
	out := map[string]any{"mode": s.cfg.EnforcementMode, "states": s.enf.States(), "pauses": s.enf.Pauses()}
	if s.pauses != nil {
		// When each cluster was paused (to the second): capacity is not
		// billed for these spans, except what PAUSE_KEEP_BILLING keeps.
		out["pause_spans"] = s.pauses.All()
		out["pause_keep_billing"] = s.cfg.PauseKeepBilling
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setBillingState(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil {
		writeErr(w, http.StatusNotFound, "enforcement is disabled")
		return
	}
	var req struct {
		State  enforcement.State `json:"state"`
		Reason string            `json:"reason"`
		// Hold names the reason class: "operator" (default) or any other,
		// e.g. "support" or "abuse", which no webhook touches and only an
		// operator clears; or "billing", the hold billing webhooks raise and
		// a payment clears once nothing is owed (for a portal that wants a
		// payment to lift its hold).
		Hold string `json:"hold"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Hold == "" {
		req.Hold = enforcement.HoldOperator
	}
	if req.Reason == "" {
		req.Reason = "set by operator"
	}
	tenant := r.PathValue("tenant")
	if err := s.enf.SetHold(r.Context(), tenant, req.Hold, req.State, req.Reason, "api", "manual"); err != nil {
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
