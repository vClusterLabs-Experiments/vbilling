package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
)

// Options configure the Resolver.
type Options struct {
	Client ClientOptions
	// Cluster is this control plane cluster's name in the Platform
	// (spec.clusterRef.cluster of its instances). Empty matches any.
	Cluster string
	// Refresh is how often the Platform is read.
	Refresh time.Duration
	// OrphanGrace: a cluster whose instance disappeared while its workload
	// keeps running stays with its last owner this long, then falls back to
	// the vbilling.vcluster.com label rules. Zero keeps it forever.
	OrphanGrace time.Duration
	// StatePath persists bindings and their history (DATA_DIR/platform-bindings.json).
	StatePath string
	// Committed returns the end of the last window in the ledger. A move is
	// never dated before it: committed windows stay with the owner they
	// were billed to, and the history must agree with the ledger.
	Committed func() (time.Time, bool)
	Health    *health.Registry
	Now       func() time.Time
}

// forgetAfter drops bindings of workloads not seen for this long.
const forgetAfter = 7 * 24 * time.Hour

// maxHistory bounds the earlier owners kept per cluster.
const maxHistory = 8

// Resolver binds clusters to their Platform owners. It caches the last
// successful read: while the Platform is unreachable every known cluster
// stays with the owner it had, so a transient failure never moves usage to
// another tenant. Bindings are persisted, so a restart during an outage
// keeps them too.
type Resolver struct {
	client *Client
	opts   Options
	store  *kv.Store
	keyTag string

	mu      sync.Mutex
	snap    *Snapshot // last successful read
	lastTry time.Time
	lastErr error
	entries map[string]*entry // namespace/name of the workload -> binding
}

// entry is one cluster's binding and its earlier owners.
type entry struct {
	WorkloadUID string                    `json:"workloadUID,omitempty"`
	Binding     discovery.PlatformBinding `json:"binding"`
	// Since is when Binding took effect: the creation of the instance that
	// moved the cluster to its owner when that is known, else when vBilling
	// first saw the owner.
	Since time.Time `json:"since"`
	// MissingSince is set while no instance points at the cluster.
	MissingSince time.Time `json:"missingSince,omitempty"`
	LastSeen     time.Time `json:"lastSeen"`
	History      []epoch   `json:"history,omitempty"`
}

// epoch is an earlier owner, for windows metered after the fact.
type epoch struct {
	Binding discovery.PlatformBinding `json:"binding"`
	Since   time.Time                 `json:"since"`
	Until   time.Time                 `json:"until"`
}

// ErrNotReady means the Platform has never answered and nothing is
// persisted, so no cluster can be attributed yet.
var ErrNotReady = errors.New("vCluster Platform has not answered yet")

