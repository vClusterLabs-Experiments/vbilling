package records

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
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
	MeteringSource       string // stable collector identity, shared by its standby
	Now                  func() time.Time
	// BoundTokens is INGEST_TOKENS (token -> tenant). A pull pinned to a
	// tenant stores its rows under the record IDs of that tenant's bound
	// token (see CheckPin). Only the tokens' IDs (TokenID) are kept.
	BoundTokens map[string]string
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
	// MaxRequestIDLen bounds a record's request ID, in bytes. Gateways use
	// UUIDs and short prefixed IDs (chatcmpl-...); a longer ID (a client can
	// choose LiteLLM's call ID) is replaced by its hash (RequestID).
	MaxRequestIDLen = 256
	// MaxUsagePerRecord bounds the usage keys (metrics) of one record. A
	// request reports a handful; each key becomes a usage row.
	MaxUsagePerRecord = 64
	// MaxRejections bounds the refused records a Result lists;
	// RejectedCount counts them all.
	MaxRejections = 100
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

// TokenID identifies an ingest token without revealing it: 8 bytes of its
// SHA-256, in hex. Record IDs, SKU caps and rate limits use it, never the
// token.
func TokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// boundNamespace is the record ID namespace of records sent with the
// tenant-bound token tokenID under client (and of rows a pull pinned to the
// token's tenant stores). Each token has its own: a tenant chooses its
// client names and can learn the request IDs of the gateways that serve
// it, and billing keeps the first copy of an ID, so a copy sent with
// another token must never take the ID of the gateway's own record.
func boundNamespace(tokenID, client string) string { return "bound\x00" + tokenID + "\x00" + client }

// RequestID is the request ID a record is stored under: id itself, or, for
// an ID longer than MaxRequestIDLen, "sha256:" and its hash in hex. Pushed
// and pulled copies of a request get the same. An ID that starts with
// "sha256:" is hashed too, so no request can take the ID a long one is
// stored under.
func RequestID(id string) string {
	if len(id) <= MaxRequestIDLen && !strings.HasPrefix(id, "sha256:") {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:])
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
	before   *storedBefore

	mu    sync.Mutex
	pulls map[string]pullIdentity // format -> what its pull stores rows under

	pins map[string][]string // tenant -> IDs of its tokens in Options.BoundTokens, sorted

	// clusters knows the clusters a record may name (UseClusters). Nil knows
	// none: a record that names a cluster is refused.
	clusters *safeguard.Clusters
	// oldest is the oldest usage the destinations accept, and oldestBy the
	// destination that sets it (LimitAge). Zero: none sets a limit.
	oldest   time.Duration
	oldestBy string
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
	SKUSourceBound  = "bound"  // prefix of the source of each token bound to a tenant (SKUSourceToken)
)

// SKUSourceToken is the SKU source of the tenant-bound token tokenID
// (TokenID): every bound token counts on its own, so a tenant's own token
// never uses up what the operator's gateway reports with the tenant's other
// token.
func SKUSourceToken(tokenID string) string { return SKUSourceBound + ":" + tokenID }

// SKUSourcePull is the SKU source of the pulls of client's request log.
func SKUSourcePull(client string) string { return "pull:" + client }

// UseSKUGate applies g to every usage row stored, pushed or pulled. Set it
// before records are ingested.
func (in *Ingester) UseSKUGate(g SKUGate) { in.skuGate = g }

// UseClusters makes c decide which cluster a record may name (its
// tenant_cluster): one of the record's tenant, for a window in which it
// existed. A pushed record that names another is refused; a pulled row
// keeps its usage and loses the cluster. Set it before records are
// ingested.
func (in *Ingester) UseClusters(c *safeguard.Clusters) { in.clusters = c }

// LimitAge refuses records older than limit, the oldest usage the named
// destination accepts (destinations.AgeLimiter): it would dead-letter their
// usage. Options.MaxAge still applies. Set it before records are ingested.
func (in *Ingester) LimitAge(limit time.Duration, destination string) {
	in.oldest, in.oldestBy = limit, destination
}

