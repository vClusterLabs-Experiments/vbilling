package stripe

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStripe models the slice of Stripe's v1 API the adapter uses, with the
// constraints that matter: one meter per event_name, the 35-day timestamp
// window, 15 significant digits, identifier dedupe, eventually consistent
// customer search, idempotency-key replay, and minute-aligned summaries.
type fakeStripe struct {
	t   *testing.T
	srv *httptest.Server
	now func() time.Time

	mu         sync.Mutex
	meters     []*Meter
	customers  map[string]*Customer
	searchable map[string]bool // customers visible to search (eventual consistency)
	events     []fakeEvent
	idents     map[string]bool
	prices     []Price
	subs       map[string]*Subscription
	idem       map[string]string // idempotency key -> response body
	failNext   map[string]int    // "METHOD path" -> number of 500s to return
	searchAll  bool              // search ignores the query (like stripe-mock fixtures)
	noSearch   bool              // search is not offered in the account's region
	requests   []string
	seq        int
}

type fakeEvent struct {
	EventName, Customer, Value, Identifier string
	Timestamp                              int64
}

func newFakeStripe(t *testing.T, now func() time.Time) *fakeStripe {
	f := &fakeStripe{
		t: t, now: now,
		customers:  map[string]*Customer{},
		searchable: map[string]bool{},
		idents:     map[string]bool{},
		subs:       map[string]*Subscription{},
		idem:       map[string]string{},
		failNext:   map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStripe) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%04d", prefix, f.seq)
}

func (f *fakeStripe) fail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": code, "message": msg}})
}

var (
	reMeterAction = regexp.MustCompile(`^/v1/billing/meters/([^/]+)/(reactivate|event_summaries)$`)
	reCustomer    = regexp.MustCompile(`^/v1/customers/(cus_[^/]+)$`)
	reSearch      = regexp.MustCompile(`^metadata\['([^']+)'\]:'(.*)'$`)
)

func (f *fakeStripe) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer sk_test_123" {
		f.fail(w, 401, "", "Invalid API Key provided")
		return
	}
	if r.Header.Get("Stripe-Version") == "" {
		f.t.Errorf("%s %s without Stripe-Version", r.Method, r.URL.Path)
	}
	r.ParseForm()
	key := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, key)
	if n := f.failNext[key]; n > 0 {
		f.failNext[key] = n - 1
		f.fail(w, 500, "", "simulated outage")
		return
	}
	if ik := r.Header.Get("Idempotency-Key"); ik != "" && r.Method == http.MethodPost {
		if body, ok := f.idem[ik]; ok {
			w.Header().Set("Idempotent-Replayed", "true")
			w.Write([]byte(body))
			return
		}
		rec := httptest.NewRecorder()
		f.route(rec, r)
		if rec.Code < 300 {
			f.idem[ik] = rec.Body.String()
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
		return
	}
	f.route(w, r)
}