// New returns a Resolver and loads persisted bindings.
func New(opts Options) (*Resolver, error) {
	c, err := NewClient(opts.Client)
	if err != nil {
		return nil, err
	}
	if opts.Refresh <= 0 {
		opts.Refresh = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	store, err := kv.Open(opts.StatePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", opts.StatePath, err)
	}
	sum := sha256.Sum256([]byte(opts.Client.AccessKey))
	r := &Resolver{client: c, opts: opts, store: store, keyTag: hex.EncodeToString(sum[:4]), entries: map[string]*entry{}}
	for k, v := range store.WithPrefix("binding/") {
		var e entry
		if json.Unmarshal([]byte(v), &e) == nil {
			r.entries[k] = &e
		}
	}
	if n := len(r.entries); n > 0 {
		log.Printf("[platform] loaded %d persisted cluster binding(s); they stand until the Platform answers", n)
	}
	return r, nil
}

// Client returns the Platform client.
func (r *Resolver) Client() *Client { return r.client }

// refresh reads the Platform when the last read is older than Refresh, or
// when force is set and the last attempt is more than a few seconds old.
func (r *Resolver) refresh(ctx context.Context, force bool) {
	now := r.opts.Now()
	r.mu.Lock()
	since := now.Sub(r.lastTry)
	due := r.lastTry.IsZero() || since >= r.opts.Refresh || (force && since >= forceGap)
	if due {
		r.lastTry = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	snap, err := r.client.Snapshot(ctx, now.UTC())
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = err
		if r.opts.Health != nil {
			hint := "no owner is known yet, so clusters are not attributed until it answers"
			if r.snap != nil || len(r.entries) > 0 {
				hint = fmt.Sprintf("keeping the last known owners of %d cluster(s); usage is never moved to another tenant while the Platform is unreachable", len(r.entries))
			}
			r.opts.Health.Fail("platform", "platform", r.client.Host(), err, hint)
		}
		return
	}
	if r.snap == nil {
		tenants := "no Tenant resource (Platform before 4.12): projects are the billing accounts"
		if snap.HasTenants() {
			tenants = fmt.Sprintf("%d Tenant(s)", len(snap.Tenants))
		}
		log.Printf("[platform] %s: access key of %q, %d project(s), %s, %d virtual cluster instance(s)",
			r.client.Host(), snap.Self.Subject, len(snap.Projects), tenants, len(snap.Instances))
	}
	r.snap, r.lastErr = snap, nil
	if r.opts.Health != nil {
		r.opts.Health.OK("platform", "platform", r.client.Host(), len(snap.Instances))
	}
}

// forceGap limits reads forced by new clusters.
const forceGap = 5 * time.Second

// young reports whether a cluster without a binding may be newer than the
// last read: the Platform creates the instance first, so it may simply not
// have been seen yet.
func (r *Resolver) young(c *discovery.TenantCluster) bool {
	return r.snap == nil || c.CreatedAt.After(r.snap.At.Add(-time.Minute))
}

// unseen reports whether a cluster has no binding and no instance in the last read.
func (r *Resolver) unseen(c *discovery.TenantCluster, idx map[string][]*Object) bool {
	key := c.Namespace + "/" + c.Name
	return !c.External && r.entries[key] == nil && len(idx[key]) == 0
}

// Bind implements discovery.Binder.
func (r *Resolver) Bind(ctx context.Context, clusters []discovery.TenantCluster) ([]discovery.TenantCluster, error) {
	r.refresh(ctx, false)
	r.mu.Lock()
	force := false
	if r.snap != nil && r.lastErr == nil {
		idx := r.snap.index(r.opts.Cluster)
		for i := range clusters {
			if c := &clusters[i]; r.unseen(c, idx) && r.young(c) {
				force = true // created after the last read: read again before guessing
				break
			}
		}
	}
	r.mu.Unlock()
	if force {
		r.refresh(ctx, true)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snap == nil && len(r.entries) == 0 {
		return nil, fmt.Errorf("%w at %s: %v", ErrNotReady, r.client.Host(), r.lastErr)
	}
	now := r.opts.Now().UTC()
	var idx map[string][]*Object
	if r.snap != nil {
		idx = r.snap.index(r.opts.Cluster)
	}
	out := clusters[:0]
	for i := range clusters {
		c := clusters[i]
		if c.External {
			out = append(out, c) // TENANT_CLUSTERS_FILE clusters carry their own identity
			continue
		}
		key := c.Namespace + "/" + c.Name
		id := c.ExternalID()
		e := r.entries[key]
		if e != nil && e.WorkloadUID != "" && c.UID != "" && e.WorkloadUID != c.UID {
			r.forget(key) // a new cluster under an old name
			e = nil
		}
		if e == nil && r.lastErr != nil && r.young(&c) {
			// New since the last read and the Platform is unreachable: its
			// owner is unknown, and billing a guess would move its usage to
			// another tenant once the Platform answers.
			r.report(id, health.Failing, "created while vCluster Platform is unreachable; not metered until the Platform names its owner", hintHeld)
			continue
		}
		prevUID := ""
		if e != nil {
			prevUID = e.Binding.InstanceUID
			e.LastSeen = now.Truncate(time.Hour)
		}
		var (
			b       discovery.PlatformBinding
			problem string
			ok      bool
		)
		if r.snap != nil {
			b, problem, ok = r.snap.resolve(idx[key], prevUID)
		}
		switch {
		case ok:
			e = r.record(key, c.UID, b, now)
			c.ApplyPlatform(e.Binding)
			r.report(id, statusOf(problem), problem, hintFix)
		case e != nil && r.snap != nil && problem == "" && r.orphaned(e, now):
			log.Printf("[platform] %s: no instance for %s; billed by vbilling.vcluster.com labels from now on (was %s)",
				id, r.opts.OrphanGrace, e.Binding.Owner())
			r.forget(key)
			r.report(id, health.Empty, "no VirtualClusterInstance points at this cluster any more; billed by its vbilling.vcluster.com labels after PLATFORM_ORPHAN_GRACE", hintRegister)
		case e != nil:
			c.ApplyPlatform(e.Binding)
			switch {
			case r.snap == nil:
				r.report(id, health.OK, "", "") // the Platform has not answered since the restart: persisted owner
			case problem == "":
				if e.MissingSince.IsZero() {
					e.MissingSince = now
					log.Printf("[platform] %s: no VirtualClusterInstance points at it any more; still billed to %s", id, e.Binding.Owner())
				}
				r.report(id, health.Failing, "no VirtualClusterInstance points at this cluster any more; still billed to its last owner, "+e.Binding.Owner(), hintRegister)
			default:
				r.report(id, health.Failing, problem+"; still billed to its last owner, "+e.Binding.Owner(), hintFix)
			}
		case problem != "":
			r.report(id, health.Failing, problem, hintFix)
		case c.Meta("tenant") != "":
			r.report(id, health.OK, "", "") // deliberately billed by its vbilling.vcluster.com/tenant label
		default:
			r.report(id, health.Empty, "no VirtualClusterInstance points at this cluster; billed by vbilling.vcluster.com labels (TENANT_SOURCE=cluster rules)", hintRegister)
		}
		if e = r.entries[key]; e != nil {
			r.persist(key, e)
		}
		out = append(out, c)
	}
	for key, e := range r.entries {
		if now.Sub(e.LastSeen) > forgetAfter {
			r.forget(key)
		}
	}
	return out, nil
}

// record makes b the cluster's binding. When the owner changed, the old one
// becomes history: the move is dated to the creation of the instance that
// now points at the cluster (the Platform creates a new instance when it
// moves a cluster to another project), else to now, and never before the
// last window already in the ledger.
func (r *Resolver) record(key, workloadUID string, b discovery.PlatformBinding, now time.Time) *entry {
	e := r.entries[key]
	if e == nil {
		since := now
		if !b.InstanceCreated.IsZero() && b.InstanceCreated.Before(now) {
			since = b.InstanceCreated
		}
		e = &entry{WorkloadUID: workloadUID, Binding: b, Since: since, LastSeen: now.Truncate(time.Hour)}
		r.entries[key] = e
		return e
	}
	e.MissingSince = time.Time{}
	if e.WorkloadUID == "" {
		e.WorkloadUID = workloadUID
	}
	if e.Binding.Same(&b) {
		e.Binding = b // names, settings and phase may change
		return e
	}
	at := now
	if b.InstanceUID != e.Binding.InstanceUID && b.InstanceCreated.After(e.Since) && b.InstanceCreated.Before(now) {
		at = b.InstanceCreated
	}
	if r.opts.Committed != nil {
		if wm, ok := r.opts.Committed(); ok && at.Before(wm) {
			at = wm // windows up to wm are in the ledger under the old owner
		}
	}
	log.Printf("[platform] %s moved from %s (tenant %s) to %s (tenant %s) at %s; windows before stay billed to the old owner",
		key, e.Binding.Owner(), e.Binding.TenantID(), b.Owner(), b.TenantID(), at.Format(time.RFC3339))
	e.History = append(e.History, epoch{Binding: e.Binding, Since: e.Since, Until: at})
	if len(e.History) > maxHistory {
		e.History = e.History[len(e.History)-maxHistory:]
	}
	e.Binding, e.Since = b, at
	return e
}

// statusOf is OK for a binding without problems, else Failing.
func statusOf(problem string) health.Status {
	if problem == "" {
		return health.OK
	}
	return health.Failing
}

func (r *Resolver) orphaned(e *entry, now time.Time) bool {
	return r.opts.OrphanGrace > 0 && !e.MissingSince.IsZero() && now.Sub(e.MissingSince) >= r.opts.OrphanGrace
}

// Hints for per-cluster binding problems.
const (
	hintFix      = "fix the Platform objects; vBilling never guesses another owner"
	hintRegister = "register the cluster in vCluster Platform, or set vbilling.vcluster.com/tenant on it"
	hintHeld     = "metered from the first window after vCluster Platform answers; windows before that are not billed"
)

// report records the binding health of one cluster.
func (r *Resolver) report(id string, st health.Status, msg, hint string) {
	h := r.opts.Health
	if h == nil {
		return
	}
	name := health.Name("platform", id)
	if st == health.OK {
		h.OK(name, "platform", id, 1)
		return
	}
	h.Set(name, "platform", id, st, msg, hint)
}

func (r *Resolver) persist(key string, e *entry) {
	b, err := json.Marshal(e)
	if err == nil {
		err = r.store.Set("binding/"+key, string(b))
	}
	if err != nil {
		log.Printf("[platform] persist binding of %s: %v", key, err)
	}
}

func (r *Resolver) forget(key string) {
	delete(r.entries, key)
	_ = r.store.Delete("binding/" + key)
}

// OwnerAt returns cl as it was bound during the window ending at end, for
// windows metered after the fact (backfill after a restart, fills after a
// tenant API outage). ok is false when the current binding applies.
func (r *Resolver) OwnerAt(cl discovery.TenantCluster, end time.Time) (discovery.TenantCluster, bool) {
	if cl.Platform == nil || cl.External {
		return cl, false
	}
	last := end.Add(-time.Nanosecond) // the window's last instant
	r.mu.Lock()
	e := r.entries[cl.Namespace+"/"+cl.Name]
	if e == nil || len(e.History) == 0 || !last.Before(e.Since) {
		r.mu.Unlock()
		return cl, false
	}
	b := e.History[0].Binding // before anything known: the oldest owner
	for _, h := range e.History {
		if !last.Before(h.Since) && last.Before(h.Until) {
			b = h.Binding
		}
	}
	r.mu.Unlock()
	if b.Same(cl.Platform) {
		return cl, false
	}
	out := cl
	out.Labels = copyMap(cl.Labels)
	out.Annotations = copyMap(cl.Annotations)
	out.ApplyPlatform(b)
	return out, true
}

// ProxyConfig reaches a bound cluster's API through the Platform proxy. A
// sleeping instance is not woken for metering: the error makes its windows
// a tenant API gap, filled once it wakes.
func (r *Resolver) ProxyConfig(cl discovery.TenantCluster) (*rest.Config, string, error) {
	b := cl.Platform
	if b == nil {
		return nil, "", fmt.Errorf("not bound to a vCluster Platform instance")
	}
	if strings.EqualFold(b.InstancePhase, instancePhaseSleeping) {
		return nil, "", fmt.Errorf("instance %s/%s is sleeping in vCluster Platform and is not woken for metering", b.Project, b.Instance)
	}
	cfg := r.client.ProxyConfig(b.Project, b.Instance)
	return cfg, "platform:" + b.Project + "/" + b.Instance + "@" + b.InstanceUID + "#" + r.keyTag, nil
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