// maxAge is the oldest record accepted: Options.MaxAge, or the destinations'
// limit when that is tighter.
func (in *Ingester) maxAge() time.Duration {
	if in.oldest > 0 && in.oldest < in.opts.MaxAge {
		return in.oldest
	}
	return in.opts.MaxAge
}

// ResolveTenant maps a tenant value an emitter sends to a billing tenant,
// as records do: the ingest API resolves the tenant of pushed events with
// it, so both push paths know the same tenants.
func (in *Ingester) ResolveTenant(v string) (tenant string, known bool) { return in.resolve(v) }

// RejectsUnknownTenants reports whether records for a tenant vBilling does
// not know are refused (Options.RejectUnknownTenants).
func (in *Ingester) RejectsUnknownTenants() bool { return in.opts.RejectUnknownTenants }

// HasFormat reports whether records can be sent in format.
func (in *Ingester) HasFormat(format string) bool {
	_, ok := in.formats[format]
	return ok || format == Native
}

// recordValues checks a pushed record's values and dimensions, which reach
// dashboards, exports and billing platforms.
func recordValues(rec *Record) string {
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
	in := &Ingester{spool: sp, formats: formats, opts: opts, resolve: resolve, health: h, tel: tel, arrivals: newArrivals(), pins: map[string][]string{}}
	for tok, tenant := range opts.BoundTokens {
		in.pins[tenant] = append(in.pins[tenant], TokenID(tok))
	}
	for _, ids := range in.pins {
		sort.Strings(ids)
	}
	in.opts.BoundTokens = nil // the tokens themselves are not kept
	in.loadRecent(sp, opts.Now().UTC())
	return in
}

// CheckPin refuses to pin a pull to a tenant with more than one token in
// INGEST_TOKENS. A pinned pull stores its rows under the record IDs of the
// tenant's bound token, so that a request the pulled gateway pushed with
// that token and the pull reads back count once; with two tokens it cannot
// tell which one the gateway uses, and the other's copies, sent first,
// would replace the gateway's records.
func (in *Ingester) CheckPin(tenant string) error {
	if n := len(in.pins[tenant]); tenant != "" && n > 1 {
		return fmt.Errorf("tenant %s has %d tokens in INGEST_TOKENS: a pull pinned to a tenant stores its rows under the record IDs of the tenant's bound token, "+
			"and cannot tell which of them the gateway it reads pushes with. Bind one token to %s, the one that gateway pushes with, or do not pin the pull", tenant, n, tenant)
	}
	return nil
}

