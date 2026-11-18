package enforcement

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

func TestReviewFailedPrivateNodeDrainMustBeRetried(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("inference", "default", "private-gpu"))
	failDelete := true
	inside.PrependReactor("delete", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		if failDelete {
			return true, nil, apierrors.NewServiceUnavailable("temporary pod-delete failure")
		}
		return false, nil, nil
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	failDelete = false
	e.Resync(ctx)
	e.Wait()
	_, err := inside.CoreV1().Pods("default").Get(ctx, "inference", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		sp, open := spans.Current(cl.ExternalID())
		t.Fatalf("private-node inference survives a recovered drain failure and resync: control-plane replicas=%d, status=%+v, pause=%+v open=%v, pod error=%v (want %v)", *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas, e.Pauses(), sp, open, err, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "inference"))
	}
}

func TestReviewTenantDaemonSetMustStopWhenPaused(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	pod := podOn("customer-renderer", "customer-workloads", "private-worker")
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "customer-renderer"}}
	pod.Spec.Containers = []corev1.Container{{Name: "renderer", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi")},
	}}}
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-worker"}}, pod)
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	e.Resync(ctx)
	e.Wait()
	if *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas != 0 {
		t.Fatal("control plane did not stop")
	}
	if _, err := inside.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("tenant's 8-core/32Gi workload was exempted as infrastructure solely because it is a DaemonSet; control plane is stopped, pod error=%v, status=%+v, unbilled spans=%+v", err, e.Pauses(), spans.Spans(cl.ExternalID()))
	}
}

// A pod still in its termination grace period at the drain timeout was told
// to stop: the pause goes on and stops the control plane.
func TestPauseProceedsWhenPodsAreStillStopping(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("slow", "default", "private-gpu"))
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil // accepted; the pod takes its grace period
	})
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if r := *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas; r != 0 {
		t.Fatalf("control plane replicas = %d, want 0 after the drain timeout", r)
	}
	for _, st := range e.Pauses() {
		if st.Error != "" {
			t.Fatalf("pause status error %q, want none for pods still stopping", st.Error)
		}
	}
}

// While the drain fails, the control plane keeps running and the status
// carries the error.
func TestFailedDrainKeepsControlPlaneUp(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "private-gpu"}}, podOn("inference", "default", "private-gpu"))
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("temporary pod-delete failure")
	})
	spans := pause.Memory()
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	if err := e.Set(ctx, "acme", Paused, "credit exhausted", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	s := sts(t, host, cl.Namespace, cl.Name)
	if *s.Spec.Replicas != 1 {
		t.Fatalf("control plane replicas = %d, want 1 while the drain fails", *s.Spec.Replicas)
	}
	st := e.Pauses()
	if len(st) != 1 || st[0].Error == "" {
		t.Fatalf("pause status = %+v, want the drain error", st)
	}
	// It still runs, so it bills: no pause mark and no open pause span.
	if s.Annotations[discovery.AnnotationPausedBy] != "" {
		t.Fatalf("pause mark %q left on a cluster that still runs", s.Annotations[discovery.AnnotationPausedBy])
	}
	if sp, open := spans.Current(cl.ExternalID()); open {
		t.Fatalf("pause span %+v still open while the cluster runs", sp)
	}
}
