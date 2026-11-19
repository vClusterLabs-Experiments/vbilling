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

// Bounds of one pushed request. Every usage row of a record carries the
// record's request ID and dimensions, so a record of many metrics is
// stored many times over.
const (
	// MaxRequestIDLen bounds a pushed record's request ID, in bytes.
	// Gateways use UUIDs and short prefixed IDs (chatcmpl-...).
	MaxRequestIDLen = 256
	// MaxStoredPerRequest bounds what one pushed request writes to the
	// records ledger, estimated before anything is written: about 120,000
	// usage rows. A gateway that batches (LiteLLM posts up to 512 records
	// per request) stays far below it.
	MaxStoredPerRequest = 64 << 20
)

// ErrTooLarge is returned for a push whose usage rows would take more than
// MaxStoredPerRequest in the records ledger. Nothing was stored.
type ErrTooLarge struct{ Bytes int }

func (e ErrTooLarge) Error() string {
	return fmt.Sprintf("these records would take about %d MiB in the records ledger, more than the %d MiB one request may write: send fewer records per request",
		e.Bytes>>20, MaxStoredPerRequest>>20)
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
	skuGate  SKUGate
	arrivals *arrivals
}

// SKUGate bounds the new SKUs usage brings in, per source (SKUSource...)
// and tenant.
type SKUGate interface {
	// Admit returns the SKU a usage row is stored under: sku itself, or a
	// replacement (overflow true) once the source brought in too many new
	// SKUs for the tenant.
	Admit(source, tenant, sku string) (stored string, overflow bool)
	// Overflowed reports that n usage rows of one request or pull, now
	// stored, carry the replacement.
	Overflowed(source, tenant string, n int)
}

// SKU sources: what usage came in with. Each counts new SKUs on its own, so
// what a tenant sends with its own token never uses up what the operator's
// gateway or pulls report for it.
const (
	SKUSourceShared = "shared" // the shared ingest token
	SKUSourceBound  = "bound"  // a token bound to the tenant
)

// SKUSourcePull is the SKU source of the pulls of client's request log.
func SKUSourcePull(client string) string { return "pull:" + client }

// UseSKUGate applies g to every usage row stored, pushed or pulled. Set it
// before records are ingested.
func (in *Ingester) UseSKUGate(g SKUGate) { in.skuGate = g }

// HasFormat reports whether records can be sent in format.
func (in *Ingester) HasFormat(format string) bool {
	_, ok := in.formats[format]
	return ok || format == Native
}

// recordValues checks a pushed record's request ID, values and dimensions,
// which reach dashboards, exports and billing platforms.
func recordValues(rec *Record) string {
	if len(rec.ID) > MaxRequestIDLen {
		return fmt.Sprintf("id is longer than %d bytes", MaxRequestIDLen)
	}
	for _, f := range []struct{ name, v string }{{"sku", rec.SKU}, {"region", rec.Region}, {"project", rec.Project}, {"tenant_cluster", rec.TenantCluster}} {
		if err := usage.CheckValue(f.v); err != nil {
			return f.name + " " + err.Error()
		}
	}
	if len(rec.Dimensions) > usage.MaxDimensions {
		return fmt.Sprintf("%d dimensions, more than %d", len(rec.Dimensions), usage.MaxDimensions)
	}
	keys := make([]string, 0, len(rec.Dimensions))
	for k := range rec.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if usage.ReservedDimension(k) {
			return fmt.Sprintf("dimension %s is the name of a property of the usage itself; rename it", k)
		}
		if err := usage.CheckValue(rec.Dimensions[k]); err != nil {
			return fmt.Sprintf("dimension %s %v", k, err)
		}
	}
	return ""
}

