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

// A cluster is paused only while its control plane is really down: no
// replicas wanted and none ready. Annotations, which whoever writes
// vcluster.yaml writes, never make it paused.
func TestPausedMeansTheControlPlaneIsDown(t *testing.T) {
	zero, one := int32(0), int32(1)
	withReplicas := func(s *appsv1.StatefulSet, r *int32) *appsv1.StatefulSet { s.Spec.Replicas = r; return s }
	kube := fake.NewSimpleClientset(
		nsObj("a", nil),
		withReplicas(sts("a", "annotated", 1, map[string]string{AnnotationPaused: "true", AnnotationPausedBy: "vbilling",
			AnnotationPausedAt: "2020-01-01T00:00:00Z"}), &one),
		withReplicas(sts("a", "stopping", 1, nil), &zero), // scaled down, its pod still ready
		withReplicas(sts("a", "down", 0, nil), &zero),
		sts("a", "unset", 0, nil), // replicas unset means one: asleep or starting, not paused
	)
	got, err := NewDiscoverer(kube, nil, nil, "").Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"annotated": false, "stopping": false, "down": true, "unset": false}
	if len(got) != len(want) {
		t.Fatalf("discovered %d clusters", len(got))
	}
	for _, c := range got {
		if c.Paused != want[c.Name] {
			t.Errorf("%s: paused %t, want %t", c.Name, c.Paused, want[c.Name])
		}
	}
}

// Objects a cluster syncs to other namespaces carry only its name, so every
// vCluster with that name counts: excluded ones, and with WATCH_NAMESPACES
// those outside it too.
func TestNamesakesAreEveryVClusterWithTheName(t *testing.T) {
	kube := fake.NewSimpleClientset(
		nsObj("team-a", nil), nsObj("team-b", nil), nsObj("team-c", nil),
		sts("team-a", "gpu", 1, nil),
		sts("team-b", "gpu", 1, map[string]string{AnnotationPrefix + "exclude": "true"}),
		sts("team-c", "web", 1, nil),
	)
	for _, scope := range [][]string{nil, {"team-a", "team-c"}} {
		got, err := NewDiscoverer(kube, nil, scope, "").Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("scope %v: discovered %+v", scope, got)
		}
		gpu, web := got[0], got[1]
		if len(gpu.Namesakes) != 1 || gpu.Namesakes[0] != "team-b" || !gpu.NameShared() ||
			whose(gpu, "gpu-x-ml") != Uncertain || whose(gpu, "team-a") != Owned || whose(gpu, "team-b") != NotOwned {
			t.Errorf("scope %v: gpu %+v, want team-b's gpu as its namesake", scope, gpu)
		}
		if web.NameShared() || whose(web, "web-x-frontend") != Owned {
			t.Errorf("scope %v: web %+v, want its name unique", scope, web)
		}
	}
	// Scoped, and the other namespaces cannot be listed: no name is unique.
	kube.PrependReactor("list", "statefulsets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "" {
			return true, nil, errors.New("forbidden")
		}
		return false, nil, nil
	})
	got, err := NewDiscoverer(kube, nil, []string{"team-c"}, "").Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].NamesakesUnknown || whose(got[0], "web-x-frontend") != Uncertain {
		t.Fatalf("namesakes unknown: %+v", got)
	}
}

// whose decides for a namespace that exists without vCluster's labels.
func whose(cl TenantCluster, namespace string) Ownership {
	var h HostNamespaces
	h.Note(namespace, true, nil)
	own, _ := h.Whose(cl, namespace)
	return own
}

// vCluster labels each namespace it syncs a cluster's objects into with the
// cluster's name and namespace: those labels decide whose the objects there
// are, whoever else has the name. A namespace another cluster runs in is
// never another's, labels or not. Without labels, what sits where a cluster
// ran is its leftover, and a name used twice, a namespace that is gone, or
// one that could not be read tells nothing.
func TestWhoseSyncedObjectsAre(t *testing.T) {
	dev := TenantCluster{Name: "dev", Namespace: "team-a", Namesakes: []string{"team-b"}}
	unique := TenantCluster{Name: "web", Namespace: "team-c"}
	var h HostNamespaces
	h.Note("dev-x-ml", true, map[string]string{LabelSyncedName: "dev", LabelSyncedNamespace: "team-a"})
	h.Note("dev-x-data", true, map[string]string{LabelSyncedName: "dev", LabelSyncedNamespace: "team-b"})
	h.Note("half", true, map[string]string{LabelSyncedName: "dev"})
	h.Note("plain", true, nil)
	h.Note("gone", false, nil)
	h.Note("orphaned", true, map[string]string{LabelSyncedName: "dev", LabelSyncedNamespace: "team-a"})
	h.NoteControlPlane("orphaned")
	h.Note("deleted-dev", true, nil)
	h.Note("reused", true, map[string]string{LabelSyncedName: "web", LabelSyncedNamespace: "team-c"})
	h.UseClusters(func(ns string) bool { return ns == "team-d" }, func(ns string) bool { return ns == "deleted-dev" || ns == "reused" })
	for _, tc := range []struct {
		cl     TenantCluster
		ns     string
		want   Ownership
		reason string
	}{
		{dev, "team-a", Owned, ""},
		{dev, "dev-x-ml", Owned, ""},
		{dev, "dev-x-data", NotOwned, ""},
		{dev, "team-b", NotOwned, ""},
		{dev, "team-d", NotOwned, ""},
		{dev, "orphaned", Uncertain, ReasonOtherCluster},
		{dev, "half", Uncertain, ReasonNameShared},
		{dev, "plain", Uncertain, ReasonNameShared},
		{unique, "plain", Owned, ""},
		{unique, "half", Owned, ""},
		{unique, "dev-x-ml", NotOwned, ""},
		{unique, "gone", Uncertain, ReasonMissing},
		{unique, "never-read", Uncertain, ReasonUnread},
		{unique, "deleted-dev", NotOwned, ""},
		{unique, "reused", Owned, ""},
		{TenantCluster{Name: "dev", External: true}, "dev-x-ml", NotOwned, ""},
	} {
		if got, why := h.Whose(tc.cl, tc.ns); got != tc.want || why != tc.reason {
			t.Errorf("%s/%s, object in %s: %v %q, want %v %q", tc.cl.Namespace, tc.cl.Name, tc.ns, got, why, tc.want, tc.reason)
		}
	}
}
