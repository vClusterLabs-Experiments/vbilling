package metrics

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/health"
)

// A cluster on shared nodes is selected in Prometheus by its namespace and
// the namespaces it syncs objects to. When those cannot be read, its
// Prometheus usage is not billed and its sources fail (it never holds the
// other clusters' windows back); when some of its name's objects cannot be
// told apart from another cluster's, the rest is billed and its sources
// fail with why.
func TestPrometheusSelectsTheNamespacesAClusterSyncsTo(t *testing.T) {
	engine := pod("engine-0", "h100-1", inNamespace("gpu-x-ml"))
	other := pod("job", "h100-1", inNamespace("plain"))
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns, Ready: true}, Tenant: "acme"}
	run := func(t *testing.T, kube *fake.Clientset, targets ...Target) (*fakeProm, *health.Registry) {
		t.Helper()
		fp := &fakeProm{}
		srv := httptest.NewServer(fp.handler(t))
		t.Cleanup(srv.Close)
		c := NewCollector(kube, nil, Options{Region: "r1", PrometheusURL: srv.URL, GPUUtilQuery: "none", EgressQuery: "none",
			PromMetrics: []PromMetric{tokens}, Now: func() time.Time { return we.Add(2 * time.Second) }})
		h := health.New(nil)
		h.SetLogger(func(string, ...any) {})
		p, err := c.NewPromSource(PromOptions{Health: h})
		if err != nil {
			t.Fatal(err)
		}
		if len(targets) == 0 {
			targets = []Target{tg}
		}
		if _, err := p.Collect(context.Background(), targets, latest()); err != nil {
			t.Fatal(err)
		}
		return fp, h
	}
	status := func(h *health.Registry) health.Source {
		s, _ := sourceNamed(h, "prometheus/tokens_out/vcluster-team-a-gpu")
		return s
	}

	t.Run("labeled", func(t *testing.T) {
		fp, h := run(t, fake.NewSimpleClientset(engine, syncedNamespace("gpu-x-ml", "gpu", ns)))
		if q := strings.Join(fp.queries, "\n"); !strings.Contains(q, `vllm:generation_tokens_total{namespace=~"team-a|gpu-x-ml"}[660s]`) {
			t.Fatalf("queries %q, want the cluster's namespace and gpu-x-ml", fp.queries)
		}
		if s := status(h); s.Status == health.Failing {
			t.Fatalf("source %+v", s)
		}
	})
	t.Run("cannot be read", func(t *testing.T) {
		kube := fake.NewSimpleClientset(engine)
		kube.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetNamespace() == "" && a.GetResource().Resource == "pods" {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("namespaced role"))
			}
			return false, nil, nil
		})
		fp, h := run(t, kube)
		if len(fp.queries) != 0 {
			t.Fatalf("queries %q, want none until the namespaces it syncs to can be read", fp.queries)
		}
		if s := status(h); s.Status != health.Failing || !strings.Contains(s.Message, "cannot be determined") {
			t.Fatalf("source %+v, want failing", s)
		}
	})
	t.Run("name used twice", func(t *testing.T) {
		namesake := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: "team-b", Ready: true}, Tenant: "globex"}
		other.Labels[LabelManagedBy] = "gpu"
		fp, h := run(t, fake.NewSimpleClientset(engine, other, syncedNamespace("gpu-x-ml", "gpu", ns),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}}), tg, namesake)
		if q := strings.Join(fp.queries, "\n"); !strings.Contains(q, `vllm:generation_tokens_total{namespace=~"team-a|gpu-x-ml"}[660s]`) ||
			strings.Contains(q, "plain") {
			t.Fatalf("queries %q, want the cluster's namespace and gpu-x-ml, not plain", fp.queries)
		}
		if s := status(h); s.Status != health.Failing || !strings.Contains(s.Message, discovery.ReasonNameShared) {
			t.Fatalf("source %+v, want failing for the object in plain", s)
		}
	})
}
