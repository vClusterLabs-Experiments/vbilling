package records

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/safehttp"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
)

// A gateway's push can lose records: a batch it drops while vBilling is
// down, a crash before it sends. Most gateways also keep their own log of
// every request (a database, a logs API). A pull reads that log back over
// HTTP and ingests the rows through the same format and Ingester as the
// push, so pushed and pulled copies of a request get the same record ID and
// count once. Rows the push never delivered are billed, late ones as late
// adjustments to their windows.
//
// PullSpec declares one such log (RECORDS_PULL_FILE, Helm records.pull).
// LiteLLM has a dedicated puller (LiteLLMPuller), because its spend logs
// need translating back into the payload its logger pushes.

// PullSpec is one gateway request log to read back.
type PullSpec struct {
	// Name identifies the pull in health, metrics and logs.
	Name string `json:"name"`
	// Format maps each row to a record: a format from RECORDS_MAPPINGS_FILE,
	// a built-in one, or vbilling. Rows must carry the same request IDs the
	// push sends.
	Format string `json:"format"`
	// Client is the push's records client (X-VBilling-Client, default the
	// format name). It is part of every record ID, so it must match for
	// pushed and pulled copies to count once.
	Client string `json:"client,omitempty"`
	// Tenant bills every row to one tenant, like a tenant-bound ingest
	// token. Set it when the push uses such a token: the pull stores rows
	// under the record IDs of the tenant's bound token (Ingester.CheckPin).
	Tenant string `json:"tenant,omitempty"`
	// URL of the log, with placeholders: {{from}} and {{to}} (the time
	// range, in TimeFormat), {{page}}, {{offset}}, {{limit}} and {{cursor}}.
	URL string `json:"url"`
	// Method is GET (default) or POST; Body is the POST body, with the same
	// placeholders.
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
	// Headers are sent with every request. ${RECORDS_PULL_...} in a value is
	// replaced by that environment variable, so tokens stay in Secrets.
	// Other variables are never expanded.
	Headers map[string]string `json:"headers,omitempty"`
	// Rows is the path to the array of rows in the response; empty when the
	// response is the array itself.
	Rows Path `json:"rows,omitempty"`
	// TimeFormat of {{from}} and {{to}}: rfc3339 (default), unix, unixms,
	// or a Go layout such as "2006-01-02 15:04:05" (UTC).
	TimeFormat string `json:"timeFormat,omitempty"`
	// Paging of each range.
	Paging PullPaging `json:"paging,omitempty"`
	// Interval between pulls (default 5m); Lookback is how far before the
	// end of the last pull each pull starts (default 1h); Chunk is the
	// range read per request sequence (default 1h).
	Interval Duration `json:"interval,omitempty"`
	Lookback Duration `json:"lookback,omitempty"`
	Chunk    Duration `json:"chunk,omitempty"`
}

// PullPaging describes how the log pages.
type PullPaging struct {
	// Mode: none (one request per range, the default), page ({{page}} from
	// Start, default 1), offset ({{offset}} from 0) or cursor ({{cursor}}
	// from Next in the previous response, empty on the first request).
	Mode  string `json:"mode,omitempty"`
	Limit int    `json:"limit,omitempty"` // rows per page ({{limit}}, default 500); a shorter page is the last
	Start int    `json:"start,omitempty"`
	Next  Path   `json:"next,omitempty"`
}

// Duration is a time.Duration written as "5m" in YAML.
type Duration time.Duration

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as 5m")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

var pullPlaceholder = regexp.MustCompile(`\{\{\s*(from|to|page|offset|limit|cursor)\s*\}\}`)

