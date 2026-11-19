// Package records accepts per-request usage from AI gateways, inference
// servers and any other emitter that can POST one JSON record per request,
// and turns it into billable usage without a shim in between.
//
// A record is one request: who made it, when, and how much of each metric it
// used (input tokens, output tokens, images, seconds of audio, ...). The
// ingest API writes records to their own durable ledger and acknowledges
// only after fsync, so an emitter that retries on any non-2xx response never
// loses one. The Aggregator then sums records into one usage event per
// tenant, metric and window, deduplicated by request ID, and commits those
// events to the usage ledger, where every billing adapter picks them up.
//
// Emitters differ in what they send, so each payload format is a Mapping:
// field paths for the request ID, time, tenant, quantities and dimensions.
// The native format and LiteLLM's StandardLoggingPayload are built in; any
// other gateway (Kong, Envoy AI Gateway access logs, a custom proxy) is a
// few lines of YAML in RECORDS_MAPPINGS_FILE.
package records

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Native is the format name of vBilling's own record shape:
//
//	{"id": "req-123", "tenant": "acme", "time": "2026-11-18T10:00:01.123Z",
//	 "usage": {"inference_input_tokens": 512, "inference_output_tokens": 128},
//	 "sku": "llama-3.1-70b", "dimensions": {"endpoint": "chat"}}
const Native = "vbilling"

// Path is one or more dotted field paths; the first that resolves to a
// non-empty value wins. In YAML it is a string or a list of strings.
type Path []string

// UnmarshalJSON accepts "a.b" or ["a.b", "c"].
func (p *Path) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*p = Path{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("a field path is a string or a list of strings")
	}
	*p = many
	return nil
}

// Mapping describes one payload format.
type Mapping struct {
	ID        Path              `json:"id"`
	Time      Path              `json:"time"`
	Tenant    Path              `json:"tenant"`
	TenantMap map[string]string `json:"tenantMap,omitempty"` // emitter value -> tenant ID
	Usage     map[string]Path   `json:"usage"`               // metric code -> quantity
	// Subtract takes other metrics out of a metric, for emitters whose
	// totals include a part priced on its own: LiteLLM's prompt tokens
	// include the cached ones, so input = prompt - cached. Never below 0.
	Subtract      map[string][]string `json:"subtract,omitempty"`
	Dimensions    map[string]Path     `json:"dimensions,omitempty"`
	SKU           Path                `json:"sku,omitempty"`
	Region        Path                `json:"region,omitempty"`
	Project       Path                `json:"project,omitempty"`
	TenantCluster Path                `json:"tenantCluster,omitempty"`
	// Where skips records unless every path has the given value, e.g.
	// {status: success} to bill only successful requests.
	Where map[string]string `json:"where,omitempty"`
	// Metrics are declared when the format is loaded, unless a metric with
	// the same code already exists (CUSTOM_METRICS wins).
	Metrics []usage.CustomMetricSpec `json:"metrics,omitempty"`
	// RequestID, when set, derives the request ID instead of ID, for
	// emitters where no single field identifies a request.
	RequestID func(obj map[string]any) string `json:"-"`
	// TimePrecision rounds each record's time, to the precision at which an
	// emitter stores its own copy of the request. A record it pushes and
	// the same request read back from its store then fall in the same
	// window, where their IDs deduplicate.
	TimePrecision time.Duration `json:"-"`
	// TimeRound is TimePrecision in YAML (timePrecision: 1ms): set it when a
	// gateway's own log, read back by a pull, stores times less precisely
	// than its push sends them.
	TimeRound Duration `json:"timePrecision,omitempty"`
}

