package lago

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/version"
)

// Client is an HTTP client for the Lago billing API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// --- API Types ---

type BillableMetric struct {
	LagoID          string         `json:"lago_id,omitempty"`
	Name            string         `json:"name"`
	Code            string         `json:"code"`
	Description     string         `json:"description,omitempty"`
	AggregationType string         `json:"aggregation_type"`
	FieldName       string         `json:"field_name,omitempty"`
	Filters         []MetricFilter `json:"filters,omitempty"`
}

// MetricFilter is one key of a billable metric's filters, with the values
// that charge filters may price.
type MetricFilter struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

type Charge struct {
	BillableMetricID   string            `json:"billable_metric_id,omitempty"`
	BillableMetricCode string            `json:"billable_metric_code,omitempty"`
	ChargeModel        string            `json:"charge_model"`
	PayInAdvance       bool              `json:"pay_in_advance"`
	Properties         map[string]string `json:"properties"`
}

type Plan struct {
	LagoID         string   `json:"lago_id,omitempty"`
	Name           string   `json:"name"`
	Code           string   `json:"code"`
	Interval       string   `json:"interval"`
	AmountCents    int      `json:"amount_cents"`
	AmountCurrency string   `json:"amount_currency"`
	PayInAdvance   bool     `json:"pay_in_advance"`
	Charges        []Charge `json:"charges"`
}

// PlanDetail is a plan as GET /api/v1/plans/{code} returns it. A Lago plan
// update deletes every charge it does not receive, and resets the display
// name, properties and filters of the charges it does receive, so charges
// keep those fields verbatim for UpdatePlan to send back unchanged.
type PlanDetail struct {
	LagoID             string          `json:"lago_id"`
	Name               string          `json:"name"`
	Code               string          `json:"code"`
	Interval           string          `json:"interval"`
	BillChargesMonthly json.RawMessage `json:"bill_charges_monthly,omitempty"`
	Charges            []PlanCharge    `json:"charges"`
}

// PlanCharge is a charge as Lago returns it. UpdatePlan creates the charges
// that have no LagoID.
type PlanCharge struct {
	LagoID               string          `json:"lago_id,omitempty"`
	LagoBillableMetricID string          `json:"lago_billable_metric_id"`
	BillableMetricCode   string          `json:"billable_metric_code,omitempty"`
	InvoiceDisplayName   json.RawMessage `json:"invoice_display_name,omitempty"`
	ChargeModel          string          `json:"charge_model"`
	PayInAdvance         json.RawMessage `json:"pay_in_advance,omitempty"`
	Prorated             json.RawMessage `json:"prorated,omitempty"`
	Invoiceable          json.RawMessage `json:"invoiceable,omitempty"`
	RegroupPaidFees      json.RawMessage `json:"regroup_paid_fees,omitempty"`
	MinAmountCents       json.RawMessage `json:"min_amount_cents,omitempty"`
	Properties           json.RawMessage `json:"properties,omitempty"`
	Filters              []ChargeFilter  `json:"filters,omitempty"`
	Taxes                []Tax           `json:"taxes,omitempty"`
}

// ChargeFilter prices the events whose properties match Values.
type ChargeFilter struct {
	InvoiceDisplayName json.RawMessage     `json:"invoice_display_name,omitempty"`
	Properties         json.RawMessage     `json:"properties"`
	Values             map[string][]string `json:"values"`
}

type Tax struct {
	Code string `json:"code"`
}

// chargeInput is a charge in a plan update (PlansController#input_params).
type chargeInput struct {
	ID                 string          `json:"id,omitempty"`
	BillableMetricID   string          `json:"billable_metric_id"`
	InvoiceDisplayName json.RawMessage `json:"invoice_display_name,omitempty"`
	ChargeModel        string          `json:"charge_model"`
	PayInAdvance       json.RawMessage `json:"pay_in_advance,omitempty"`
	Prorated           json.RawMessage `json:"prorated,omitempty"`
	Invoiceable        json.RawMessage `json:"invoiceable,omitempty"`
	RegroupPaidFees    json.RawMessage `json:"regroup_paid_fees,omitempty"`
	MinAmountCents     json.RawMessage `json:"min_amount_cents,omitempty"`
	Properties         json.RawMessage `json:"properties,omitempty"`
	Filters            []ChargeFilter  `json:"filters,omitempty"`
	// Omitted when empty: Lago removes a charge's taxes when it receives an
	// empty list, and leaves them alone when it receives none.
	TaxCodes []string `json:"tax_codes,omitempty"`
}

