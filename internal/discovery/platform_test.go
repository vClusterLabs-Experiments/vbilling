package discovery

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// holdBinder binds "train" and holds back "fresh".
type holdBinder struct{}

func (holdBinder) Bind(_ context.Context, clusters []TenantCluster) ([]TenantCluster, error) {
	var out []TenantCluster
	for _, c := range clusters {
		switch c.Name {
		case "train":
			c.ApplyPlatform(PlatformBinding{Instance: "train-vci", InstanceDisplayName: "Training", Project: "acme-train",
				Tenant: "acme", TenantDisplayName: "Acme AI", Settings: map[string]string{"plan": "enterprise", "prometheus-url": "http://default"}})
			out = append(out, c)
		case "fresh":
		default:
			out = append(out, c)
		}
	}
	return out, nil
}

func TestPlatformBindingDecidesTenantAndSettings(t *testing.T) {
	kube := fake.NewSimpleClientset(
		nsObj("team-a", map[string]string{AnnotationPrefix + "tenant": "globex", AnnotationPrefix + "project": "other"}),
		nsObj("team-b", nil),
		nsObj("team-c", nil),
		sts("team-a", "train", 1, map[string]string{AnnotationPrefix + "plan": "free", AnnotationPrefix + "display-name": "Not Acme",
			AnnotationPrefix + "prometheus-url": "http://prom.team-a:9090"}),
		sts("team-b", "fresh", 1, nil),
		sts("team-c", "plain", 1, map[string]string{AnnotationPrefix + "tenant": "initech"}),
	)
	got, err := NewDiscoverer(kube, nil, nil, "").WithBinder(holdBinder{}).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a held-back cluster must not be metered: %+v", got)
	}
	train, plain := got[0], got[1]
	if train.Name != "train" {
		train, plain = plain, train
	}
	if id := train.TenantID(TenantPerPlatform); id != "tenant-acme" || train.Project != "acme-train" || train.Instance != "train-vci" ||
		train.DisplayName != "Training" || train.Meta("display-name") != "Acme AI" || train.Meta("plan") != "enterprise" {
		t.Fatalf("bound cluster: tenant %s project %s instance %s name %q account %q plan %q", id, train.Project, train.Instance,
			train.DisplayName, train.Meta("display-name"), train.Meta("plan"))
	}
	if train.Meta("prometheus-url") != "http://prom.team-a:9090" {
		t.Errorf("a per-cluster setting on the workload beats the account default: %q", train.Meta("prometheus-url"))
	}
	if plain.TenantID(TenantPerPlatform) != "initech" || plain.Platform != nil {
		t.Errorf("clusters the Platform does not know keep the label rules: %+v", plain)
	}
}
