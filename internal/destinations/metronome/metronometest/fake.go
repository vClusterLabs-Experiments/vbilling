// Package metronometest is an in-process fake of the parts of Metronome's API
// that vBilling uses, for tests. It models what an AI Cloud's Metronome
// account looks like when its own portal and Finance own it:
//
//   - ingest, with transaction_id dedupe, and usage sent under an ingest
//     alias that no customer holds yet attached to the customer that gets
//     the alias later;
//   - customers with several ingest aliases (409 when an alias is taken,
//     archived customers included);
//   - billable metrics, products, rate cards and rates (Finance's catalog);
//     a billable metric counts only the usage ingested after it was
//     created, as Metronome's are not retroactive;
//   - contracts listed per customer (the portal's, with prepaid commits), and
//     the ones vBilling creates, ends and archives with METRONOME_PROVISION:
//     a rate card alias is resolved once, when a contract is created, so a
//     contract stays on its rate card when the alias moves;
//   - usage aggregation (POST /v1/usage) per billable metric;
//   - threshold notifications in alarm for a customer, as
//     POST /v1/customer-alerts/get and POST /v1/customer-alerts/list
//     report them;
//   - signed webhooks, sent to any URL with Metronome's signature scheme.
//
// Every request body is validated against the request schemas vendored in
// internal/destinations/metronome/testdata, and the price check endpoints'
// responses against their vendored response schemas, so a field vBilling
// misspells, or a fixture that drifts from Metronome's documented shape,
// fails the test. Requests the fake does not implement fail the test too.
//
// The portal and Finance act through Go methods (CreateCustomer, AddContract,
// AddBillableMetric, ...), which are not counted as writes. Writes() lists
// the objects vBilling itself created over the API.
package metronometest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Token is the API token the fake accepts.
const Token = "mtk_test"

// ErrAliasInUse is returned when an ingest alias already belongs to a
// customer (archived ones included), as Metronome answers 409.
var ErrAliasInUse = errors.New("ingest alias already in use")

// Customer is a Metronome customer.
type Customer struct {
	ID            string
	Name          string
	IngestAliases []string
	Archived      bool
}

// Event is an accepted usage event.
type Event struct {
	TransactionID string         `json:"transaction_id"`
	CustomerID    string         `json:"customer_id"` // as sent: a customer ID or an ingest alias
	EventType     string         `json:"event_type"`
	Timestamp     string         `json:"timestamp"`
	Properties    map[string]any `json:"properties,omitempty"`
	// Customer is the customer the event belongs to: set at ingest when
	// customer_id is a customer's ID or one of its aliases, or later, when a
	// customer gets the alias. Empty while the event is unattributed.
	Customer string `json:"-"`

	seq int // when it was ingested, in the fake's order of changes
}

// EventTypeFilter selects event types.
type EventTypeFilter struct {
	InValues    []string `json:"in_values,omitempty"`
	NotInValues []string `json:"not_in_values,omitempty"`
}

// PropertyFilter is one property rule of a billable metric.
type PropertyFilter struct {
	Name        string   `json:"name"`
	Exists      *bool    `json:"exists,omitempty"`
	InValues    []string `json:"in_values,omitempty"`
	NotInValues []string `json:"not_in_values,omitempty"`
}

// BillableMetric is a streaming billable metric.
type BillableMetric struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	AggregationType string           `json:"aggregation_type,omitempty"`
	AggregationKey  string           `json:"aggregation_key,omitempty"`
	EventTypeFilter *EventTypeFilter `json:"event_type_filter,omitempty"`
	PropertyFilters []PropertyFilter `json:"property_filters,omitempty"`
	GroupKeys       [][]string       `json:"group_keys,omitempty"`
	ArchivedAt      string           `json:"archived_at,omitempty"`
	// SQL is the query of a SQL billable metric; the fake does not run it.
	SQL string `json:"sql,omitempty"`

	seq int // when it was created, in the fake's order of changes
}

// Product is a usage product on a billable metric.
type Product struct {
	ID                   string
	Name                 string
	BillableMetricID     string
	PricingGroupKey      []string
	PresentationGroupKey []string
}

// RateCard is a rate card with optional aliases.
type RateCard struct {
	ID      string
	Name    string
	Aliases []string
}

// Rate is a flat, entitled rate of a product on a rate card.
type Rate struct {
	RateCardID         string
	ProductID          string
	PricingGroupValues map[string]string
	Price              float64
	StartingAt         time.Time
	EndingBefore       time.Time // zero: open
	NotEntitled        bool
}

// Contract is a customer's contract, as the portal creates it.
type Contract struct {
	ID            string
	CustomerID    string
	RateCardID    string
	StartingAt    time.Time
	EndingBefore  time.Time // zero: open
	Archived      bool
	UniquenessKey string
	// PrepaidCommit is the amount of a prepaid commit on the contract (the
	// wallet a top-up adds to). vBilling never reads it.
	PrepaidCommit float64
}

// Server is the fake. Methods are safe for concurrent use.
type Server struct {
	URL string

	t   testing.TB
	srv *httptest.Server

	mu        sync.Mutex
	now       func() time.Time
	seq       int
	tick      int // orders ingests and billable metric creations
	customers []*Customer
	events    []*Event
	txns      map[string]bool
	metrics   []*BillableMetric
	products  []*Product
	cards     []*RateCard
	rates     []*Rate
	contracts []*Contract
	alerts    map[string]*alertState
	writes    []string
	calls     map[string]int
	fail      map[string]int
}

