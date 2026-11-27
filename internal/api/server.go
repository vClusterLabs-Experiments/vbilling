// Package api serves vBilling's HTTP surface: health and Prometheus
// metrics, tenant-visible usage line items and exports, the ingest endpoint
// for custom usage emitters, reconciliation against billing platforms,
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
	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
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
	started time.Time         // pushed usage waits for the first discovery after this
	checks  []crosscheck.Check
	pricing *pricing.Service // nil: PRICE_CHECK=off

	limiter *rateLimiter // requests per ingest token
	clients *clientNames // client names per ingest token
	skus    *skuQuota    // new SKUs per source, tenant and day
	priced  pricedSKUs   // SKUs the price check found priced

	// clusters knows the clusters pushed usage may name (pushed events, and
	// records once WithRecords shares it).
	clusters *safeguard.Clusters

	timeouts serverTimeouts // see HTTPServer
}

// WithCrossChecks enables GET /api/v1/crosscheck.
func (s *Server) WithCrossChecks(c []crosscheck.Check) *Server { s.checks = c; return s }

// WithHealth exposes source health in the status API and /api/v1/sources,
// and reports usage stored past the SKU cap there.
func (s *Server) WithHealth(h *health.Registry) *Server { s.health = h; s.skus.health = h; return s }

// WithPauses exposes when clusters were paused in GET /api/v1/billing-states.
func (s *Server) WithPauses(p *pause.Store) *Server { s.pauses = p; return s }

// WithRecords enables POST /api/v1/records, and makes both push paths
// follow one set of rules. Records (pushed or pulled) count towards the
// same SKU caps as ingested events, may name the same clusters, and are
// refused past the same destination age limit; pushed events resolve their
// tenant as records do (see pushedTenant).
func (s *Server) WithRecords(in *records.Ingester) *Server {
	s.records = in
	in.UseSKUGate(s.skus)
	in.UseClusters(s.clusters)
	if limit, by := s.oldestUsage(); by != "" {
		in.LimitAge(limit, by)
	}
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
	// What vBilling knows about clusters, for usage that names one: the
	// clusters discovery lists now, and those the usage ledger still holds
	// measured usage of (it keeps cfg.Retention). A cluster deleted before a
	// window was collected (the window's end plus the collection delay) is
	// not metered for it, so it may have existed that much longer than its
	// last metered window.
	s.clusters = safeguard.NewClusters(sp, safeguard.ClusterOptions{
		Live: s.liveClusters, Grace: cfg.CollectionInterval + cfg.CollectionDelay, Remember: cfg.Retention, Now: func() time.Time { return s.now() },
	})
	return s
}

