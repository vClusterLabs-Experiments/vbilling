package discovery

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Every pod vCluster synced on a large control plane cluster is listed a
// page at a time (Limit and Continue), and once more in full when the
// server expires the continue token midway.
func TestListPagedReadsEveryPage(t *testing.T) {
	const total = 1234
	var all []corev1.Pod
	for i := 0; i < total; i++ {
		all = append(all, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%04d", i)}})
	}
	for _, expire := range []bool{false, true} {
		var limits []int64
		expired := false
		list := func(_ context.Context, o metav1.ListOptions) (*corev1.PodList, error) {
			limits = append(limits, o.Limit)
			from := 0
			if o.Continue != "" {
				if expire && !expired {
					expired = true
					return nil, apierrors.NewResourceExpired("continue token expired")
				}
				from, _ = strconv.Atoi(o.Continue)
			}
			if o.Limit == 0 {
				return &corev1.PodList{Items: all}, nil
			}
			to := min(from+int(o.Limit), total)
			out := &corev1.PodList{Items: all[from:to]}
			if to < total {
				out.Continue = strconv.Itoa(to)
			}
			return out, nil
		}
		got, err := ListPaged(context.Background(), metav1.ListOptions{LabelSelector: "vcluster.loft.sh/managed-by"}, list)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != total || got.Items[total-1].Name != "p1233" {
			t.Fatalf("expire %t: listed %d pods, want %d", expire, len(got.Items), total)
		}
		if limits[0] != listPageSize {
			t.Fatalf("expire %t: first request asked for %d, want pages of %d", expire, limits[0], listPageSize)
		}
		if !expire && len(limits) != 3 {
			t.Fatalf("requests %v, want 3 pages", limits)
		}
	}
}

// What ReadHostNamespaces reads: the namespaces given (one by one, or all
// at once when there are many), and where vCluster control planes run. A
// forbidden read is kept, so every namespace is then unread.
func TestReadHostNamespaces(t *testing.T) {
	ctx := context.Background()
	cp := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dev", Namespace: "team-b", Labels: map[string]string{"app": "vcluster"}}}
	synced := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "fake-cp", Namespace: "dev-x-ml",
		Labels: map[string]string{"app": "vcluster", labelManagedBy: "dev"}}}
	objs := []runtime.Object{cp, synced, nsObj("team-b", nil),
		nsObj("dev-x-ml", map[string]string{LabelSyncedName: "dev", LabelSyncedNamespace: "team-a"})}
	for i := 0; i < hostNamespaceGets+5; i++ {
		objs = append(objs, nsObj(fmt.Sprintf("extra-%d", i), nil))
	}
	kube := fake.NewSimpleClientset(objs...)
	if h, err := ReadHostNamespaces(ctx, kube, nil); err != nil || len(h.ControlPlanes()) != 0 {
		t.Fatalf("nothing to read: %v %v", h.ControlPlanes(), err)
	}
	many := []string{"dev-x-ml", "team-b", "gone"}
	for i := 0; i < hostNamespaceGets; i++ {
		many = append(many, fmt.Sprintf("extra-%d", i))
	}
	for _, read := range [][]string{{"dev-x-ml", "team-b", "gone"}, many} {
		h, err := ReadHostNamespaces(ctx, kube, read)
		if err != nil {
			t.Fatal(err)
		}
		if exists, labels, known := h.Lookup("dev-x-ml"); !known || !exists || labels[LabelSyncedNamespace] != "team-a" {
			t.Errorf("%d read: dev-x-ml %v %v %v", len(read), exists, labels, known)
		}
		if exists, _, known := h.Lookup("gone"); !known || exists {
			t.Errorf("%d read: a namespace that does not exist read as %v %v", len(read), exists, known)
		}
		if planes := h.ControlPlanes(); len(planes) != 1 || planes[0] != "team-b" {
			t.Errorf("%d read: control planes %v, want team-b's only (a synced Service never counts)", len(read), planes)
		}
	}
	kube.PrependReactor("get", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", fmt.Errorf("no"))
	})
	h, err := ReadHostNamespaces(ctx, kube, []string{"dev-x-ml"})
	if err != nil || h.Err() == nil {
		t.Fatalf("forbidden: %v %v", h.Err(), err)
	}
	if own, why := h.Whose(TenantCluster{Name: "dev", Namespace: "team-a"}, "dev-x-ml"); own != Uncertain || why != ReasonUnread {
		t.Fatalf("forbidden read: %v %q", own, why)
	}
}