// alertState is a threshold notification as Metronome evaluates it for one
// customer.
type alertState struct {
	id, customer, typ, name string
	threshold               float64
	status                  string // ok, in_alarm, evaluating, or "" once archived
	updated                 time.Time
}

// New starts a fake Metronome for the test; it stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	if err := load(); err != nil {
		t.Fatalf("metronometest: load schemas: %v", err)
	}
	s := &Server{t: t, now: time.Now, txns: map[string]bool{}, calls: map[string]int{}, fail: map[string]int{}, alerts: map[string]*alertState{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

// SetClock replaces the clock used for webhook dates and contract status.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// newID returns a UUID-shaped ID that is unique within the fake.
func (s *Server) newID(kind string) string {
	s.seq++
	sum := sha256.Sum256([]byte(kind + "/" + strconv.Itoa(s.seq)))
	x := hex.EncodeToString(sum[:16])
	return x[:8] + "-" + x[8:12] + "-4" + x[13:16] + "-8" + x[17:20] + "-" + x[20:32]
}

// --- portal and Finance (Go API, not counted as writes) ---

// CreateCustomer creates a customer the way an AI Cloud's portal does at
// signup, typically with an identity alias and a compute alias. Usage sent
// earlier under one of the aliases is attached to it.
func (s *Server) CreateCustomer(name string, aliases ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCustomerLocked(name, aliases)
}

func (s *Server) createCustomerLocked(name string, aliases []string) (string, error) {
	for _, a := range aliases {
		if c := s.aliasHolderLocked(a, true); c != nil {
			return "", fmt.Errorf("%w: %q belongs to customer %s", ErrAliasInUse, a, c.ID)
		}
	}
	c := &Customer{ID: s.newID("customer"), Name: name, IngestAliases: append([]string(nil), aliases...)}
	s.customers = append(s.customers, c)
	s.attachLocked(c)
	return c.ID, nil
}

// ArchiveCustomer archives a customer. It keeps its aliases: Metronome only
// lets another customer take one after it is removed from this one.
func (s *Server) ArchiveCustomer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.customerLocked(id); c != nil {
		c.Archived = true
	}
}

// SetIngestAliases replaces a customer's aliases (a migration swaps the
// compute alias this way). An alias another customer holds is refused.
func (s *Server) SetIngestAliases(id string, aliases ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.customerLocked(id)
	if c == nil {
		return fmt.Errorf("no customer %s", id)
	}
	for _, a := range aliases {
		if h := s.aliasHolderLocked(a, true); h != nil && h != c {
			return fmt.Errorf("%w: %q belongs to customer %s", ErrAliasInUse, a, h.ID)
		}
	}
	c.IngestAliases = append([]string(nil), aliases...)
	s.attachLocked(c)
	return nil
}

// Customer returns a copy of a customer.
func (s *Server) Customer(id string) (Customer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.customerLocked(id)
	if c == nil {
		return Customer{}, false
	}
	cp := *c
	cp.IngestAliases = append([]string(nil), c.IngestAliases...)
	return cp, true
}

// AddBillableMetric adds a billable metric from a create request body (what
// Finance's configuration applies), validated against Metronome's schema.
func (s *Server) AddBillableMetric(body []byte) string {
	s.t.Helper()
	schema, _ := RequestSchema("create_billable_metric")
	for _, e := range ValidateJSON(schema, body, "create_billable_metric") {
		s.t.Errorf("metronometest: billable metric: %s", e)
	}
	var m BillableMetric
	if err := json.Unmarshal(body, &m); err != nil {
		s.t.Fatalf("metronometest: billable metric: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = s.newID("billable-metric")
	s.tick++
	m.seq = s.tick
	s.metrics = append(s.metrics, &m)
	return m.ID
}

// ArchiveBillableMetric archives a billable metric.
func (s *Server) ArchiveBillableMetric(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.metrics {
		if m.ID == id {
			m.ArchivedAt = s.now().UTC().Format(time.RFC3339)
		}
	}
}

// AddProduct adds a usage product on a billable metric.
func (s *Server) AddProduct(name, billableMetricID string, pricingGroupKey ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &Product{ID: s.newID("product"), Name: name, BillableMetricID: billableMetricID, PricingGroupKey: pricingGroupKey}
	s.products = append(s.products, p)
	return p.ID
}

// AddRateCard adds a rate card with optional aliases.
func (s *Server) AddRateCard(name string, aliases ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := &RateCard{ID: s.newID("rate-card"), Name: name, Aliases: aliases}
	s.cards = append(s.cards, rc)
	return rc.ID
}

// AddRate adds a rate. A zero StartingAt means "since the start of 2026".
func (s *Server) AddRate(r Rate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.StartingAt.IsZero() {
		r.StartingAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	s.rates = append(s.rates, &r)
}

// AddContract adds a contract the way the portal creates one.
func (s *Server) AddContract(c Contract) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.ID = s.newID("contract")
	s.contracts = append(s.contracts, &c)
	return c.ID
}

// --- inspection ---

// Events returns copies of the accepted events, in order.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i, e := range s.events {
		out[i] = *e
	}
	return out
}

// Writes lists the objects vBilling created or changed over the API
// ("POST /v1/customers", ...). Usage ingest is not a write.
func (s *Server) Writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writes...)
}

