package api

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/records"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Ingest quotas. They are constants, not settings: each is far above what a
// real emitter needs, and only bounds what one token or one tenant can do
// to vBilling, and so to every other tenant.
const (
	// The shared ingest token (INGEST_TOKEN) is the operator's: its
	// gateways post records for every tenant. 5,000 requests per second, in
	// bursts of 10,000, covers gateways that post every request on its own
	// at thousands per second.
	sharedTokenRate, sharedTokenBurst = 5000, 10000
	// A tenant-bound token (INGEST_TOKENS) belongs to one tenant's own
	// gateway. 500 requests per second, in bursts of 1,000, covers a gateway
	// that posts every request on its own at that rate, and a batching one
	// (LiteLLM's generic API logger posts up to 512 records per request)
	// far beyond it, while one tenant cannot take the API from the others.
	boundTokenRate, boundTokenBurst = 500, 1000
	// Every request body is read into memory first. POST /api/v1/events
	// takes at most maxIngestEvents events in maxIngestBody bytes, and
	// POST /api/v1/records at most maxRecordsBody bytes, enough for
	// thousands of records per request (records.MaxStoredPerRequest bounds
	// what one request writes to the records ledger).
	maxIngestBody   = 10 << 20
	maxIngestEvents = 5000
	maxRecordsBody  = 16 << 20
	// maxSKUsPerTenantPerDay bounds the new SKUs one source (the shared
	// ingest token, each tenant-bound token, or one pull) brings in for a
	// tenant in one UTC day. Every new SKU can create a meter in a billing
	// platform (vBilling splits Stripe meters by SKU), and meters are shared
	// by every tenant of the account. A SKU counts only when it is new:
	// not in the tenant's recent ledger, not priced by a billing platform
	// and not seen from the source today. Real tenants use a few GPU types
	// and at most a few dozen models a day. Usage with a SKU beyond the cap
	// is still stored, under OverflowSKU, never dropped (a cap that dropped
	// usage would let a tenant drop its own), and the tenant's source turns
	// failing in health.
	maxSKUsPerTenantPerDay = 100
	// knownSKUsFor is how long a SKU a tenant had in the ledger stays known
	// (admitted without counting) after its last use.
	knownSKUsFor = 48 * time.Hour
	// pricedRefresh is how often the SKUs the price check covers are read
	// again.
	pricedRefresh = time.Minute
)

// Client names. X-VBilling-Client names the emitter: it is part of record
// IDs and ledger sources, and it labels metrics and health sources.
// Whoever holds an ingest token chooses it, so it is short and plain, and
// each token may use only a few.
const (
	// MaxHeaderBytes bounds the header of every API request (Go's default
	// is 1 MiB). Ingest copies no header into the ledger, and none of
	// vBilling's requests needs more.
	MaxHeaderBytes = 64 << 10
	// maxClientsPerBoundToken bounds the client names one tenant-bound
	// token uses while vBilling runs: each name a request was accepted
	// under keeps metric series and health sources until a restart. A
	// tenant's gateway needs one or two.
	maxClientsPerBoundToken = 16
	// maxClientsPerSharedToken bounds them for the operator's shared
	// token, which every gateway of the operator may use under its own
	// name.
	maxClientsPerSharedToken = 64
)

var clientNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// OverflowSKU is the SKU usage is stored under once its source brought in
// maxSKUsPerTenantPerDay new SKUs for its tenant in the day. Price it, or
// find out why a source sends so many.
const OverflowSKU = "sku-limit-exceeded"

// rateLimiter is a token bucket per ingest token.
type rateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	return &rateLimiter{now: now, buckets: map[string]*bucket{}}
}

// allow takes one request from key's bucket. When it is empty, it returns
// how long until the next request is allowed.
func (l *rateLimiter) allow(key string, rate, burst float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// limitKey identifies an ingest token in the rate limiter, the client
// names, the SKU caps and record IDs without keeping the token itself as a
// map key.
func limitKey(token string) string { return records.TokenID(token) }

// limitIngest answers 429 with Retry-After when the caller's token is over
// its request rate.
func (s *Server) limitIngest(w http.ResponseWriter, token string, bound bool) bool {
	rate, burst := float64(sharedTokenRate), float64(sharedTokenBurst)
	if bound {
		rate, burst = boundTokenRate, boundTokenBurst
	}
	ok, wait := s.limiter.allow(limitKey(token), rate, burst)
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(wait.Seconds(), 1)))))
	writeErr(w, http.StatusTooManyRequests, "too many requests for this ingest token (at most %.0f per second); retry later", rate)
	return false
}