// planUpdate is the body of PUT /api/v1/plans/{code}. Besides the charges it
// carries only interval and bill_charges_monthly, because Lago recomputes
// bill_charges_monthly from the interval it receives.
type planUpdate struct {
	Interval           string          `json:"interval,omitempty"`
	BillChargesMonthly json.RawMessage `json:"bill_charges_monthly,omitempty"`
	Charges            []chargeInput   `json:"charges"`
}

type Customer struct {
	ExternalID string         `json:"external_id"`
	Name       string         `json:"name"`
	Email      string         `json:"email,omitempty"`
	Currency   string         `json:"currency,omitempty"`
	Metadata   []CustomerMeta `json:"metadata,omitempty"`
}

type CustomerMeta struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Subscription struct {
	ExternalCustomerID string `json:"external_customer_id"`
	PlanCode           string `json:"plan_code"`
	ExternalID         string `json:"external_id"`
	// SubscriptionAt starts the subscription (ISO 8601). Lago counts usage
	// from it; without it the subscription starts when it is created.
	SubscriptionAt string `json:"subscription_at,omitempty"`
}

type Event struct {
	TransactionID          string                 `json:"transaction_id"`
	ExternalSubscriptionID string                 `json:"external_subscription_id"`
	Code                   string                 `json:"code"`
	Timestamp              int64                  `json:"timestamp"`
	Properties             map[string]interface{} `json:"properties"`
}

// --- API response wrappers ---

type billableMetricResp struct {
	BillableMetric BillableMetric `json:"billable_metric"`
}

type planResp struct {
	Plan Plan `json:"plan"`
}

type planDetailResp struct {
	Plan PlanDetail `json:"plan"`
}

type customerResp struct {
	Customer Customer `json:"customer"`
}

type subscriptionResp struct {
	Subscription Subscription `json:"subscription"`
}

type currentUsageResp struct {
	CustomerUsage struct {
		FromDatetime   string `json:"from_datetime"`
		ToDatetime     string `json:"to_datetime"`
		AmountCents    int    `json:"amount_cents"`
		AmountCurrency string `json:"amount_currency"`
	} `json:"customer_usage"`
}

// --- Billable Metrics ---

func (c *Client) CreateBillableMetric(ctx context.Context, m BillableMetric) (*BillableMetric, error) {
	body := map[string]BillableMetric{"billable_metric": m}
	var resp billableMetricResp
	if err := c.post(ctx, "/api/v1/billable_metrics", body, &resp); err != nil {
		return nil, fmt.Errorf("create billable metric %q: %w", m.Code, err)
	}
	return &resp.BillableMetric, nil
}

func (c *Client) GetBillableMetric(ctx context.Context, code string) (*BillableMetric, error) {
	var resp billableMetricResp
	if err := c.get(ctx, "/api/v1/billable_metrics/"+url.PathEscape(code), &resp); err != nil {
		return nil, err
	}
	return &resp.BillableMetric, nil
}

// UpdateBillableMetricFilters replaces a billable metric's filters. Lago
// discards the keys and values it does not receive, together with the charge
// filters that use them, so filters must be the metric's complete set.
func (c *Client) UpdateBillableMetricFilters(ctx context.Context, code string, filters []MetricFilter) (*BillableMetric, error) {
	if filters == nil {
		filters = []MetricFilter{}
	}
	body := map[string]map[string][]MetricFilter{"billable_metric": {"filters": filters}}
	var resp billableMetricResp
	if err := c.put(ctx, "/api/v1/billable_metrics/"+url.PathEscape(code), body, &resp); err != nil {
		return nil, fmt.Errorf("update billable metric %q filters: %w", code, err)
	}
	return &resp.BillableMetric, nil
}

// --- Plans ---

func (c *Client) CreatePlan(ctx context.Context, p Plan) (*Plan, error) {
	body := map[string]Plan{"plan": p}
	var resp planResp
	if err := c.post(ctx, "/api/v1/plans", body, &resp); err != nil {
		return nil, fmt.Errorf("create plan %q: %w", p.Code, err)
	}
	return &resp.Plan, nil
}

func (c *Client) GetPlan(ctx context.Context, code string) (*PlanDetail, error) {
	var resp planDetailResp
	if err := c.get(ctx, "/api/v1/plans/"+url.PathEscape(code), &resp); err != nil {
		return nil, err
	}
	return &resp.Plan, nil
}

