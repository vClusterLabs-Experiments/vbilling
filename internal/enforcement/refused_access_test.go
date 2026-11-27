package enforcement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A cluster created while its tenant is paused ran for about two and a
// half minutes in the live validation of v0.3.2: nothing had granted
// vBilling the access a pause needs inside it, so every drain was refused,
// and vBilling retried for the whole drain timeout before the hard stop,
// which needs no access inside the cluster.

// A cluster whose API refuses vBilling what a pause needs inside it cannot
// be drained now or after any wait: its control plane is stopped in the
// same pass. With a drain timeout of a minute and a clock that does not
// move, a pass that waited would never stop it. The pause stays unverified,
// so whatever runs on its own nodes keeps billing, and the missing access
// is reported once in the log and as the source
// enforcement-access/<cluster>, with the rule that grants it.
func TestClusterWhoseAPIRefusesVBillingIsStoppedAtOnce(t *testing.T) {
	forbidden := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("RBAC: access denied"))
	}
	for _, tc := range []struct {
		name, verb, resource string
		err                  error
	}{
		{"no access to admission policies", "get", "validatingadmissionpolicies", forbidden("validatingadmissionpolicies")},
		{"may not create the pause policy", "create", "validatingadmissionpolicies", forbidden("validatingadmissionpolicies")},
		{"may not taint nodes", "update", "nodes", forbidden("nodes")},
		{"may not delete pods", "delete", "pods", forbidden("pods")},
		{"credentials no longer accepted", "get", "validatingadmissionpolicies", apierrors.NewUnauthorized("token revoked")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			cl, host, inside := privateCluster(podOn("inference", "default", "gpu-1"))
			inside.PrependReactor(tc.verb, tc.resource, func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, tc.err })
			sources := quietHealth()
			spans := pause.Memory()
			logs := captureLog(t)
			e := New(host, Options{Mode: "enforce", DrainTimeout: time.Minute, Pauses: spans, Now: newClock().Now, Health: sources,
				Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
			if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
				t.Fatal(err)
			}
			e.Wait()
			if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
				t.Fatalf("control plane replicas = %d: the cluster keeps running while vBilling waits for a drain its API refuses", r)
			}
			st := onlyStatus(t, e)
			if st.Phase != "paused" || st.Verified || st.Error != "" || !strings.Contains(st.Unverified, "its API refuses vBilling") {
				t.Fatalf("status = %+v, want paused, unverified because its API refuses vBilling", st)
			}
			if got := spans.Spans(cl.ExternalID()); len(got) != 0 {
				t.Fatalf("pause spans %+v: a cluster that was not drained must keep billing", got)
			}
			if !podNames(t, inside, "default")["inference"] {
				t.Fatal("test setup: the pod was deleted although the drain was refused")
			}
			src, ok := source(sources, "enforcement-access/"+cl.ExternalID())
			if !ok || src.Status != health.Failing || src.Kind != "enforcement-access" || src.Subject != cl.ExternalID() ||
				!strings.Contains(src.Hint, "vbilling-enforcer") || !strings.Contains(src.Hint, PausedPolicyName) || !strings.Contains(src.Hint, "delete on pods") {
				t.Fatalf("source = %+v (found %t), want it failing, naming the cluster and the rule that grants the access", src, ok)
			}
			// Guard and resync keep the cluster down without saying it again.
			e.Guard(ctx)
			e.Wait()
			e.Resync(ctx)
			e.Wait()
			if n := logs.count(cl.ExternalID(), "refuses vBilling", "vbilling-enforcer"); n != 1 {
				t.Fatalf("the missing access is logged %d time(s), want once:\n%s", n, logs)
			}
		})
	}
}

// The same refusal on a cluster that has no nodes of its own (a cluster
// that was just created has none): nothing of it can run once its control
// plane is stopped, so the pause is verified at once.
func TestRefusedClusterWithoutNodesOfItsOwnIsPausedAndVerifiedAtOnce(t *testing.T) {
	cl, host, inside := privateCluster()
	inside.PrependReactor("*", "validatingadmissionpolicies", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "validatingadmissionpolicies"}, "", errors.New("RBAC: access denied"))
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: time.Minute, Pauses: spans, Now: newClock().Now,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside),
		Freeze:   func(context.Context, discovery.TenantCluster) error { return nil },
		OwnNodes: func(discovery.TenantCluster) (bool, bool) { return false, true }})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d, want 0", r)
	}
	if _, open := spans.Current(cl.ExternalID()); !open || !onlyStatus(t, e).Verified {
		t.Fatalf("status = %+v, want the pause verified: metering bills no node of its own", e.Pauses())
	}
}

// The source clears once the cluster's API lets vBilling through again.
func TestRefusedAccessClearsWhenTheClusterLetsVBillingIn(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster()
	deny := true
	inside.PrependReactor("*", "validatingadmissionpolicies", func(ktesting.Action) (bool, runtime.Object, error) {
		if deny {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "validatingadmissionpolicies"}, "", errors.New("RBAC: access denied"))
		}
		return false, nil, nil
	})
	sources := quietHealth()
	e := New(host, Options{Mode: "enforce", Health: sources,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err == nil {
		t.Fatal("a suspension the cluster refused reported no error")
	}
	if src, ok := source(sources, "enforcement-access/"+cl.ExternalID()); !ok || src.Status != health.Failing {
		t.Fatalf("source = %+v (found %t), want it failing", src, ok)
	}
	// It stays reported while it lasts (sources nothing reports for six
	// hours are dropped).
	before, _ := source(sources, "enforcement-access/"+cl.ExternalID())
	sources.SetClock(func() time.Time { return before.LastAttempt.Add(2 * time.Minute) })
	e.Resync(ctx)
	if src, _ := source(sources, "enforcement-access/"+cl.ExternalID()); src.Status != health.Failing || !src.LastAttempt.After(before.LastAttempt) {
		t.Fatalf("source after a resync = %+v, want it reported again", src)
	}
	deny = false // the rule is granted
	e.Resync(ctx)
	if src, ok := source(sources, "enforcement-access/"+cl.ExternalID()); !ok || src.Status != health.OK {
		t.Fatalf("source = %+v, want it ok once the cluster lets vBilling in", src)
	}
	if !hasPolicy(t, inside, PolicyName) {
		t.Fatal("the suspension was not applied once the access was there")
	}
}