// LiteLLM maps LiteLLM's StandardLoggingPayload, as its generic API logger
// (callback_type: generic_api) posts it. Records are billed to the team of
// the virtual key, and only to it: LiteLLM sets the team from the key, while
// fields such as end_user come from the client's request, and a client
// must never choose who pays. A request whose key has no team is refused
// (give every key a team). Failed calls are skipped; the model group
// becomes the SKU. Input tokens served from a
// prompt cache are their own metric (inference_cached_input_tokens) and are
// taken out of inference_input_tokens, as token factories price them
// separately; a requested service tier (priority, flex) is a dimension. The request ID is
// LiteLLMRequestID, and times are rounded to the millisecond, the precision
// of LiteLLM's spend logs, so the spend log puller (LiteLLMPuller) produces
// the same record for the same request.
var LiteLLM = Mapping{
	ID:            Path{"id"},
	RequestID:     LiteLLMRequestID,
	Time:          Path{"endTime", "startTime"},
	TimePrecision: time.Millisecond,
	Tenant:        Path{"metadata.user_api_key_team_id"},
	Usage: map[string]Path{
		"inference_input_tokens":        {"prompt_tokens"},
		"inference_cached_input_tokens": {"metadata.usage_object.prompt_tokens_details.cached_tokens"},
		"inference_output_tokens":       {"completion_tokens"},
	},
	Subtract: map[string][]string{"inference_input_tokens": {"inference_cached_input_tokens"}},
	// The pushed payload carries cost_breakdown at the top; a spend log row
	// keeps it in its metadata.
	Dimensions: map[string]Path{"service_tier": {"cost_breakdown.service_tier", "metadata.cost_breakdown.service_tier"}},
	SKU:        Path{"model_group", "model"},
	Where:      map[string]string{"status": "success"},
	Metrics: []usage.CustomMetricSpec{
		{Code: "inference_input_tokens", Name: "Inference input tokens", Unit: "tokens", GroupKeys: []string{"sku"}},
		{Code: "inference_cached_input_tokens", Name: "Inference cached input tokens", Unit: "tokens", GroupKeys: []string{"sku"}},
		{Code: "inference_output_tokens", Name: "Inference output tokens", Unit: "tokens", GroupKeys: []string{"sku"}},
	},
}

// liteLLMIDSuffix matches what LiteLLM appends to a response ID when it
// logs a request. Each copy of a request (the logging payload, the spend log
// row) appends its own cache-hit timestamp, so the suffix is cut.
var liteLLMIDSuffix = regexp.MustCompile(`(_cache_hit[0-9.]+|_batch_cost|_background_cost)$`)

// LiteLLMRequestID identifies one LiteLLM request, in a logging payload or
// a spend log row converted to one: LiteLLM's call ID for the request,
// joined with the response ID. Neither is enough alone. A client can set
// the call ID (the x-litellm-call-id header), so keying on it alone would
// let a tenant collapse its requests into one. Response IDs repeat: every
// cache hit of one entry returns the cached response's ID, and some
// providers and mocks return the same ID for every call.
func LiteLLMRequestID(obj map[string]any) string {
	call, _ := lookupString(obj, Path{"litellm_call_id"})
	resp, _ := lookupString(obj, Path{"id"})
	resp = liteLLMIDSuffix.ReplaceAllString(resp, "")
	switch {
	case call == "":
		return resp
	case resp == "":
		return call
	}
	return call + "/" + resp
}

// Formats holds the payload formats by name.
type Formats map[string]Mapping

