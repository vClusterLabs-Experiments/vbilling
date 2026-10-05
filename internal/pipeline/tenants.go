package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TenantRegistry is the shared view of billing tenants. The controller
// fills it from discovery; the dispatcher uses it to make sure a tenant
// exists in a destination before that destination sees its first event.
type TenantRegistry struct {
	mu      sync.Mutex
	tenants map[string]usage.Tenant
	ensured map[string]map[string]string // destination -> tenant -> fingerprint
}

func NewTenantRegistry() *TenantRegistry {
	return &TenantRegistry{tenants: map[string]usage.Tenant{}, ensured: map[string]map[string]string{}}
}

func (r *TenantRegistry) Upsert(t usage.Tenant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants[t.ID] = t
}

func (r *TenantRegistry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tenants, id)
	for _, m := range r.ensured {
		delete(m, id)
	}
}

// Get returns a known tenant, or a minimal one for tenants only seen in
// ingested events.
func (r *TenantRegistry) Get(id string) (usage.Tenant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[id]
	if !ok {
		return usage.Tenant{ID: id, DisplayName: id}, false
	}
	return t, true
}

// All returns tenants sorted by ID.
func (r *TenantRegistry) All() []usage.Tenant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]usage.Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NeedsEnsure reports whether t (as it is now) has not yet been ensured in dest.
func (r *TenantRegistry) NeedsEnsure(dest string, t usage.Tenant) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ensured[dest][t.ID] != fingerprint(t)
}

func (r *TenantRegistry) MarkEnsured(dest string, t usage.Tenant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ensured[dest] == nil {
		r.ensured[dest] = map[string]string{}
	}
	r.ensured[dest][t.ID] = fingerprint(t)
}

func fingerprint(t usage.Tenant) string {
	t.CreatedAt = t.CreatedAt.UTC()
	b, _ := json.Marshal(t) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}
