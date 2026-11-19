package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Ingest quotas. They are constants, not settings: each is far above what a
// real emitter needs, and only bounds what one token or one tenant can do
// to vBilling, and so to every other tenant.
const (
	// The shared ingest token (INGEST_TOKEN, by default the API token) is
	// the operator's: its gateways post records for every tenant. 5,000
	// requests per second, in bursts of 10,000, covers gateways that post
	// every request on its own at thousands per second.
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
	// maxSKUsPerTenantPerDay bounds the distinct SKUs one tenant brings in
	// through ingest in one UTC day. Every new SKU can create a meter in a
	// billing platform (vBilling splits Stripe meters by SKU), and meters
	// are shared by every tenant of the account. Real tenants use a few GPU
	// types and at most a few dozen models a day. Usage with a SKU beyond
	// the cap is still billed, under OverflowSKU, never dropped: a cap that
	// dropped usage would let a tenant drop its own.
	maxSKUsPerTenantPerDay = 100
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

// OverflowSKU is the SKU usage is billed under once its tenant brought in
// maxSKUsPerTenantPerDay distinct SKUs in the day. Price it, or find out
// why a tenant sends so many.
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

// limitKey identifies an ingest token in the rate limiter and the client
// names without keeping the token itself as a map key.
func limitKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

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

// skuQuota counts the distinct SKUs each tenant brought in during the
// current UTC day (in memory: a restart starts the count again).
type skuQuota struct {
	mu   sync.Mutex
	now  func() time.Time
	day  string
	seen map[string]map[string]bool // tenant -> SKUs today
}

func newSKUQuota(now func() time.Time) *skuQuota {
	return &skuQuota{now: now, seen: map[string]map[string]bool{}}
}

// admit returns the SKU usage is stored under: sku itself while the tenant
// is within maxSKUsPerTenantPerDay distinct SKUs today, else OverflowSKU.
// overflow reports the replacement.
func (q *skuQuota) admit(tenant, sku string) (stored string, overflow bool) {
	if sku == "" || sku == OverflowSKU {
		return sku, false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if day := q.now().UTC().Format("2006-01-02"); day != q.day {
		q.day, q.seen = day, map[string]map[string]bool{}
	}
	skus := q.seen[tenant]
	if skus == nil {
		skus = map[string]bool{}
		q.seen[tenant] = skus
	}
	if skus[sku] {
		return sku, false
	}
	if len(skus) >= maxSKUsPerTenantPerDay {
		if !skus[OverflowSKU] {
			skus[OverflowSKU] = true // log once per tenant and day
			log.Printf("[api] tenant %s sent more than %d distinct SKUs today: new SKUs are billed as %q until tomorrow (UTC)",
				tenant, maxSKUsPerTenantPerDay, OverflowSKU)
		}
		return OverflowSKU, true
	}
	skus[sku] = true
	return sku, false
}
