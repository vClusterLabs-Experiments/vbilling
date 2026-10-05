// Package webhook streams usage to any HTTP endpoint as CloudEvents 1.0
// batches, signed with HMAC-SHA256. Use it to feed a data platform, a
// Kafka/HTTP bridge, or a homegrown rating engine alongside a billing backend.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// CloudEvent types emitted.
const (
	TypeUsage      = "com.vcluster.vbilling.usage.v1"
	TypeTenant     = "com.vcluster.vbilling.tenant.v1"
	TypeOffboarded = "com.vcluster.vbilling.tenant.offboarded.v1"
	TypeCatalog    = "com.vcluster.vbilling.catalog.v1"

	SignatureHeader = "X-VBilling-Signature"
	DataSchema      = "https://github.com/vClusterLabs-Experiments/vbilling/blob/main/docs/schema/usage-event.v1.json"
	maxBatch        = 500
)

func init() {
	destinations.Register("webhook", func(cfg *config.Config) (destinations.Destination, error) {
		if cfg.WebhookURL == "" {
			return nil, errors.New("WEBHOOK_URL is required when the webhook adapter is enabled")
		}
		return New(cfg), nil
	})
}

// CloudEvent is the structured-mode envelope.
type CloudEvent struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject,omitempty"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`
	DataSchema      string `json:"dataschema,omitempty"`
	Data            any    `json:"data"`
}

type Adapter struct {
	url     string
	secret  string
	headers map[string]string
	source  string
	http    *http.Client
	now     func() time.Time
}

func New(cfg *config.Config) *Adapter {
	return &Adapter{
		url:     cfg.WebhookURL,
		secret:  cfg.WebhookSecret,
		headers: cfg.WebhookHeaders,
		source:  "vbilling/" + cfg.Region + "/" + cfg.ClusterName,
		http:    destinations.NewHTTPClient(),
		now:     time.Now,
	}
}

func (a *Adapter) Name() string { return "webhook" }

// Bootstrap publishes the metric catalog so consumers can learn units and
// group keys without reading vBilling's docs.
func (a *Adapter) Bootstrap(ctx context.Context, metrics []usage.MetricDef) error {
	return a.post(ctx, []CloudEvent{a.envelope(TypeCatalog, "catalog-"+strconv.FormatInt(a.now().Unix(), 10), "", a.now(), metrics)})
}

func (a *Adapter) EnsureTenant(ctx context.Context, t usage.Tenant) error {
	return a.post(ctx, []CloudEvent{a.envelope(TypeTenant, "tenant-"+t.ID+"-"+strconv.FormatInt(a.now().Unix(), 10), t.ID, a.now(), t)})
}

func (a *Adapter) RemoveTenant(ctx context.Context, t usage.Tenant) error {
	return a.post(ctx, []CloudEvent{a.envelope(TypeOffboarded, "offboard-"+t.ID+"-"+strconv.FormatInt(a.now().Unix(), 10), t.ID, a.now(), t)})
}

func (a *Adapter) SendEvents(ctx context.Context, events []usage.Event) error {
	for i := 0; i < len(events); i += maxBatch {
		end := i + maxBatch
		if end > len(events) {
			end = len(events)
		}
		batch := make([]CloudEvent, 0, end-i)
		for j := i; j < end; j++ {
			e := events[j]
			ce := a.envelope(TypeUsage, e.ID, e.Tenant, e.WindowEnd, e)
			ce.DataSchema = DataSchema
			batch = append(batch, ce)
		}
		if err := a.post(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (a *Adapter) envelope(typ, id, subject string, at time.Time, data any) CloudEvent {
	return CloudEvent{
		SpecVersion: "1.0", ID: id, Source: a.source, Type: typ, Subject: subject,
		Time: at.UTC().Format(time.RFC3339Nano), DataContentType: "application/json", Data: data,
	}
}

func (a *Adapter) post(ctx context.Context, batch []CloudEvent) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/cloudevents-batch+json")
	req.Header.Set("User-Agent", "vbilling/0.2")
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	if a.secret != "" {
		req.Header.Set(SignatureHeader, Sign(body, a.secret, a.now()))
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return destinations.StatusError(http.MethodPost, a.url, resp.StatusCode, b)
}

// Sign returns "t=<unix>,v1=<hex hmac-sha256(secret, t + "." + body)>", the
// same scheme as Stripe-Signature, so existing verification code works.
func Sign(body []byte, secret string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

var _ destinations.Destination = (*Adapter)(nil)