// LoadFormats returns the formats in path (a YAML map of name -> Mapping),
// plus the built-in litellm format when enabled (RECORDS_LITELLM). It is
// opt-in because it declares three token metrics, which every billing adapter
// then creates. A file entry named like a built-in replaces it.
func LoadFormats(path string, litellm bool) (Formats, error) {
	f := Formats{}
	if litellm {
		f["litellm"] = LiteLLM
	}
	if path == "" {
		return f, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file map[string]Mapping
	if err := yaml.UnmarshalStrict(b, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, m := range file {
		if name == Native {
			return nil, fmt.Errorf("%s: %q is the native format and cannot be redefined", path, Native)
		}
		if len(m.ID) == 0 || len(m.Time) == 0 || len(m.Usage) == 0 {
			return nil, fmt.Errorf("%s: format %q needs id, time and usage", path, name)
		}
		if m.TimeRound < 0 {
			return nil, fmt.Errorf("%s: format %q: timePrecision must be positive", path, name)
		}
		for metric, parts := range m.Subtract {
			for _, mm := range append([]string{metric}, parts...) {
				if _, ok := m.Usage[mm]; !ok {
					return nil, fmt.Errorf("%s: format %q: subtract names %q, which is not in its usage", path, name, mm)
				}
			}
		}
		m.TimePrecision = time.Duration(m.TimeRound)
		f[name] = m
	}
	return f, nil
}

// Declare registers the metrics every format names. Metrics a format uses
// without declaring them must exist already.
func (f Formats) Declare() error {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, spec := range f[n].Metrics {
			defs, err := usage.ParseCustomMetricsYAML(mustYAML([]usage.CustomMetricSpec{spec}))
			if err != nil {
				return fmt.Errorf("format %s: %w", n, err)
			}
			d := defs[0]
			d.Source = usage.SourceRecords
			if _, err := usage.EnsureCustom(d); err != nil {
				return fmt.Errorf("format %s: %w", n, err)
			}
		}
	}
	return nil
}

func mustYAML(v any) []byte {
	b, _ := yaml.Marshal(v)
	return b
}

// Record is a parsed request.
type Record struct {
	ID     string
	Tenant string
	// TenantKey is the emitter's own tenant value, before tenantMap and
	// aliases resolve it. Record IDs use it, so a request keeps its ID when
	// an alias is added, changed or not loaded yet.
	TenantKey     string
	Time          time.Time
	Usage         map[string]float64
	SKU           string
	Region        string
	Project       string
	TenantCluster string
	Dimensions    map[string]string
}

// Parse decodes a payload (one JSON object, a JSON array, an object with a
// "records" array, or NDJSON) into raw objects.
func Parse(body []byte) ([]map[string]any, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, errors.New("empty body")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber() // keep large IDs and nanosecond timestamps exact
	if strings.HasPrefix(trimmed, "[") {
		var arr []map[string]any
		if err := dec.Decode(&arr); err != nil {
			return nil, fmt.Errorf("invalid JSON array: %w", err)
		}
		return arr, nil
	}
	var out []map[string]any
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("invalid JSON (send one object, an array, {\"records\": [...]} or NDJSON): %w", err)
		}
		if recs, ok := obj["records"].([]any); ok && len(obj) == 1 {
			for _, r := range recs {
				if m, ok := r.(map[string]any); ok {
					out = append(out, m)
				}
			}
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// Map turns one raw object into a Record using a format. skip is true when
// a Where condition excludes it.
func Map(format string, m Mapping, obj map[string]any) (rec Record, skip bool, err error) {
	if format == Native {
		return mapNative(obj)
	}
	for path, want := range m.Where {
		if got, _ := lookupString(obj, Path{path}); got != want {
			return Record{}, true, nil
		}
	}
	if m.RequestID != nil {
		rec.ID = m.RequestID(obj)
	} else {
		rec.ID, _ = lookupString(obj, m.ID)
	}
	rawTenant, _ := lookupString(obj, m.Tenant)
	rec.Tenant, rec.TenantKey = rawTenant, rawTenant
	if t, ok := m.TenantMap[rawTenant]; ok {
		rec.Tenant = t
	}
	if tv, ok := lookup(obj, m.Time); ok {
		if rec.Time, err = parseTime(tv); err != nil {
			return Record{}, false, err
		}
		if m.TimePrecision > 0 {
			// Undo float error first (Unix seconds as a float carry a few
			// hundred nanoseconds of it), then round half up like a database
			// storing the time at that precision.
			rec.Time = rec.Time.Round(time.Microsecond).Round(m.TimePrecision)
		}
	}
	rec.Usage = map[string]float64{}
	for metric, p := range m.Usage {
		v, ok := lookup(obj, p)
		if !ok {
			continue
		}
		q, err := toFloat(v)
		if err != nil {
			return Record{}, false, fmt.Errorf("%s: %w", metric, err)
		}
		rec.Usage[metric] = q
	}
	for metric, parts := range m.Subtract {
		q, ok := rec.Usage[metric]
		if !ok {
			continue
		}
		for _, part := range parts {
			q -= rec.Usage[part]
		}
		rec.Usage[metric] = max(q, 0)
	}
	rec.SKU, _ = lookupString(obj, m.SKU)
	rec.Region, _ = lookupString(obj, m.Region)
	rec.Project, _ = lookupString(obj, m.Project)
	rec.TenantCluster, _ = lookupString(obj, m.TenantCluster)
	for k, p := range m.Dimensions {
		if v, ok := lookupString(obj, p); ok && v != "" {
			if rec.Dimensions == nil {
				rec.Dimensions = map[string]string{}
			}
			rec.Dimensions[k] = v
		}
	}
	return rec, false, nil
}

func mapNative(obj map[string]any) (Record, bool, error) {
	var rec Record
	rec.ID, _ = lookupString(obj, Path{"id"})
	rec.Tenant, _ = lookupString(obj, Path{"tenant"})
	rec.TenantKey = rec.Tenant
	if tv, ok := lookup(obj, Path{"time", "timestamp"}); ok {
		t, err := parseTime(tv)
		if err != nil {
			return Record{}, false, err
		}
		rec.Time = t
	}
	rec.Usage = map[string]float64{}
	if u, ok := obj["usage"].(map[string]any); ok {
		for k, v := range u {
			q, err := toFloat(v)
			if err != nil {
				return Record{}, false, fmt.Errorf("usage.%s: %w", k, err)
			}
			rec.Usage[k] = q
		}
	}
	// Single-metric shorthand: {"metric": "x", "quantity": 3}.
	if m, ok := lookupString(obj, Path{"metric"}); ok && m != "" {
		v, _ := lookup(obj, Path{"quantity"})
		q, err := toFloat(v)
		if err != nil {
			return Record{}, false, fmt.Errorf("quantity: %w", err)
		}
		rec.Usage[m] = q
	}
	rec.SKU, _ = lookupString(obj, Path{"sku"})
	rec.Region, _ = lookupString(obj, Path{"region"})
	rec.Project, _ = lookupString(obj, Path{"project"})
	rec.TenantCluster, _ = lookupString(obj, Path{"tenant_cluster"})
	if d, ok := obj["dimensions"].(map[string]any); ok {
		rec.Dimensions = map[string]string{}
		for k, v := range d {
			rec.Dimensions[k] = fmt.Sprint(v)
		}
	}
	return rec, false, nil
}

func lookup(obj map[string]any, p Path) (any, bool) {
	for _, path := range p {
		var cur any = obj
		ok := true
		for _, part := range strings.Split(path, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			if cur, ok = m[part]; !ok {
				break
			}
		}
		if ok && cur != nil && cur != "" {
			return cur, true
		}
	}
	return nil, false
}

func lookupString(obj map[string]any, p Path) (string, bool) {
	v, ok := lookup(obj, p)
	if !ok {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return fmt.Sprint(x), true
	}
}

func toFloat(v any) (float64, error) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, err
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return 0, fmt.Errorf("%q is not a number", x)
		}
	case nil:
		return 0, errors.New("missing quantity")
	default:
		return 0, fmt.Errorf("%v is not a number", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("%v must be a finite, non-negative number", f)
	}
	return f, nil
}

