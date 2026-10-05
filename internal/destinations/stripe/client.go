// Package stripe ships usage to Stripe Billing Meters and keeps Stripe
// customers (and optionally subscriptions) in sync with vBilling tenants.
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
)

// Client is a minimal Stripe REST client (form-encoded v1 API).
type Client struct {
	base    string
	key     string
	version string
	http    *http.Client
	limit   *limiter

	noSearch atomic.Bool // the account has no customer search (not offered in every region)
}

// NewClient builds a client. rps <= 0 picks a default from the key mode.
func NewClient(base, key, version string, rps float64) *Client {
	base = strings.TrimRight(base, "/")
	if rps <= 0 {
		switch {
		case !strings.Contains(base, "api.stripe.com"):
			rps = 1000 // stripe-mock or a proxy
		case strings.HasPrefix(key, "sk_test_") || strings.HasPrefix(key, "rk_test_"):
			rps = 20 // sandboxes share a 25 req/s global limit
		default:
			rps = 200
		}
	}
	return &Client{base: base, key: key, version: version, http: destinations.NewHTTPClient(), limit: newLimiter(rps)}
}

// APIError is a Stripe error response.
type APIError struct {
	Status  int
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param"`
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("stripe %s: HTTP %d %s/%s: %s", e.Path, e.Status, e.Type, e.Code, e.Message)
}

// IsCode reports whether err is a Stripe API error with the given code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return asAPIError(err, &apiErr) && apiErr.Code == code
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, idemKey string, out any) error {
	if err := c.limit.Wait(ctx); err != nil {
		return err
	}
	target := c.base + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Stripe-Version", c.version)
	req.Header.Set("User-Agent", "vbilling/0.2")
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("stripe %s %s: decode: %w", method, path, err)
		}
		return nil
	}

	apiErr := &APIError{Status: resp.StatusCode, Path: method + " " + path}
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if json.Unmarshal(b, &envelope) == nil && envelope.Error != nil {
		envelope.Error.Status, envelope.Error.Path = apiErr.Status, apiErr.Path
		apiErr = envelope.Error
	} else {
		apiErr.Message = strings.TrimSpace(string(b))
	}
	// Stripe tells us explicitly when it knows better than the status code.
	switch resp.Header.Get("Stripe-Should-Retry") {
	case "true":
		return apiErr
	case "false":
		return destinations.Permanent(apiErr)
	}
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return destinations.Permanent(apiErr)
	default: // 401/403 (config), 404, 409 (concurrency), 429, 5xx
		return apiErr
	}
}

// --- objects ---

type Meter struct {
	ID              string `json:"id"`
	EventName       string `json:"event_name"`
	DisplayName     string `json:"display_name"`
	Status          string `json:"status"`
	CustomerMapping struct {
		EventPayloadKey string `json:"event_payload_key"`
	} `json:"customer_mapping"`
	ValueSettings struct {
		EventPayloadKey string `json:"event_payload_key"`
	} `json:"value_settings"`
}

func (m Meter) customerKey() string {
	if m.CustomerMapping.EventPayloadKey != "" {
		return m.CustomerMapping.EventPayloadKey
	}
	return "stripe_customer_id"
}

func (m Meter) valueKey() string {
	if m.ValueSettings.EventPayloadKey != "" {
		return m.ValueSettings.EventPayloadKey
	}
	return "value"
}

type Customer struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Email    string            `json:"email"`
	Deleted  bool              `json:"deleted"`
	Metadata map[string]string `json:"metadata"`
}

type Price struct {
	ID        string            `json:"id"`
	Active    bool              `json:"active"`
	Currency  string            `json:"currency"`
	Metadata  map[string]string `json:"metadata"`
	Recurring *struct {
		UsageType string `json:"usage_type"`
		Meter     string `json:"meter"`
	} `json:"recurring"`
}

