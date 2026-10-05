package discovery

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func sts(ns, name string, ready int32, ann map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name), Labels: map[string]string{"app": "vcluster"}, Annotations: ann},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: ready},
	}
}

func nsObj(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestDiscoverMergesMetadataAndReadiness(t *testing.T) {
	kube := fake.NewSimpleClientset(
		nsObj("team-a", map[string]string{AnnotationPrefix + "tenant": "acme", AnnotationPrefix + "tenant-class": "public"}),
		nsObj("team-b", nil),
		nsObj("excluded", nil),
		sts("team-a", "train", 1, map[string]string{AnnotationPrefix + "display-name": "Acme AI"}),
		sts("team-b", "sleepy", 0, nil),
		sts("excluded", "internal", 1, map[string]string{AnnotationPrefix + "exclude": "true"}),
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "team-a", Labels: map[string]string{"app": "postgres"}}},
	)
	d := NewDiscoverer(kube, nil, nil, "")
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d clusters: %+v", len(got), got)
	}
	a, b := got[0], got[1]
	if a.ExternalID() != "vcluster-team-a-train" || !a.Ready || a.TenantID(TenantPerCluster) != "acme" || a.DisplayName != "Acme AI" || a.Meta("tenant-class") != "public" {
		t.Fatalf("team-a cluster: %+v", a)
	}
	if b.Ready || b.TenantID(TenantPerCluster) != "vcluster-team-b-sleepy" {
		t.Fatalf("sleeping cluster must be discovered but not ready: %+v", b)
	}
}

func TestDiscoverFailsLoudlyInsteadOfReturningEmpty(t *testing.T) {
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("list", "statefulsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if _, err := NewDiscoverer(kube, nil, nil, "").Discover(context.Background()); err == nil {
		t.Fatal("a failed list must be an error, never an empty (all clusters deleted) result")
	}
}

func TestPlatformEnrichment(t *testing.T) {
	kube := fake.NewSimpleClientset(nsObj("loft-research-v-train", nil), sts("loft-research-v-train", "train", 1, nil))
	vci := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "management.loft.sh/v1", "kind": "VirtualClusterInstance",
		"metadata": map[string]interface{}{"name": "train", "namespace": "p-research", "annotations": map[string]interface{}{AnnotationPrefix + "plan": "gpu-pro"}},
		"spec": map[string]interface{}{"displayName": "Research Training",
			"clusterRef": map[string]interface{}{"cluster": "syd-1", "namespace": "loft-research-v-train", "virtualCluster": "train"}},
	}}
	other := vci.DeepCopy()
	other.SetName("elsewhere")
	other.Object["spec"].(map[string]interface{})["clusterRef"] = map[string]interface{}{"cluster": "mel-1", "namespace": "loft-research-v-train", "virtualCluster": "train"}
	other.Object["spec"].(map[string]interface{})["displayName"] = "Wrong Region"

	scheme := runtime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "management.loft.sh", Version: "v1", Resource: "virtualclusterinstances"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{gvr: "VirtualClusterInstanceList"}, vci, other)

	got, err := NewDiscoverer(kube, dyn, nil, "syd-1").Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := got[0]
	if c.Project != "research" || c.DisplayName != "Research Training" || c.Meta("plan") != "gpu-pro" {
		t.Fatalf("platform enrichment: %+v", c)
	}
	if c.TenantID(TenantPerProject) != "project-research" {
		t.Fatalf("project tenant = %q", c.TenantID(TenantPerProject))
	}
}