// Calls counts requests to an endpoint, e.g. "GET /v1/customers/{id}".
func (s *Server) Calls(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[endpoint]
}

// FailNext makes the next n requests to an endpoint answer 503.
func (s *Server) FailNext(endpoint string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[endpoint] = n
}

// --- model ---

func (s *Server) customerLocked(id string) *Customer {
	for _, c := range s.customers {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// aliasHolderLocked returns the customer holding an alias.
func (s *Server) aliasHolderLocked(alias string, includeArchived bool) *Customer {
	for _, c := range s.customers {
		if c.Archived && !includeArchived {
			continue
		}
		for _, a := range c.IngestAliases {
			if a == alias {
				return c
			}
		}
	}
	return nil
}

// ownerLocked resolves an event's customer_id: a customer ID, or an alias
// an active customer holds.
func (s *Server) ownerLocked(id string) string {
	if c := s.customerLocked(id); c != nil && !c.Archived {
		return c.ID
	}
	if c := s.aliasHolderLocked(id, false); c != nil {
		return c.ID
	}
	return ""
}

// attachLocked gives a customer the unattributed events sent under its aliases.
func (s *Server) attachLocked(c *Customer) {
	for _, e := range s.events {
		if e.Customer != "" {
			continue
		}
		for _, a := range c.IngestAliases {
			if e.CustomerID == a {
				e.Customer = c.ID
			}
		}
	}
}

func (m *BillableMetric) matches(e *Event) bool {
	if m.ArchivedAt != "" || m.SQL != "" {
		return false // the fake does not run SQL billable metrics
	}
	if f := m.EventTypeFilter; f != nil {
		if len(f.InValues) > 0 && !contains(f.InValues, e.EventType) {
			return false
		}
		if contains(f.NotInValues, e.EventType) {
			return false
		}
	}
	for _, f := range m.PropertyFilters {
		raw, present := e.Properties[f.Name]
		val := fmt.Sprint(raw)
		if f.Exists != nil && *f.Exists != present {
			return false
		}
		if len(f.InValues) > 0 && (!present || !contains(f.InValues, val)) {
			return false
		}
		if present && contains(f.NotInValues, val) {
			return false
		}
	}
	return true
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

// aggregateLocked aggregates a billable metric over a customer's events in
// [from, to). Billable metrics are not retroactive: "When using streaming
// billable metrics, the billable metric must be defined in Metronome before
// any usage can be associated with it. If usage is sent before a streaming
// metric is defined, it isn't attributed to the metric by default"
// (docs.metronome.com, guides/implement-metronome/core-concepts/
// create-billable-metrics), and "billable metrics only match usage events
// sent after the billable metric is created" (guides/get-started/
// developer-sdks). So only events ingested after the metric was created
// count, whatever their timestamp. (An alias attaches earlier usage to a
// customer that gets it later: that is retroactive, and stays so.)
func (s *Server) aggregateLocked(m *BillableMetric, customer string, from, to time.Time) float64 {
	var v float64
	for _, e := range s.events {
		if e.Customer != customer || e.seq <= m.seq || !m.matches(e) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil || ts.Before(from) || !ts.Before(to) {
			continue
		}
		switch strings.ToUpper(m.AggregationType) {
		case "COUNT":
			v++
		case "MAX":
			v = max(v, number(e.Properties[m.AggregationKey]))
		default: // SUM
			v += number(e.Properties[m.AggregationKey])
		}
	}
	return v
}

// --- HTTP ---

func (s *Server) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) errorf(w http.ResponseWriter, status int, format string, args ...any) {
	s.reply(w, status, map[string]string{"message": fmt.Sprintf(format, args...)})
}

// check validates a request body; violations fail the test and answer 400,
// as Metronome refuses a body that does not match its schema.
func (s *Server) check(w http.ResponseWriter, schema any, name string, body []byte) bool {
	errs := ValidateJSON(schema, body, name)
	for _, e := range errs {
		s.t.Errorf("metronometest: schema violation: %s", e)
	}
	if len(errs) > 0 {
		s.errorf(w, http.StatusBadRequest, "%s", strings.Join(errs, "; "))
		return false
	}
	return true
}

func (s *Server) checkRequest(w http.ResponseWriter, name string, body []byte) bool {
	schema, err := RequestSchema(name)
	if err != nil {
		s.t.Errorf("metronometest: %v", err)
		return false
	}
	return s.check(w, schema, name, body)
}

// checkPricing validates the request and query of a price check endpoint
// and returns its schemas for the response.
func (s *Server) checkPricing(w http.ResponseWriter, r *http.Request, name string, body []byte) (Endpoint, bool) {
	ep, err := PricingEndpoint(name)
	if err != nil {
		s.t.Errorf("metronometest: %v", err)
		return ep, false
	}
	for k := range r.URL.Query() {
		if _, ok := ep.Query[k]; !ok {
			s.t.Errorf("metronometest: %s: undocumented query parameter %q", name, k)
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err != nil || n < 1 || n > 100 {
			s.t.Errorf("metronometest: %s: limit %q outside 1..100", name, l)
		}
	}
	if ep.Request == nil {
		if len(bytes.TrimSpace(body)) > 0 {
			s.t.Errorf("metronometest: %s: unexpected body %q", name, body)
		}
		return ep, true
	}
	return ep, s.check(w, ep.Request, name+" request", body)
}

// replyPricing validates a price check response before sending it, so the
// fake never serves something Metronome would not.
func (s *Server) replyPricing(w http.ResponseWriter, ep Endpoint, name string, resp any) {
	for _, e := range ValidateValue(ep.Response, resp, name+" response") {
		s.t.Errorf("metronometest: fixture does not match Metronome's schema: %s", e)
	}
	s.reply(w, http.StatusOK, resp)
}

func endpointOf(r *http.Request) string {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/customers/") && strings.Count(p, "/") == 3 {
		p = "/v1/customers/{id}"
	}
	return r.Method + " " + p
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+Token {
		s.errorf(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	body, _ := io.ReadAll(r.Body)
	key := endpointOf(r)
	s.calls[key]++
	if n := s.fail[key]; n > 0 {
		s.fail[key] = n - 1
		s.errorf(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	switch key {
	case "POST /v1/ingest":
		s.ingest(w, body)
	case "GET /v1/customers":
		s.listCustomers(w, r)
	case "GET /v1/customers/{id}":
		c := s.customerLocked(strings.TrimPrefix(r.URL.Path, "/v1/customers/"))
		if c == nil {
			s.errorf(w, http.StatusNotFound, "customer not found")
			return
		}
		s.reply(w, http.StatusOK, map[string]any{"data": s.customerJSON(c)})
	case "POST /v1/customers":
		s.createCustomer(w, body)
	case "GET /v1/billable-metrics":
		s.listBillableMetrics(w, r, body)
	case "POST /v1/billable-metrics/create":
		if !s.checkRequest(w, "create_billable_metric", body) {
			return
		}
		var m BillableMetric
		json.Unmarshal(body, &m)
		m.ID = s.newID("billable-metric")
		s.tick++
		m.seq = s.tick
		s.metrics = append(s.metrics, &m)
		s.writes = append(s.writes, key)
		s.reply(w, http.StatusOK, map[string]any{"data": map[string]string{"id": m.ID}})
	case "POST /v2/contracts/list":
		s.listContracts(w, body)
	case "POST /v1/contracts/create":
		s.createContract(w, key, body)
	case "POST /v1/contracts/updateEndDate", "POST /v1/contracts/archive":
		s.endContract(w, key, body)
	case "POST /v1/contract-pricing/products/list":
		s.listProducts(w, r, body)
	case "POST /v1/contract-pricing/rate-cards/list":
		s.listRateCards(w, r, body)
	case "POST /v1/contract-pricing/rate-cards/getRates":
		s.getRates(w, r, body)
	case "POST /v1/usage":
		s.usage(w, body)
	case "POST /v1/customer-alerts/get":
		s.customerAlert(w, r, body)
	case "POST /v1/customer-alerts/list":
		s.customerAlerts(w, r, body)
	default:
		s.t.Errorf("metronometest: vBilling called %s, which the fake does not implement", key)
		s.errorf(w, http.StatusNotFound, "not implemented by the fake: %s", key)
	}
}

func (s *Server) ingest(w http.ResponseWriter, body []byte) {
	if !s.checkRequest(w, "ingest", body) {
		return
	}
	var evs []*Event
	if err := json.Unmarshal(body, &evs); err != nil {
		s.errorf(w, http.StatusBadRequest, "%v", err)
		return
	}
	for _, e := range evs {
		for k, v := range e.Properties {
			if str, ok := v.(string); ok && str == "" {
				s.t.Errorf("metronometest: event %s has an empty property %s", e.TransactionID, k)
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, e.Timestamp); err != nil {
			s.errorf(w, http.StatusBadRequest, "timestamp %q is not RFC 3339", e.Timestamp)
			return
		}
	}
	for _, e := range evs {
		if s.txns[e.TransactionID] {
			continue // Metronome drops a repeated transaction_id for 34 days
		}
		s.txns[e.TransactionID] = true
		e.Customer = s.ownerLocked(e.CustomerID)
		s.tick++
		e.seq = s.tick
		s.events = append(s.events, e)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) customerJSON(c *Customer) map[string]any {
	out := map[string]any{"id": c.ID, "name": c.Name, "external_id": firstOr(c.IngestAliases, c.ID),
		"ingest_aliases": append([]string{}, c.IngestAliases...), "custom_fields": map[string]string{}}
	if c.Archived {
		out["archived_at"] = s.now().UTC().Format(time.RFC3339)
	}
	return out
}

func firstOr(l []string, def string) string {
	if len(l) > 0 {
		return l[0]
	}
	return def
}

func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k := range q {
		switch k {
		case "ingest_alias", "only_archived", "customer_ids", "limit", "next_page":
		default:
			s.t.Errorf("metronometest: GET /v1/customers: undocumented query parameter %q", k)
		}
	}
	archived := q.Get("only_archived") == "true"
	alias := q.Get("ingest_alias")
	data := []map[string]any{}
	for _, c := range s.customers {
		if c.Archived != archived {
			continue // by default only active customers; only_archived only archived ones
		}
		if alias != "" && !contains(c.IngestAliases, alias) {
			continue
		}
		data = append(data, s.customerJSON(c))
	}
	s.reply(w, http.StatusOK, map[string]any{"data": data, "next_page": nil})
}

func (s *Server) createCustomer(w http.ResponseWriter, body []byte) {
	if !s.checkRequest(w, "create_customer", body) {
		return
	}
	var req struct {
		Name          string   `json:"name"`
		IngestAliases []string `json:"ingest_aliases"`
	}
	json.Unmarshal(body, &req)
	id, err := s.createCustomerLocked(req.Name, req.IngestAliases)
	if errors.Is(err, ErrAliasInUse) {
		s.errorf(w, http.StatusConflict, "%v", err)
		return
	}
	s.writes = append(s.writes, "POST /v1/customers")
	s.reply(w, http.StatusOK, map[string]any{"data": s.customerJSON(s.customerLocked(id))})
}

func (s *Server) listBillableMetrics(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "list_billable_metrics", body)
	if !ok {
		return
	}
	withArchived := r.URL.Query().Get("include_archived") == "true"
	data := []*BillableMetric{}
	for _, m := range s.metrics {
		if m.ArchivedAt == "" || withArchived {
			data = append(data, m)
		}
	}
	s.replyPricing(w, ep, "list_billable_metrics", map[string]any{"data": data, "next_page": nil})
}

func (s *Server) listContracts(w http.ResponseWriter, body []byte) {
	if !s.checkRequest(w, "list_contracts_v2", body) {
		return
	}
	var req struct {
		CustomerID      string `json:"customer_id"`
		IncludeArchived bool   `json:"include_archived"`
		CoveringDate    string `json:"covering_date"`
		StartingAt      string `json:"starting_at"`
		Limit           int    `json:"limit"`
		Cursor          string `json:"cursor"`
	}
	json.Unmarshal(body, &req)
	if req.CoveringDate != "" && req.StartingAt != "" {
		s.errorf(w, http.StatusBadRequest, "covering_date cannot be combined with starting_at")
		return
	}
	var list []*Contract
	for _, c := range s.contracts {
		if c.CustomerID != req.CustomerID || (c.Archived && !req.IncludeArchived) {
			continue
		}
		if req.CoveringDate != "" {
			at, err := time.Parse(time.RFC3339, req.CoveringDate)
			if err != nil {
				s.errorf(w, http.StatusBadRequest, "covering_date %q", req.CoveringDate)
				return
			}
			if c.StartingAt.After(at) || (!c.EndingBefore.IsZero() && !c.EndingBefore.After(at)) {
				continue
			}
		}
		if req.StartingAt != "" {
			if from, err := time.Parse(time.RFC3339, req.StartingAt); err == nil && c.StartingAt.Before(from) {
				continue
			}
		}
		list = append(list, c)
	}
	limit := req.Limit
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	from, _ := strconv.Atoi(req.Cursor)
	to := min(from+limit, len(list))
	var cursor any
	if to < len(list) {
		cursor = strconv.Itoa(to)
	}
	data := []map[string]any{}
	for _, c := range list[min(from, len(list)):to] {
		data = append(data, s.contractJSON(c))
	}
	s.reply(w, http.StatusOK, map[string]any{"data": data, "cursor": cursor})
}

// createContract answers POST /v1/contracts/create, as vBilling calls it with
// METRONOME_PROVISION. A rate card alias is resolved here, once: the contract
// keeps the rate card the alias points to now, wherever the alias moves
// later. A uniqueness key can be used once (409 afterwards).
func (s *Server) createContract(w http.ResponseWriter, key string, body []byte) {
	if !s.checkRequest(w, "create_contract", body) {
		return
	}
	var req struct {
		CustomerID    string `json:"customer_id"`
		StartingAt    string `json:"starting_at"`
		RateCardID    string `json:"rate_card_id"`
		RateCardAlias string `json:"rate_card_alias"`
		UniquenessKey string `json:"uniqueness_key"`
	}
	json.Unmarshal(body, &req)
	if s.customerLocked(req.CustomerID) == nil {
		s.errorf(w, http.StatusNotFound, "customer %s not found", req.CustomerID)
		return
	}
	start, err := time.Parse(time.RFC3339, req.StartingAt)
	if err != nil {
		s.errorf(w, http.StatusBadRequest, "starting_at %q is not RFC 3339", req.StartingAt)
		return
	}
	card := req.RateCardID
	if req.RateCardAlias != "" {
		card = ""
		for _, rc := range s.cards {
			if contains(rc.Aliases, req.RateCardAlias) {
				card = rc.ID
			}
		}
		if card == "" {
			s.errorf(w, http.StatusBadRequest, "no rate card has the alias %q", req.RateCardAlias)
			return
		}
	}
	if req.UniquenessKey != "" {
		for _, c := range s.contracts {
			if c.UniquenessKey == req.UniquenessKey {
				s.errorf(w, http.StatusConflict, "This uniqueness key has already been used.")
				return
			}
		}
	}
	c := &Contract{ID: s.newID("contract"), CustomerID: req.CustomerID, RateCardID: card, StartingAt: start, UniquenessKey: req.UniquenessKey}
	s.contracts = append(s.contracts, c)
	s.writes = append(s.writes, key)
	s.reply(w, http.StatusOK, map[string]any{"data": map[string]string{"id": c.ID}})
}

// endContract answers POST /v1/contracts/updateEndDate and
// POST /v1/contracts/archive.
func (s *Server) endContract(w http.ResponseWriter, key string, body []byte) {
	archive := strings.HasSuffix(key, "/archive")
	name := "update_contract_end_date"
	if archive {
		name = "archive_contract"
	}
	if !s.checkRequest(w, name, body) {
		return
	}
	var req struct {
		CustomerID   string `json:"customer_id"`
		ContractID   string `json:"contract_id"`
		EndingBefore string `json:"ending_before"`
	}
	json.Unmarshal(body, &req)
	var c *Contract
	for _, x := range s.contracts {
		if x.ID == req.ContractID && x.CustomerID == req.CustomerID {
			c = x
		}
	}
	if c == nil {
		s.errorf(w, http.StatusNotFound, "contract not found")
		return
	}
	if archive {
		c.Archived = true
	} else {
		end, err := time.Parse(time.RFC3339, req.EndingBefore)
		if err != nil || !end.After(c.StartingAt) {
			s.errorf(w, http.StatusBadRequest, "ending_before must be after starting_at")
			return
		}
		c.EndingBefore = end
	}
	s.writes = append(s.writes, key)
	s.reply(w, http.StatusOK, map[string]any{"data": map[string]string{"id": c.ID}})
}

// MoveRateCardAlias points a rate card alias at another rate card, as
// Finance does when a new price list goes live: the alias leaves the rate
// card that had it. Contracts created on the alias stay where they are.
func (s *Server) MoveRateCardAlias(alias, rateCardID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rc := range s.cards {
		kept := rc.Aliases[:0:0]
		for _, a := range rc.Aliases {
			if a != alias {
				kept = append(kept, a)
			}
		}
		if rc.ID == rateCardID {
			kept = append(kept, alias)
		}
		rc.Aliases = kept
	}
}

// Contracts returns copies of a customer's contracts, in creation order.
func (s *Server) Contracts(customerID string) []Contract {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Contract
	for _, c := range s.contracts {
		if c.CustomerID == customerID {
			out = append(out, *c)
		}
	}
	return out
}

// EndContract ends a contract the way the account's owner does in Metronome
// (not counted as a write by vBilling).
func (s *Server) EndContract(id string, endingBefore time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.contracts {
		if c.ID == id {
			c.EndingBefore = endingBefore
		}
	}
}

func (s *Server) contractJSON(c *Contract) map[string]any {
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	out := map[string]any{
		"id": c.ID, "customer_id": c.CustomerID, "starting_at": rfc(c.StartingAt),
		"created_at": rfc(c.StartingAt), "created_by": "portal",
		"overrides": []any{}, "scheduled_charges": []any{}, "transitions": []any{},
		"usage_statement_schedule": map[string]any{"frequency": "MONTHLY"},
		"usage_filter":             map[string]any{"current": nil, "initial": map[string]any{}, "updates": []any{}},
	}
	if c.RateCardID != "" {
		out["rate_card_id"] = c.RateCardID
	}
	if !c.EndingBefore.IsZero() {
		out["ending_before"] = rfc(c.EndingBefore)
	}
	if c.Archived {
		out["archived_at"] = rfc(s.now())
	}
	if c.UniquenessKey != "" {
		out["uniqueness_key"] = c.UniquenessKey
	}
	commits := []any{}
	if c.PrepaidCommit > 0 {
		commits = append(commits, map[string]any{"id": c.ID + "-commit", "type": "PREPAID",
			"access_schedule": map[string]any{"schedule_items": []any{map[string]any{
				"amount": c.PrepaidCommit, "starting_at": rfc(c.StartingAt), "ending_before": rfc(c.StartingAt.AddDate(1, 0, 0))}}}})
	}
	out["commits"] = commits
	return out
}

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "list_products", body)
	if !ok {
		return
	}
	data := []map[string]any{}
	for _, p := range s.products {
		state := map[string]any{"name": p.Name, "created_at": "2026-01-01T00:00:00Z", "created_by": "finance",
			"billable_metric_id": p.BillableMetricID, "starting_at": "2026-01-01T00:00:00Z"}
		if len(p.PricingGroupKey) > 0 {
			state["pricing_group_key"] = p.PricingGroupKey
		}
		if len(p.PresentationGroupKey) > 0 {
			state["presentation_group_key"] = p.PresentationGroupKey
		}
		data = append(data, map[string]any{"id": p.ID, "type": "USAGE", "initial": state, "current": state, "updates": []any{}})
	}
	s.replyPricing(w, ep, "list_products", map[string]any{"data": data, "next_page": nil})
}

func (s *Server) listRateCards(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "list_rate_cards", body)
	if !ok {
		return
	}
	data := []map[string]any{}
	for _, rc := range s.cards {
		aliases := []map[string]any{}
		for _, a := range rc.Aliases {
			aliases = append(aliases, map[string]any{"name": a})
		}
		data = append(data, map[string]any{"id": rc.ID, "name": rc.Name, "created_at": "2026-01-01T00:00:00Z",
			"created_by": "finance", "aliases": aliases})
	}
	s.replyPricing(w, ep, "list_rate_cards", map[string]any{"data": data, "next_page": nil})
}

func (s *Server) getRates(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "get_rates", body)
	if !ok {
		return
	}
	var req struct {
		RateCardID string `json:"rate_card_id"`
		At         string `json:"at"`
		Selectors  []struct {
			ProductID string `json:"product_id"`
		} `json:"selectors"`
	}
	json.Unmarshal(body, &req)
	at, err := time.Parse(time.RFC3339, req.At)
	if err != nil {
		s.errorf(w, http.StatusBadRequest, "at %q is not RFC 3339", req.At)
		return
	}
	want := map[string]bool{}
	for _, sel := range req.Selectors {
		want[sel.ProductID] = true
	}
	data := []map[string]any{}
	for _, rt := range s.rates {
		if rt.RateCardID != req.RateCardID || (len(want) > 0 && !want[rt.ProductID]) {
			continue
		}
		if rt.StartingAt.After(at) || (!rt.EndingBefore.IsZero() && !rt.EndingBefore.After(at)) {
			continue // the schedule segment does not cover at
		}
		name := ""
		for _, p := range s.products {
			if p.ID == rt.ProductID {
				name = p.Name
			}
		}
		row := map[string]any{"product_id": rt.ProductID, "product_name": name, "product_tags": []string{},
			"product_custom_fields": map[string]string{}, "starting_at": rt.StartingAt.UTC().Format(time.RFC3339),
			"entitled": !rt.NotEntitled, "rate": map[string]any{"rate_type": "FLAT", "price": rt.Price}}
		if len(rt.PricingGroupValues) > 0 {
			row["pricing_group_values"] = rt.PricingGroupValues
		}
		if !rt.EndingBefore.IsZero() {
			row["ending_before"] = rt.EndingBefore.UTC().Format(time.RFC3339)
		}
		data = append(data, row)
	}
	s.replyPricing(w, ep, "get_rates", map[string]any{"data": data, "next_page": nil})
}

func (s *Server) usage(w http.ResponseWriter, body []byte) {
	if !s.checkRequest(w, "usage", body) {
		return
	}
	var req struct {
		StartingOn      string   `json:"starting_on"`
		EndingBefore    string   `json:"ending_before"`
		WindowSize      string   `json:"window_size"`
		CustomerIDs     []string `json:"customer_ids"`
		BillableMetrics []struct {
			ID string `json:"id"`
		} `json:"billable_metrics"`
	}
	json.Unmarshal(body, &req)
	from, err1 := time.Parse(time.RFC3339, req.StartingOn)
	to, err2 := time.Parse(time.RFC3339, req.EndingBefore)
	if err1 != nil || err2 != nil || !to.After(from) {
		s.errorf(w, http.StatusBadRequest, "starting_on and ending_before must be RFC 3339, ending after starting")
		return
	}
	step := to.Sub(from)
	switch strings.ToLower(req.WindowSize) {
	case "day":
		step = 24 * time.Hour
		if !from.Equal(from.Truncate(step)) || !to.Equal(to.Truncate(step)) {
			s.errorf(w, http.StatusBadRequest, "day windows need UTC midnights")
			return
		}
	case "hour":
		step = time.Hour
	}
	customers := req.CustomerIDs
	if len(customers) == 0 {
		for _, c := range s.customers {
			customers = append(customers, c.ID)
		}
	}
	var metrics []*BillableMetric
	if len(req.BillableMetrics) == 0 {
		metrics = s.metrics
	}
	for _, ref := range req.BillableMetrics {
		for _, m := range s.metrics {
			if m.ID == ref.ID {
				metrics = append(metrics, m)
			}
		}
	}
	sort.Strings(customers)
	data := []map[string]any{}
	for _, c := range customers {
		for _, m := range metrics {
			for ws := from; ws.Before(to); ws = ws.Add(step) {
				we := ws.Add(step)
				if we.After(to) {
					we = to
				}
				data = append(data, map[string]any{"customer_id": c, "billable_metric_id": m.ID, "billable_metric_name": m.Name,
					"start_timestamp": ws.UTC().Format(time.RFC3339), "end_timestamp": we.UTC().Format(time.RFC3339),
					"value": s.aggregateLocked(m, c, ws, we)})
			}
		}
	}
	s.reply(w, http.StatusOK, map[string]any{"data": data, "next_page": nil})
}

// customerAlert answers POST /v1/customer-alerts/get: "the real-time
// evaluation status for a specific threshold notification-customer pair"
// (docs.metronome.com, alerts/get-a-threshold-notification), with
// customer_status null once the notification is archived, and 404 for a
// customer or notification the account does not have.
func (s *Server) customerAlert(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "get_customer_alert", body)
	if !ok {
		return
	}
	var req struct {
		CustomerID string `json:"customer_id"`
		AlertID    string `json:"alert_id"`
	}
	json.Unmarshal(body, &req)
	a := s.alerts[req.AlertID]
	if a == nil || a.customer != req.CustomerID || s.customerLocked(req.CustomerID) == nil {
		s.errorf(w, http.StatusNotFound, "threshold notification %s not found for customer %s", req.AlertID, req.CustomerID)
		return
	}
	s.replyPricing(w, ep, "get_customer_alert", map[string]any{"data": a.json()})
}

// json is the CustomerAlert Metronome answers with: customer_status null
// once the notification is archived.
func (a *alertState) json() map[string]any {
	var status any = a.status
	state := "enabled"
	if a.status == "" {
		status, state = nil, "archived"
	}
	return map[string]any{
		"customer_status": status,
		"alert": map[string]any{"id": a.id, "name": a.name, "type": a.typ, "status": state, "threshold": a.threshold,
			"updated_at": a.updated.UTC().Format(time.RFC3339)},
	}
}

// alertsPage is how many threshold notifications the fake answers
// POST /v1/customer-alerts/list with per page: few, so paging is always
// exercised.
const alertsPage = 2

// customerAlerts answers POST /v1/customer-alerts/list: "all threshold
// notification configurations and their current statuses for a specific
// customer" (docs.metronome.com, alerts/get-all-threshold-notifications),
// only the enabled ones unless alert_statuses asks for archived ones too,
// paged with next_page, and 404 for a customer the account does not have.
func (s *Server) customerAlerts(w http.ResponseWriter, r *http.Request, body []byte) {
	ep, ok := s.checkPricing(w, r, "list_customer_alerts", body)
	if !ok {
		return
	}
	var req struct {
		CustomerID    string   `json:"customer_id"`
		AlertStatuses []string `json:"alert_statuses"`
	}
	json.Unmarshal(body, &req)
	if s.customerLocked(req.CustomerID) == nil {
		s.errorf(w, http.StatusNotFound, "customer %s not found", req.CustomerID)
		return
	}
	want := map[string]bool{"enabled": len(req.AlertStatuses) == 0}
	for _, st := range req.AlertStatuses {
		want[strings.ToLower(st)] = true
	}
	var matched []*alertState
	for _, a := range s.alerts {
		state := "enabled"
		if a.status == "" {
			state = "archived"
		}
		if a.customer == req.CustomerID && want[state] {
			matched = append(matched, a)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].id < matched[j].id })
	from := 0
	if p := r.URL.Query().Get("next_page"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > len(matched) {
			s.errorf(w, http.StatusBadRequest, "invalid next_page %q", p)
			return
		}
		from = n
	}
	to := min(from+alertsPage, len(matched))
	data := []map[string]any{}
	for _, a := range matched[from:to] {
		data = append(data, a.json())
	}
	var next any
	if to < len(matched) {
		next = strconv.Itoa(to)
	}
	s.replyPricing(w, ep, "list_customer_alerts", map[string]any{"data": data, "next_page": next})
}