// UpdatePlan sends p's charges to Lago: the existing ones (with a LagoID)
// exactly as GetPlan returned them, plus any charge or filter added to p.
func (c *Client) UpdatePlan(ctx context.Context, p *PlanDetail) (*PlanDetail, error) {
	up := planUpdate{Interval: p.Interval, BillChargesMonthly: p.BillChargesMonthly, Charges: make([]chargeInput, 0, len(p.Charges))}
	for _, ch := range p.Charges {
		in := chargeInput{
			ID:                 ch.LagoID,
			BillableMetricID:   ch.LagoBillableMetricID,
			InvoiceDisplayName: ch.InvoiceDisplayName,
			ChargeModel:        ch.ChargeModel,
			PayInAdvance:       ch.PayInAdvance,
			Prorated:           ch.Prorated,
			Invoiceable:        ch.Invoiceable,
			RegroupPaidFees:    ch.RegroupPaidFees,
			MinAmountCents:     ch.MinAmountCents,
			Properties:         ch.Properties,
			Filters:            ch.Filters,
		}
		for _, t := range ch.Taxes {
			in.TaxCodes = append(in.TaxCodes, t.Code)
		}
		up.Charges = append(up.Charges, in)
	}
	var resp planDetailResp
	if err := c.put(ctx, "/api/v1/plans/"+url.PathEscape(p.Code), map[string]planUpdate{"plan": up}, &resp); err != nil {
		return nil, fmt.Errorf("update plan %q: %w", p.Code, err)
	}
	return &resp.Plan, nil
}

// --- Customers ---

func (c *Client) UpsertCustomer(ctx context.Context, cust Customer) (*Customer, error) {
	body := map[string]Customer{"customer": cust}
	var resp customerResp
	if err := c.post(ctx, "/api/v1/customers", body, &resp); err != nil {
		return nil, fmt.Errorf("upsert customer %q: %w", cust.ExternalID, err)
	}
	return &resp.Customer, nil
}

// --- Subscriptions ---

func (c *Client) CreateSubscription(ctx context.Context, sub Subscription) (*Subscription, error) {
	body := map[string]Subscription{"subscription": sub}
	var resp subscriptionResp
	if err := c.post(ctx, "/api/v1/subscriptions", body, &resp); err != nil {
		return nil, fmt.Errorf("create subscription %q: %w", sub.ExternalID, err)
	}
	return &resp.Subscription, nil
}

func (c *Client) TerminateSubscription(ctx context.Context, externalID string) error {
	return c.delete(ctx, "/api/v1/subscriptions/"+externalID)
}

// --- Events ---

func (c *Client) SendEvent(ctx context.Context, event Event) error {
	body := map[string]Event{"event": event}
	return c.post(ctx, "/api/v1/events", body, nil)
}

func (c *Client) SendEvents(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	body := map[string][]Event{"events": events}
	return c.post(ctx, "/api/v1/events/batch", body, nil)
}

// --- Current Usage ---

func (c *Client) GetCurrentUsage(ctx context.Context, customerExternalID, subscriptionExternalID string) (int, error) {
	path := fmt.Sprintf("/api/v1/customers/%s/current_usage?external_subscription_id=%s",
		customerExternalID, subscriptionExternalID)
	var resp currentUsageResp
	if err := c.get(ctx, path, &resp); err != nil {
		return 0, err
	}
	return resp.CustomerUsage.AmountCents, nil
}

// --- HTTP helpers ---

// isNotFound reports whether err is Lago's 404 for a missing object.
func isNotFound(err error) bool {
	var httpErr *destinations.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound
}

func (c *Client) post(ctx context.Context, path string, body interface{}, result interface{}) error {
	return c.do(ctx, http.MethodPost, path, body, result)
}

func (c *Client) put(ctx context.Context, path string, body interface{}, result interface{}) error {
	return c.do(ctx, http.MethodPut, path, body, result)
}

func (c *Client) get(ctx context.Context, path string, result interface{}) error {
	return c.do(ctx, http.MethodGet, path, nil, result)
}

// do sends one request. A non-2xx response becomes a *destinations.HTTPError,
// permanent for payload rejections (see destinations.StatusError).
func (c *Client) do(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return destinations.StatusError(method, path, resp.StatusCode, respBody)
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return destinations.StatusError(http.MethodDelete, path, resp.StatusCode, respBody)
	}

	log.Printf("[lago] DELETE %s -> %d", path, resp.StatusCode)
	return nil
}
