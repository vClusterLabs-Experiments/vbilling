package records

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
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// LiteLLMPullOptions configure a LiteLLMPuller.
type LiteLLMPullOptions struct {
	// URL is the LiteLLM proxy's base URL, e.g. http://litellm.litellm.svc:4000.
	URL string
	// Key is a LiteLLM key that may read every team's spend logs: the
	// master key or a proxy admin key.
	Key string
	// Client is the records client the pulled records are stored under. It
	// is part of every record ID, so it must equal the client the push
	// uses (X-VBilling-Client, which defaults to the format name, litellm).
	Client string
	// Tenant bills every pulled request to one tenant, like a tenant-bound
	// ingest token. It must equal the push token's tenant when the push
	// uses one: the pull stores rows under the record IDs of the tenant's
	// bound token (Ingester.CheckPin).
	Tenant string
	// Interval between pulls (default 5m).
	Interval time.Duration
	// Lookback is how far before the last pull each pull starts again
	// (default 1h). Spend log rows LiteLLM writes late, or that one pull
	// missed, are read by the next.
	Lookback time.Duration
	// PageSize is the number of rows per request (default 1000, LiteLLM's
	// maximum).
	PageSize int
	// State keeps the end of the last successful pull across restarts, so
	// the first pull after an outage covers the outage. Nil keeps it in
	// memory only.
	State *kv.Store
	// Ready reports whether tenants and aliases are loaded. Pulls wait for
	// it, so pulled records are billed to the right tenant.
	Ready func() bool
	HTTP  *http.Client
	Now   func() time.Time
}

// LiteLLMPuller reads LiteLLM's spend logs (GET /spend/logs/v2), LiteLLM's
// own durable record of every request, and ingests the rows through the
// Ingester as records in the litellm format. Pushed and pulled copies of a
// request get the same record ID, so the pull fills what the push lost
// (vBilling down, a batch LiteLLM dropped) without counting anything twice.
// A pulled record that arrives after its window settled is billed as a late
// adjustment to that window, like any late record.
type LiteLLMPuller struct {
	opts   LiteLLMPullOptions
	in     *Ingester
	health *health.Registry
	tel    *telemetry.Registry
	name   string
}

// liteLLMPullState is the State key of the end of the last successful pull.
const liteLLMPullState = "litellm-pull/through"