// pinToken is the ID of the bound token whose namespace rows a pull pins to
// tenant use: the tenant's only token in INGEST_TOKENS, or "" (a namespace
// no token has) when it has none, or several (CheckPin refuses those).
func (in *Ingester) pinToken(tenant string) string {
	if ids := in.pins[tenant]; len(ids) == 1 {
		return ids[0]
	}
	return ""
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
// Only rows stored in the records ledger are remembered, so a refused
// record leaves nothing behind. While a request is stored, the rows it
// dated are pending, and a retry that arrives meanwhile gets their time.
type arrivals struct {
	mu      sync.Mutex
	first   map[string]time.Time // rows stored
	order   []arrival            // by arrival, to forget the oldest
	pending map[string]pending   // rows of requests in flight
}

type arrival struct {
	id string
	at time.Time
}

type pending struct {
	at    time.Time
	holds int // requests in flight that dated the row
}

func newArrivals() *arrivals {
	return &arrivals{first: map[string]time.Time{}, pending: map[string]pending{}}
}

// date returns the time a record whose rows have these IDs first arrived:
// when a stored row of it arrived, when a request in flight dated it, or
// now. It returns the IDs it dated now, which stay pending until release.
func (a *arrivals) date(ids []string, now time.Time) (time.Time, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forget(now)
	for _, id := range ids {
		if at, ok := a.first[id]; ok {
			return at, nil
		}
		if p, ok := a.pending[id]; ok {
			return p.at, nil
		}
	}
	for _, id := range ids {
		a.pending[id] = pending{at: now, holds: a.pending[id].holds + 1}
	}
	return now, ids
}

// release lets go of IDs date returned. Remember the stored rows first.
func (a *arrivals) release(ids []string) {
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, id := range ids {
		if p := a.pending[id]; p.holds > 1 {
			p.holds--
			a.pending[id] = p
		} else {
			delete(a.pending, id)
		}
	}
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

// add remembers the arrivals of rows in the records ledger: found there at
// the start, or just stored.
func (a *arrivals) add(found []arrival) {
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

const upgradeMemory = 48 * time.Hour

// storedBefore holds compact hashes of the IDs stored before this start,
// for the same 48-hour retry horizon as the ingest index. Tenant-bound rows
// get IDs of their own since v0.3.1, so a batch an emitter stored with
// v0.3.0 and retries after the upgrade is recognized here by the ID v0.3.0
// gave it.
type storedBefore struct {
	mu    sync.Mutex
	until time.Time
	ids   map[[16]byte]int64 // ID hash -> when the row was appended
}

func idHash(id string) (key [16]byte) {
	sum := sha256.Sum256([]byte(id))
	copy(key[:], sum[:16])
	return key
}

// has reports whether id was stored before the start, while that is still
// remembered.
func (b *storedBefore) has(id string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ids == nil {
		return false
	}
	if now.After(b.until) {
		b.ids = nil // the complete upgrade retry horizon has elapsed
		return false
	}
	at, ok := b.ids[idHash(id)]
	return ok && !time.Unix(at, 0).Before(now.Add(-upgradeMemory))
}

// loadRecent remembers pre-start IDs for the 48-hour upgrade retry horizon.
// Arrival-time records also keep their timestamp for arrivalMemory, so a
// retry without a timestamp keeps the time assigned to its first copy.
func (in *Ingester) loadRecent(sp *spool.Spool, now time.Time) {
	in.before = &storedBefore{until: now.Add(upgradeMemory), ids: map[[16]byte]int64{}}
	if sp == nil {
		return
	}
	var found []arrival
	_ = sp.ScanAppended(now.Add(-upgradeMemory), now.Add(24*time.Hour), func(ev *usage.Event, appended time.Time) error {
		key := idHash(ev.ID)
		if prev, ok := in.before.ids[key]; !ok || appended.Unix() < prev {
			in.before.ids[key] = appended.Unix()
		}
		if ev.Properties["arrival_time"] != true || appended.Before(now.Add(-arrivalMemory)) {
			return nil
		}
		s, _ := ev.Properties["at"].(string)
		if at, err := time.Parse(time.RFC3339Nano, s); err == nil {
			found = append(found, arrival{ev.ID, at})
		}
		return nil
	})
	in.arrivals.add(found)
}

// pullIdentity is the client, tenant and token namespace a pull stores a
// gateway's rows under. A push of the same gateway must use the same, or
// each request is stored under two record IDs and billed twice.
type pullIdentity struct{ client, tenant, token string }

// expectPull records the identity LiteLLM's pull stores rows under, so
// pushes in the litellm format from the gateway it reads, under another
// identity, are flagged in health.
func (in *Ingester) expectPull(format, client, tenant string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.pulls == nil {
		in.pulls = map[string]pullIdentity{}
	}
	in.pulls[format] = pullIdentity{client, tenant, in.pinToken(tenant)}
}

// checkPull marks a push source failing when the push comes from the
// gateway vBilling pulls under another identity. Only two pushes can be
// that gateway: one with the shared ingest token, the operator's, under
// the pull's client (LITELLM_CLIENT) while the pull is pinned to a tenant,
// and one with the token of the pinned tenant whose namespace the pull
// uses, under another client. A push with any other tenant-bound token is
// that tenant's own gateway, never flagged: a hint to pin the pull to it
// would bill every request of the pulled gateway to that tenant.
func (in *Ingester) checkPull(format, client, bound, token string) {
	in.mu.Lock()
	p, ok := in.pulls[format]
	in.mu.Unlock()
	if !ok {
		return
	}
	const twice = "every request the pull reads back is stored a second time under another record ID and billed twice"
	name := health.Name("records", client, bound, "litellm-pull")
	switch {
	case bound == "" && client == p.client && p.tenant != "":
		in.health.Fail(name, "records", client, fmt.Errorf("LiteLLM pushes with the shared ingest token, but the LiteLLM pull bills every request to tenant %s (LITELLM_TENANT): %s", p.tenant, twice),
			"unset LITELLM_TENANT, or push with the token INGEST_TOKENS binds to "+p.tenant)
	case bound != "" && bound == p.tenant && token == p.token && client != p.client:
		in.health.Fail(name, "records", client, fmt.Errorf("LiteLLM pushes as client %s with the token INGEST_TOKENS binds to %s, but the LiteLLM pull reads its spend logs as client %s (LITELLM_CLIENT): %s",
			client, bound, p.client, twice),
			"set LITELLM_CLIENT="+client+", or have LiteLLM send X-VBilling-Client: "+p.client)
	}
}

// Rejection explains why one record (or one ingested event) was refused:
// Error says it in full, Reason names the kind of problem.
type Rejection struct {
	Index  int    `json:"index"`
	ID     string `json:"id,omitempty"`
	Reason Reason `json:"reason,omitempty"`
	Error  string `json:"error"`
}

// Result reports what happened to a payload.
type Result struct {
	Format            string `json:"format"`
	Received          int    `json:"received"`
	Accepted          int    `json:"accepted"`
	UsageRows         int    `json:"usage_rows"`
	Skipped           int    `json:"skipped,omitempty"`
	DuplicatesInBatch int    `json:"duplicates_in_batch,omitempty"`
	// Rejected lists the first MaxRejections refused records, and
	// RejectedCount counts them all.
	Rejected       []Rejection `json:"rejected,omitempty"`
	RejectedCount  int         `json:"rejected_count,omitempty"`
	UnknownTenants []string    `json:"unknown_tenants,omitempty"`
	Warnings       []string    `json:"warnings,omitempty"`
	// AlreadyStored counts valid records that were in the records ledger
	// already, every usage row of each, so none was stored again: rows a
	// puller read again, or a push retried across an upgrade.
	AlreadyStored int `json:"already_stored,omitempty"`

	rejectedBy map[Reason]int // RejectedCount by reason, for the counters
}

// reject records a refused record.
func (r *Result) reject(rej Rejection) {
	r.RejectedCount++
	if r.rejectedBy == nil {
		r.rejectedBy = map[Reason]int{}
	}
	r.rejectedBy[rej.Reason]++
	if len(r.Rejected) < MaxRejections {
		r.Rejected = append(r.Rejected, rej)
	}
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
	// Token identifies the bound token (TokenID), never the token itself:
	// records sent with it get IDs in its own namespace. "" is the bound
	// tenant's only token in Options.BoundTokens, the one its pinned pulls
	// use.
	Token string
	// Known reports whether a push of this client was accepted before.
	// Metric series and health sources name a client only then, or once
	// its push is accepted: a name that only ever comes with refused
	// records costs nothing.
	Known bool
}

// Ingest maps, validates and durably stores a payload pushed by a known
// client. boundTenant is the tenant the caller's ingest token is bound to,
// or "" for an unbound token; a bound push comes with the tenant's only
// token in Options.BoundTokens (see Push.Token).
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
	return in.ingest(ingestCall{format: format, client: p.Client, bound: p.Bound, token: p.Token, known: p.Known}, objs)
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
	token                 string // TokenID of the bound token; "" for the bound tenant's pin
	// stored indexes the records ledger for pulled records; nil for pushes.
	stored *storedIndex
	// known: the pushing client had a push accepted before.
	known bool
}

// ingest stores records.
func (in *Ingester) ingest(c ingestCall, objs []map[string]any) (Result, error) {
	client, boundTenant, stored := c.client, c.bound, c.stored
	// Records sent with a token bound to a tenant get IDs in the token's own
	// namespace, and rows a pull pins to a tenant in that of the tenant's
	// bound token (boundNamespace).
	idClient, token := client, ""
	if boundTenant != "" {
		if token = c.token; token == "" {
			token = in.pinToken(boundTenant)
		}
		idClient = boundNamespace(token, client)
	}
	pulled := stored != nil
	res := Result{Format: c.format}
	mapping := in.formats[c.format]
	res.Received = len(objs)
	now := in.opts.Now().UTC()
	source := "records:" + client
	seen := map[string]bool{}
	unknown := map[string]bool{}
	// v030 maps the ID of a row to the ID v0.3.0 gave the same row, before
	// bound rows had a namespace of their own and long request IDs were
	// hashed: a row stored under it is the same request (see dropStored and
	// storedBefore).
	v030 := map[string]string{}
	noTime := 0
	size := 0
	var recs [][]usage.Event // usage rows per accepted record
	// Rows of records without a time are dated on arrival (arrivals). Those
	// dated here are pending until the request ends, and only the ones
	// stored are remembered.
	var dating []string
	defer func() { in.arrivals.release(dating) }()
	datedAt := map[string]time.Time{}
	for i, obj := range objs {
		rec, skip, err := Map(c.format, mapping, obj)
		if skip {
			res.Skipped++
			continue
		}
		sentID := rec.ID
		rec.ID = RequestID(rec.ID)
		reject := func(reason Reason, msg string) {
			res.reject(Rejection{Index: i, ID: rec.ID, Reason: reason, Error: msg})
		}
		if err != nil {
			reject(ReasonInvalid, err.Error())
			continue
		}
		if rec.ID == "" {
			if !in.opts.AllowUnkeyed {
				reject(ReasonInvalid, "no request ID: every record needs a unique, stable ID so retries are not billed twice")
				continue
			}
			rec.ID = ContentID(obj)
			sentID = rec.ID
		}
		if len(rec.Usage) > MaxUsagePerRecord {
			reject(ReasonInvalid, fmt.Sprintf("%d usage keys, more than %d", len(rec.Usage), MaxUsagePerRecord))
			continue
		}
		if pulled {
			makeSafe(&rec)
		} else if msg := recordValues(&rec); msg != "" {
			reject(ReasonInvalid, msg)
			continue
		}
		switch {
		case boundTenant != "":
			// The token identifies the source, and the source is the tenant.
			rec.Tenant, rec.TenantKey = boundTenant, boundTenant
		case rec.Tenant == "":
			reject(ReasonTenant, "no tenant: the record has no tenant field (or use an ingest token bound to a tenant, see INGEST_TOKENS)")
			continue
		default:
			t, known := in.resolve(rec.Tenant)
			if !known {
				if in.opts.RejectUnknownTenants {
					reject(ReasonTenant, fmt.Sprintf("unknown tenant %q: map it with the vbilling.vcluster.com/aliases annotation or the format's tenantMap", rec.Tenant))
					continue
				}
				unknown[t] = true
			}
			rec.Tenant = t
		}
		dated := rec.Time.IsZero()
		if dated {
			var ids, held []string
			for metric, q := range rec.Usage {
				if q != 0 {
					ids = append(ids, RecordID(idClient, rec.TenantKey, rec.ID, metric))
				}
			}
			rec.Time, held = in.arrivals.date(ids, now)
			dating = append(dating, held...)
			noTime++
		}
		if rec.Time.After(now.Add(in.opts.MaxFuture)) {
			reject(ReasonInFuture, fmt.Sprintf("time %s is in the future", rec.Time.Format(time.RFC3339)))
			continue
		}
		if rec.Time.Before(now.Add(-in.opts.MaxAge)) {
			reject(ReasonTooOld, fmt.Sprintf("time %s is older than %s, the oldest usage vBilling accepts", rec.Time.Format(time.RFC3339), in.opts.MaxAge))
			continue
		}
		ws, we := usage.Window(rec.Time, in.opts.Window)
		// A destination compares the start of the usage's window with its own
		// limit and dead-letters what is older: refuse that here, where the
		// emitter still gets the answer.
		if in.oldest > 0 && ws.Before(now.Add(-in.oldest)) {
			reject(ReasonTooOld, fmt.Sprintf("time %s is in a window that started more than %s ago, the oldest usage destination %s accepts",
				rec.Time.Format(time.RFC3339), safeguard.Age(in.oldest), in.oldestBy))
			continue
		}
		// A record names a cluster of its tenant, for a window in which that
		// cluster existed, or none. A pulled row was served, so it keeps its
		// usage and loses the cluster instead.
		unknownCluster := ""
		if cluster := rec.cluster(); cluster != "" {
			switch err := in.clusters.Check(rec.Tenant, cluster, ws, we); {
			case err == nil:
			case errors.Is(err, safeguard.ErrNotReady):
				return Result{Format: c.format, Received: res.Received}, fmt.Errorf("%w (tenant_cluster %q): retry in a few seconds", err, cluster)
			case pulled:
				unknownCluster = cluster
				rec.dropCluster()
			default:
				reject(ReasonCluster, err.Error())
				continue
			}
		}
		var recRows []usage.Event
		var problems Problems
		duplicates := 0
		metrics := make([]string, 0, len(rec.Usage))
		for m := range rec.Usage {
			metrics = append(metrics, m)
		}
		sort.Strings(metrics)
		for _, metric := range metrics {
			q := rec.Usage[metric]
			def, ok := usage.Lookup(metric)
			if !ok {
				problems.Add(ReasonUnknownMetric, fmt.Sprintf("unknown metric %q (declare it in CUSTOM_METRICS or the format's metrics)", metric))
				continue
			}
			// A metric vBilling measures itself takes no records, pushed or
			// pulled: its usage would be billed like measured usage.
			if err := safeguard.PushedMetric(def, safeguard.PathRecords); err != nil {
				problems.Add(ReasonReservedMetric, err.Error())
				continue
			}
			if q == 0 {
				continue
			}
			id := RecordID(idClient, rec.TenantKey, rec.ID, metric)
			if seen[id] {
				duplicates++
				continue
			}
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
			if unknownCluster != "" {
				ev.Properties[propUnknownCluster] = unknownCluster
			}
			if dated {
				ev.Properties["arrival_time"] = true // "at" is when it first arrived
				datedAt[id] = rec.Time
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
				problems.Add(ReasonInvalid, err.Error())
				continue
			}
			if boundTenant != "" || sentID != rec.ID {
				v030[id] = RecordID(client, rec.TenantKey, sentID, metric)
			}
			recRows = append(recRows, ev)
		}
		if problems.Any() {
			reject(problems.Reason(), problems.String())
			continue
		}
		for i := range recRows {
			// Only a valid record reserves its IDs. A refused copy must
			// not suppress a valid replacement later in the same batch.
			seen[recRows[i].ID] = true
			size += rowSize(&recRows[i])
		}
		res.DuplicatesInBatch += duplicates
		res.Accepted++
		recs = append(recs, recRows)
	}
	if !pulled && size > MaxStoredPerRequest {
		return Result{Format: c.format, Received: res.Received}, ErrTooLarge{Bytes: size}
	}
	if pulled {
		var err error
		if recs, err = in.dropStored(recs, stored, v030); err != nil {
			return res, fmt.Errorf("read records ledger: %w", err)
		}
	} else if len(v030) > 0 {
		recs = in.dropStoredBefore(recs, v030, now)
	}
	res.AlreadyStored = res.Accepted - len(recs)
	res.Accepted = len(recs)
	var rows []usage.Event
	for _, r := range recs {
		rows = append(rows, r...)
	}
	skuSource := SKUSourceShared
	switch {
	case pulled:
		skuSource = SKUSourcePull(client)
	case boundTenant != "":
		skuSource = SKUSourceToken(token)
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
	report := pulled || c.known || res.Accepted > 0 || res.AlreadyStored > 0 // health and metrics may name the client
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
		var arrived []arrival
		for _, ev := range rows {
			if at, ok := datedAt[ev.ID]; ok {
				arrived = append(arrived, arrival{ev.ID, at})
			}
		}
		in.arrivals.add(arrived)
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
	if n := withoutCluster(recs); n > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d pulled record(s) named a cluster vBilling does not know for their tenant: their usage is stored without a cluster "+
			"(the %s property of their rows keeps the name)", n, propUnknownCluster))
		log.Printf("[records] %s: %d pulled record(s) named a cluster vBilling does not know for their tenant; their usage is stored without a cluster", client, n)
	}
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
	if pulled {
		return res, nil // the puller reports its own health and counters
	}
	// Every pushed record is counted, whoever sent it. The counters name the
	// client only once a push of it was accepted (UnacceptedClient).
	labelled := UnacceptedClient
	if report {
		labelled = client
		if res.Accepted > 0 || res.AlreadyStored > 0 {
			in.health.OK(health.Name("records", client), "records", client, res.Accepted)
			in.checkPull(c.format, client, boundTenant, token)
		}
		in.tel.Add("vbilling_records_accepted_total", "Per-request usage records durably stored.", map[string]string{"client": client}, float64(res.Accepted))
	}
	in.tel.Add("vbilling_records_received_total", "Per-request usage records received.", map[string]string{"client": labelled}, float64(res.Received))
	for _, reason := range Reasons {
		if n := res.rejectedBy[reason]; n > 0 {
			in.tel.Add("vbilling_records_rejected_total", "Per-request usage records refused, by reason.",
				map[string]string{"client": labelled, "reason": string(reason)}, float64(n))
		}
	}
	return res, nil
}