// parseTime accepts RFC 3339 (with or without zone, UTC assumed) or a Unix
// timestamp in seconds, milliseconds, microseconds or nanoseconds.
func parseTime(v any) (time.Time, error) {
	var num float64
	switch x := v.(type) {
	case float64:
		num = x
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return time.Time{}, err
		}
		num = f
	case string:
		s := strings.TrimSpace(x)
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), nil
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("time %q is not RFC 3339 or a Unix timestamp", s)
		}
		num = f
	default:
		return time.Time{}, fmt.Errorf("time %v is not RFC 3339 or a Unix timestamp", v)
	}
	switch {
	case num < 1e11: // seconds
		sec, frac := math.Modf(num)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC(), nil
	case num < 1e14:
		return time.UnixMilli(int64(num)).UTC(), nil
	case num < 1e17:
		return time.UnixMicro(int64(num)).UTC(), nil
	default:
		return time.Unix(0, int64(num)).UTC(), nil
	}
}

// RecordID derives the ledger ID of one metric of one request. The client
// is part of it so two emitters with overlapping request ID schemes never
// deduplicate each other's records. tenant is the token's bound tenant, or
// else the emitter's tenant value before aliases resolve it (TenantKey):
// the same request then gets the same ID however and whenever it arrives,
// pushed by a gateway or pulled from its logs, before or after an alias
// changes.
func RecordID(client, tenant, requestID, metric string) string {
	sum := sha256.Sum256([]byte(client + "\x00" + tenant + "\x00" + requestID + "\x00" + metric))
	return "rq1_" + hex.EncodeToString(sum[:16])
}

// ContentID derives a request ID from the record's content, for emitters
// that send none (RECORDS_ALLOW_UNKEYED). Two identical requests at the same
// instant then count once.
func ContentID(obj map[string]any) string {
	b, _ := json.Marshal(obj) // map keys are sorted, so this is canonical
	sum := sha256.Sum256(b)
	return "content-" + hex.EncodeToString(sum[:12])
}