// NewLiteLLMPuller prepares a puller that ingests through in. The litellm
// format must be configured on in.
func NewLiteLLMPuller(in *Ingester, opts LiteLLMPullOptions, h *health.Registry, tel *telemetry.Registry) *LiteLLMPuller {
	if opts.Client == "" {
		opts.Client = "litellm"
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.Lookback <= 0 {
		opts.Lookback = time.Hour
	}
	if opts.PageSize <= 0 || opts.PageSize > 1000 {
		opts.PageSize = 1000
	}
	if opts.State == nil {
		opts.State = kv.Memory()
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: time.Minute}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.URL = strings.TrimRight(opts.URL, "/")
	if h == nil {
		h = health.New(tel)
	}
	if tel == nil {
		tel = telemetry.New()
	}
	// A push of the same LiteLLM under another client or tenant would be
	// stored twice: the ingester flags such pushes.
	in.expectPull("litellm", opts.Client, opts.Tenant)
	return &LiteLLMPuller{opts: opts, in: in, health: h, tel: tel, name: health.Name("records", opts.Client, "pull")}
}

// PullResult reports one pull.
type PullResult struct {
	From, To time.Time
	Pages    int
	// Rows is every spend log row read; NoCallID the rows skipped because
	// they have no litellm_call_id.
	Rows, NoCallID int
	Result
}

// Run pulls every Interval until ctx ends, once tenants are loaded.
func (p *LiteLLMPuller) Run(ctx context.Context) {
	for p.opts.Ready != nil && !p.opts.Ready() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	t := time.NewTicker(p.opts.Interval)
	defer t.Stop()
	for {
		if res, err := p.Pull(ctx); err == nil && (res.Accepted > 0 || res.NoCallID > 0 || res.RejectedCount > 0) {
			log.Printf("[records] pulled %d LiteLLM spend log rows (%s to %s): %d new, %d already stored, %d skipped, %d rejected",
				res.Rows, res.From.Format(time.RFC3339), res.To.Format(time.RFC3339), res.Accepted, res.AlreadyStored, res.Skipped+res.NoCallID, res.RejectedCount)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pull reads the spend logs from the end of the last successful pull minus
// Lookback (or now minus Lookback) until now, and ingests the rows.
func (p *LiteLLMPuller) Pull(ctx context.Context) (PullResult, error) {
	res, err := p.pull(ctx)
	labels := map[string]string{"client": p.opts.Client}
	p.tel.Add("vbilling_records_pull_runs_total", "Pulls of a gateway's own request logs.", labels, 1)
	p.tel.Add("vbilling_records_pulled_rows_total", "Rows read from a gateway's own request logs.", labels, float64(res.Rows))
	p.tel.Add("vbilling_records_pulled_new_total", "Pulled requests that were not in the records ledger yet (the push missed them).", labels, float64(res.Accepted))
	failed := 0.0
	if err != nil {
		failed = 1
	}
	p.tel.Add("vbilling_records_pull_failures_total", "Failed pulls of a gateway's own request logs.", labels, failed)
	if err != nil {
		p.health.Fail(p.name, "records", p.opts.Client, err, "check LITELLM_URL and that LITELLM_API_KEY is a LiteLLM master or proxy admin key")
		return res, err
	}
	p.tel.Set("vbilling_records_pull_last_success_timestamp_seconds", "When the last pull of a gateway's own request logs succeeded.", labels, float64(p.opts.Now().Unix()))
	if res.NoCallID > 0 {
		p.health.Fail(p.name, "records", p.opts.Client,
			fmt.Errorf("%d spend log row(s) have no litellm_call_id and were not ingested", res.NoCallID),
			"upgrade LiteLLM to v1.103.0 or later: without the call ID, a pulled request cannot be matched to the same request pushed by the generic API logger")
		return res, nil
	}
	p.health.OK(p.name, "records", p.opts.Client, res.Rows)
	return res, nil
}

func (p *LiteLLMPuller) pull(ctx context.Context) (PullResult, error) {
	now := p.opts.Now().UTC()
	res := PullResult{From: now.Add(-p.opts.Lookback), To: now}
	if v, ok := p.opts.State.Get(liteLLMPullState); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.Add(-p.opts.Lookback).Before(res.From) {
			res.From = t.Add(-p.opts.Lookback) // cover the time since the last pull
		}
	}
	// Records older than the ingester's limit are refused anyway.
	if oldest := now.Add(-p.in.opts.MaxAge).Add(time.Minute); res.From.Before(oldest) {
		res.From = oldest
	}
	res.Result.Format = "litellm"
	// An hour of start times at a time: the records ledger is indexed once
	// per chunk, and a pull after a long outage reads it chunk by chunk.
	for start := res.From; start.Before(now); {
		end := start.Add(pullChunk)
		if !end.Before(now) {
			end = now.Add(time.Second) // LiteLLM compares to the second
		}
		if err := p.pullRange(ctx, start, end, &res); err != nil {
			return res, err
		}
		start = end
	}
	if err := p.opts.State.Set(liteLLMPullState, now.Format(time.RFC3339Nano)); err != nil {
		return res, fmt.Errorf("save pull state: %w", err)
	}
	return res, nil
}

// pullChunk is the span of start times read per ledger index.
const pullChunk = time.Hour

// pullRange reads the requests started in [from, to] page by page.
func (p *LiteLLMPuller) pullRange(ctx context.Context, from, to time.Time, res *PullResult) error {
	// Records land in the window of their end time, which can be later
	// than to; a longer request is looked up in the ledger on its own.
	stored, err := p.in.indexStored(from.Truncate(p.in.opts.Window), to.Add(time.Hour))
	if err != nil {
		return fmt.Errorf("read records ledger: %w", err)
	}
	for page := 1; ; page++ {
		rows, err := p.fetch(ctx, from, to, page)
		if err != nil {
			return err
		}
		res.Pages++
		res.Rows += len(rows)
		objs := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if s, _ := lookupString(row, Path{"litellm_call_id"}); s == "" {
				res.NoCallID++
				continue
			}
			objs = append(objs, SpendLogPayload(row))
		}
		if len(objs) > 0 {
			r, err := p.in.ingestPulled("litellm", p.opts.Client, p.opts.Tenant, objs, stored)
			if err != nil {
				return err
			}
			res.add(r)
		}
		if len(rows) < p.opts.PageSize {
			return nil
		}
	}
}

func (r *PullResult) add(o Result) {
	r.Received += o.Received
	r.Accepted += o.Accepted
	r.AlreadyStored += o.AlreadyStored
	r.UsageRows += o.UsageRows
	r.Skipped += o.Skipped
	r.DuplicatesInBatch += o.DuplicatesInBatch
	r.RejectedCount += o.RejectedCount
	for _, rej := range o.Rejected {
		if len(r.Rejected) < MaxRejections {
			r.Rejected = append(r.Rejected, rej)
		}
	}
	for _, t := range o.UnknownTenants {
		if !containsString(r.UnknownTenants, t) {
			r.UnknownTenants = append(r.UnknownTenants, t)
		}
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fetch reads one page of successful requests started in [from, to],
// oldest first. LiteLLM compares start_date and end_date to startTime, in
// UTC, to the second, so consecutive ranges share their boundary second.
func (p *LiteLLMPuller) fetch(ctx context.Context, from, to time.Time, page int) ([]map[string]any, error) {
	q := url.Values{}
	q.Set("start_date", from.UTC().Format("2006-01-02 15:04:05"))
	q.Set("end_date", to.UTC().Format("2006-01-02 15:04:05"))
	q.Set("page", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(p.opts.PageSize))
	q.Set("sort_by", "startTime")
	q.Set("sort_order", "asc")
	q.Set("status_filter", "success")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.opts.URL+"/spend/logs/v2?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.opts.Key)
	req.Header.Set("Accept", "application/json")
	resp, err := p.opts.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("GET /spend/logs/v2: %s: %s", resp.Status, msg)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep token counts and timestamps exact
	var out struct {
		Data *[]map[string]any `json:"data"`
	}
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("GET /spend/logs/v2: unexpected response: %w", err)
	}
	if out.Data == nil {
		return nil, errors.New("GET /spend/logs/v2: the response has no data; is this a LiteLLM proxy with a database?")
	}
	return *out.Data, nil
}

// SpendLogPayload turns a LiteLLM spend log row (LiteLLM_SpendLogs, as
// /spend/logs/v2 returns it) into the shape of the StandardLoggingPayload
// that LiteLLM's generic API logger posts for the same request, so one
// mapping, the litellm format, reads both. The row's columns are the
// payload fields they were written from: request_id is the payload id,
// team_id, organization_id and user the key's team, organization and user.
func SpendLogPayload(row map[string]any) map[string]any {
	md := map[string]any{}
	if m, ok := row["metadata"].(map[string]any); ok {
		for k, v := range m {
			md[k] = v
		}
	}
	for col, key := range map[string]string{"team_id": "user_api_key_team_id", "organization_id": "user_api_key_org_id", "user": "user_api_key_user_id"} {
		if v, ok := lookupString(row, Path{col}); ok {
			md[key] = v
		}
	}
	status, _ := lookupString(row, Path{"status"})
	if status == "" {
		status = "success" // LiteLLM wrote no status before it tracked failures
	}
	out := map[string]any{"metadata": md, "status": status}
	for col, key := range map[string]string{
		"request_id": "id", "litellm_call_id": "litellm_call_id", "startTime": "startTime", "endTime": "endTime",
		"model": "model", "model_group": "model_group", "prompt_tokens": "prompt_tokens",
		"completion_tokens": "completion_tokens", "total_tokens": "total_tokens", "end_user": "end_user",
		"call_type": "call_type", "custom_llm_provider": "custom_llm_provider", "api_base": "api_base",
	} {
		if v, ok := row[col]; ok && v != nil {
			out[key] = v
		}
	}
	return out
}
