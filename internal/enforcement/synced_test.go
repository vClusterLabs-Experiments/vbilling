package enforcement

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A cluster that syncs its pods to namespaces of their own
// (sync.toHost.namespaces) is suspended and paused there too: the
// namespaces are labeled for the admission policy, the labels are lifted
// once its tenant is active, and a pause stops its pods there. Those of a
// name another vCluster also has are left alone: they may be the other's.
func TestSuspensionAndPauseReachNamespacesAClusterSyncsTo(t *testing.T) {
	ns := "vc-team-a"
	host := fake.NewSimpleClientset(
		controlPlane("gpu", ns, 1, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gpu-x-ml"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "web-x-ml"}},
		syncedPod("trainer", ns, "gpu"),
		syncedPod("trainer-2", "gpu-x-ml", "gpu"),
		syncedPod("other", "web-x-ml", "web"),
	)
	cl := discovery.TenantCluster{Name: "gpu", Namespace: ns, Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", Now: func() time.Time { return now }, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	ctx := context.Background()
	labelsOf := func(name string) (map[string]string, map[string]string) {
		n, err := host.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n.Labels, n.Annotations
	}

	if err := e.Set(ctx, "acme", Suspended, "unpaid", "stripe", "customer.subscription.updated"); err != nil {
		t.Fatal(err)
	}
	if l, a := labelsOf("gpu-x-ml"); l[LabelSuspended] != "true" || l[LabelState] != "suspended" || a[AnnotationSyncedFrom] != cl.ExternalID() {
		t.Fatalf("namespace the cluster syncs to: labels %v, annotations %v", l, a)
	}
	if l, _ := labelsOf("web-x-ml"); len(l) != 0 {
		t.Fatalf("another cluster's namespace was labeled: %v", l)
	}

	if err := e.Set(ctx, "acme", Paused, "wallet empty", "metronome", "alerts.low_remaining_credit_balance_reached"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if pods := podNames(t, host, "gpu-x-ml"); len(pods) != 0 {
		t.Fatalf("the pause left the cluster's pods in gpu-x-ml: %v", pods)
	}
	if pods := podNames(t, host, "web-x-ml"); !pods["other"] {
		t.Fatal("the pause stopped another cluster's pod")
	}

	// Active again: the labels go, although no pod is left there.
	if err := e.Set(ctx, "acme", Active, "paid", "stripe", "invoice.paid"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if l, a := labelsOf("gpu-x-ml"); l[LabelSuspended] != "" || l[LabelState] != "" || a[AnnotationSyncedFrom] != "" {
		t.Fatalf("labels left after the tenant is active: %v %v", l, a)
	}

	// The same name in another namespace: gpu-x-ml could be either's.
	cl.Namesakes = []string{"vc-team-b"}
	if _, err := host.CoreV1().Pods("gpu-x-ml").Create(ctx, syncedPod("trainer-3", "gpu-x-ml", "gpu"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "metronome", "alerts.low_remaining_credit_balance_reached"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if l, _ := labelsOf("gpu-x-ml"); l[LabelSuspended] != "" {
		t.Fatalf("a namespace whose pods may be a namesake's was labeled: %v", l)
	}
	if pods := podNames(t, host, "gpu-x-ml"); !pods["trainer-3"] {
		t.Fatal("the pause stopped a pod that may be a namesake's")
	}
}

// vCluster labels each namespace it syncs a cluster into with the
// cluster's name and namespace: a vCluster of the same name in another
// project no longer stops a suspension or a pause from reaching them, and
// the namesake's namespaces are left alone.
func TestSuspensionAndPauseReachLabeledNamespacesDespiteANamesake(t *testing.T) {
	ns := "vc-team-a"
	labeled := func(name, cpNS string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name,
			Labels: map[string]string{discovery.LabelSyncedName: "gpu", discovery.LabelSyncedNamespace: cpNS}}}
	}
	host := fake.NewSimpleClientset(
		controlPlane("gpu", ns, 1, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		labeled("gpu-x-ml", ns), labeled("gpu-x-data", "vc-team-b"),
		syncedPod("trainer", ns, "gpu"),
		syncedPod("trainer-2", "gpu-x-ml", "gpu"),
		syncedPod("theirs", "gpu-x-data", "gpu"),
	)
	cl := discovery.TenantCluster{Name: "gpu", Namespace: ns, Source: "statefulset", Namesakes: []string{"vc-team-b"}}
	e := New(host, Options{Mode: "enforce", Now: func() time.Time { return now }, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "stripe", "customer.subscription.updated"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"gpu-x-ml": "true", "gpu-x-data": ""} {
		n, err := host.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if n.Labels[LabelSuspended] != want {
			t.Errorf("namespace %s: suspended label %q, want %q (labels %v)", name, n.Labels[LabelSuspended], want, n.Labels)
		}
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "metronome", "alerts.low_remaining_credit_balance_reached"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if pods := podNames(t, host, "gpu-x-ml"); len(pods) != 0 {
		t.Fatalf("the pause left the cluster's pods in gpu-x-ml: %v", pods)
	}
	if pods := podNames(t, host, "gpu-x-data"); !pods["theirs"] {
		t.Fatal("the pause stopped a pod of the namesake's namespace")
	}
}
