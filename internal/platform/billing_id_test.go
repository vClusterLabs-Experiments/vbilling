package platform

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// Editing the tenant setting of a Tenant changes the ID its usage is billed
// under, and a billing platform keys customers on that ID: from then on the
// usage goes to another customer, without the wallet of the old one. Nothing
// stopped or reported that. It is still the operator's call, but it is now
// said where it can be found: the log, the sources API, a counter, and the
// customer record (previous_tenant_id). A move to another Tenant is not
// such a change.
func TestBillingIDChangeOfTheSameAccountIsReported(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 4, 16, 0, time.UTC)
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	var changes [][2]string
	r, err := New(Options{
		Client:            ClientOptions{URL: "https://platform.test", AccessKey: testKey, UserAgent: "vbilling/test"},
		StatePath:         filepath.Join(t.TempDir(), "bindings.json"),
		Health:            h,
		Now:               func() time.Time { return now },
		OnBillingIDChange: func(was, is string) { changes = append(changes, [2]string{was, is}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := func(tenant, tenantUID, id string) discovery.PlatformBinding {
		b := discovery.PlatformBinding{Instance: "ov", InstanceUID: "uid-inst", Project: "team-a", ProjectUID: "uid-proj", Tenant: tenant, TenantUID: tenantUID}
		if id != "" {
			b.Settings = map[string]string{"tenant": id}
		}
		return b
	}
	const key = "loft-team-a-v-ov/ov"
	r.mu.Lock()
	defer r.mu.Unlock()

	r.record(key, "uid-sts", binding("co", "uid-co", "legacy-1"), now)
	if len(changes) != 0 {
		t.Fatalf("a first binding is not a change: %v", changes)
	}
	// The override on the same Tenant is edited.
	e := r.record(key, "uid-sts", binding("co", "uid-co", "legacy-2"), now.Add(time.Minute))
	if e.Binding.TenantID() != "legacy-2" || e.Binding.PreviousTenantID != "legacy-1" || !e.Binding.TenantIDChanged.Equal(now.Add(time.Minute)) {
		t.Fatalf("binding %+v, want billed as legacy-2 with legacy-1 on record", e.Binding)
	}
	if len(changes) != 1 || changes[0] != [2]string{"legacy-1", "legacy-2"} {
		t.Fatalf("reported changes %v, want legacy-1 to legacy-2", changes)
	}
	var note *health.Source
	for _, s := range h.List() {
		if s.Name == health.Name("billing-id", "legacy-2") {
			s := s
			note = &s
		}
	}
	if note == nil || !strings.Contains(note.Message, `"legacy-1"`) || !strings.Contains(note.Hint, "ingest aliases") {
		t.Fatalf("the change is not in the sources API: %+v", note)
	}
	cl := discovery.TenantCluster{Name: "ov", Namespace: "loft-team-a-v-ov", Annotations: map[string]string{}}
	cl.ApplyPlatform(e.Binding)
	if meta := cl.PlatformMetadata(); meta["previous_tenant_id"] != "legacy-1" || meta["tenant_id_changed_at"] == "" {
		t.Errorf("customer metadata %v does not carry the previous ID", meta)
	}
	// Reading the same binding again keeps the record.
	e = r.record(key, "uid-sts", binding("co", "uid-co", "legacy-2"), now.Add(2*time.Minute))
	if e.Binding.PreviousTenantID != "legacy-1" || len(changes) != 1 {
		t.Errorf("the previous ID was lost on the next read: %+v, changes %v", e.Binding, changes)
	}
	// The project handed to another Tenant is a move, not an ID change.
	e = r.record(key, "uid-sts", binding("other", "uid-other", ""), now.Add(3*time.Minute))
	if e.Binding.PreviousTenantID != "" || len(changes) != 1 {
		t.Errorf("a move to another Tenant was reported as an ID change: %+v, changes %v", e.Binding, changes)
	}
}
