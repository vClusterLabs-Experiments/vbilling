package enforcement

import (
	"context"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// Inside a tenant's cluster only kube-system is exempt, and the policy and
// its binding get vBilling's spec back at every pass, whoever changed them.
func TestInClusterPolicyIsRestoredAndExemptsOnlyKubeSystem(t *testing.T) {
	host := fake.NewSimpleClientset(controlPlane("a", "vc-a", 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vc-a"}})
	inside := fake.NewSimpleClientset()
	cl := discovery.TenantCluster{Name: "a", Namespace: "vc-a", Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", Now: time.Now,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	api := inside.AdmissionregistrationV1()
	p, err := api.ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if req := p.Spec.MatchConstraints.NamespaceSelector.MatchExpressions; len(req) != 1 || req[0].Key != "kubernetes.io/metadata.name" || req[0].Values[0] != "kube-system" {
		t.Fatalf("namespace selector = %+v, want only kube-system exempt", req)
	}
	// The tenant, cluster admin inside, disarms both.
	p.Spec.Validations[0].Expression = "true"
	if _, err := api.ValidatingAdmissionPolicies().Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _ := api.ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{})
	b.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Audit}
	b.Spec.MatchResources = &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"nothing": "matches"}}}
	if _, err := api.ValidatingAdmissionPolicyBindings().Update(ctx, b, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	p, _ = api.ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	b, _ = api.ValidatingAdmissionPolicyBindings().Get(ctx, PolicyName, metav1.GetOptions{})
	if p.Spec.Validations[0].Expression != "false" || len(b.Spec.ValidationActions) != 1 || b.Spec.ValidationActions[0] != admissionv1.Deny || b.Spec.MatchResources != nil {
		t.Fatalf("after resync: policy %q, binding %+v; want vBilling's spec back", p.Spec.Validations[0].Expression, b.Spec)
	}
}

// Two tenants' clusters in one namespace: the namespace is labeled
// suspended while either is blocked, and the other's synced pods are let
// through by name (the chart's policy reads vbilling.vcluster.com/active.*).
// Neither tenant's state lifts or sets the other's.
func TestTwoTenantsShareANamespace(t *testing.T) {
	ns := "shared"
	host := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: ns}},
	)
	clusters := map[string]discovery.TenantCluster{
		"tenant-a": {Name: "a", Namespace: ns, Source: "statefulset"},
		"tenant-b": {Name: "b", Namespace: ns, Source: "statefulset"},
	}
	e := New(host, Options{Mode: "enforce", Now: time.Now,
		Clusters:  func(tenant string) []discovery.TenantCluster { return []discovery.TenantCluster{clusters[tenant]} },
		Neighbors: func(string) map[string]string { return map[string]string{"a": "tenant-a", "b": "tenant-b"} },
	})
	ctx := context.Background()
	nsMeta := func() (map[string]string, map[string]string) {
		got, err := host.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return got.Labels, got.Annotations
	}
	if err := e.Set(ctx, "tenant-a", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	l, a := nsMeta()
	if l[LabelSuspended] != "true" || l[LabelState] != "suspended" || a[AnnotationActivePrefix+"b"] != "true" || a[AnnotationActivePrefix+"a"] != "" {
		t.Fatalf("tenant-a suspended: labels %v, annotations %v", l, a)
	}
	// tenant-b's own state change keeps tenant-a suspended.
	if err := e.Set(ctx, "tenant-b", Warning, "budget alert", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	l, a = nsMeta()
	if l[LabelSuspended] != "true" || l[LabelState] != "suspended" || a[AnnotationActivePrefix+"b"] != "true" {
		t.Fatalf("tenant-b's change lifted tenant-a's suspension: labels %v, annotations %v", l, a)
	}
	if err := e.Set(ctx, "tenant-a", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	l, a = nsMeta()
	if l[LabelSuspended] != "" || l[LabelState] != "warning" || len(a) != 0 {
		t.Fatalf("nobody blocked: labels %v, annotations %v", l, a)
	}
}
