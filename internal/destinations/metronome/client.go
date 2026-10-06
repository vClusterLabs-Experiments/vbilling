// Package metronome ships usage to Metronome for rating and invoicing, and
// keeps Metronome customers (optionally linked to Stripe for payment, tax
// and invoice delivery) and contracts in sync with vBilling tenants.
package metronome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
)

// Client is a minimal Metronome REST client.
type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(base, token string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: destinations.NewHTTPClient()}
}

// APIError is a Metronome error response ({"message": "..."}).
type APIError struct {
	Status  int
	Message string `json:"message"`
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("metronome %s: HTTP %d: %s", e.Path, e.Status, e.Message)
}

// IsConflict reports a 409 (ingest alias or uniqueness key already used).
func IsConflict(err error) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			return e.Status == http.StatusConflict
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "vbilling/0.2")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("metronome %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || len(bytes.TrimSpace(b)) == 0 {
			return nil
		}
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("metronome %s %s: decode: %w", method, path, err)
		}
		return nil
	}
	apiErr := &APIError{Status: resp.StatusCode, Path: method + " " + path}
	if json.Unmarshal(b, apiErr) != nil || apiErr.Message == "" {
		apiErr.Message = strings.TrimSpace(string(b))
	}
	apiErr.Status = resp.StatusCode
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return destinations.Permanent(apiErr)
	default: // 401/403 config, 404, 408, 409 (callers decide), 429, 5xx
		return apiErr
	}
}

// --- ingest ---

// IngestEvent is one usage event in Metronome's wire format. Property values
// are strings, as Metronome recommends for numeric precision.
type IngestEvent struct {
	TransactionID string            `json:"transaction_id"`
	CustomerID    string            `json:"customer_id"`
	EventType     string            `json:"event_type"`
	Timestamp     string            `json:"timestamp"`
	Properties    map[string]string `json:"properties,omitempty"`
}

// Ingest sends up to 100 events. No Idempotency-Key: Metronome caches error
// responses under a key, and transaction_id already deduplicates for 34 days.
func (c *Client) Ingest(ctx context.Context, events []IngestEvent) error {
	return c.do(ctx, http.MethodPost, "/v1/ingest", nil, events, nil)
}

// --- customers ---

type Customer struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	IngestAliases []string `json:"ingest_aliases"`
}

type BillingProviderConfig struct {
	BillingProvider string            `json:"billing_provider"`
	DeliveryMethod  string            `json:"delivery_method,omitempty"`
	Configuration   map[string]string `json:"configuration,omitempty"`
}

type CreateCustomerRequest struct {
	Name                   string                  `json:"name"`
	IngestAliases          []string                `json:"ingest_aliases"`
	BillingProviderConfigs []BillingProviderConfig `json:"customer_billing_provider_configurations,omitempty"`
}

func (c *Client) CreateCustomer(ctx context.Context, req CreateCustomerRequest) (*Customer, error) {
	var resp struct {
		Data Customer `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/customers", nil, req, &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

func (c *Client) FindCustomerByAlias(ctx context.Context, alias string) (*Customer, error) {
	q := url.Values{}
	q.Set("ingest_alias", alias)
	var resp struct {
		Data []Customer `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/customers", q, nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return &resp.Data[0], nil
}

func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	var resp struct {
		Data Customer `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/customers/"+url.PathEscape(id), nil, nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// --- billable metrics ---

type PropertyFilter struct {
	Name   string `json:"name"`
	Exists *bool  `json:"exists,omitempty"`
}

type EventTypeFilter struct {
	InValues []string `json:"in_values"`
}

type BillableMetric struct {
	ID              string           `json:"id,omitempty"`
	Name            string           `json:"name"`
	AggregationType string           `json:"aggregation_type"`
	AggregationKey  string           `json:"aggregation_key,omitempty"`
	EventTypeFilter *EventTypeFilter `json:"event_type_filter,omitempty"`
	PropertyFilters []PropertyFilter `json:"property_filters,omitempty"`
	GroupKeys       [][]string       `json:"group_keys,omitempty"`
	ArchivedAt      string           `json:"archived_at,omitempty"`
}

func (c *Client) ListBillableMetrics(ctx context.Context) ([]BillableMetric, error) {
	var out []BillableMetric
	q := url.Values{}
	q.Set("limit", "100")
	for page := 0; page < 50; page++ {
		var resp struct {
			Data     []BillableMetric `json:"data"`
			NextPage *string          `json:"next_page"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/billable-metrics", q, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.NextPage == nil || *resp.NextPage == "" {
			break
		}
		q.Set("next_page", *resp.NextPage)
	}
	return out, nil
}

func (c *Client) CreateBillableMetric(ctx context.Context, m BillableMetric) (string, error) {
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/billable-metrics/create", nil, m, &resp); err != nil {
		return "", err
	}
	return resp.Data.ID, nil
}

// --- contracts ---

type UsageStatementSchedule struct {
	Frequency string `json:"frequency"`
	Day       string `json:"day"`
}

