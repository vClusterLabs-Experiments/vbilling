package enforcement

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A span an older version opened verified nothing. After the upgrade it
// ends at the first pass; the pause is then verified afresh (a cluster
// without nodes of its own), with a span that carries the mark, or keeps
// billing, and its status says an older version opened it.
func TestPauseOpenedByAnOlderVersionIsVerifiedAfresh(t *testing.T) {
	for _, tc := range []struct {
		own      bool
		verified bool
	}{{own: false, verified: true}, {own: true, verified: false}} {
		ns := "vc-o"
		host := fake.NewSimpleClientset(controlPlane("o", ns, 0, map[string]string{discovery.AnnotationPausedBy: pause.ByVBilling,
			discovery.AnnotationPausedReplicas: "1"}), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		cl := discovery.TenantCluster{Name: "o", Namespace: ns, Source: "statefulset"}
		clock := newClock()
		spans := pause.Memory()
		opened := clock.Now().Add(-6 * time.Hour)
		if _, err := spans.Begin(cl.ExternalID(), opened, pause.ByVBilling); err != nil { // as v0.3.0 left it
			t.Fatal(err)
		}
		own := tc.own
		e := New(host, Options{Mode: "enforce", Now: clock.Now, State: kvWith(t, "acme", Paused), DrainTimeout: 20 * time.Millisecond, Pauses: spans,
			Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
			Freeze:       func(context.Context, discovery.TenantCluster) error { return nil },
			OwnNodes:     func(discovery.TenantCluster) (bool, bool) { return own, true },
			TenantClient: clientFor(fake.NewSimpleClientset())})
		e.Resync(context.Background())
		e.Wait()
		got := spans.Spans(cl.ExternalID())
		if len(got) == 0 || !got[0].From.Equal(opened) || !got[0].To.Equal(clock.Now()) || got[0].Verified() {
			t.Fatalf("own nodes %t: spans %+v, want the older span ended at the upgrade, unverified", tc.own, got)
		}
		st := onlyStatus(t, e)
		if tc.verified {
			if len(got) != 2 || !got[1].Open() || !got[1].Verified() || !got[1].Drained || !got[1].From.Equal(clock.Now()) || !st.Verified {
				t.Fatalf("no own nodes: spans %+v, status %+v; want a verified span from the upgrade on", got, st)
			}
			continue
		}
		if len(got) != 1 || st.Verified || !strings.HasPrefix(st.Unverified, "an older version opened this pause") {
			t.Fatalf("own nodes: spans %+v, status %+v; want no open span and the status naming the older version", got, st)
		}
		// Later passes keep saying so.
		e.Resync(context.Background())
		e.Wait()
		if st := onlyStatus(t, e); !strings.HasPrefix(st.Unverified, "an older version opened this pause") {
			t.Fatalf("status at the next pass = %+v", st)
		}
	}
}

// An external cluster's pause an older version opened is replaced at the
// next clean pass (dropped: that pass also finds no pause admission policy,
// so nothing before it is vouched for), and resuming ends it whether or not
// it was verified.
func TestExternalPauseOpenedByAnOlderVersion(t *testing.T) {
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}})
	cl := discovery.TenantCluster{Name: "edge", External: true}
	clock := newClock()
	spans := pause.Memory()
	opened := clock.Now().Add(-time.Hour)
	if _, err := spans.Begin(cl.ExternalID(), opened, pause.ByVBilling); err != nil {
		t.Fatal(err)
	}
	state := kvWith(t, "acme", Paused)
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: clock.Now, State: state, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	ctx := context.Background()
	e.Resync(ctx)
	e.Wait()
	got := spans.Spans(cl.ExternalID())
	if len(got) != 1 || !got[0].Verified() || !got[0].Drained || !got[0].Open() || !got[0].From.Equal(clock.Now()) {
		t.Fatalf("spans after the first pass = %+v, want only a verified one, open from now", got)
	}

	// A pause an older version opened is ended on resume too.
	other := pause.Memory()
	if _, err := other.Begin(cl.ExternalID(), opened, pause.ByVBilling); err != nil {
		t.Fatal(err)
	}
	r := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: clock.Now, State: kvWith(t, "acme", Active), DrainTimeout: 20 * time.Millisecond, Pauses: other,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	r.Resync(ctx)
	r.Wait()
	if sp, open := other.Current(cl.ExternalID()); open {
		t.Fatalf("an active tenant's external cluster keeps the older pause span %+v open", sp)
	}
}