// makeSafe repairs a pulled record's values instead of refusing it: the
// request was served, and dropping its usage would be worse. Dimensions
// named after a property of the usage itself are dropped, and so are those
// past usage.MaxDimensions (in key order).
func makeSafe(rec *Record) {
	rec.SKU = usage.SafeValue(rec.SKU)
	rec.Region = usage.SafeValue(rec.Region)
	rec.Project = usage.SafeValue(rec.Project)
	rec.TenantCluster = usage.SafeValue(rec.TenantCluster)
	keys := make([]string, 0, len(rec.Dimensions))
	for k := range rec.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := 0
	for _, k := range keys {
		if usage.ReservedDimension(k) || kept == usage.MaxDimensions {
			delete(rec.Dimensions, k)
			continue
		}
		rec.Dimensions[k] = usage.SafeValue(rec.Dimensions[k])
		kept++
	}
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

// Push identifies one pushed payload (POST /api/v1/records).
type Push struct {
	// Client names the emitter: X-VBilling-Client, or the format name.
	Client string
	// Bound is the tenant the caller's ingest token is bound to; "" for
	// the shared ingest token.
	Bound string
	// Known reports whether a push of this client was accepted before.
	// Metric series and health sources name a client only then, or once
	// its push is accepted: a name that only ever comes with refused
	// records costs nothing.
	Known bool
}

// Ingest maps, validates and durably stores a payload pushed by a known
// client. boundTenant is the tenant the caller's ingest token is bound to,
// or "" for an unbound token.
func (in *Ingester) Ingest(format, client, boundTenant string, body []byte) (Result, error) {
	return in.IngestPush(format, Push{Client: client, Bound: boundTenant, Known: true}, body)
}

// IngestPush maps, validates and durably stores a pushed payload. It
// returns only after the records are fsynced; a returned error means
// nothing was stored and the emitter should retry, except ErrUnknownFormat
// and ErrTooLarge.
func (in *Ingester) IngestPush(format string, p Push, body []byte) (Result, error) {
	if !in.HasFormat(format) {
		return Result{Format: format}, ErrUnknownFormat{format}
	}
	objs, err := Parse(body)
	if err != nil {
		return Result{Format: format}, err
	}
	return in.ingest(ingestCall{format: format, client: p.Client, bound: p.Bound, known: p.Known}, objs)
}

// ingestPulled stores records a puller read from an emitter's own logs,
// such as LiteLLM's spend logs, exactly as if the emitter had pushed them:
// same format, same client, same record IDs, so a request that was both
// pushed and pulled counts once. Records already in the records ledger
// (stored, or in its index) are not stored again, so a puller can re-read
// a trailing range on every pass. Pulled records do not touch the health
// of the client's push source.
func (in *Ingester) ingestPulled(format, client, boundTenant string, objs []map[string]any, stored *storedIndex) (Result, error) {
	if !in.HasFormat(format) {
		return Result{Format: format}, ErrUnknownFormat{format}
	}
	if stored == nil {
		stored = &storedIndex{ids: map[string]struct{}{}}
	}
	return in.ingest(ingestCall{format: format, client: client, bound: boundTenant, stored: stored}, objs)
}

// ingestCall is one payload to store.
type ingestCall struct {
	format, client, bound string
	// stored indexes the records ledger for pulled records; nil for pushes.
	stored *storedIndex
	// known: the pushing client had a push accepted before.
	known bool
}

// ingest stores records.
func (in *Ingester) ingest(c ingestCall, objs []map[string]any) (Result, error) {
	client, boundTenant, stored := c.client, c.bound, c.stored
	// Records sent with a token bound to a tenant (and rows a pull pins to
	// one) get IDs in their own namespace. The tenant chooses the client
	// name and can learn the gateway's request IDs from its own requests,
	// so without it a bound copy sent first could take the ID of the
	// gateway's own record, and billing keeps only the first copy of an ID.
	idClient := client
	if boundTenant != "" {
		idClient = "bound\x00" + client
	}
	pulled := stored != nil
	res := Result{Format: c.format}
	mapping := in.formats[c.format]
	res.Received = len(objs)
	now := in.opts.Now().UTC()
	source := "records:" + client
	seen := map[string]bool{}
	unknown := map[string]bool{}
	noTime := 0
	size := 0
	var recs [][]usage.Event // usage rows per accepted record
	for i, obj := range objs {
		rec, skip, err := Map(c.format, mapping, obj)
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
				reject("no request ID: every record needs a unique, stable ID so retries are not billed twice")
				continue
			}
			rec.ID = ContentID(obj)
		}
		if pulled {
			makeSafe(&rec)
		} else if msg := recordValues(&rec); msg != "" {
			if len(rec.ID) > MaxRequestIDLen {
				rec.ID = rec.ID[:MaxRequestIDLen] + "..." // keep the response small
			}
			reject(msg)
			continue
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
					ids = append(ids, RecordID(idClient, rec.TenantKey, rec.ID, metric))
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
			id := RecordID(idClient, rec.TenantKey, rec.ID, metric)
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
		for i := range recRows {
			size += rowSize(&recRows[i])
		}
		res.Accepted++
		recs = append(recs, recRows)
	}
	if !pulled && size > MaxStoredPerRequest {
		return Result{Format: c.format, Received: res.Received}, ErrTooLarge{Bytes: size}
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
	skuSource := SKUSourceShared
	switch {
	case pulled:
		skuSource = SKUSourcePull(client)
	case boundTenant != "":
		skuSource = SKUSourceBound
	}
	overflow := map[string]int{} // tenant -> rows stored under another SKU
	if in.skuGate != nil {
		for i := range rows {
			var over bool
			if rows[i].SKU, over = in.skuGate.Admit(skuSource, rows[i].Tenant, rows[i].SKU); over {
				overflow[rows[i].Tenant]++
			}
		}
	}
	report := pulled || c.known || res.Accepted > 0 // health and metrics may name the client
	if len(rows) > 0 {
		if _, err := in.spool.AppendBatch(source, rows); err != nil {
			if !pulled && c.known {
				in.health.Fail(health.Name("records", client), "records", client, err, "the records ledger could not be written; the emitter must retry")
			}
			return res, fmt.Errorf("records ledger write failed: %w", err)
		}
		if pulled {
			for _, ev := range rows {
				stored.ids[ev.ID] = struct{}{}
			}
		}
		total := 0
		for t, n := range overflow {
			in.skuGate.Overflowed(skuSource, t, n)
			total += n
		}
		if total > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d usage row(s) are billed under another SKU: their source sent too many new SKUs for the tenant today", total))
		}
	}
	res.UsageRows = len(rows)
	for t := range unknown {
		res.UnknownTenants = append(res.UnknownTenants, t)
	}
	sort.Strings(res.UnknownTenants)
	if len(res.UnknownTenants) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("billed to tenants vBilling does not know (%s); map gateway identities with the vbilling.vcluster.com/aliases annotation or the format's tenantMap", strings.Join(res.UnknownTenants, ", ")))
		if report {
			in.health.Set(health.Name("records", client, "unknown-tenants"), "records", client, health.Empty,
				"records for unknown tenants: "+strings.Join(res.UnknownTenants, ", "), "map them with the vbilling.vcluster.com/aliases annotation or tenantMap")
		}
	}
	if noTime > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d record(s) had no time; the time each first arrived was used (a retry within an hour keeps it; send a time to be safe from later retries)", noTime))
	}
	if pulled || !report {
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

// rowSize estimates the bytes a usage row takes in the records ledger: its
// JSON, and the record envelope (sequence, time, hash chain) around it.
func rowSize(ev *usage.Event) int {
	n := 450 + len(ev.ID) + len(ev.Tenant) + len(ev.Metric) + len(ev.Unit) + len(ev.Region) + len(ev.Project) + len(ev.SKU) + len(ev.Source)
	for k, v := range ev.Dimensions {
		n += len(k) + len(v) + 6
	}
	if id, ok := ev.Properties["request_id"].(string); ok {
		n += len(id)
	}
	return n
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