// LoadPullSpecs reads RECORDS_PULL_FILE, a YAML list of PullSpec, and
// checks each against the configured formats.
func LoadPullSpecs(path string, formats Formats) ([]PullSpec, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var specs []PullSpec
	if err := yaml.UnmarshalStrict(b, &specs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range specs {
		s := &specs[i]
		if err := s.normalize(formats); err != nil {
			return nil, fmt.Errorf("%s: pull %d (%s): %w", path, i, s.Name, err)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("%s: pull name %q is used twice", path, s.Name)
		}
		seen[s.Name] = true
	}
	return specs, nil
}

func (s *PullSpec) normalize(formats Formats) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if _, ok := formats[s.Format]; !ok && s.Format != Native {
		return fmt.Errorf("format %q is not configured (declare it in records.formats / RECORDS_MAPPINGS_FILE)", s.Format)
	}
	if s.Client == "" {
		s.Client = s.Format
	}
	u, err := url.Parse(pullPlaceholder.ReplaceAllString(s.URL, "x"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an http(s) URL", s.URL)
	}
	usesFrom := false
	for _, m := range pullPlaceholder.FindAllStringSubmatch(s.URL+" "+s.Body, -1) {
		usesFrom = usesFrom || m[1] == "from"
	}
	if !usesFrom {
		return fmt.Errorf("url or body must use {{from}}: a pull reads a time range")
	}
	s.Method = strings.ToUpper(s.Method)
	switch s.Method {
	case "":
		s.Method = http.MethodGet
	case http.MethodGet, http.MethodPost:
	default:
		return fmt.Errorf("method %q must be GET or POST", s.Method)
	}
	switch s.Paging.Mode {
	case "":
		s.Paging.Mode = "none"
	case "none", "page", "offset":
	case "cursor":
		if len(s.Paging.Next) == 0 {
			return fmt.Errorf("paging mode cursor needs paging.next, the path of the next cursor in the response")
		}
	default:
		return fmt.Errorf("paging mode %q must be none, page, offset or cursor", s.Paging.Mode)
	}
	if s.Paging.Limit <= 0 {
		s.Paging.Limit = 500
	}
	if s.Paging.Mode == "page" && s.Paging.Start == 0 {
		s.Paging.Start = 1
	}
	if s.Interval == 0 {
		s.Interval = Duration(5 * time.Minute)
	}
	if s.Lookback == 0 {
		s.Lookback = Duration(time.Hour)
	}
	if s.Chunk == 0 {
		s.Chunk = Duration(time.Hour)
	}
	if time.Duration(s.Interval) < 10*time.Second || time.Duration(s.Lookback) < time.Minute || time.Duration(s.Chunk) < time.Minute {
		return fmt.Errorf("interval must be at least 10s, lookback and chunk at least 1m")
	}
	for k, v := range s.Headers {
		for _, m := range envRef.FindAllStringSubmatch(v, -1) {
			if !strings.HasPrefix(m[1], "RECORDS_PULL_") {
				return fmt.Errorf("header %s: only ${RECORDS_PULL_...} variables are expanded, not ${%s}", k, m[1])
			}
		}
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Puller reads one gateway log on a schedule.
type Puller struct {
	spec   PullSpec
	in     *Ingester
	state  *kv.Store
	health *health.Registry
	tel    *telemetry.Registry
	http   *http.Client
	now    func() time.Time
	ready  func() bool
	name   string
}

// NewPuller prepares a pull. state keeps the end of the last successful
// pull across restarts (nil: memory only); ready, when set, holds the first
// pull until tenants and aliases are loaded.
func NewPuller(in *Ingester, spec PullSpec, state *kv.Store, ready func() bool, h *health.Registry, tel *telemetry.Registry) *Puller {
	if state == nil {
		state = kv.Memory()
	}
	if tel == nil {
		tel = telemetry.New()
	}
	if h == nil {
		h = health.New(tel)
	}
	// A log may redirect (a CDN, a signed download URL). Redirects are
	// followed, but the pull's headers, which carry its tokens, are only sent
	// to the origin of the configured URL.
	headers := func() []string {
		names := make([]string, 0, len(spec.Headers))
		for k := range spec.Headers {
			names = append(names, k)
		}
		return names
	}
	hc := &http.Client{Timeout: time.Minute, CheckRedirect: safehttp.KeepHeadersOnOrigin(headers)}
	return &Puller{spec: spec, in: in, state: state, health: h, tel: tel, http: hc,
		now: time.Now, ready: ready, name: health.Name("records", spec.Client, "pull", spec.Name)}
}

// Run pulls every Interval until ctx ends.
func (p *Puller) Run(ctx context.Context) {
	for p.ready != nil && !p.ready() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	t := time.NewTicker(time.Duration(p.spec.Interval))
	defer t.Stop()
	for {
		if res, err := p.Pull(ctx); err == nil && (res.Accepted > 0 || res.RejectedCount > 0) {
			log.Printf("[records] pull %s read %d rows (%s to %s): %d new, %d already stored, %d skipped, %d rejected",
				p.spec.Name, res.Rows, res.From.Format(time.RFC3339), res.To.Format(time.RFC3339), res.Accepted, res.AlreadyStored, res.Skipped, res.RejectedCount)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Puller) stateKey() string { return "pull/" + p.spec.Name + "/through" }

// Pull reads the log from the end of the last successful pull minus
// Lookback (or now minus Lookback) until now, and ingests the rows.
func (p *Puller) Pull(ctx context.Context) (PullResult, error) {
	res, err := p.pull(ctx)
	labels := map[string]string{"client": p.spec.Client, "pull": p.spec.Name}
	p.tel.Add("vbilling_records_pull_runs_total", "Pulls of a gateway's own request logs.", labels, 1)
	p.tel.Add("vbilling_records_pulled_rows_total", "Rows read from a gateway's own request logs.", labels, float64(res.Rows))
	p.tel.Add("vbilling_records_pulled_new_total", "Pulled requests that were not in the records ledger yet (the push missed them).", labels, float64(res.Accepted))
	if err != nil {
		p.tel.Add("vbilling_records_pull_failures_total", "Failed pulls of a gateway's own request logs.", labels, 1)
		p.health.Fail(p.name, "records", p.spec.Client, err, "check the pull's url, headers and rows path in records.pull")
		return res, err
	}
	p.tel.Set("vbilling_records_pull_last_success_timestamp_seconds", "When the last pull of a gateway's own request logs succeeded.", labels, float64(p.now().Unix()))
	if res.RejectedCount > 0 && res.Accepted == 0 && res.AlreadyStored == 0 {
		p.health.Fail(p.name, "records", p.spec.Client, fmt.Errorf("every row was rejected: %s", res.Rejected[0].Error),
			"check that the pull's format maps this log's rows (records.formats)")
		return res, nil
	}
	p.health.OK(p.name, "records", p.spec.Client, res.Rows)
	return res, nil
}

func (p *Puller) pull(ctx context.Context) (PullResult, error) {
	now := p.now().UTC()
	lookback := time.Duration(p.spec.Lookback)
	res := PullResult{From: now.Add(-lookback), To: now}
	if v, ok := p.state.Get(p.stateKey()); ok {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.Add(-lookback).Before(res.From) {
			res.From = t.Add(-lookback)
		}
	}
	if oldest := now.Add(-p.in.maxAge()).Add(time.Minute); res.From.Before(oldest) {
		res.From = oldest
	}
	res.Result.Format = p.spec.Format
	chunk := time.Duration(p.spec.Chunk)
	for start := res.From; start.Before(now); {
		end := start.Add(chunk)
		if end.After(now) {
			end = now
		}
		if err := p.pullRange(ctx, start, end, &res); err != nil {
			return res, err
		}
		start = end
	}
	if err := p.state.Set(p.stateKey(), now.Format(time.RFC3339Nano)); err != nil {
		return res, fmt.Errorf("save pull state: %w", err)
	}
	return res, nil
}

// maxPagesPerRange stops a log that never returns a short page.
const maxPagesPerRange = 10000

func (p *Puller) pullRange(ctx context.Context, from, to time.Time, res *PullResult) error {
	stored, err := p.in.indexStored(from.Truncate(p.in.opts.Window), to.Add(time.Hour))
	if err != nil {
		return fmt.Errorf("read records ledger: %w", err)
	}
	page, offset, cursor := p.spec.Paging.Start, 0, ""
	for n := 0; n < maxPagesPerRange; n++ {
		rows, next, err := p.fetch(ctx, from, to, page, offset, cursor)
		if err != nil {
			return err
		}
		res.Pages++
		res.Rows += len(rows)
		if len(rows) > 0 {
			r, err := p.in.ingestPulled(p.spec.Format, p.spec.Client, p.spec.Tenant, rows, stored)
			if err != nil {
				return err
			}
			res.add(r)
		}
		switch p.spec.Paging.Mode {
		case "none":
			return nil
		case "cursor":
			if next == "" || next == cursor || len(rows) == 0 {
				return nil
			}
			cursor = next
		default: // page, offset
			if len(rows) < p.spec.Paging.Limit {
				return nil
			}
			page++
			offset += len(rows)
		}
	}
	return fmt.Errorf("more than %d pages for %s to %s: check paging", maxPagesPerRange, from.Format(time.RFC3339), to.Format(time.RFC3339))
}

func (p *Puller) formatTime(t time.Time) string {
	t = t.UTC()
	switch p.spec.TimeFormat {
	case "", "rfc3339":
		return t.Format(time.RFC3339)
	case "unix":
		return strconv.FormatInt(t.Unix(), 10)
	case "unixms":
		return strconv.FormatInt(t.UnixMilli(), 10)
	default:
		return t.Format(p.spec.TimeFormat)
	}
}

func (p *Puller) expand(tmpl string, from, to time.Time, page, offset int, cursor string, escape func(string) string) string {
	return pullPlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		var v string
		switch pullPlaceholder.FindStringSubmatch(m)[1] {
		case "from":
			v = p.formatTime(from)
		case "to":
			v = p.formatTime(to)
		case "page":
			v = strconv.Itoa(page)
		case "offset":
			v = strconv.Itoa(offset)
		case "limit":
			v = strconv.Itoa(p.spec.Paging.Limit)
		case "cursor":
			v = cursor
		}
		return escape(v)
	})
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// fetch reads one page of rows.
func (p *Puller) fetch(ctx context.Context, from, to time.Time, page, offset int, cursor string) ([]map[string]any, string, error) {
	u := p.expand(p.spec.URL, from, to, page, offset, cursor, url.QueryEscape)
	var body io.Reader
	if p.spec.Method == http.MethodPost && p.spec.Body != "" {
		body = strings.NewReader(p.expand(p.spec.Body, from, to, page, offset, cursor, jsonEscape))
	}
	req, err := http.NewRequestWithContext(ctx, p.spec.Method, u, body)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range p.spec.Headers {
		req.Header.Set(k, envRef.ReplaceAllStringFunc(v, func(m string) string {
			name := envRef.FindStringSubmatch(m)[1]
			if !strings.HasPrefix(name, "RECORDS_PULL_") {
				return m
			}
			return strings.TrimSpace(os.Getenv(name))
		}))
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, "", fmt.Errorf("%s %s: %s: %s", p.spec.Method, redactQuery(u), resp.Status, msg)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, "", fmt.Errorf("%s: the response is not JSON: %w", p.spec.Name, err)
	}
	var list any = doc
	next := ""
	if obj, ok := doc.(map[string]any); ok {
		if len(p.spec.Rows) > 0 {
			v, found := lookup(obj, p.spec.Rows)
			if !found {
				return nil, "", fmt.Errorf("%s: the response has no %s", p.spec.Name, strings.Join(p.spec.Rows, " or "))
			}
			list = v
		}
		if len(p.spec.Paging.Next) > 0 {
			next, _ = lookupString(obj, p.spec.Paging.Next)
		}
	}
	items, ok := list.([]any)
	if !ok {
		if list == nil {
			return nil, next, nil // an empty page written as null
		}
		return nil, "", fmt.Errorf("%s: rows must be a JSON array (set rows to its path)", p.spec.Name)
	}
	out := make([]map[string]any, 0, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, "", fmt.Errorf("%s: row %d is not a JSON object", p.spec.Name, i)
		}
		out = append(out, m)
	}
	return out, next, nil
}

// redactQuery keeps query values out of error messages; they can carry keys.
func redactQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the log URL"
	}
	u.RawQuery = ""
	return u.String()
}
