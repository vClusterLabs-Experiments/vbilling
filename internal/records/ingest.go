package records

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/telemetry"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Options configure records ingest and aggregation.
type Options struct {
	// Window is the metering window (COLLECTION_INTERVAL).
	Window time.Duration
	// Settle is how long after a window ends records still count as on
	// time (RECORDS_SETTLE, default 30s). Records that arrive later are
	// billed as a separate late adjustment to the same window.
	Settle time.Duration
	// MaxAge is the oldest record accepted. It must stay inside the records
	// retention, because late records are deduplicated against the stored
	// ones.
	MaxAge time.Duration
	// MaxFuture is how far ahead of vBilling's clock a record may be.
	MaxFuture time.Duration
	// AllowUnkeyed derives a request ID from the record's content when the
	// emitter sends none.
	AllowUnkeyed bool
	// RejectUnknownTenants refuses records whose tenant matches no known
	// tenant or alias. By default they are accepted and reported.
	RejectUnknownTenants bool
	Region               string
	Now                  func() time.Time
}

func (o *Options) defaults() {
	if o.Window <= 0 {
		o.Window = time.Minute
	}
	if o.Settle <= 0 {
		o.Settle = 30 * time.Second
	}
	if o.Settle >= o.Window {
		o.Settle = o.Window / 2
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 34 * 24 * time.Hour
	}
	if o.MaxFuture <= 0 {
		o.MaxFuture = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// TenantResolver maps an emitter's tenant value (a team ID, an API key
// alias, a customer ID) to a billing tenant. known is false when nothing
// matched, in which case tenant is the value itself.
type TenantResolver func(value string) (tenant string, known bool)

// Ingester validates records and writes them to the records ledger.
type Ingester struct {
	spool   *spool.Spool
	formats Formats
	opts    Options
	resolve TenantResolver
	health  *health.Registry
	tel     *telemetry.Registry
}

// NewIngester returns an ingester writing to sp, the records ledger.
func NewIngester(sp *spool.Spool, formats Formats, opts Options, resolve TenantResolver, h *health.Registry, tel *telemetry.Registry) *Ingester {
	opts.defaults()
	if resolve == nil {
		resolve = func(v string) (string, bool) { return v, true }
	}
	if h == nil {
		h = health.New(tel)
	}
	if tel == nil {
		tel = telemetry.New()
	}
	return &Ingester{spool: sp, formats: formats, opts: opts, resolve: resolve, health: h, tel: tel}
}

// Rejection explains why one record was refused.
type Rejection struct {
	Index int    `json:"index"`
	ID    string `json:"id,omitempty"`
	Error string `json:"error"`
}

// Result reports what happened to a payload.
type Result struct {
	Format            string      `json:"format"`
	Received          int         `json:"received"`
	Accepted          int         `json:"accepted"`
	UsageRows         int         `json:"usage_rows"`
	Skipped           int         `json:"skipped,omitempty"`
	DuplicatesInBatch int         `json:"duplicates_in_batch,omitempty"`
	Rejected          []Rejection `json:"rejected,omitempty"`
	UnknownTenants    []string    `json:"unknown_tenants,omitempty"`
	Warnings          []string    `json:"warnings,omitempty"`
}

// ErrUnknownFormat is returned for a format that is not configured.
type ErrUnknownFormat struct{ Name string }

func (e ErrUnknownFormat) Error() string {
	return fmt.Sprintf("unknown format %q (built in: %s; litellm with RECORDS_LITELLM=true; others in RECORDS_MAPPINGS_FILE)", e.Name, Native)
}

// Ingest maps, validates and durably stores a payload. boundTenant is the
// tenant the caller's ingest token is bound to, or "" for an unbound token.
// It returns only after the records are fsynced; a returned error means
// nothing was stored and the emitter should retry.
func (in *Ingester) Ingest(format, client, boundTenant string, body []byte) (Result, error) {
	res := Result{Format: format}
	mapping, ok := in.formats[format]
	if !ok && format != Native {
		return res, ErrUnknownFormat{format}
	}
	objs, err := Parse(body)
	if err != nil {
		return res, err
	}
	res.Received = len(objs)
	now := in.opts.Now().UTC()
	source := "records:" + client
	seen := map[string]bool{}
	unknown := map[string]bool{}
	noTime := 0
	var rows []usage.Event
	for i, obj := range objs {
		rec, skip, err := Map(format, mapping, obj)
		if skip {
			res.Skipped++
			continue
		}
		reject := func(msg string) { res.Rejected = append(res.Rejected, Rejection{Index: i, ID: rec.ID, Error: msg}) }
		if err != nil {
			reject(err.Error())
			continue
		}
		if rec.ID == "" {
			if !in.opts.AllowUnkeyed {
				reject("no request ID: every record needs a unique, stable ID so retries are not billed twice (set RECORDS_ALLOW_UNKEYED=true to derive one from the record's content)")
				continue
			}
			rec.ID = ContentID(obj)
		}
		switch {
		case boundTenant != "":
			// The token identifies the source, and the source is the tenant.
			rec.Tenant = boundTenant
		case rec.Tenant == "":
			reject("no tenant: the record has no tenant field (or use an ingest token bound to a tenant, see INGEST_TOKENS)")
			continue
		default:
			t, known := in.resolve(rec.Tenant)
			if !known {
				if in.opts.RejectUnknownTenants {
					reject(fmt.Sprintf("unknown tenant %q: map it with the vbilling.vcluster.com/aliases annotation or the format's tenantMap", rec.Tenant))
					continue
				}
				unknown[t] = true
			}
			rec.Tenant = t
		}
		if rec.Time.IsZero() {
			rec.Time = now
			noTime++
		}
		if rec.Time.After(now.Add(in.opts.MaxFuture)) {
			reject(fmt.Sprintf("time %s is in the future", rec.Time.Format(time.RFC3339)))
			continue
		}
		if rec.Time.Before(now.Add(-in.opts.MaxAge)) {
			reject(fmt.Sprintf("time %s is older than %s, the oldest usage vBilling accepts", rec.Time.Format(time.RFC3339), in.opts.MaxAge))
			continue
		}
		ws, we := usage.Window(rec.Time, in.opts.Window)
		var recRows []usage.Event
		var problems []string
		metrics := make([]string, 0, len(rec.Usage))
		for m := range rec.Usage {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, metric := range metrics {
			q := rec.Usage[metric]
			def, ok := usage.Lookup(metric)
			if !ok {
				problems = append(problems, fmt.Sprintf("unknown metric %q (declare it in CUSTOM_METRICS or the format's metrics)", metric))
				continue
			}
			if q == 0 {
				continue
			}
			id := RecordID(client, rec.Tenant, rec.ID, metric)
			if seen[id] {
				res.DuplicatesInBatch++
				continue
			}
			seen[id] = true
			ev := usage.Event{
				ID: id, Tenant: rec.Tenant, Metric: metric, Quantity: q, Unit: def.Unit,
				WindowStart: ws, WindowEnd: we,
				Region: rec.Region, Project: rec.Project, SKU: rec.SKU,
				Dimensions: map[string]string{},
				Properties: map[string]any{"request_id": rec.ID, "at": rec.Time.Format(time.RFC3339Nano)},
				Source:     source, RecordedAt: now,
			}
			if ev.Region == "" {
				ev.Region = in.opts.Region
			}
			for k, v := range rec.Dimensions {
				ev.Dimensions[k] = v
			}
			if rec.TenantCluster != "" {
				ev.Dimensions[usage.DimTenantCluster] = rec.TenantCluster
			}
			ev.Finalize()
			if err := ev.Validate(); err != nil {
				problems = append(problems, err.Error())
				continue
			}
			recRows = append(recRows, ev)
		}
		if len(problems) > 0 {
			reject(strings.Join(problems, "; "))
			continue
		}
		res.Accepted++
		rows = append(rows, recRows...)
	}
	if len(rows) > 0 {
		if _, err := in.spool.AppendBatch(source, rows); err != nil {
			in.health.Fail(health.Name("records", client), "records", client, err, "the records ledger could not be written; the emitter must retry")
			return res, fmt.Errorf("records ledger write failed: %w", err)
		}
	}
	res.UsageRows = len(rows)
	for t := range unknown {
		res.UnknownTenants = append(res.UnknownTenants, t)
	}
	sort.Strings(res.UnknownTenants)
	if len(res.UnknownTenants) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("billed to tenants vBilling does not know (%s); map gateway identities with the vbilling.vcluster.com/aliases annotation or the format's tenantMap", strings.Join(res.UnknownTenants, ", ")))
		in.health.Set(health.Name("records", client, "unknown-tenants"), "records", client, health.Empty,
			"records for unknown tenants: "+strings.Join(res.UnknownTenants, ", "), "map them with the vbilling.vcluster.com/aliases annotation or tenantMap")
	}
	if noTime > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d record(s) had no time; the arrival time was used", noTime))
	}
	if res.Accepted > 0 {
		in.health.OK(health.Name("records", client), "records", client, res.Accepted)
	}
	in.tel.Add("vbilling_records_received_total", "Per-request usage records received.", map[string]string{"client": client}, float64(res.Received))
	in.tel.Add("vbilling_records_accepted_total", "Per-request usage records durably stored.", map[string]string{"client": client}, float64(res.Accepted))
	in.tel.Add("vbilling_records_rejected_total", "Per-request usage records refused.", map[string]string{"client": client}, float64(len(res.Rejected)))
	return res, nil
}
