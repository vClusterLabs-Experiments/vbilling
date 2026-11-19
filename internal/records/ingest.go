package records

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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
	spool    *spool.Spool
	formats  Formats
	opts     Options
	resolve  TenantResolver
	health   *health.Registry
	tel      *telemetry.Registry
	arrivals *arrivals
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
	in := &Ingester{spool: sp, formats: formats, opts: opts, resolve: resolve, health: h, tel: tel, arrivals: newArrivals()}
	in.arrivals.load(sp, opts.Now().UTC())
	return in
}

// arrivalMemory is how long vBilling remembers when a record without a
// time first arrived. Gateways retry within seconds to minutes; send a time
// with every record to be safe from retries any later than this.
const arrivalMemory = time.Hour

// arrivals dates records without a time. Such a record is dated when it
// first arrives, and a retry of it gets that same time: dated on arrival,
// a retry that crossed a window boundary would land in another window,
// where its request ID is no duplicate, and be billed twice. Keyed by row
// ID (request and metric); rebuilt from the records ledger on start.
type arrivals struct {
	mu    sync.Mutex
	first map[string]time.Time
	order []arrival // by arrival, to forget the oldest
}

type arrival struct {
	id string
	at time.Time
}

func newArrivals() *arrivals { return &arrivals{first: map[string]time.Time{}} }

// date returns the time a record whose rows have these IDs first arrived,
// remembering now when none of them is known.
func (a *arrivals) date(ids []string, now time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forget(now)
	for _, id := range ids {
		if at, ok := a.first[id]; ok {
			return at
		}
	}
	for _, id := range ids {
		a.first[id] = now
		a.order = append(a.order, arrival{id, now})
	}
	return now
}

// forget drops arrivals older than arrivalMemory. a.mu held.
func (a *arrivals) forget(now time.Time) {
	n := 0
	for n < len(a.order) && now.Sub(a.order[n].at) > arrivalMemory {
		if a.first[a.order[n].id].Equal(a.order[n].at) {
			delete(a.first, a.order[n].id)
		}
		n++
	}
	a.order = a.order[n:]
}

// load remembers the records without a time stored in the last
// arrivalMemory, so a retry after a restart is recognized too.
func (a *arrivals) load(sp *spool.Spool, now time.Time) {
	if sp == nil {
		return
	}
	var found []arrival
	_ = sp.ScanAppended(now.Add(-arrivalMemory), now.Add(24*time.Hour), func(ev *usage.Event, _ time.Time) error {
		if ev.Properties["arrival_time"] != true {
			return nil
		}
		s, _ := ev.Properties["at"].(string)
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			found = append(found, arrival{ev.ID, at})
		}
		return nil
	})
	sort.SliceStable(found, func(i, j int) bool { return found[i].at.Before(found[j].at) })
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range found {
		if _, ok := a.first[f.id]; !ok {
			a.first[f.id] = f.at
			a.order = append(a.order, f)
		}
	}
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
	// AlreadyStored counts valid records a puller read again: every usage
	// row of each was in the records ledger already, so none was stored.
	AlreadyStored int `json:"already_stored,omitempty"`
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
	if _, ok := in.formats[format]; !ok && format != Native {
		return Result{Format: format}, ErrUnknownFormat{format}
	}
	objs, err := Parse(body)
	if err != nil {
		return Result{Format: format}, err
	}
	return in.ingest(format, client, boundTenant, objs, nil)
}

// ingestPulled stores records a puller read from an emitter's own logs,
// such as LiteLLM's spend logs, exactly as if the emitter had pushed them:
// same format, same client, same record IDs, so a request that was both
// pushed and pulled counts once. Records already in the records ledger
// (stored, or in its index) are not stored again, so a puller can re-read
// a trailing range on every pass. Pulled records do not touch the health
// of the client's push source.
func (in *Ingester) ingestPulled(format, client, boundTenant string, objs []map[string]any, stored *storedIndex) (Result, error) {
	if _, ok := in.formats[format]; !ok && format != Native {
		return Result{Format: format}, ErrUnknownFormat{format}
	}
	if stored == nil {
		stored = &storedIndex{ids: map[string]struct{}{}}
	}
	return in.ingest(format, client, boundTenant, objs, stored)
}