// --- webhooks ---

// Notification is a Metronome webhook notification.
type Notification struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// Alert is a threshold notification ("alerts.<type>") for a customer, such
// as alerts.low_remaining_commit_balance_reached for a prepaid commit wallet.
// The notification is in alarm for the customer from then on, as
// POST /v1/customer-alerts/get and /list report, until SetAlertStatus says
// otherwise. Its type is alertType without "alerts.".
func (s *Server) Alert(alertType, customerID, alertName string, remaining, threshold float64) Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.newID("notification")
	alertID := s.newID("alert")
	s.alerts[alertID] = &alertState{id: alertID, customer: customerID, typ: strings.TrimPrefix(alertType, "alerts."), name: alertName,
		threshold: threshold, status: "in_alarm", updated: s.now()}
	return Notification{ID: id, Type: alertType, Properties: map[string]any{
		"customer_id": customerID, "alert_id": alertID, "alert_name": alertName,
		"timestamp": s.now().UTC().Format(time.RFC3339Nano), "threshold": threshold,
		"remaining_balance": remaining, "triggered_by": "usage",
	}}
}

// SetAlertStatus sets a threshold notification's evaluation for its
// customer, as Metronome re-evaluates it after usage or a commit change:
// "ok", "in_alarm", "evaluating", or "" to archive the notification.
func (s *Server) SetAlertStatus(alertID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.alerts[alertID]; a != nil {
		a.status, a.updated = status, s.now()
	}
}

// PaymentStatus is a payment_gate.payment_status notification, sent after a
// payment for a payment-gated commit (a top-up) succeeds ("paid") or fails.
func (s *Server) PaymentStatus(customerID, contractID, status string) Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Notification{ID: s.newID("notification"), Type: "payment_gate.payment_status", Properties: map[string]any{
		"workflow_type": "manual_commit", "customer_id": customerID, "contract_id": contractID,
		"invoice_id": s.newID("invoice"), "payment_status": status, "timestamp": s.now().UTC().Format(time.RFC3339),
	}}
}

// Sign returns Metronome-Webhook-Signature for a body and date:
// hex(HMAC-SHA256(secret, date + "\n" + body)).
func Sign(body []byte, date, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(date))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SendWebhook posts a signed notification to url, as Metronome delivers
// webhooks, and returns the status code and body of the answer.
func (s *Server) SendWebhook(url, secret string, n Notification) (int, []byte, error) {
	body, err := json.Marshal(n)
	if err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	date := s.now().UTC().Format(http.TimeFormat)
	s.mu.Unlock()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metronome-Date", date)
	req.Header.Set("Metronome-Webhook-Signature", Sign(body, date, secret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}
