package enforcement

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// After a paused cluster was deleted in the live validation of v0.3.2,
// GET /api/v1/billing-states still listed it as paused and verified six
// minutes later.

// metered is the list of clusters metering has, as discovery last found
// them.
type metered struct {
	mu       sync.Mutex
	clusters []discovery.TenantCluster
	ready    bool
}

func (m *metered) all() ([]discovery.TenantCluster, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]discovery.TenantCluster(nil), m.clusters...), m.ready
}

func (m *metered) of(string) []discovery.TenantCluster {
	c, _ := m.all()
	return c
}

func (m *metered) set(ready bool, clusters ...discovery.TenantCluster) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clusters, m.ready = clusters, ready
}

// pausedShared pauses a cluster on shared nodes and returns everything a
// test of its deletion needs.
func pausedShared(t *testing.T) (discovery.TenantCluster, *fake.Clientset, *Enforcer, *pause.Store, *testClock, *metered) {
	t.Helper()
	ns := "vc-pz"
	cl := discovery.TenantCluster{Name: "pz", Namespace: ns, Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane("pz", ns, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	clock := newClock()
	spans := pause.Memory()
	view := &metered{}
	view.set(true, cl)
	e := New(host, Options{Mode: "enforce", Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters: view.of, AllClusters: view.all})
	if err := e.Set(context.Background(), "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if st := onlyStatus(t, e); !st.Verified || !e.hasRecord(cl.ExternalID()) {
		t.Fatalf("test setup: status %+v, want a verified pause on record", st)
	}
	return cl, host, e, spans, clock, view
}

func deleteCluster(t *testing.T, host *fake.Clientset, cl discovery.TenantCluster) {
	t.Helper()
	if err := host.AppsV1().StatefulSets(cl.Namespace).Delete(context.Background(), cl.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A paused cluster that is deleted, and that discovery no longer finds,
// loses its pause entry and its pause record at the next guard pass (every
// 15 seconds). Its pause span is left as it is: the controller closes it
// once the cluster's last window is committed.
func TestDeletedClusterLosesItsPauseEntry(t *testing.T) {
	ctx := context.Background()
	cl, host, e, spans, clock, view := pausedShared(t)
	logs := captureLog(t)

	clock.add(5 * time.Minute)
	e.Guard(ctx)
	e.Wait()
	if len(e.Pauses()) != 1 || !e.hasRecord(cl.ExternalID()) {
		t.Fatalf("a paused cluster that is still there lost its pause entry: %+v", e.Pauses())
	}

	deleteCluster(t, host, cl)
	view.set(true) // discovery, 30 seconds later at most, finds it gone
	clock.add(40 * time.Second)
	e.Guard(ctx)
	e.Wait()
	if st := e.Pauses(); len(st) != 0 {
		t.Fatalf("pauses = %+v: a cluster that was deleted is still listed", st)
	}
	if e.hasRecord(cl.ExternalID()) {
		t.Fatal("the pause record of a deleted cluster is kept")
	}
	if sp, open := spans.Current(cl.ExternalID()); !open || !sp.Verified() {
		t.Fatalf("spans = %+v: the enforcer ended the span of a deleted cluster, which is the controller's to close once its last window is committed", spans.Spans(cl.ExternalID()))
	}
	if logs.count(cl.ExternalID(), "the cluster was deleted", "no longer listed") != 1 {
		t.Fatalf("the log does not say the pause entry of the deleted cluster was dropped:\n%s", logs)
	}
	// Nothing left to do at the next pass.
	e.Guard(ctx)
	if logs.count(cl.ExternalID(), "the cluster was deleted") != 1 {
		t.Fatalf("the deleted cluster is reported again:\n%s", logs)
	}
}

// A cluster counts as gone only when it is deleted and metering no longer
// has it. Anything less keeps its pause entry and its record, which a
// restore goes by.
func TestPauseEntryIsKeptUnlessTheClusterIsDeletedAndNoLongerMetered(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		then func(host *fake.Clientset, cl discovery.TenantCluster, view *metered)
	}{
		{"nothing discovered yet (a restart)", func(_ *fake.Clientset, _ discovery.TenantCluster, view *metered) {
			view.set(false)
		}},
		{"held back or excluded from metering, but still there", func(_ *fake.Clientset, _ discovery.TenantCluster, view *metered) {
			view.set(true)
		}},
		{"deleted, but discovery fails and metering still has it", func(host *fake.Clientset, cl discovery.TenantCluster, view *metered) {
			deleteCluster(t, host, cl)
			view.set(true, cl)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cl, host, e, spans, clock, view := pausedShared(t)
			tc.then(host, cl, view)
			clock.add(10 * time.Minute)
			e.Guard(ctx)
			e.Wait()
			if len(e.Pauses()) != 1 || !e.hasRecord(cl.ExternalID()) {
				t.Fatalf("pauses = %+v, record kept %t: want the cluster's pause entry and record kept", e.Pauses(), e.hasRecord(cl.ExternalID()))
			}
			if _, open := spans.Current(cl.ExternalID()); !open {
				t.Fatalf("the pause span was closed: %+v", spans.Spans(cl.ExternalID()))
			}
		})
	}
}