// propUnknownCluster is the property of a pulled row that named a cluster
// vBilling does not know for its tenant: the row is stored without the
// cluster, and the property keeps the name the gateway logged.
const propUnknownCluster = "unknown_tenant_cluster"

// withoutCluster counts the records whose rows carry propUnknownCluster.
func withoutCluster(recs [][]usage.Event) int {
	n := 0
	for _, r := range recs {
		if len(r) > 0 && r[0].Properties[propUnknownCluster] != nil {
			n++
		}
	}
	return n
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
// records left with none. A bound row, or one whose request ID was hashed,
// also counts as stored under the ID v0.3.0 gave it (v030), so a pull that
// re-reads requests v0.3.0 stored does not store them again. Rows the index does not cover (a request longer than
// the range the puller indexed) are looked up in the ledger. A row stored
// between the scan and the append is stored twice, which the aggregator's
// deduplication absorbs.
func (in *Ingester) dropStored(recs [][]usage.Event, idx *storedIndex, v030 map[string]string) ([][]usage.Event, error) {
	want := map[string]bool{}
	var from, to time.Time
	for _, r := range recs {
		for i := range r {
			ev := &r[i]
			if idx.covers(ev) {
				continue
			}
			want[ev.ID] = false
			if old := v030[ev.ID]; old != "" {
				want[old] = false
			}
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
	have := func(id string) bool {
		_, ok := idx.ids[id]
		return ok || want[id]
	}
	kept := recs[:0]
	for _, r := range recs {
		var left []usage.Event
		for i := range r {
			if old := v030[r[i].ID]; !have(r[i].ID) && (old == "" || !have(old)) {
				left = append(left, r[i])
			}
		}
		if len(left) > 0 {
			kept = append(kept, left)
		}
	}
	return kept, nil
}

// dropStoredBefore removes the rows of a push that were stored before this
// start under the ID v0.3.0 gave them (a batch retried across the
// upgrade), and the records left with none.
func (in *Ingester) dropStoredBefore(recs [][]usage.Event, v030 map[string]string, now time.Time) [][]usage.Event {
	kept := recs[:0]
	for _, r := range recs {
		var left []usage.Event
		for i := range r {
			if old := v030[r[i].ID]; old == "" || !in.before.has(old, now) {
				left = append(left, r[i])
			}
		}
		if len(left) > 0 {
			kept = append(kept, left)
		}
	}
	return kept
}
