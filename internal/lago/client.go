package lago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
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
	LagoID          string `json:"lago_id,omitempty"`
	Name            string `json:"name"`
	Code            string `json:"code"`
	Description     string `json:"description,omitempty"`
	AggregationType string `json:"aggregation_type"`
	FieldName       string `json:"field_name,omitempty"`
}

type Charge struct {
	BillableMetricID string            `json:"billable_metric_id,omitempty"`
	BillableMetricCode string          `json:"billable_metric_code,omitempty"`
	ChargeModel      string            `json:"charge_model"`
	PayInAdvance     bool              `json:"pay_in_advance"`
	Properties       map[string]string `json:"properties"`
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

type Customer struct {
	ExternalID string            `json:"external_id"`
	Name       string            `json:"name"`
	Email      string            `json:"email,omitempty"`
	Currency   string            `json:"currency,omitempty"`
	Metadata   []CustomerMeta    `json:"metadata,omitempty"`
}

type CustomerMeta struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Subscription struct {
	ExternalCustomerID string `json:"external_customer_id"`
	PlanCode           string `json:"plan_code"`
	ExternalID         string `json:"external_id"`
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

type apiError struct {
	Status  int    `json:"status"`
	Error   string `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Billable Metrics ---

func (c *Client) CreateBillableMetric(m BillableMetric) (*BillableMetric, error) {
	body := map[string]BillableMetric{"billable_metric": m}
	var resp billableMetricResp
	if err := c.post("/api/v1/billable_metrics", body, &resp); err != nil {
		return nil, fmt.Errorf("create billable metric %q: %w", m.Code, err)
	}
	return &resp.BillableMetric, nil
}

func (c *Client) GetBillableMetric(code string) (*BillableMetric, error) {
	var resp billableMetricResp
	if err := c.get("/api/v1/billable_metrics/"+code, &resp); err != nil {
		return nil, err
	}
	return &resp.BillableMetric, nil
}

// --- Plans ---

func (c *Client) CreatePlan(p Plan) (*Plan, error) {
	body := map[string]Plan{"plan": p}
	var resp planResp
	if err := c.post("/api/v1/plans", body, &resp); err != nil {
		return nil, fmt.Errorf("create plan %q: %w", p.Code, err)
	}
	return &resp.Plan, nil
}

func (c *Client) GetPlan(code string) (*Plan, error) {
	var resp planResp
	if err := c.get("/api/v1/plans/"+code, &resp); err != nil {
		return nil, err
	}
	return &resp.Plan, nil
}

// --- Customers ---

func (c *Client) UpsertCustomer(cust Customer) (*Customer, error) {
	body := map[string]Customer{"customer": cust}
	var resp customerResp
	if err := c.post("/api/v1/customers", body, &resp); err != nil {
		return nil, fmt.Errorf("upsert customer %q: %w", cust.ExternalID, err)
	}
	return &resp.Customer, nil
}

// --- Subscriptions ---

func (c *Client) CreateSubscription(sub Subscription) (*Subscription, error) {
	body := map[string]Subscription{"subscription": sub}
	var resp subscriptionResp
	if err := c.post("/api/v1/subscriptions", body, &resp); err != nil {
		return nil, fmt.Errorf("create subscription %q: %w", sub.ExternalID, err)
	}
	return &resp.Subscription, nil
}

func (c *Client) TerminateSubscription(externalID string) error {
	return c.delete("/api/v1/subscriptions/" + externalID)
}

// --- Events ---

func (c *Client) SendEvent(event Event) error {
	body := map[string]Event{"event": event}
	return c.post("/api/v1/events", body, nil)
}

func (c *Client) SendEvents(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	body := map[string][]Event{"events": events}
	return c.post("/api/v1/events/batch", body, nil)
}

// --- Current Usage ---

func (c *Client) GetCurrentUsage(customerExternalID, subscriptionExternalID string) (int, error) {
	path := fmt.Sprintf("/api/v1/customers/%s/current_usage?external_subscription_id=%s",
		customerExternalID, subscriptionExternalID)
	var resp currentUsageResp
	if err := c.get(path, &resp); err != nil {
		return 0, err
	}
	return resp.CustomerUsage.AmountCents, nil
}

// --- HTTP helpers ---

func (c *Client) post(path string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		var apiErr apiError
		if json.Unmarshal(respBody, &apiErr) == nil && apiErr.Code != "" {
			return fmt.Errorf("lago API error %d: %s - %s", resp.StatusCode, apiErr.Code, apiErr.Message)
		}
		return fmt.Errorf("lago API error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	log.Printf("[lago] POST %s -> %d", path, resp.StatusCode)
	return nil
}

func (c *Client) get(path string, result interface{}) error {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 404 {
		return fmt.Errorf("not found: %s", path)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("lago API error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) delete(path string) error {
	req, err := http.NewRequest("DELETE", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("lago API error %d: %s", resp.StatusCode, string(respBody))
	}

	log.Printf("[lago] DELETE %s -> %d", path, resp.StatusCode)
	return nil
}