// Handler returns the routed handler. Everything but /healthz, /readyz,
// the billing webhooks (signed by the billing platform), ingest (ingest
// tokens; the API token only for an operator adjustment) and the
// dashboard's static page needs the API token.
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
	mux.HandleFunc("POST /api/v1/destinations/{name}/cursor", s.auth(s.longRoute(s.setCursor)))
	mux.HandleFunc("POST /api/v1/destinations/{name}/pause", s.auth(s.pauseDestination))
	mux.HandleFunc("POST /api/v1/destinations/{name}/resume", s.auth(s.resumeDestination))
	mux.HandleFunc("POST /api/v1/destinations/{name}/start", s.auth(s.longRoute(s.startDestination)))
	mux.HandleFunc("GET /api/v1/destinations/{name}/delivery/{id}", s.auth(s.deliveryReceipt))
	mux.HandleFunc("PUT /api/v1/destinations/{name}/delivery/{id}", s.auth(s.resolveDelivery))
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
		"metering_source":  s.cfg.MeteringSource,
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
// platform prices every billable metric and every recent series (plan,
// metric, SKU and the values of the other pricing keys), one row per series.
// POST runs the checks first. Filters: destination, status. With
// PRICE_CHECK=off the answer says, in its note, that nothing is checked.
func (s *Server) pricingReport(w http.ResponseWriter, r *http.Request) {
	if s.pricing == nil {
		writeJSON(w, http.StatusOK, pricing.Off())
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
	bound string // the tenant a tenant-bound token is bound to; "" for the shared token and the operator
	key   string // the token's ID (records.TokenID): rate limiter, client names, SKU caps, record IDs
	// operator: the API token, sending an operator adjustment
	// (POST /api/v1/events?operator_adjustment=true).
	operator bool
}

// skuSourceOperator is the SKU cap operator adjustments count against.
const skuSourceOperator = "operator adjustments"

// skuSource is the SKU cap the caller's usage counts against: each
// tenant-bound token has its own, so a tenant's own token never uses up
// what the operator's gateway reports with another token bound to it.
func (c ingestCaller) skuSource() string {
	switch {
	case c.operator:
		return skuSourceOperator
	case c.bound != "":
		return records.SKUSourceToken(c.key)
	}
	return records.SKUSourceShared
}

// The ingest endpoints, as the endpoint label of
// vbilling_ingest_requests_refused_total.
const (
	endpointEvents  = "events"
	endpointRecords = "records"
)

// Why an ingest request is refused as a whole, before any event or record
// in it is stored: the reason label of
// vbilling_ingest_requests_refused_total. Events and records refused one by
// one inside a request that was read are counted by reason too, in
// vbilling_ingest_events_rejected_total and vbilling_records_rejected_total
// (records.Reason).
const (
	refusedDisabled     = "disabled"           // 403, 404: ingest, or records ingest, is not enabled
	refusedUnauthorized = "unauthorized"       // 401, 403: no token, a wrong one, or not the token the request needs
	refusedRateLimited  = "rate_limited"       // 429: the token is over its request rate
	refusedClient       = "bad_client"         // 400: X-VBilling-Client is malformed, or one name too many for the token
	refusedFormat       = "unknown_format"     // 400: records in a format that is not configured
	refusedTooLarge     = "too_large"          // 413: the body, or what it would store, is over the limit
	refusedBadRequest   = "bad_request"        // 400: not JSON, no events, too many events, a malformed parameter
	refusedStarting     = "starting"           // 503 with Retry-After: tenants or clusters are not known yet
	refusedLedger       = "ledger_unavailable" // 503: the ledger could not be written
)

// refuse answers an ingest request that is refused as a whole, and counts
// it. Every refusal of the ingest endpoints goes through here, or is an
// event or record rejected by reason.
func (s *Server) refuse(w http.ResponseWriter, endpoint, reason string, status int, format string, args ...any) {
	s.tel.Add("vbilling_ingest_requests_refused_total", "Ingest requests refused as a whole, by endpoint and reason: nothing in them was stored.",
		map[string]string{"endpoint": endpoint, "reason": reason}, 1)
	writeErr(w, status, format, args...)
}

// operatorAdjustment is the query parameter of POST /api/v1/events that
// makes a request an operator adjustment, and the property vBilling marks
// the events of such a request with.
const operatorAdjustment = "operator_adjustment"

// ingestAuth checks an ingest token and its request rate. A token from
// INGEST_TOKENS is bound to a tenant: everything sent with it is billed to
// that tenant. The API token is never an ingest token (PrepareTokens gives
// ingest a token of its own): gateways must not hold it.
func (s *Server) ingestAuth(w http.ResponseWriter, r *http.Request, endpoint string) (ingestCaller, bool) {
	if s.cfg.IngestToken == "" && len(s.cfg.IngestTokens) == 0 {
		s.refuse(w, endpoint, refusedDisabled, http.StatusForbidden, "ingest is disabled: set INGEST_TOKEN, or INGEST_TOKENS for tokens bound to a tenant")
		return ingestCaller{}, false
	}
	got := bearer(r)
	if c, ok := s.ingestToken(got); ok {
		return c, s.limitIngest(w, got, c.bound != "", endpoint)
	}
	if s.cfg.APIToken != "" && tokenOK(got, s.cfg.APIToken) {
		s.refuse(w, endpoint, refusedUnauthorized, http.StatusUnauthorized, "the API token is not an ingest token: send the ingest token "+
			"(INGEST_TOKEN, DATA_DIR/ingest-token, or the chart Secret's ingest-token key). "+
			"To correct usage as the operator, send the API token to POST /api/v1/events?%s=true", operatorAdjustment)
		return ingestCaller{}, false
	}
	s.refuse(w, endpoint, refusedUnauthorized, http.StatusUnauthorized, "missing or invalid bearer token")
	return ingestCaller{}, false
}

// ingestToken returns who holds an ingest token: the shared one, or one
// bound to a tenant.
func (s *Server) ingestToken(got string) (ingestCaller, bool) {
	if s.cfg.IngestToken != "" && tokenOK(got, s.cfg.IngestToken) {
		return ingestCaller{key: limitKey(got)}, true
	}
	for tok, tenant := range s.cfg.IngestTokens {
		if tokenOK(got, tok) {
			return ingestCaller{bound: tenant, key: limitKey(tok)}, true
		}
	}
	return ingestCaller{}, false
}

// eventsAuth checks who sends events. Ingest tokens send usage under the
// metrics emitters may write. With ?operator_adjustment=true the request
// is an operator's own correction, which may go under any declared metric,
// the ones vBilling measures itself included: it takes the API token, never
// an ingest token, and every event it stores is marked as one. The API
// token alone, without the parameter, stays refused (ingestAuth): a gateway
// configured with it by mistake must not pass for the operator.
func (s *Server) eventsAuth(w http.ResponseWriter, r *http.Request) (ingestCaller, bool) {
	v := r.URL.Query().Get(operatorAdjustment)
	if v == "" {
		return s.ingestAuth(w, r, endpointEvents)
	}
	adjust, err := strconv.ParseBool(v)
	if err != nil {
		s.refuse(w, endpointEvents, refusedBadRequest, http.StatusBadRequest, "%s must be true or false", operatorAdjustment)
		return ingestCaller{}, false
	}
	if !adjust {
		return s.ingestAuth(w, r, endpointEvents)
	}
	got := bearer(r)
	if s.cfg.APIToken != "" && tokenOK(got, s.cfg.APIToken) {
		return ingestCaller{operator: true, key: limitKey(got)}, s.limitIngest(w, got, false, endpointEvents)
	}
	if _, ok := s.ingestToken(got); ok {
		s.refuse(w, endpointEvents, refusedUnauthorized, http.StatusForbidden, "an operator adjustment (%s=true) needs the API token: "+
			"an ingest token sends usage under metric codes of its own only", operatorAdjustment)
		return ingestCaller{}, false
	}
	s.refuse(w, endpointEvents, refusedUnauthorized, http.StatusUnauthorized, "missing or invalid bearer token")
	return ingestCaller{}, false
}

// ingestStartupGrace bounds how long pushed usage waits for the first
// discovery.
const ingestStartupGrace = 2 * time.Minute

// starting reports whether vBilling has not discovered tenants and clusters
// yet and may still do so soon: until the first discovery, or the end of a
// short startup grace, it cannot tell which tenant a pushed value means
// (aliases come from clusters) or whether a cluster exists, so it asks
// emitters to retry rather than refuse or bill a raw identity. It never
// waits for longer than the grace.
func (s *Server) starting() bool {
	return s.ctrl != nil && !s.ctrl.Ready() && time.Since(s.started) < ingestStartupGrace
}

// pushedTenant resolves the tenant value of pushed usage to the tenant it
// bills to, and reports whether vBilling knows it. Both push paths use the
// records ingester's resolver (the controller's: a tenant vBilling knows,
// or an alias of one), so they know the same tenants. Without records it
// knows the tenants in the registry and those ingest tokens are bound to.
func (s *Server) pushedTenant(v string) (tenant string, known bool) {
	if s.records != nil {
		return s.records.ResolveTenant(v)
	}
	if s.tenants != nil {
		if _, ok := s.tenants.Get(v); ok {
			return v, true
		}
	}
	for _, bound := range s.cfg.IngestTokens {
		if bound == v {
			return v, true
		}
	}
	return v, false
}

// rejectUnknownTenants reports whether pushed usage for a tenant vBilling
// does not know is refused (RECORDS_UNKNOWN_TENANTS=reject) rather than
// accepted and reported (accept, the default): one setting for both push
// paths.
func (s *Server) rejectUnknownTenants() bool {
	if s.records != nil {
		return s.records.RejectsUnknownTenants()
	}
	return s.cfg.RecordsRejectUnknown
}

// liveClusters lists the clusters discovery knows now, for safeguard.Clusters.
// It is not settled while vBilling is starting.
func (s *Server) liveClusters() ([]safeguard.LiveCluster, bool) {
	if s.ctrl == nil {
		return nil, true
	}
	if !s.ctrl.Ready() {
		return nil, !s.starting()
	}
	src := discovery.TenantSource(s.cfg.TenantSource)
	clusters := s.ctrl.Clusters()
	out := make([]safeguard.LiveCluster, len(clusters))
	for i := range clusters {
		out[i] = safeguard.LiveCluster{ID: clusters[i].ExternalID(), Tenant: clusters[i].TenantID(src), Created: clusters[i].CreatedAt}
	}
	return out, true
}

// oldestUsage returns how old pushed usage may be, and the destination that
// sets the limit: the tightest limit a configured destination declares
// (destinations.AgeLimiter), which would dead-letter anything older. It is
// never more than ingestMaxAge, the limit when no destination sets a
// tighter one (by is "" then).
func (s *Server) oldestUsage() (limit time.Duration, by string) {
	limit = ingestMaxAge
	if s.disp == nil {
		return limit, ""
	}
	if l, name, ok := destinations.OldestUsage(s.disp.Destinations()); ok && l < limit {
		return l, name
	}
	return limit, ""
}

// ingestRecords accepts per-request usage (one record per request) from
// gateways and inference servers. Records are acknowledged only once they
// are on disk; any non-2xx answer means nothing was stored and the emitter
// should retry. ?format= (or X-VBilling-Format) picks the payload format:
// vbilling (default), litellm, or a name from RECORDS_MAPPINGS_FILE.
func (s *Server) ingestRecords(w http.ResponseWriter, r *http.Request) {
	const ep = endpointRecords
	if s.records == nil {
		s.refuse(w, ep, refusedDisabled, http.StatusNotFound, "records ingest is not enabled")
		return
	}
	caller, ok := s.ingestAuth(w, r, ep)
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
		s.refuse(w, ep, refusedFormat, http.StatusBadRequest, "%v", records.ErrUnknownFormat{Name: format})
		return
	}
	client, known, done, ok := s.ingestClient(w, r, caller, format, ep)
	if !ok {
		return
	}
	accepted := false
	defer func() { done(accepted) }()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRecordsBody+1))
	if err != nil || len(body) > maxRecordsBody {
		s.refuse(w, ep, refusedTooLarge, http.StatusRequestEntityTooLarge, "body must be at most %d bytes", maxRecordsBody)
		return
	}
	// Until the first discovery, gateway identities cannot be mapped to
	// tenants (vbilling.vcluster.com/aliases): ask the emitter to retry
	// rather than bill a raw identity. Tokens bound to a tenant need no
	// mapping. Never wait for longer than a short startup grace.
	if caller.bound == "" && s.starting() {
		s.retryLater(w, ep, "vBilling is starting and has not discovered tenants yet; retry in a few seconds")
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
		s.refuse(w, ep, refusedFormat, http.StatusBadRequest, "%v", err)
		return
	case errors.As(err, &tooLarge):
		s.refuse(w, ep, refusedTooLarge, http.StatusRequestEntityTooLarge, "%v", err)
		return
	case errors.Is(err, safeguard.ErrNotReady):
		// A record names a cluster vBilling cannot check yet (it is starting,
		// or cannot read its ledger): nothing was stored.
		s.retryLater(w, ep, "%v", err)
		return
	case err != nil && res.Received == 0:
		s.refuse(w, ep, refusedBadRequest, http.StatusBadRequest, "%v", err)
		return
	case err != nil:
		s.refuse(w, ep, refusedLedger, http.StatusServiceUnavailable, "%v", err)
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
			"a destination refused for good or holds until priced, by destination. Prices and invoices come from your billing platform."})
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
	// ingestMaxAge is the oldest pushed usage vBilling takes when no
	// destination sets a tighter limit (oldestUsage): 34 days, the window
	// of the strictest billing platform.
	ingestMaxAge  = 34 * 24 * time.Hour
	ingestMaxSkew = 5 * time.Minute
)