type Subscription struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Customer string            `json:"customer"`
	Metadata map[string]string `json:"metadata"`
	Items    struct {
		Data []struct {
			ID    string `json:"id"`
			Price Price  `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type list[T any] struct {
	Data     []T    `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

type idObject interface{ objectID() string }

func (m Meter) objectID() string        { return m.ID }
func (p Price) objectID() string        { return p.ID }
func (s Subscription) objectID() string { return s.ID }

// listAll pages through a v1 list endpoint (max 50 pages as a safety net).
func listAll[T idObject](ctx context.Context, c *Client, path string, params url.Values) ([]T, error) {
	var out []T
	if params == nil {
		params = url.Values{}
	}
	params.Set("limit", "100")
	for page := 0; page < 50; page++ {
		var l list[T]
		if err := c.do(ctx, http.MethodGet, path, params, "", &l); err != nil {
			return nil, err
		}
		out = append(out, l.Data...)
		if !l.HasMore || len(l.Data) == 0 {
			return out, nil
		}
		params.Set("starting_after", l.Data[len(l.Data)-1].objectID())
	}
	return out, nil
}

// --- meters ---

func (c *Client) ListMeters(ctx context.Context) ([]Meter, error) {
	return listAll[Meter](ctx, c, "/v1/billing/meters", nil)
}

func (c *Client) CreateMeter(ctx context.Context, eventName, displayName string) (Meter, error) {
	f := url.Values{}
	f.Set("display_name", truncate(displayName, 250))
	f.Set("event_name", eventName)
	f.Set("default_aggregation[formula]", "sum")
	f.Set("customer_mapping[type]", "by_id")
	f.Set("customer_mapping[event_payload_key]", "stripe_customer_id")
	f.Set("value_settings[event_payload_key]", "value")
	var m Meter
	err := c.do(ctx, http.MethodPost, "/v1/billing/meters", f, "vbilling-meter-"+eventName, &m)
	return m, err
}

func (c *Client) ReactivateMeter(ctx context.Context, id string) (Meter, error) {
	var m Meter
	err := c.do(ctx, http.MethodPost, "/v1/billing/meters/"+url.PathEscape(id)+"/reactivate", url.Values{}, "", &m)
	return m, err
}

// CreateMeterEvent records usage. The identifier deduplicates server-side;
// no Idempotency-Key is sent so a cached 5xx can never pin the event.
func (c *Client) CreateMeterEvent(ctx context.Context, m Meter, customerID, value, identifier string, ts time.Time) error {
	f := url.Values{}
	f.Set("event_name", m.EventName)
	f.Set("payload["+m.customerKey()+"]", customerID)
	f.Set("payload["+m.valueKey()+"]", value)
	f.Set("identifier", identifier)
	f.Set("timestamp", strconv.FormatInt(ts.Unix(), 10))
	return c.do(ctx, http.MethodPost, "/v1/billing/meter_events", f, "", nil)
}

// MeterTotal sums a meter's aggregated value for one customer over
// [start, end). Both bounds must sit on minute boundaries.
func (c *Client) MeterTotal(ctx context.Context, meterID, customerID string, start, end time.Time) (float64, error) {
	params := url.Values{}
	params.Set("customer", customerID)
	params.Set("start_time", strconv.FormatInt(start.Unix(), 10))
	params.Set("end_time", strconv.FormatInt(end.Unix(), 10))
	type summary struct {
		ID              string  `json:"id"`
		AggregatedValue float64 `json:"aggregated_value"`
	}
	var total float64
	for page := 0; page < 50; page++ {
		var l list[summary]
		if err := c.do(ctx, http.MethodGet, "/v1/billing/meters/"+url.PathEscape(meterID)+"/event_summaries", params, "", &l); err != nil {
			return 0, err
		}
		for _, s := range l.Data {
			total += s.AggregatedValue
		}
		if !l.HasMore || len(l.Data) == 0 {
			break
		}
		params.Set("starting_after", l.Data[len(l.Data)-1].ID)
	}
	return total, nil
}

// --- customers ---

func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	var cus Customer
	if err := c.do(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(id), nil, "", &cus); err != nil {
		return nil, err
	}
	return &cus, nil
}

// SearchCustomer finds a customer by metadata. Stripe search is eventually
// consistent (usually under a minute), so callers keep their own mapping.
func (c *Client) SearchCustomer(ctx context.Context, key, value string) (*Customer, error) {
	params := url.Values{}
	params.Set("query", fmt.Sprintf("metadata['%s']:'%s'", key, strings.ReplaceAll(value, "'", `\'`)))
	var l list[Customer]
	if err := c.do(ctx, http.MethodGet, "/v1/customers/search", params, "", &l); err != nil {
		return nil, err
	}
	for _, cus := range l.Data {
		// Trust the metadata, not the search engine: a customer is only ours
		// if it carries exactly this value. Usage must never be attributed to
		// another tenant's customer.
		if !cus.Deleted && cus.Metadata[key] == value {
			return &cus, nil
		}
	}
	return nil, nil
}

// FindCustomer returns the customer whose metadata[key] is value. It uses
// search where the account has it; Stripe does not offer search in every
// region (accounts in India get "The search feature is temporarily
// unavailable in your region."), so a rejected search falls back to listing
// customers, filtered by email first when one is known.
func (c *Client) FindCustomer(ctx context.Context, key, value, email string) (*Customer, error) {
	if !c.noSearch.Load() {
		cus, err := c.SearchCustomer(ctx, key, value)
		var apiErr *APIError
		if err == nil || !asAPIError(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			return cus, err
		}
		// The query is well formed, so a 400 means search is unusable here.
		c.noSearch.Store(true)
		log.Printf("[stripe] customer search unavailable (%s); finding customers by listing them", apiErr.Message)
	}
	if email != "" {
		if cus, err := c.scanCustomers(ctx, url.Values{"email": {email}}, key, value); err != nil || cus != nil {
			return cus, err
		}
	}
	return c.scanCustomers(ctx, url.Values{}, key, value)
}

// scanCustomers pages through customers until one carries metadata[key] = value.
func (c *Client) scanCustomers(ctx context.Context, params url.Values, key, value string) (*Customer, error) {
	params.Set("limit", "100")
	for page := 0; page < 1000; page++ {
		var l list[Customer]
		if err := c.do(ctx, http.MethodGet, "/v1/customers", params, "", &l); err != nil {
			return nil, err
		}
		for _, cus := range l.Data {
			if !cus.Deleted && cus.Metadata[key] == value {
				return &cus, nil
			}
		}
		if !l.HasMore || len(l.Data) == 0 {
			return nil, nil
		}
		params.Set("starting_after", l.Data[len(l.Data)-1].ID)
	}
	return nil, fmt.Errorf("no customer with %s=%s in the first 100,000 customers; pin it with the stripe-customer-id annotation", key, value)
}

func (c *Client) CreateCustomer(ctx context.Context, f url.Values, idemKey string) (*Customer, error) {
	var cus Customer
	if err := c.do(ctx, http.MethodPost, "/v1/customers", f, idemKey, &cus); err != nil {
		return nil, err
	}
	return &cus, nil
}

func (c *Client) UpdateCustomer(ctx context.Context, id string, f url.Values) error {
	return c.do(ctx, http.MethodPost, "/v1/customers/"+url.PathEscape(id), f, "", nil)
}

// --- prices & subscriptions ---

func (c *Client) ListMeteredPrices(ctx context.Context) ([]Price, error) {
	params := url.Values{}
	params.Set("active", "true")
	params.Set("type", "recurring")
	return listAll[Price](ctx, c, "/v1/prices", params)
}

func (c *Client) ListSubscriptions(ctx context.Context, customerID string) ([]Subscription, error) {
	params := url.Values{}
	params.Set("customer", customerID)
	params.Set("status", "all")
	return listAll[Subscription](ctx, c, "/v1/subscriptions", params)
}

func (c *Client) CreateSubscription(ctx context.Context, customerID string, priceIDs []string, meta map[string]string, idemKey string) (*Subscription, error) {
	f := url.Values{}
	f.Set("customer", customerID)
	for i, p := range priceIDs {
		f.Set(fmt.Sprintf("items[%d][price]", i), p)
	}
	for k, v := range meta {
		f.Set("metadata["+k+"]", v)
	}
	var sub Subscription
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions", f, idemKey, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

func (c *Client) AddSubscriptionItem(ctx context.Context, subID, priceID string) error {
	f := url.Values{}
	f.Set("subscription", subID)
	f.Set("price", priceID)
	return c.do(ctx, http.MethodPost, "/v1/subscription_items", f, "vbilling-si-"+subID+"-"+priceID, nil)
}

func (c *Client) CancelAtPeriodEnd(ctx context.Context, subID string) error {
	f := url.Values{}
	f.Set("cancel_at_period_end", "true")
	return c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(subID), f, "", nil)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// limiter is a small token bucket (avoids a dependency for one function).
type limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newLimiter(rps float64) *limiter { return &limiter{rate: rps, tokens: 1, last: time.Now()} }

func (l *limiter) Wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if burst := l.rate / 4; l.tokens > burst+1 {
			l.tokens = burst + 1
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