// ingest stores records; stored is non-nil for pulled records.
func (in *Ingester) ingest(format, client, boundTenant string, objs []map[string]any, stored *storedIndex) (Result, error) {
	pulled := stored != nil
	res := Result{Format: format}
	mapping := in.formats[format]
	res.Received = len(objs)
	now := in.opts.Now().UTC()
	source := "records:" + client
	seen := map[string]bool{}
	unknown := map[string]bool{}
	noTime := 0
	var recs [][]usage.Event // usage rows per accepted record
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
			rec.Tenant, rec.TenantKey = boundTenant, boundTenant
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
		dated := rec.Time.IsZero()
		if dated {
			var ids []string
			for metric, q := range rec.Usage {
				if q != 0 {
					ids = append(ids, RecordID(client, rec.TenantKey, rec.ID, metric))
				}
			}
			rec.Time = in.arrivals.date(ids, now)
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
			id := RecordID(client, rec.TenantKey, rec.ID, metric)
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
			if pulled {
				ev.Properties["via"] = "pull"
			}
			if dated {
				ev.Properties["arrival_time"] = true // "at" is when it first arrived
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
		recs = append(recs, recRows)
	}
	if pulled {
		var err error
		if recs, err = in.dropStored(recs, stored); err != nil {
			return res, fmt.Errorf("read records ledger: %w", err)
		}
		res.AlreadyStored = res.Accepted - len(recs)
		res.Accepted = len(recs)
	}
	var rows []usage.Event
	for _, r := range recs {
		rows = append(rows, r...)
	}
	if len(rows) > 0 {
		if _, err := in.spool.AppendBatch(source, rows); err != nil {
			if !pulled {
				in.health.Fail(health.Name("records", client), "records", client, err, "the records ledger could not be written; the emitter must retry")
			}
			return res, fmt.Errorf("records ledger write failed: %w", err)
		}
		if pulled {
			for _, ev := range rows {
				stored.ids[ev.ID] = struct{}{}
			}
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
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d record(s) had no time; the time each first arrived was used (a retry within an hour keeps it; send a time to be safe from later retries)", noTime))
	}
	if pulled {
		return res, nil // the puller reports its own health and counters
	}
	if res.Accepted > 0 {
		in.health.OK(health.Name("records", client), "records", client, res.Accepted)
	}
	in.tel.Add("vbilling_records_received_total", "Per-request usage records received.", map[string]string{"client": client}, float64(res.Received))
	in.tel.Add("vbilling_records_accepted_total", "Per-request usage records durably stored.", map[string]string{"client": client}, float64(res.Accepted))
	in.tel.Add("vbilling_records_rejected_total", "Per-request usage records refused.", map[string]string{"client": client}, float64(len(res.Rejected)))
	return res, nil
}

// storedIndex holds the IDs of the records stored for windows within
// [from, to), so a puller scans the records ledger once per range it reads
// instead of once per page.
type storedIndex struct {
	from, to time.Time
	ids      map[string]struct{}
}

// indexStored scans the records ledger for windows within [from, to).
func (in *Ingester) indexStored(from, to time.Time) (*storedIndex, error) {
	idx := &storedIndex{from: from, to: to, ids: map[string]struct{}{}}
	err := in.spool.ScanRecords(from, to, func(ev *usage.Event, _ time.Time) error {
		idx.ids[ev.ID] = struct{}{}
		return nil
	})
	return idx, err
}

func (idx *storedIndex) covers(ev *usage.Event) bool {
	return !idx.from.IsZero() && !ev.WindowStart.Before(idx.from) && !ev.WindowEnd.After(idx.to)
}

// dropStored removes the usage rows already in the records ledger, and the
// records left with none. Rows the index does not cover (a request longer
// than the range the puller indexed) are looked up in the ledger. A row
// stored between the scan and the append is stored twice, which the
// aggregator's deduplication absorbs.
func (in *Ingester) dropStored(recs [][]usage.Event, idx *storedIndex) ([][]usage.Event, error) {
	have := func(ev *usage.Event) bool { _, ok := idx.ids[ev.ID]; return ok }
	want := map[string]bool{}
	var from, to time.Time
	for _, r := range recs {
		for i := range r {
			ev := &r[i]
			if idx.covers(ev) {
				continue
			}
			want[ev.ID] = false
			if from.IsZero() || ev.WindowStart.Before(from) {
				from = ev.WindowStart
			}
			if ev.WindowEnd.After(to) {
				to = ev.WindowEnd
			}
		}
	}
	if len(want) > 0 {
		err := in.spool.ScanRecords(from, to, func(ev *usage.Event, _ time.Time) error {
			if _, ok := want[ev.ID]; ok {
				want[ev.ID] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	kept := recs[:0]
	for _, r := range recs {
		var left []usage.Event
		for i := range r {
			if !have(&r[i]) && !want[r[i].ID] {
				left = append(left, r[i])
			}
		}
		if len(left) > 0 {
			kept = append(kept, left)
		}
	}
	return kept, nil
}
