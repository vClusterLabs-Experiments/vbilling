package lago

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeLago models the slice of Lago v1.17's API the adapter uses, with the
// update semantics that make careless updates destructive:
//   - a plan update deletes the charges it omits, and resets the display
//     name, properties (to amount "0") and filters of the charges it sends;
//   - min_amount_cents falls back to 0 when omitted (as on premium Lago);
//   - every charge must name an existing billable_metric_id, or 404;
//   - charge filter values must be listed by the billable metric's filters;
//   - a billable metric update discards the filter keys and values it omits,
//     and prunes the charge filters that used them.
//
// Every request is recorded with its body.
type fakeLago struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	metrics map[string]*fakeMetric // code ->
	plans   map[string]*fakePlan   // code ->
	events  []Event
	fail    map[string]int // "METHOD /path" -> status to answer
	reqs    []fakeReq
	seq     int
}

type fakeReq struct {
	Method, Path string
	Body         []byte
}

type fakeMetric struct {
	ID, Code, Name string
	Filters        []MetricFilter
}

type fakePlan struct {
	ID, Code, Name, Interval string
	Charges                  []map[string]any // as GET returns them
}

func newFakeLago(t *testing.T) *fakeLago {
	f := &fakeLago{t: t, metrics: map[string]*fakeMetric{}, plans: map[string]*fakePlan{}, fail: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLago) client() *Client { return NewClient(f.srv.URL, "key") }

func (f *fakeLago) id(kind string) string {
	f.seq++
	return fmt.Sprintf("%s-%04d", kind, f.seq)
}

// addMetric creates a billable metric directly in the fake.
func (f *fakeLago) addMetric(code, name string, filters ...MetricFilter) *fakeMetric {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &fakeMetric{ID: f.id("bm"), Code: code, Name: name, Filters: filters}
	f.metrics[code] = m
	return m
}

// addPlan creates a plan directly in the fake from GET-shaped charges.
func (f *fakeLago) addPlan(code string, charges ...map[string]any) *fakePlan {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakePlan{ID: f.id("plan"), Code: code, Name: code, Interval: "monthly"}
	for _, c := range charges {
		p.Charges = append(p.Charges, f.normalizeCharge(c))
	}
	f.plans[code] = p
	return p
}

// charge builds a GET-shaped standard charge for a metric.
func (f *fakeLago) charge(metric, amount string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.metrics[metric]
	if m == nil {
		f.t.Fatalf("fake: no metric %s", metric)
	}
	return map[string]any{
		"lago_id": f.id("charge"), "lago_billable_metric_id": m.ID, "billable_metric_code": m.Code,
		"charge_model": "standard", "properties": map[string]any{"amount": amount},
	}
}

func (f *fakeLago) normalizeCharge(c map[string]any) map[string]any {
	out := map[string]any{
		"invoice_display_name": nil, "pay_in_advance": false, "prorated": false, "invoiceable": true,
		"regroup_paid_fees": nil, "min_amount_cents": float64(0), "filters": []any{}, "taxes": []any{},
		"created_at": "2026-11-01T00:00:00Z",
	}
	for k, v := range roundTrip(c) {
		out[k] = v
	}
	return out
}

// roundTrip returns v as generic JSON values.
func roundTrip(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// planJSON is the plan as GET returns it.
func (f *fakeLago) planJSON(code string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.planLocked(f.plans[code])
}

func (f *fakeLago) planLocked(p *fakePlan) map[string]any {
	return roundTrip(map[string]any{
		"lago_id": p.ID, "code": p.Code, "name": p.Name, "interval": p.Interval,
		"amount_cents": 0, "amount_currency": "USD", "bill_charges_monthly": nil, "charges": p.Charges,
	})
}

func (f *fakeLago) metricLocked(m *fakeMetric) map[string]any {
	filters := []MetricFilter{}
	for _, mf := range m.Filters {
		vs := append([]string(nil), mf.Values...)
		sort.Strings(vs) // Lago returns filter values sorted
		filters = append(filters, MetricFilter{Key: mf.Key, Values: vs})
	}
	return roundTrip(map[string]any{
		"lago_id": m.ID, "code": m.Code, "name": m.Name, "aggregation_type": "sum_agg",
		"field_name": fieldNameFor(m.Code), "filters": filters,
	})
}

// requests returns the recorded requests matching method and path prefix.
func (f *fakeLago) requests(method, prefix string) []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReq
	for _, r := range f.reqs {
		if r.Method == method && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeLago) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = nil
}

func (f *fakeLago) setFail(key string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.fail, key)
		return
	}
	f.fail[key] = status
}

