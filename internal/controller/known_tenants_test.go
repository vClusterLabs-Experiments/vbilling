package controller

import (
	"context"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// A tenant vBilling knows without a cluster (bound to an ingest token,
// named by the records configuration, holding a billing state) always
// bills to itself: a cluster's alias equal to its ID never captures it, for
// billing or for the gateway. Aliases that name no known tenant still map.
func TestAliasesNeverCaptureKnownTenants(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.IngestTokens = map[string]string{"tok-1": "bound-co"} })
	h.c.UseKnownTenants(func(id string) bool { return id == "billed-co" || id == "mapped-co" })
	h.disc.clusters = []discovery.TenantCluster{
		cluster("acme", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "acme",
			discovery.AnnotationPrefix + "aliases": "bound-co,billed-co,mapped-co,acme-team"}),
	}
	h.c.Reconcile(context.Background())
	for _, id := range []string{"bound-co", "billed-co", "mapped-co"} {
		if got, known := h.c.ResolveTenant(id); got != id || !known {
			t.Errorf("ResolveTenant(%s) = %q %t, want the tenant itself", id, got, known)
		}
	}
	if got, known := h.c.ResolveTenant("acme-team"); got != "acme" || !known {
		t.Errorf("ResolveTenant(acme-team) = %q %t, want acme", got, known)
	}
	if got, known := h.c.ResolveTenant("stranger"); got != "stranger" || known {
		t.Errorf("ResolveTenant(stranger) = %q %t, want it unknown", got, known)
	}
	if ids := h.c.GatewayIdentities("acme"); len(ids) != 2 || ids[0] != "acme" || ids[1] != "acme-team" {
		t.Errorf("GatewayIdentities(acme) = %v, want [acme acme-team]", ids)
	}
	if !h.c.KnownTenant("acme") || h.c.KnownTenant("acme-team") || h.c.KnownTenant("") {
		t.Error("KnownTenant: a discovered tenant is known; an alias and the empty ID are not")
	}
}