// clientNames counts the client names each ingest token uses. A name
// counts once a request under it stored usage; requests in flight hold a
// place, so concurrent requests cannot pass the cap together.
type clientNames struct {
	mu     sync.Mutex
	tokens map[string]*tokenClients // limitKey(token) -> its names
}

type tokenClients struct {
	accepted map[string]bool
	pending  map[string]int // names of requests in flight, not accepted yet
}

func newClientNames() *clientNames { return &clientNames{tokens: map[string]*tokenClients{}} }

// reserve lets a request of the token use name. known is whether a
// request under it was accepted before; call done with whether this one
// was. It refuses a name beyond the token's limit.
func (c *clientNames) reserve(key, name string, limit int) (known bool, done func(accepted bool), err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tc := c.tokens[key]
	if tc == nil {
		tc = &tokenClients{accepted: map[string]bool{}, pending: map[string]int{}}
		c.tokens[key] = tc
	}
	if tc.accepted[name] {
		return true, func(bool) {}, nil
	}
	if tc.pending[name] == 0 && len(tc.accepted)+len(tc.pending) >= limit {
		return false, nil, fmt.Errorf("this ingest token already sent usage under %d client names (X-VBilling-Client), its limit while vBilling runs: "+
			"name each emitter once and keep the name stable", limit)
	}
	tc.pending[name]++
	var once sync.Once
	return false, func(accepted bool) {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if tc.pending[name]--; tc.pending[name] <= 0 {
				delete(tc.pending, name)
			}
			if accepted {
				tc.accepted[name] = true
			}
		})
	}, nil
}

// ingestClient returns a request's client: X-VBilling-Client, or def
// (configured, such as a format name) without the header. It answers 400
// for a malformed name or one past the token's limit; otherwise call done
// with whether the request stored usage. known reports whether the client
// had a request accepted before: only such clients, and those of accepted
// requests, get metric series and health sources.
func (s *Server) ingestClient(w http.ResponseWriter, r *http.Request, c ingestCaller, def string) (client string, known bool, done func(bool), ok bool) {
	client = r.Header.Get("X-VBilling-Client")
	if client == "" {
		client = def
	} else if !clientNameRe.MatchString(client) {
		writeErr(w, http.StatusBadRequest, "X-VBilling-Client must be 1 to 64 letters, digits, dots, dashes or underscores, starting with a letter or digit")
		return "", false, nil, false
	}
	limit := maxClientsPerSharedToken
	if c.bound != "" {
		limit = maxClientsPerBoundToken
	}
	known, done, err := s.clients.reserve(c.key, client, limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return "", false, nil, false
	}
	return client, known, done, true
}

// skuQuota counts the new SKUs each source brought in for each tenant
// during the current UTC day (in memory). It implements records.SKUGate.
type skuQuota struct {
	mu     sync.Mutex
	now    func() time.Time
	day    string
	seen   map[skuKey]map[string]bool // SKUs counted today
	logged map[skuKey]bool
	known  *skuIndex             // SKUs each tenant has in the ledger
	priced func(sku string) bool // nil: no price check
	health *health.Registry      // nil: no source health
}

type skuKey struct{ source, tenant string }

func newSKUQuota(now func() time.Time) *skuQuota {
	return &skuQuota{now: now, seen: map[skuKey]map[string]bool{}, logged: map[skuKey]bool{}, known: newSKUIndex()}
}