// retryLater answers 503 with Retry-After: vBilling cannot judge the
// request yet, nothing of it was stored, and the emitter sends it again.
func (s *Server) retryLater(w http.ResponseWriter, endpoint, format string, args ...any) {
	w.Header().Set("Retry-After", "5")
	s.refuse(w, endpoint, refusedStarting, http.StatusServiceUnavailable, format, args...)
}

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
// No emitter can send one, so an event that carries one has vBilling's
// word for it: operator_adjustment marks what the operator pushed with the
// API token.
var ownProperties = map[string]bool{"client_event_id": true, operatorAdjustment: true}

// operatorIDScope takes the place of a token's ID in the IDs of operator
// adjustments sent without an id (IngestEventID): no ingest token has it,
// so an adjustment never takes the ID of an event a gateway sent, nor the
// other way round.
const operatorIDScope = "operator"

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
// Slurm accounting, storage or network exporters. An event needs a closed
// window and a metric an emitter may write: one declared for pushed usage
// (CUSTOM_METRICS_FILE), never one vBilling measures itself or builds from
// records (safeguard.PushedMetric). Its tenant is resolved as a record's
// is: a tenant vBilling does not know is refused or reported as
// RECORDS_UNKNOWN_TENANTS says, on both push paths alike. An event that
// names a cluster (the tenant_cluster dimension) names one of its tenant,
// for a window in which it existed (safeguard.Clusters). An omitted ID is
// derived from the event identity, so resends deduplicate.
//
// Ingested events never take an ID vBilling's own sources use: an event's
// own id is scoped to the token (TenantEventID) when the token is bound to a
// tenant, and must not use a reserved prefix otherwise; an event without an
// id gets IngestEventID.
//
// An operator adjustment (eventsAuth) follows the same rules but the one on
// metrics: it may go under any declared metric, and every event it stores
// carries the property operator_adjustment.
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	const ep = endpointEvents
	caller, ok := s.eventsAuth(w, r)
	if !ok {
		return
	}
	bound := caller.bound
	defaultClient := "api"
	if caller.operator {
		defaultClient = "operator"
	}
	client, known, done, ok := s.ingestClient(w, r, caller, defaultClient, ep)
	if !ok {
		return
	}
	var accepted []string
	defer func() { done(len(accepted) > 0) }()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBody+1))
	if err != nil || len(body) > maxIngestBody {
		s.refuse(w, ep, refusedTooLarge, http.StatusRequestEntityTooLarge, "body must be at most %d bytes", maxIngestBody)
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
		s.refuse(w, ep, refusedBadRequest, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if len(events) == 0 || len(events) > maxIngestEvents {
		s.refuse(w, ep, refusedBadRequest, http.StatusBadRequest, "send 1 to %d events", maxIngestEvents)
		return
	}
	// Until the first discovery, a tenant value cannot be resolved (aliases
	// come from clusters) and no tenant is known: ask the emitter to retry
	// rather than refuse its usage or bill a raw identity, as records do.
	// Tokens bound to a tenant need no resolving.
	if bound == "" && s.starting() {
		s.retryLater(w, ep, "vBilling is starting and has not discovered tenants yet; retry in a few seconds")
		return
	}
	source := "ingest:" + client
	// idScope keeps the IDs of events sent without an id apart: the bound
	// token's ID (records.TokenID), "" for the shared token, and a scope of
	// their own for operator adjustments.
	idScope := ""
	switch {
	case bound != "":
		idScope = caller.key
	case caller.operator:
		idScope = operatorIDScope
	}
	now := s.now().UTC()
	oldest, oldestBy := s.oldestUsage()
	tooOld := "window_start is older than 34 days"
	if oldestBy != "" {
		// The destination would dead-letter the event: say so now.
		tooOld = fmt.Sprintf("window_start is older than %s, the oldest usage destination %s accepts", safeguard.Age(oldest), oldestBy)
	}
	rejectUnknown := s.rejectUnknownTenants()
	var valid []usage.Event
	var rejected []records.Rejection
	rejectedBy := map[records.Reason]int{}
	rejectedCount := 0
	var warnings []string
	derived := map[string]bool{}     // IDs vBilling derived because the event had none
	legacy := map[string]string{}    // derived ID -> the ID v0.3.0 gave the same event
	unknownOf := map[string]string{} // ID -> the tenant of an event billed to a tenant vBilling does not know
	firstAt := map[string]int{}
	waiting := "" // a cluster vBilling cannot check yet
	overflow := 0
	for i := range events {
		e := events[i]
		e.Source = source
		e.RecordedAt = now
		if e.Region == "" {
			e.Region = s.cfg.Region
		}
		var problems records.Problems
		// billTo is the tenant the event bills to. e.Tenant stays the value
		// as sent until the event has its ID, so an event keeps its ID when
		// an alias is added or changed, as a record does.
		billTo, tenantOK, tenantKnown := e.Tenant, true, true
		switch {
		case bound != "":
			if e.Tenant != "" && e.Tenant != bound {
				problems.Add(records.ReasonTenant, fmt.Sprintf("tenant %q does not match the tenant this token is bound to", e.Tenant))
				tenantOK = false
			}
			e.Tenant, billTo = bound, bound
		case e.Tenant == "" || len(e.Tenant) > 128:
			tenantOK = false // Validate says so below
		default:
			t, isTenant := s.pushedTenant(e.Tenant)
			switch {
			case isTenant:
				billTo = t
			case rejectUnknown:
				problems.Add(records.ReasonTenant, fmt.Sprintf("unknown tenant %q: map it with the vbilling.vcluster.com/aliases annotation of a cluster of the tenant, "+
					"send a tenant ID vBilling knows (GET /api/v1/tenants), or use an ingest token bound to the tenant (INGEST_TOKENS)", e.Tenant))
				tenantOK = false
			default:
				tenantKnown = false // accepted and reported, as a record is
			}
		}
		for _, msg := range propertyProblems(&e) {
			problems.Add(records.ReasonInvalid, msg)
		}
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
				problems.Add(records.ReasonInvalid, "id "+err.Error())
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
			problems.Add(records.ReasonInvalid, fmt.Sprintf("id %q starts with a prefix vBilling reserves for the IDs it derives (%s)",
				e.ID, strings.Join(reservedIDPrefixes, ", ")))
		}
		if caller.operator {
			if e.Properties == nil {
				e.Properties = map[string]any{}
			}
			e.Properties[operatorAdjustment] = true
		}
		for _, msg := range valueProblems(&e) {
			problems.Add(records.ReasonInvalid, msg)
		}
		e.Finalize()
		if noID {
			// e.ID is now usage.DeriveID: the ID of the same usage measured by
			// vBilling itself. Move it into the ingest namespace.
			id := IngestEventID(source, bound, idScope, client, e.ID)
			if !caller.operator {
				legacy[id] = e.ID // v0.3.0 had no operator adjustments
			}
			e.ID = id
		}
		e.Tenant = billTo
		if j, dup := firstAt[e.ID]; dup && (noID || derived[e.ID]) {
			problems.Add(records.ReasonDuplicate, fmt.Sprintf("same tenant, metric, window and dimensions as event %d and no id: "+
				"events without an id are deduplicated by those fields, so separate requests would be counted once. "+
				"Give every event a unique id, or send per-request usage to POST /api/v1/records, which sums it", j))
		}
		if _, seen := firstAt[e.ID]; !seen {
			firstAt[e.ID] = i
		}
		if noID {
			derived[e.ID] = true
		}
		def, declared := usage.Lookup(e.Metric)
		switch {
		case !declared:
			problems.Add(records.ReasonUnknownMetric, fmt.Sprintf("unknown metric %q (declare it in CUSTOM_METRICS)", e.Metric))
		case caller.operator:
			// The operator's own correction may go under any declared metric.
		default:
			if err := safeguard.PushedMetric(def, safeguard.PathEvents); err != nil {
				problems.Add(records.ReasonReservedMetric, err.Error())
			}
		}
		if e.WindowEnd.After(now.Add(ingestMaxSkew)) {
			problems.Add(records.ReasonInFuture, "window_end is in the future")
		}
		if e.WindowStart.Before(now.Add(-oldest)) {
			problems.Add(records.ReasonTooOld, tooOld)
		}
		if err := e.Validate(); err != nil {
			reason := records.ReasonInvalid
			if e.Tenant == "" || len(e.Tenant) > 128 {
				reason = records.ReasonTenant
			}
			problems.Add(reason, err.Error())
		}
		// The cluster is checked last: while vBilling cannot check it yet, an
		// event that is refused anyway needs no retry.
		if cluster := e.Dim(usage.DimTenantCluster); cluster != "" && tenantOK && e.WindowEnd.After(e.WindowStart) {
			switch err := s.clusters.Check(billTo, cluster, e.WindowStart, e.WindowEnd); {
			case err == nil:
			case !errors.Is(err, safeguard.ErrNotReady):
				problems.Add(records.ReasonCluster, err.Error())
			case !problems.Any():
				waiting = cluster
			}
		}
		if problems.Any() {
			rejectedBy[problems.Reason()]++
			if rejectedCount++; len(rejected) < maxRejections {
				rejected = append(rejected, records.Rejection{Index: i, ID: e.ID, Reason: problems.Reason(), Error: problems.String()})
			}
			continue
		}
		if !tenantKnown {
			unknownOf[e.ID] = e.Tenant
		}
		valid = append(valid, e)
	}
	if waiting != "" {
		// Nothing is stored: the emitter sends the request again, and gets a
		// definite answer for every event once vBilling knows its clusters.
		s.retryLater(w, ep, "%v (tenant_cluster %q): retry in a few seconds", safeguard.ErrNotReady, waiting)
		return
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
			s.refuse(w, ep, refusedLedger, http.StatusServiceUnavailable, "ledger write failed: %v", err)
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
	// Every event is counted, whoever sent it. The counters name the client
	// only once a request of it stored usage (records.UnacceptedClient), so
	// refused requests never add series.
	labelled := records.UnacceptedClient
	if known || len(accepted) > 0 {
		labelled = client
	}
	s.tel.Add("vbilling_ingest_events_received_total", "Events received by the ingest API.", map[string]string{"client": labelled}, float64(len(events)))
	for _, reason := range records.Reasons {
		if n := rejectedBy[reason]; n > 0 {
			s.tel.Add("vbilling_ingest_events_rejected_total", "Events the ingest API refused, by reason.",
				map[string]string{"client": labelled, "reason": string(reason)}, float64(n))
		}
	}
	if len(accepted) > 0 {
		s.tel.Add("vbilling_ingested_events_total", "Events accepted through the ingest API.", map[string]string{"client": client}, float64(len(accepted)))
	}
	// rejected lists the first maxRejections refused events.
	out := map[string]any{"accepted": len(accepted), "duplicates": len(dups), "rejected": rejected, "rejected_count": rejectedCount, "ids": accepted}
	if caller.operator {
		out[operatorAdjustment] = true
		if len(accepted) > 0 {
			log.Printf("[api] operator adjustment (client %s): %d event(s) stored, each marked with the %s property", client, len(accepted), operatorAdjustment)
		}
	}
	// Events for tenants vBilling does not know are stored only with
	// RECORDS_UNKNOWN_TENANTS=accept, and reported as records for them are.
	if unknown := unknownTenants(accepted, unknownOf); len(unknown) > 0 {
		out["unknown_tenants"] = unknown
		list := listSome(unknown, 20)
		warnings = append(warnings, fmt.Sprintf("billed to tenants vBilling does not know (%s); send a tenant ID vBilling knows (GET /api/v1/tenants), "+
			"or map these values with the vbilling.vcluster.com/aliases annotation", list))
		if s.health != nil {
			s.health.Set(health.Name("ingest", client, "unknown-tenants"), "ingest", client, health.Empty,
				"events for unknown tenants: "+list, "map them with the vbilling.vcluster.com/aliases annotation, or refuse them with RECORDS_UNKNOWN_TENANTS=reject")
		}
	}
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

// unknownTenants lists, sorted, the tenants vBilling does not know that the
// stored events (accepted, by ID) are billed to.
func unknownTenants(accepted []string, unknownOf map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range accepted {
		if t, ok := unknownOf[id]; ok && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// listSome joins the first n of list, and says how many more there are.
func listSome(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(list[:n], ", "), len(list)-n)
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
// the shared ingest token, "" and operatorIDScope for an operator
// adjustment) and the client are hashed with it, so a retry gets the same ID
// while an ingested event can never take the ID of vBilling's own usage, nor
// of another tenant's, token's or client's.
func IngestEventID(source, bound, token, client, derived string) string {
	sum := sha256.Sum256([]byte("ingest\x00" + source + "\x00" + bound + "\x00" + token + "\x00" + client + "\x00" + derived))
	return "ie1_" + hex.EncodeToString(sum[:16])
}

// --- reconciliation ---

type reconcileRow struct {
	Tenant string  `json:"tenant"`
	Metric string  `json:"metric"`
	Ledger float64 `json:"ledger"`
	// BeforeStart is the part of Ledger that was recorded before the
	// destination started (reconcileResult.Start). The destination was never
	// meant to receive it: it is not expected, not missing, and never makes
	// the row a mismatch. Always 0 for a destination with no recorded start.
	BeforeStart float64 `json:"before_start"`
	// DeadLettered is what the destination refused of the usage it was meant
	// to receive. Expected = Ledger - BeforeStart - DeadLettered.
	DeadLettered float64 `json:"dead_lettered"`
	Expected     float64 `json:"expected"`
	Recorded     float64 `json:"recorded"`
	Difference   float64 `json:"difference"`
	Match        bool    `json:"match"`
	// Note says why the backend has no total for the tenant, when it says,
	// or that it also holds usage from before the destination's start.
	Note string `json:"note,omitempty"`
}

type reconcileResult struct {
	Destination string `json:"destination"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
	// Start is where the destination started: the comparison covers the
	// usage recorded after it. Absent for a destination created before
	// starts were recorded, which is compared from the beginning.
	Start *spool.DestinationStart `json:"start,omitempty"`
	Rows  []reconcileRow          `json:"rows"`
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
	dls, _ := s.spool.DeadLetters(d.Name())
	// Where the destination started. Usage recorded before that was never
	// meant for it: it is reported as before_start, not as missing. A
	// destination with no recorded start is compared from the beginning.
	start := s.spool.DestinationState(d.Name()).Start
	var before map[key]float64
	if start == nil {
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
		for i := range dls {
			if e := &dls[i].Event; in(e) {
				k := key{e.Tenant, e.Metric}
				dead[k] += e.Quantity
				if keep(e) {
					selected[k] = true
				}
			}
		}
	} else {
		res.Start = start
		deadIDs := map[string]bool{}
		for i := range dls {
			if e := &dls[i].Event; in(e) {
				deadIDs[e.ID] = true
				if keep(e) {
					selected[key{e.Tenant, e.Metric}] = true
				}
			}
		}
		// The ledger total counts each event ID once, like the usage API.
		// What the destination was meant to receive is each event ID that has
		// a copy after its start, once: it never saw the copies before it.
		// Only a dead letter of such an event was refused of what it was
		// meant to receive (one skipped at its start was not).
		meant := map[key]float64{}
		all, sinceStart := newUsageScan(), newUsageScan()
		err := s.spool.ScanAllSeq(from, to, func(e *usage.Event, seq uint64) error {
			first := !all.seen(e)
			late := seq > start.Seq && !sinceStart.seen(e)
			if !in(e) {
				return nil
			}
			k := key{e.Tenant, e.Metric}
			if first {
				ledger[k] += e.Quantity
				if keep(e) {
					selected[k] = true
				}
			}
			if late {
				meant[k] += e.Quantity
				if deadIDs[e.ID] {
					dead[k] += e.Quantity
				}
			}
			return nil
		})
		if err != nil {
			res.Error = err.Error()
			return res
		}
		before = map[key]float64{}
		for k, q := range ledger {
			if b := q - meant[k]; b > 1e-9 {
				before[k] = b
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
	notes := map[string]string{}
	if len(q.Tenants) > 0 {
		totals, err := rec.RecordedTotals(ctx, q)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		for _, t := range totals {
			if t.Metric == "" {
				if t.Note != "" {
					notes[t.Tenant] = t.Note
				}
				continue
			}
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
		expected := ledger[k] - before[k] - dead[k]
		diff := recorded[k] - expected
		tolerance := math.Max(1e-6, 1e-3*math.Abs(expected))
		match := math.Abs(diff) <= tolerance
		note := notes[k.tenant]
		// A destination an operator positioned had an earlier life: it may
		// hold some of the usage from before its start. That is not expected
		// of it, and not wrong either. (One created at the ledger head never
		// received any, so for it more than expected stays a mismatch.)
		if !match && diff > 0 && start != nil && start.How == spool.StartPositioned && diff <= before[k]+tolerance {
			match = true
			if note == "" {
				note = fmt.Sprintf("%s also holds %v recorded before the destination's start, which it received earlier", d.Name(), usage.Round(diff))
			}
		}
		res.OK = res.OK && match
		res.Rows = append(res.Rows, reconcileRow{Tenant: k.tenant, Metric: k.metric, Ledger: usage.Round(ledger[k]), BeforeStart: usage.Round(before[k]),
			DeadLettered: usage.Round(dead[k]), Expected: usage.Round(expected), Recorded: usage.Round(recorded[k]), Difference: usage.Round(diff), Match: match, Note: note})
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

// destinationRow is one destination in GET /api/v1/destinations and in the
// answers of the calls that change one.
type destinationRow struct {
	pipeline.Status
	DeadLetters int `json:"dead_letters"`
	// TenantsNotSetUp lists the tenants the destination could not set up,
	// with what is parked for each (pipeline.BlockedTenant).
	TenantsNotSetUp []pipeline.BlockedTenant `json:"tenants_not_set_up,omitempty"`
}

func (s *Server) destinationRow(st pipeline.Status) destinationRow {
	dls, _ := s.spool.DeadLetters(st.Name)
	return destinationRow{Status: st, DeadLetters: len(dls), TenantsNotSetUp: s.disp.Blocked(st.Name)}
}

// listDestinations lists every destination the ledger knows: the configured
// ones, and the detached ones (a cursor, but not in ADAPTERS), each with its
// state, cursor, backlog and held events.
func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	var out []destinationRow
	for _, st := range append(s.disp.Statuses(), s.disp.Detached()...) {
		out = append(out, s.destinationRow(st))
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

// deadLetters lists a destination's dead letters, also a detached one's (the
// held events an operator skipped when positioning it are there).
func (s *Server) deadLetters(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, known := s.disp.Status(name); !known {
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

// maxReplayReasons bounds the reasons a replay answers with.
const maxReplayReasons = 50

// replayDeadLetters resends parked events (after fixing the cause, for
// example creating a missing customer), as delivery would: tenants are set
// up first, and one event the destination still refuses never keeps the
// others from being delivered. Destinations enforce their own retry horizons
// and require reconciliation for ambiguous old deliveries.
//
// It answers 200 with what was delivered and what the destination still
// refuses (kept), and 502 when the destination could not take the replay at
// all (unreachable, a server or credentials error): then nothing was
// removed, and the replay can be repeated.
func (s *Server) replayDeadLetters(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d := s.destination(name)
	if d == nil {
		if _, known := s.disp.Status(name); !known {
			writeErr(w, http.StatusNotFound, "no destination %q", name)
			return
		}
		writeErr(w, http.StatusConflict, "destination %s is detached (not in ADAPTERS): its dead letters can be replayed once it is configured", name)
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
	res, err := s.disp.Replay(r.Context(), name, events)
	var state *pipeline.StateError
	switch {
	case errors.As(err, &state):
		writeErr(w, http.StatusConflict, "%v", err)
		return
	case err != nil:
		writeErr(w, http.StatusBadGateway, "replay failed, dead letters kept: %v", err)
		return
	}
	var delivered []string
	var confirmed []usage.Event
	for _, ev := range events {
		if _, refused := res.Refused[ev.ID]; refused {
			continue
		}
		if _, waits := res.NotSetUp[ev.Tenant]; waits {
			continue
		}
		delivered = append(delivered, ev.ID)
		confirmed = append(confirmed, ev)
	}
	if err := s.spool.RemoveDeadLetters(name, delivered); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist replay results: %v", err)
		return
	}
	if checkpoint, ok := d.(destinations.DeliveryCheckpointer); ok {
		if err := checkpoint.CheckpointDelivery(confirmed); err != nil {
			log.Printf("[api] %s: replay result is durable but receipt cleanup failed: %v", name, err)
		}
	}
	out := map[string]any{"replayed": len(delivered), "still_rejected": len(events) - len(delivered)}
	if len(res.Refused) > 0 {
		// Why the destination refuses what it still refuses, by event ID.
		reasons := map[string]string{}
		for _, ev := range events {
			if reason, ok := res.Refused[ev.ID]; ok && len(reasons) < maxReplayReasons {
				reasons[ev.ID] = reason
			}
		}
		out["refused"] = reasons
	}
	if len(res.NotSetUp) > 0 {
		// Tenants the destination could not set up: their events were not sent.
		out["tenants_not_set_up"] = res.NotSetUp
	}
	log.Printf("[api] replayed %d of %d dead letter(s) of %s (%d still refused, %d tenant(s) not set up)",
		len(delivered), len(events), name, len(res.Refused), len(res.NotSetUp))
	writeJSON(w, http.StatusOK, out)
}

// setCursor rewinds or advances a destination, e.g. {"seq": 0} replays the
// retained ledger into a newly added backend. It also works for a detached
// destination (not in ADAPTERS), which can be positioned before it is
// configured again, and for one that awaits its start: there positioning is
// the start decision (see startDestination and pipeline.Dispatcher.Position).
func (s *Server) setCursor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, known := s.disp.Status(name); !known {
		writeErr(w, http.StatusNotFound, "no destination %q", name)
		return
	}
	var req struct {
		Seq  *uint64 `json:"seq"`
		From string  `json:"from"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || (req.Seq == nil) == (req.From == "") {
		writeErr(w, http.StatusBadRequest, `body must be {"seq": <number>} or {"from": "<RFC 3339 time>"}`)
		return
	}
	if req.From != "" {
		// From a time: the destination receives again everything the ledger
		// committed since then, which holds every window that ended since
		// (late usage of older windows too, which it dedupes).
		from, err := time.Parse(time.RFC3339Nano, req.From)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "from: %v", err)
			return
		}
		seq, err := s.firstSeqSince(from)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "%v", err)
			return
		}
		req.Seq = &seq
	}
	res, err := s.disp.Position(r.Context(), name, *req.Seq, false)
	if s.valveFailed(w, name, err) {
		return
	}
	log.Printf("[api] cursor for %s set to %d (was %d, state %s, %d held event(s) moved to its dead letters)", name, res.To, res.From, res.Status.State, res.SkippedHeld)
	// "cursor" is the destination's cursor now: one that delivers may have
	// moved on from "position" already.
	s.writeValve(w, res.Status, map[string]any{"position": res.To, "previous_cursor": res.From, "skipped_held_events": res.SkippedHeld})
}

// firstSeqSince returns the cursor that replays every event the ledger
// committed at or after t: the sequence number just before the first one.
// With none since t, it is the ledger's head (nothing to replay).
func (s *Server) firstSeqSince(t time.Time) (uint64, error) {
	errFound := errors.New("found")
	var first uint64
	err := s.spool.ScanAppendedSeq(t, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), func(_ *usage.Event, _ time.Time, seq uint64) error {
		first = seq
		return errFound
	})
	if err != nil && !errors.Is(err, errFound) {
		return 0, err
	}
	if first == 0 {
		return s.spool.Committed(), nil
	}
	return first - 1, nil
}

// --- billing state ---

func (s *Server) billingStates(w http.ResponseWriter, r *http.Request) {
	if s.enf == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enforcement": "disabled", "states": []any{}})
		return
	}
	out := map[string]any{"mode": s.cfg.EnforcementMode, "states": s.enf.States(), "pauses": s.enf.Pauses()}
	if rules := s.enf.Escalations(); len(rules) > 0 {
		// The grace periods each state's escalation is computed from.
		out["escalations"] = rules
	}
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
