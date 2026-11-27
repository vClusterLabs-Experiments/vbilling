package telemetry

import (
	"strings"
	"testing"
)

// A series about something that is gone can be removed: it leaves the
// exposition, and the other series of its family stay.
func TestDeleteRemovesOneSeries(t *testing.T) {
	r := New()
	gone := map[string]string{"tenant": "globex", "state": "active"}
	kept := map[string]string{"tenant": "acme", "state": "active"}
	r.Set("vbilling_tenant_billing_state", "1 for the tenant's current billing state.", gone, 1)
	r.Set("vbilling_tenant_billing_state", "1 for the tenant's current billing state.", kept, 1)
	if !r.Has("vbilling_tenant_billing_state", gone) {
		t.Fatal("a series that was set is not there")
	}
	r.Delete("vbilling_tenant_billing_state", gone)
	r.Delete("vbilling_tenant_billing_state", gone)                  // gone already
	r.Delete("vbilling_no_such_metric", map[string]string{"a": "b"}) // never there
	if r.Has("vbilling_tenant_billing_state", gone) || !r.Has("vbilling_tenant_billing_state", kept) {
		t.Fatal("Delete removed the wrong series, or none")
	}
	var out strings.Builder
	if err := r.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "globex") || !strings.Contains(out.String(), `vbilling_tenant_billing_state{state="active",tenant="acme"} 1`) {
		t.Fatalf("exposition after the delete:\n%s", out.String())
	}
}