func (f *fakeLago) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeLago) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, fakeReq{Method: r.Method, Path: r.URL.Path, Body: body})
	if status := f.fail[r.Method+" "+r.URL.Path]; status != 0 {
		f.write(w, status, map[string]any{"status": status, "error": "injected"})
		return
	}
	var req map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			f.write(w, 400, map[string]any{"status": 400, "error": err.Error()})
			return
		}
	}
	notFound := func(resource string) {
		f.write(w, 404, map[string]any{"status": 404, "error": "Not Found", "code": resource + "_not_found"})
	}
	switch path := r.URL.Path; {
	case r.Method == http.MethodPost && path == "/api/v1/events/batch":
		var batch struct {
			Events []Event `json:"events"`
		}
		json.Unmarshal(body, &batch)
		f.events = append(f.events, batch.Events...)
		f.write(w, 200, map[string]any{})

	case r.Method == http.MethodPost && path == "/api/v1/billable_metrics":
		in := req["billable_metric"].(map[string]any)
		code := in["code"].(string)
		if f.metrics[code] != nil {
			f.write(w, 422, map[string]any{"status": 422, "code": "validation_errors", "error_details": map[string]any{"code": []string{"value_already_exist"}}})
			return
		}
		m := &fakeMetric{ID: f.id("bm"), Code: code, Name: in["name"].(string)}
		f.metrics[code] = m
		f.write(w, 200, map[string]any{"billable_metric": f.metricLocked(m)})

	case strings.HasPrefix(path, "/api/v1/billable_metrics/"):
		m := f.metrics[strings.TrimPrefix(path, "/api/v1/billable_metrics/")]
		if m == nil {
			notFound("billable_metric")
			return
		}
		if r.Method == http.MethodPut {
			in := req["billable_metric"].(map[string]any)
			if raw, ok := in["filters"]; ok {
				var filters []MetricFilter
				b, _ := json.Marshal(raw)
				json.Unmarshal(b, &filters)
				m.Filters = filters
				f.pruneChargeFilters(m)
			}
		}
		f.write(w, 200, map[string]any{"billable_metric": f.metricLocked(m)})

	case r.Method == http.MethodPost && path == "/api/v1/plans":
		in := req["plan"].(map[string]any)
		code := in["code"].(string)
		if f.plans[code] != nil {
			f.write(w, 422, map[string]any{"status": 422, "code": "validation_errors", "error_details": map[string]any{"code": []string{"value_already_exist"}}})
			return
		}
		p := &fakePlan{ID: f.id("plan"), Code: code, Name: in["name"].(string), Interval: in["interval"].(string)}
		if status, msg := f.applyCharges(p, in["charges"]); status != 0 {
			f.write(w, status, map[string]any{"status": status, "error": msg})
			return
		}
		f.plans[code] = p
		f.write(w, 200, map[string]any{"plan": f.planLocked(p)})

	case strings.HasPrefix(path, "/api/v1/plans/"):
		p := f.plans[strings.TrimPrefix(path, "/api/v1/plans/")]
		if p == nil {
			notFound("plan")
			return
		}
		if r.Method == http.MethodPut {
			in := req["plan"].(map[string]any)
			if charges, ok := in["charges"]; ok && charges != nil {
				if status, msg := f.applyCharges(p, charges); status != 0 {
					f.write(w, status, map[string]any{"status": status, "error": msg})
					return
				}
			}
		}
		f.write(w, 200, map[string]any{"plan": f.planLocked(p)})

	case r.Method == http.MethodPost && path == "/api/v1/customers":
		f.write(w, 200, map[string]any{"customer": req["customer"]})
	case r.Method == http.MethodPost && path == "/api/v1/subscriptions":
		f.write(w, 200, map[string]any{"subscription": req["subscription"]})
	default:
		notFound("route")
	}
}

