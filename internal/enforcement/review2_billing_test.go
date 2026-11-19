package enforcement

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// TestReview2FailedDrainLeavesNoUnbilledSpan: PR #9 made a failed drain
// lift the pause mark so "the cluster bills while it runs". unmarkPaused
// ends the pause span at the moment of the failure, so the span from the
// mark to the failure stays recorded, and the collector clips every kind
// of capacity and the workloads for it although the control plane was
// never stopped and the pods kept running. A drain that fails slowly (a
// pod-delete admission webhook that times out, an API that hangs) is
// retried by Guard every 15 seconds, so most of the cluster's running time
// can end up inside such spans.
func TestReview2FailedDrainLeavesNoUnbilledSpan(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("inference", "default", "private-gpu"))
	// The pod delete fails after a 30-second webhook timeout.
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		now = now.Add(30 * time.Second)
		mu.Unlock()
		return true, nil, apierrors.NewTimeoutError("admission webhook timed out", 0)
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", Now: clock, DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "tenant-a", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 1 {
		t.Fatalf("setup: the failed drain must leave the control plane running, replicas=%d", r)
	}
	if _, err := inside.CoreV1().Pods("default").Get(ctx, "inference", metav1.GetOptions{}); err != nil {
		t.Fatalf("setup: the pod must still run: %v", err)
	}
	// The cluster never paused, so none of its time may be clipped.
	var unbilled time.Duration
	for _, sp := range spans.Spans(cl.ExternalID()) {
		end := sp.To
		if sp.Open() {
			end = clock()
		}
		unbilled += end.Sub(sp.From)
	}
	if unbilled > 0 {
		t.Fatalf("a pause that never took effect left %s of pause spans %+v: the running cluster is not billed for them", unbilled, spans.Spans(cl.ExternalID()))
	}
}
