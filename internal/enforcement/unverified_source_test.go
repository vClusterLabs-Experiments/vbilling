package enforcement

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A paused cluster whose stop could not be verified is a source of its own:
// its tenant is paused, yet something of it may still run and its nodes keep
// billing. Until v0.3.3 only GET /api/v1/billing-states said so, and no
// alert could. The source stays while the pause is unverified, is reported
// again so that it is not forgotten, and goes when the pause is lifted.
func TestUnverifiedPauseIsReportedAsASource(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(podOn("stuck", "ml", "gpu-1"))
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // accepted, never gone
	})
	sources := quietHealth()
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans, Health: sources,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	name := "pause/" + cl.ExternalID()
	if _, ok := source(sources, name); ok {
		t.Fatal("a cluster that is not paused has a pause source")
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if st := onlyStatus(t, e); st.Phase != "paused" || st.Verified || st.Unverified == "" {
		t.Fatalf("test setup: status = %+v, want paused and unverified", st)
	}
	src, ok := source(sources, name)
	if !ok || src.Status != health.Failing || src.Kind != "pause" || src.Subject != cl.ExternalID() ||
		!strings.Contains(src.Message, "paused, but the stop is not verified") || !strings.Contains(src.Message, "ml/stuck") ||
		!strings.Contains(src.Hint, "keep billing") || !strings.Contains(src.Hint, "When a pause cannot be verified") {
		t.Fatalf("source = %+v (found %t), want it failing, naming the cluster, the reason and what it means", src, ok)
	}
	// It is reported again while it lasts (sources nothing reports for six
	// hours are dropped).
	sources.SetClock(func() time.Time { return src.LastAttempt.Add(2 * time.Minute) })
	e.Resync(ctx)
	e.Wait()
	if again, _ := source(sources, name); again.Status != health.Failing || !again.LastAttempt.After(src.LastAttempt) {
		t.Fatalf("source after a resync = %+v, want it reported again", again)
	}
	// The hold is lifted: the cluster runs again and nothing is left to report.
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	e.Resync(ctx)
	e.Wait()
	if src, ok := source(sources, name); ok {
		t.Fatalf("source = %+v after the pause was lifted, want none", src)
	}
}

// A verified pause is no problem and has no source.
func TestVerifiedPauseHasNoSource(t *testing.T) {
	cl, host, inside := privateCluster(podOn("trainer", "ml", "gpu-1"))
	stopsWithItsControlPlane(host, inside) // its API goes with it: nothing can run unseen
	sources := quietHealth()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 40 * time.Millisecond, Pauses: pause.Memory(), Health: sources,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if st := onlyStatus(t, e); st.Phase != "paused" || !st.Verified {
		t.Fatalf("test setup: status = %+v, want paused and verified", st)
	}
	e.Resync(context.Background())
	e.Wait()
	if src, ok := source(sources, "pause/"+cl.ExternalID()); ok {
		t.Fatalf("a verified pause is reported as %+v", src)
	}
}