type CreateContractRequest struct {
	CustomerID             string                  `json:"customer_id"`
	StartingAt             string                  `json:"starting_at"`
	RateCardID             string                  `json:"rate_card_id,omitempty"`
	RateCardAlias          string                  `json:"rate_card_alias,omitempty"`
	UniquenessKey          string                  `json:"uniqueness_key,omitempty"`
	UsageStatementSchedule *UsageStatementSchedule `json:"usage_statement_schedule,omitempty"`
	BillingProviderConfig  *BillingProviderConfig  `json:"billing_provider_configuration,omitempty"`
}

func (c *Client) CreateContract(ctx context.Context, req CreateContractRequest) (string, error) {
	var resp struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/contracts/create", nil, req, &resp); err != nil {
		return "", err
	}
	return resp.Data.ID, nil
}

// Contract is the part of a contract vBilling reads back.
type Contract struct {
	ID            string `json:"id"`
	UniquenessKey string `json:"uniqueness_key"`
	StartingAt    string `json:"starting_at"`
	EndingBefore  string `json:"ending_before"`
	ArchivedAt    string `json:"archived_at"`
}

type listContractsRequest struct {
	CustomerID      string `json:"customer_id"`
	IncludeArchived bool   `json:"include_archived"`
	Limit           int    `json:"limit"`
	Cursor          string `json:"cursor,omitempty"`
}

// ListContracts pages through a customer's contracts, archived ones included.
func (c *Client) ListContracts(ctx context.Context, customerID string) ([]Contract, error) {
	var out []Contract
	req := listContractsRequest{CustomerID: customerID, IncludeArchived: true, Limit: 20}
	for page := 0; page < 100; page++ {
		var resp struct {
			Data   []Contract `json:"data"`
			Cursor *string    `json:"cursor"`
		}
		if err := c.do(ctx, http.MethodPost, "/v2/contracts/list", nil, req, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.Cursor == nil || *resp.Cursor == "" {
			break
		}
		req.Cursor = *resp.Cursor
	}
	return out, nil
}

// EndContract sets a contract's (exclusive) end.
func (c *Client) EndContract(ctx context.Context, customerID, contractID string, endingBefore time.Time) error {
	req := struct {
		CustomerID   string `json:"customer_id"`
		ContractID   string `json:"contract_id"`
		EndingBefore string `json:"ending_before"`
	}{customerID, contractID, rfc3339(endingBefore)}
	return c.do(ctx, http.MethodPost, "/v1/contracts/updateEndDate", nil, req, nil)
}

// ArchiveContract removes a contract that should never have run. Finalized
// invoices are kept.
func (c *Client) ArchiveContract(ctx context.Context, customerID, contractID string) error {
	req := struct {
		CustomerID   string `json:"customer_id"`
		ContractID   string `json:"contract_id"`
		VoidInvoices bool   `json:"void_invoices"`
	}{customerID, contractID, false}
	return c.do(ctx, http.MethodPost, "/v1/contracts/archive", nil, req, nil)
}

// --- billing provider configurations ---

// CustomerBillingConfig is a billing provider configuration of a customer.
type CustomerBillingConfig struct {
	ID              string         `json:"id"`
	BillingProvider string         `json:"billing_provider"`
	Configuration   map[string]any `json:"configuration"`
	ArchivedAt      string         `json:"archived_at"`
}

// BillingProviderConfigs lists a customer's active billing provider configurations.
func (c *Client) BillingProviderConfigs(ctx context.Context, customerID string) ([]CustomerBillingConfig, error) {
	req := struct {
		CustomerID string `json:"customer_id"`
	}{customerID}
	var resp struct {
		Data []CustomerBillingConfig `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/getCustomerBillingProviderConfigurations", nil, req, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// SetBillingProviderConfig adds a billing provider configuration to a customer.
func (c *Client) SetBillingProviderConfig(ctx context.Context, customerID string, cfg BillingProviderConfig) error {
	type entry struct {
		CustomerID string `json:"customer_id"`
		BillingProviderConfig
	}
	req := struct {
		Data []entry `json:"data"`
	}{[]entry{{customerID, cfg}}}
	return c.do(ctx, http.MethodPost, "/v1/setCustomerBillingProviderConfigurations", nil, req, nil)
}

// --- usage (reconciliation) ---

type UsageRequest struct {
	StartingOn      string           `json:"starting_on"`
	EndingBefore    string           `json:"ending_before"`
	WindowSize      string           `json:"window_size"`
	CustomerIDs     []string         `json:"customer_ids,omitempty"`
	BillableMetrics []map[string]any `json:"billable_metrics,omitempty"`
}

type UsageRow struct {
	CustomerID       string      `json:"customer_id"`
	BillableMetricID string      `json:"billable_metric_id"`
	Value            json.Number `json:"value"`
}

// Usage pages through POST /v1/usage. Bounds must be UTC midnights.
func (c *Client) Usage(ctx context.Context, req UsageRequest) ([]UsageRow, error) {
	var out []UsageRow
	q := url.Values{}
	for page := 0; page < 200; page++ {
		var resp struct {
			Data     []UsageRow `json:"data"`
			NextPage *string    `json:"next_page"`
		}
		if err := c.do(ctx, http.MethodPost, "/v1/usage", q, req, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if resp.NextPage == nil || *resp.NextPage == "" {
			break
		}
		q.Set("next_page", *resp.NextPage)
	}
	return out, nil
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
