// Command fixturegen writes vCluster Platform API fixtures for the platform
// package tests from the real loft-sh/api Go types: Platform 4.12 (Tenant
// bound by tenant.platform.vcluster.com/name) and 5.x (tenant.vcluster.com/owner
// and tenant.vcluster.com/exclusive-to). It lives under testdata, so the go
// tool ignores it; run it with
//
//	cd internal/platform/testdata/gen && go run . ..
//
// Platform 4.9 fixtures (no Tenant resource) were captured from a live
// Platform 4.9.0 instead (testdata/platform-4.9.0).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	mv4 "github.com/loft-sh/api/v4/pkg/apis/management/v1"
	sv4 "github.com/loft-sh/api/v4/pkg/apis/storage/v1"
	mv5 "github.com/loft-sh/api/v5/pkg/apis/management/v1"
	sv5 "github.com/loft-sh/api/v5/pkg/apis/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Version-neutral scenario.
type tenant struct {
	name, uid, display string
	ann                map[string]string
}
type project struct {
	name, uid, display string
	labels, ann        map[string]string
}
type instance struct {
	project, name, uid, display, cluster, ns, vc, phase string
	created                                             time.Time
	ignore, standalone                                  bool
	labels                                              map[string]string
}

var t0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func scenario(tenantLabel, holderLabel string) ([]tenant, []project, []instance) {
	ts := []tenant{
		{"acme", "uid-tenant-acme", "Acme AI", map[string]string{"vbilling.vcluster.com/email": "billing@acme.example", "vbilling.vcluster.com/plan": "enterprise"}},
		{"globex", "uid-tenant-globex", "Globex", nil},
	}
	ps := []project{
		{"acme-train", "uid-project-acme-train", "Acme training", map[string]string{tenantLabel: "acme"},
			map[string]string{"loft.sh/project-namespace": "p-acme-train", "vbilling.vcluster.com/plan": "free"}},
		{"acme-infer", "uid-project-acme-infer", "Acme inference", map[string]string{tenantLabel: "acme"},
			map[string]string{"loft.sh/project-namespace": "p-acme-infer"}},
		{"research", "uid-project-research", "Research", nil,
			map[string]string{"loft.sh/project-namespace": "p-research", "vbilling.vcluster.com/tenant": "research-legacy"}},
		{"orphanage", "uid-project-orphanage", "", map[string]string{tenantLabel: "initech"},
			map[string]string{"loft.sh/project-namespace": "p-orphanage"}},
	}
	if holderLabel != "" {
		ps = append(ps, project{"globex-lab", "uid-project-globex-lab", "Globex lab", map[string]string{holderLabel: "globex"},
			map[string]string{"loft.sh/project-namespace": "p-globex-lab"}})
		ps = append(ps, project{"conflict", "uid-project-conflict", "", map[string]string{tenantLabel: "acme", holderLabel: "globex"},
			map[string]string{"loft.sh/project-namespace": "p-conflict"}})
	}
	is := []instance{
		{project: "acme-train", name: "train-1", uid: "uid-vci-train-1", display: "Training 1", cluster: "cp-1", ns: "vc-train-1", vc: "train-1",
			phase: "Ready", created: t0, labels: map[string]string{"vbilling.vcluster.com/tenant": "someone-else"}},
		{project: "acme-infer", name: "infer-1", uid: "uid-vci-infer-1", cluster: "cp-1", ns: "vc-infer-1", vc: "infer-1", phase: "Sleeping", created: t0},
		{project: "research", name: "lab", uid: "uid-vci-lab", cluster: "cp-1", ns: "vc-lab", vc: "lab", phase: "Ready", created: t0},
		// A move between projects: the Platform clones the instance into the
		// new project and marks the old one until it is deleted.
		{project: "acme-train", name: "mover", uid: "uid-vci-mover-old", cluster: "cp-1", ns: "vc-mover", vc: "mover", phase: "Ready", created: t0, ignore: true},
		{project: "research", name: "mover", uid: "uid-vci-mover-new", cluster: "cp-1", ns: "vc-mover", vc: "mover", phase: "Ready", created: t0.Add(48 * time.Hour)},
		// Same namespace and name on another connected cluster.
		{project: "research", name: "elsewhere", uid: "uid-vci-elsewhere", cluster: "cp-2", ns: "vc-train-1", vc: "train-1", phase: "Ready", created: t0},
		{project: "research", name: "standalone-1", uid: "uid-vci-standalone", phase: "Ready", created: t0, standalone: true},
		{project: "orphanage", name: "ghost", uid: "uid-vci-ghost", cluster: "cp-1", ns: "vc-ghost", vc: "ghost", phase: "Ready", created: t0},
	}
	if holderLabel != "" {
		is = append(is,
			instance{project: "globex-lab", name: "gpu", uid: "uid-vci-gpu", cluster: "cp-1", ns: "vc-gpu", vc: "gpu", phase: "Ready", created: t0},
			instance{project: "conflict", name: "both", uid: "uid-vci-both", cluster: "cp-1", ns: "vc-both", vc: "both", phase: "Ready", created: t0})
	}
	return ts, ps, is
}

func meta(name, ns, uid string, labels, ann map[string]string, created time.Time) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: labels, Annotations: ann, CreationTimestamp: metav1.NewTime(created)}
}