// Admit returns the SKU usage is stored under: sku itself, unless it is a
// new SKU and source already brought in maxSKUsPerTenantPerDay new SKUs for
// tenant today, then OverflowSKU. overflow reports the replacement. A SKU
// the tenant has in its recent ledger, or one a billing platform prices,
// is never new.
func (q *skuQuota) Admit(source, tenant, sku string) (stored string, overflow bool) {
	if sku == "" || sku == OverflowSKU {
		return sku, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now().UTC()
	if day := now.Format("2006-01-02"); day != q.day {
		q.day, q.seen, q.logged = day, map[skuKey]map[string]bool{}, map[skuKey]bool{}
	}
	k := skuKey{source, tenant}
	skus := q.seen[k]
	if skus[sku] || q.known.has(tenant, sku) || (q.priced != nil && q.priced(sku)) {
		return sku, false
	}
	if len(skus) >= maxSKUsPerTenantPerDay {
		if !q.logged[k] {
			q.logged[k] = true
			log.Printf("[api] %s brought in more than %d new SKUs for tenant %s today: new SKUs are stored as %q until tomorrow (UTC)",
				sourceDesc(source), maxSKUsPerTenantPerDay, tenant, OverflowSKU)
		}
		return OverflowSKU, true
	}
	if skus == nil {
		skus = map[string]bool{}
		q.seen[k] = skus
	}
	skus[sku] = true
	q.known.add(tenant, sku, now)
	return sku, false
}

// Overflowed marks the tenant's source failing: n usage rows of one
// request were stored under OverflowSKU.
func (q *skuQuota) Overflowed(source, tenant string, n int) {
	if q.health == nil || n == 0 {
		return
	}
	q.mu.Lock()
	day := q.day
	q.mu.Unlock()
	q.health.Fail(health.Name("records", tenant, "sku-limit"), "records", tenant,
		fmt.Errorf("%d usage row(s) from %s were stored under SKU %s: %s brought in more than %d new SKUs for tenant %s on %s (UTC). "+
			"A SKU counts as new unless the tenant used it in the last two days or a billing platform prices it",
			n, sourceDesc(source), OverflowSKU, sourceDesc(source), maxSKUsPerTenantPerDay, tenant, day),
		"price "+OverflowSKU+" in your billing platform, or find out why this source sends so many SKUs; the count starts again at 00:00 UTC")
}

func sourceDesc(source string) string {
	switch {
	case source == records.SKUSourceShared:
		return "the shared ingest token"
	case strings.HasPrefix(source, records.SKUSourceBound+":"):
		return "a tenant-bound ingest token"
	default:
		return source
	}
}

// skuIndex knows the SKUs each tenant had in the usage ledger during the
// last knownSKUsFor.
type skuIndex struct {
	mu   sync.Mutex
	skus map[string]map[string]time.Time // tenant -> SKU -> last appended
}

func newSKUIndex() *skuIndex { return &skuIndex{skus: map[string]map[string]time.Time{}} }

func (x *skuIndex) has(tenant, sku string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, ok := x.skus[tenant][sku]
	return ok
}

func (x *skuIndex) add(tenant, sku string, at time.Time) {
	if tenant == "" || sku == "" || sku == OverflowSKU {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	m := x.skus[tenant]
	if m == nil {
		m = map[string]time.Time{}
		x.skus[tenant] = m
	}
	if at.After(m[sku]) {
		m[sku] = at
	}
}

// prune forgets SKUs not appended since cutoff.
func (x *skuIndex) prune(cutoff time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for t, m := range x.skus {
		for sku, at := range m {
			if at.Before(cutoff) {
				delete(m, sku)
			}
		}
		if len(m) == 0 {
			delete(x.skus, t)
		}
	}
}

// follow reads the SKUs of the ledger's last knownSKUsFor, then of every
// event appended, until ctx ends.
func (x *skuIndex) follow(ctx context.Context, sp *spool.Spool, now func() time.Time) {
	read := sp.Committed()
	start := now()
	err := sp.ScanAppended(start.Add(-knownSKUsFor), start.Add(time.Hour), func(ev *usage.Event, at time.Time) error {
		x.add(ev.Tenant, ev.SKU, at)
		return ctx.Err()
	})
	if err != nil && ctx.Err() == nil {
		log.Printf("[api] read the SKUs of the last %s of the ledger: %v", knownSKUsFor, err)
	}
	pruned := now()
	for {
		changed := sp.Changed()
		for {
			recs, err := sp.Read(read, 5000)
			if err != nil {
				log.Printf("[api] follow the ledger's SKUs: %v", err)
				break
			}
			if len(recs) == 0 {
				break
			}
			for _, r := range recs {
				if r.Event != nil {
					x.add(r.Event.Tenant, r.Event.SKU, r.At)
				}
			}
			read = recs[len(recs)-1].Seq
		}
		if t := now(); t.Sub(pruned) > time.Hour {
			x.prune(t.Add(-knownSKUsFor))
			pruned = t
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(time.Minute):
		}
	}
}

// pricedSKUs are the SKUs the last price check found priced at a billing
// platform.
type pricedSKUs struct {
	mu   sync.Mutex
	skus map[string]bool
}

func (p *pricedSKUs) has(sku string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skus[sku]
}

// refreshPriced reads the SKUs the price check covers.
func (s *Server) refreshPriced() {
	if s.pricing == nil {
		return
	}
	skus := map[string]bool{}
	for _, d := range s.pricing.Report().Destinations {
		for _, row := range d.Series {
			if row.Status == destinations.PriceCovered && row.SKU != "" {
				skus[row.SKU] = true
			}
		}
	}
	s.priced.mu.Lock()
	s.priced.skus = skus
	s.priced.mu.Unlock()
}

// Run keeps what the SKU cap needs current until ctx ends: the SKUs each
// tenant had in the ledger lately, and those billing platforms price.
// Without it, only the SKUs admitted since the start are known.
func (s *Server) Run(ctx context.Context) {
	go s.skus.known.follow(ctx, s.spool, s.now)
	t := time.NewTicker(pricedRefresh)
	defer t.Stop()
	for {
		s.refreshPriced()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