func writeJSON(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

func page[T any](r *http.Request, items []T, id func(T) string) map[string]any {
	limit, _ := strconv.Atoi(r.Form.Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	start := 0
	if after := r.Form.Get("starting_after"); after != "" {
		for i, it := range items {
			if id(it) == after {
				start = i + 1
			}
		}
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	return map[string]any{"object": "list", "data": items[start:end], "has_more": end < len(items)}
}

func (f *fakeStripe) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == "GET" && p == "/v1/billing/meters":
		var ms []Meter
		for _, m := range f.meters {
			ms = append(ms, *m)
		}
		writeJSON(w, page(r, ms, func(m Meter) string { return m.ID }))

	case r.Method == "POST" && p == "/v1/billing/meters":
		name := r.Form.Get("event_name")
		if name == "" || r.Form.Get("display_name") == "" || r.Form.Get("customer_mapping[type]") != "by_id" {
			f.fail(w, 400, "parameter_missing", "missing meter params")
			return
		}
		if agg := r.Form.Get("default_aggregation[formula]"); agg != "sum" && agg != "count" && agg != "last" {
			f.fail(w, 400, "parameter_invalid", "bad aggregation")
			return
		}
		if len(name) > 100 {
			f.fail(w, 400, "parameter_invalid", "event_name too long")
			return
		}
		for _, m := range f.meters {
			if m.EventName == name {
				f.fail(w, 400, "", "An active meter with event_name already exists")
				return
			}
		}
		m := &Meter{ID: f.id("mtr"), EventName: name, DisplayName: r.Form.Get("display_name"), Status: "active"}
		m.CustomerMapping.EventPayloadKey = r.Form.Get("customer_mapping[event_payload_key]")
		m.ValueSettings.EventPayloadKey = r.Form.Get("value_settings[event_payload_key]")
		f.meters = append(f.meters, m)
		writeJSON(w, m)

	case reMeterAction.MatchString(p):
		parts := reMeterAction.FindStringSubmatch(p)
		var meter *Meter
		for _, m := range f.meters {
			if m.ID == parts[1] {
				meter = m
			}
		}
		if meter == nil {
			f.fail(w, 404, "resource_missing", "no such meter")
			return
		}
		if parts[2] == "reactivate" {
			meter.Status = "active"
			writeJSON(w, meter)
			return
		}
		start, _ := strconv.ParseInt(r.Form.Get("start_time"), 10, 64)
		end, _ := strconv.ParseInt(r.Form.Get("end_time"), 10, 64)
		if start%60 != 0 || end%60 != 0 {
			f.fail(w, 400, "parameter_invalid", "start_time and end_time must be aligned with minute boundaries")
			return
		}
		sum := new(big.Float)
		for _, e := range f.events {
			if e.EventName == meter.EventName && e.Customer == r.Form.Get("customer") && e.Timestamp >= start && e.Timestamp < end {
				v, _ := new(big.Float).SetString(e.Value)
				sum.Add(sum, v)
			}
		}
		total, _ := sum.Float64()
		writeJSON(w, map[string]any{"object": "list", "has_more": false, "data": []map[string]any{{"id": "mtrusg_1", "aggregated_value": total, "start_time": start, "end_time": end}}})

	case r.Method == "POST" && p == "/v1/billing/meter_events":
		name := r.Form.Get("event_name")
		var meter *Meter
		for _, m := range f.meters {
			if m.EventName == name && m.Status == "active" {
				meter = m
			}
		}
		if meter == nil {
			f.fail(w, 400, "no_meter", "no active meter for "+name)
			return
		}
		cus := r.Form.Get("payload[" + meter.customerKey() + "]")
		val := r.Form.Get("payload[" + meter.valueKey() + "]")
		if cus == "" || val == "" {
			f.fail(w, 400, "parameter_missing", "payload keys missing")
			return
		}
		if _, ok := new(big.Float).SetString(val); !ok || strings.ContainsAny(val, "eE") || significantDigits(val) > 15 {
			f.fail(w, 400, "meter_event_invalid_value", "invalid value "+val)
			return
		}
		ts, _ := strconv.ParseInt(r.Form.Get("timestamp"), 10, 64)
		now := f.now().Unix()
		if ts < now-35*86400 || ts > now+300 {
			f.fail(w, 400, "timestamp_too_far_in_past", "timestamp out of range")
			return
		}
		ident := r.Form.Get("identifier")
		if f.idents[ident] {
			f.fail(w, 400, "duplicate_meter_event", "An event already exists with identifier "+ident)
			return
		}
		f.idents[ident] = true
		f.events = append(f.events, fakeEvent{EventName: name, Customer: cus, Value: val, Identifier: ident, Timestamp: ts})
		writeJSON(w, map[string]any{"object": "billing.meter_event", "identifier": ident})

	case r.Method == "GET" && p == "/v1/customers/search":
		if f.noSearch {
			f.fail(w, 400, "", "The search feature is temporarily unavailable in your region.")
			return
		}
		m := reSearch.FindStringSubmatch(r.Form.Get("query"))
		if m == nil {
			f.fail(w, 400, "parameter_invalid", "bad query")
			return
		}
		var out []Customer
		for id, c := range f.customers {
			if f.searchable[id] && (f.searchAll || c.Metadata[m[1]] == strings.ReplaceAll(m[2], `\'`, "'")) {
				out = append(out, *c)
			}
		}
		writeJSON(w, map[string]any{"object": "search_result", "data": out, "has_more": false})

	case r.Method == "GET" && p == "/v1/customers":
		var out []Customer
		for _, c := range f.customers {
			if email := r.Form.Get("email"); email == "" || c.Email == email {
				out = append(out, *c)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		writeJSON(w, page(r, out, func(c Customer) string { return c.ID }))

	case r.Method == "POST" && p == "/v1/customers":
		c := &Customer{ID: f.id("cus"), Name: r.Form.Get("name"), Email: r.Form.Get("email"), Metadata: metadataOf(r)}
		f.customers[c.ID] = c
		writeJSON(w, c)

	case reCustomer.MatchString(p):
		id := reCustomer.FindStringSubmatch(p)[1]
		c, ok := f.customers[id]
		if !ok {
			f.fail(w, 404, "resource_missing", "No such customer: "+id)
			return
		}
		if r.Method == "POST" {
			c.Name = r.Form.Get("name")
			for k, v := range metadataOf(r) {
				c.Metadata[k] = v
			}
		}
		writeJSON(w, c)

	case r.Method == "GET" && p == "/v1/prices":
		writeJSON(w, page(r, f.prices, func(p Price) string { return p.ID }))

	case r.Method == "GET" && p == "/v1/subscriptions":
		var out []Subscription
		for _, s := range f.subs {
			if s.Customer == r.Form.Get("customer") {
				out = append(out, *s)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		writeJSON(w, page(r, out, func(s Subscription) string { return s.ID }))

	case r.Method == "POST" && p == "/v1/subscriptions":
		s := &Subscription{ID: f.id("sub"), Status: "active", Customer: r.Form.Get("customer"), Metadata: metadataOf(r)}
		for i := 0; ; i++ {
			pid := r.Form.Get(fmt.Sprintf("items[%d][price]", i))
			if pid == "" {
				break
			}
			if r.Form.Get(fmt.Sprintf("items[%d][quantity]", i)) != "" {
				f.fail(w, 400, "parameter_invalid", "quantity not allowed for metered prices")
				return
			}
			f.addItem(s, pid)
		}
		f.subs[s.ID] = s
		writeJSON(w, s)

	case r.Method == "POST" && p == "/v1/subscription_items":
		s, ok := f.subs[r.Form.Get("subscription")]
		if !ok {
			f.fail(w, 400, "resource_missing", "no such subscription")
			return
		}
		f.addItem(s, r.Form.Get("price"))
		writeJSON(w, map[string]any{"object": "subscription_item"})

	default:
		f.fail(w, 404, "", "unhandled "+r.Method+" "+p)
	}
}

func (f *fakeStripe) addItem(s *Subscription, priceID string) {
	for _, p := range f.prices {
		if p.ID == priceID {
			s.Items.Data = append(s.Items.Data, struct {
				ID    string `json:"id"`
				Price Price  `json:"price"`
			}{ID: f.id("si"), Price: p})
		}
	}
}

func metadataOf(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.Form {
		if strings.HasPrefix(k, "metadata[") && strings.HasSuffix(k, "]") {
			out[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
		}
	}
	return out
}

func significantDigits(s string) int {
	s = strings.TrimLeft(strings.Replace(strings.TrimPrefix(s, "-"), ".", "", 1), "0")
	return len(s)
}

func (f *fakeStripe) makeSearchable() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.customers {
		f.searchable[id] = true
	}
}

func (f *fakeStripe) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}