func v412(dir string) {
	ts, ps, is := scenario(sv4.TenantLabel, "")
	tl := mv4.TenantList{TypeMeta: metav1.TypeMeta{Kind: "TenantList", APIVersion: "management.loft.sh/v1"}}
	for _, t := range ts {
		x := mv4.Tenant{TypeMeta: metav1.TypeMeta{Kind: "Tenant", APIVersion: "management.loft.sh/v1"}, ObjectMeta: meta(t.name, "", t.uid, nil, t.ann, t0)}
		x.Spec.DisplayName = t.display
		tl.Items = append(tl.Items, x)
	}
	pl := mv4.ProjectList{TypeMeta: metav1.TypeMeta{Kind: "ProjectList", APIVersion: "management.loft.sh/v1"}}
	for _, p := range ps {
		x := mv4.Project{TypeMeta: metav1.TypeMeta{Kind: "Project", APIVersion: "management.loft.sh/v1"}, ObjectMeta: meta(p.name, "", p.uid, p.labels, p.ann, t0)}
		x.Spec.DisplayName = p.display
		pl.Items = append(pl.Items, x)
	}
	il := mv4.VirtualClusterInstanceList{TypeMeta: metav1.TypeMeta{Kind: "VirtualClusterInstanceList", APIVersion: "management.loft.sh/v1"}}
	for _, i := range is {
		x := mv4.VirtualClusterInstance{TypeMeta: metav1.TypeMeta{Kind: "VirtualClusterInstance", APIVersion: "management.loft.sh/v1"},
			ObjectMeta: meta(i.name, "p-"+i.project, i.uid, i.labels, nil, i.created)}
		x.Spec.DisplayName = i.display
		x.Spec.Standalone = i.standalone
		x.Spec.ClusterRef = sv4.VirtualClusterClusterRef{ClusterRef: sv4.ClusterRef{Cluster: i.cluster, Namespace: i.ns}, VirtualCluster: i.vc}
		x.Status.Phase = sv4.InstancePhase(i.phase)
		x.Status.IgnoreReconciliation = i.ignore
		il.Items = append(il.Items, x)
	}
	write(dir, "platform-4.12.7", tl, pl, il)
}

func v5(dir string) {
	ts, ps, is := scenario(sv5.TenantLabel, sv5.TenantExclusiveHolderLabel)
	tl := mv5.TenantList{TypeMeta: metav1.TypeMeta{Kind: "TenantList", APIVersion: "management.loft.sh/v1"}}
	for _, t := range ts {
		x := mv5.Tenant{TypeMeta: metav1.TypeMeta{Kind: "Tenant", APIVersion: "management.loft.sh/v1"}, ObjectMeta: meta(t.name, "", t.uid, nil, t.ann, t0)}
		x.Spec.DisplayName = t.display
		tl.Items = append(tl.Items, x)
	}
	pl := mv5.ProjectList{TypeMeta: metav1.TypeMeta{Kind: "ProjectList", APIVersion: "management.loft.sh/v1"}}
	for _, p := range ps {
		x := mv5.Project{TypeMeta: metav1.TypeMeta{Kind: "Project", APIVersion: "management.loft.sh/v1"}, ObjectMeta: meta(p.name, "", p.uid, p.labels, p.ann, t0)}
		x.Spec.DisplayName = p.display
		pl.Items = append(pl.Items, x)
	}
	il := mv5.VirtualClusterInstanceList{TypeMeta: metav1.TypeMeta{Kind: "VirtualClusterInstanceList", APIVersion: "management.loft.sh/v1"}}
	for _, i := range is {
		x := mv5.VirtualClusterInstance{TypeMeta: metav1.TypeMeta{Kind: "VirtualClusterInstance", APIVersion: "management.loft.sh/v1"},
			ObjectMeta: meta(i.name, "p-"+i.project, i.uid, i.labels, nil, i.created)}
		x.Spec.DisplayName = i.display
		x.Spec.Standalone = i.standalone
		x.Spec.ClusterRef = sv5.VirtualClusterClusterRef{ClusterRef: sv5.ClusterRef{Cluster: i.cluster, Namespace: i.ns}, VirtualCluster: i.vc}
		x.Status.Phase = sv5.InstancePhase(i.phase)
		x.Status.IgnoreReconciliation = i.ignore
		il.Items = append(il.Items, x)
	}
	write(dir, "platform-5.0.0-rc.8", tl, pl, il)
}

func write(dir, version string, tenants, projects, instances any) {
	out := filepath.Join(dir, version)
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	disc := metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "management.loft.sh/v1",
		APIResources: []metav1.APIResource{
			{Name: "projects", Kind: "Project", Verbs: []string{"get", "list", "watch"}},
			{Name: "tenants", Kind: "Tenant", Verbs: []string{"get", "list", "watch"}},
			{Name: "virtualclusterinstances", Namespaced: true, Kind: "VirtualClusterInstance", Verbs: []string{"get", "list", "watch"}},
			{Name: "selves", Kind: "Self", Verbs: []string{"create"}},
		}}
	prefix := "p-"
	self := mv4.Self{TypeMeta: metav1.TypeMeta{Kind: "Self", APIVersion: "management.loft.sh/v1"}}
	self.Status.Subject = "vbilling"
	self.Status.ProjectNamespacePrefix = &prefix
	for name, v := range map[string]any{"tenants.json": tenants, "projects.json": projects, "virtualclusterinstances.json": instances, "discovery.json": disc, "self.json": self} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), append(b, '\n'), 0o644); err != nil {
			panic(err)
		}
	}
	fmt.Println("wrote", out)
}

func main() {
	dir := ".."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	v412(dir)
	v5(dir)
}