// applyCharges replaces p's charges with payload the way Lago's plan
// update does. It returns a non-zero status when Lago would reject it.
func (f *fakeLago) applyCharges(p *fakePlan, payload any) (int, string) {
	list, _ := payload.([]any)
	byID := map[string]*fakeMetric{}
	for _, m := range f.metrics {
		byID[m.ID] = m
	}
	var out []map[string]any
	for _, raw := range list {
		in := raw.(map[string]any)
		m := byID[fmt.Sprint(in["billable_metric_id"])]
		if m == nil {
			return 404, "billable_metrics_not_found"
		}
		var cur map[string]any
		for _, c := range p.Charges {
			if id, ok := in["id"].(string); ok && c["lago_id"] == id {
				cur = c
			}
		}
		if cur == nil {
			cur = f.normalizeCharge(map[string]any{"lago_id": f.id("charge")})
		}
		cur["lago_billable_metric_id"] = m.ID
		cur["billable_metric_code"] = m.Code
		cur["charge_model"] = in["charge_model"]
		cur["invoice_display_name"] = in["invoice_display_name"] // nil when omitted, like Lago
		props, _ := in["properties"].(map[string]any)
		if len(props) == 0 {
			props = map[string]any{"amount": "0"}
		}
		if _, ok := props["amount"].(string); !ok && in["charge_model"] == "standard" {
			return 422, "invalid_amount"
		}
		cur["properties"] = props
		cur["min_amount_cents"] = float64(0)
		for _, k := range []string{"pay_in_advance", "prorated", "invoiceable", "regroup_paid_fees", "min_amount_cents"} {
			if v, ok := in[k]; ok && v != nil {
				cur[k] = v
			}
		}
		if raw, ok := in["filters"]; ok && raw != nil {
			var filters []any
			for _, fr := range raw.([]any) {
				fm := fr.(map[string]any)
				values, _ := fm["values"].(map[string]any)
				for key, vs := range values {
					allowed := map[string]bool{allFilterValues: true}
					found := false
					for _, mf := range m.Filters {
						if mf.Key == key {
							found = true
							for _, v := range mf.Values {
								allowed[v] = true
							}
						}
					}
					for _, v := range vs.([]any) {
						if !found || !allowed[v.(string)] {
							return 422, fmt.Sprintf("charge filter value %s=%v is not a value of billable metric %s", key, v, m.Code)
						}
					}
				}
				fp, _ := fm["properties"].(map[string]any)
				if _, ok := fp["amount"].(string); !ok {
					return 422, "invalid_amount"
				}
				filters = append(filters, map[string]any{"invoice_display_name": fm["invoice_display_name"], "properties": fp, "values": values})
			}
			if filters == nil {
				filters = []any{}
			}
			cur["filters"] = filters
		}
		if raw, ok := in["tax_codes"]; ok && raw != nil {
			taxes := []any{}
			for _, c := range raw.([]any) {
				taxes = append(taxes, map[string]any{"code": c})
			}
			cur["taxes"] = taxes
		}
		out = append(out, cur)
	}
	p.Charges = out // charges the payload omits are deleted
	return 0, ""
}

// pruneChargeFilters drops charge filter values that m no longer lists.
func (f *fakeLago) pruneChargeFilters(m *fakeMetric) {
	allowed := map[string]map[string]bool{}
	for _, mf := range m.Filters {
		allowed[mf.Key] = map[string]bool{allFilterValues: true}
		for _, v := range mf.Values {
			allowed[mf.Key][v] = true
		}
	}
	for _, p := range f.plans {
		for _, c := range p.Charges {
			if c["lago_billable_metric_id"] != m.ID {
				continue
			}
			kept := []any{}
			for _, fr := range c["filters"].([]any) {
				fm := fr.(map[string]any)
				values := map[string]any{}
				for key, vs := range fm["values"].(map[string]any) {
					var keep []any
					for _, v := range vs.([]any) {
						if allowed[key][v.(string)] {
							keep = append(keep, v)
						}
					}
					if len(keep) > 0 {
						values[key] = keep
					}
				}
				if len(values) > 0 {
					fm["values"] = values
					kept = append(kept, fm)
				}
			}
			c["filters"] = kept
		}
	}
}

// chargeFor returns the GET-shaped charge of metric on plan.
func (f *fakeLago) chargeFor(plan, metric string) map[string]any {
	for _, c := range f.planJSON(plan)["charges"].([]any) {
		if c.(map[string]any)["billable_metric_code"] == metric {
			return c.(map[string]any)
		}
	}
	return nil
}
