package controller

import (
	"context"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

func TestReviewGatewayIdentityMustResolveToSameTenant(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{
		cluster("acme", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "acme", discovery.AnnotationPrefix + "aliases": "globex"}),
		cluster("globex", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "globex"}),
	}
	h.c.Reconcile(context.Background())
	if got, known := h.c.ResolveTenant("globex"); !known || got != "globex" {
		t.Fatalf("unexpected billing resolution: %q %v", got, known)
	}
	for _, id := range h.c.GatewayIdentities("acme") {
		if owner, _ := h.c.ResolveTenant(id); owner != "acme" {
			t.Fatalf("suspending acme would block gateway team %q, which bills to tenant %q", id, owner)
		}
	}
}

// An alias two tenants claim is never blocked for either of them.
func TestGatewayIdentitiesSkipAmbiguousAlias(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{
		cluster("acme", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "acme", discovery.AnnotationPrefix + "aliases": "shared-team,acme-team"}),
		cluster("globex", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "globex", discovery.AnnotationPrefix + "aliases": "shared-team"}),
	}
	h.c.Reconcile(context.Background())
	for _, tenant := range []string{"acme", "globex"} {
		for _, id := range h.c.GatewayIdentities(tenant) {
			if id == "shared-team" {
				t.Fatalf("GatewayIdentities(%s) includes the alias both tenants claim", tenant)
			}
		}
	}
	if got := h.c.GatewayIdentities("acme"); len(got) != 2 || got[1] != "acme-team" {
		t.Fatalf("GatewayIdentities(acme) = %v, want [acme acme-team]", got)
	}
}
