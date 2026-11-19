package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"k8s.io/client-go/kubernetes/fake"
)

// A Prometheus named by a cluster's own settings may answer with a
// redirect to any host. vBilling never follows it.
func TestClusterPrometheusRedirectIsNotFollowed(t *testing.T) {
	var mu sync.Mutex
	var followed int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		followed++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer other.Close()
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer own.Close()
	c := NewCollector(fake.NewSimpleClientset(), nil, Options{GPUUtilQuery: "none", EgressQuery: "none", PromMetrics: []PromMetric{tokens}})
	p, _ := c.NewPromSource(PromOptions{})
	target := Target{Cluster: discovery.TenantCluster{Name: "x", Namespace: "vc-x",
		Annotations: map[string]string{discovery.AnnotationPrefix + "prometheus-url": own.URL}}, Tenant: "acme"}
	_, _ = p.Collect(context.Background(), []Target{target}, latest())
	mu.Lock()
	defer mu.Unlock()
	if followed > 0 {
		t.Fatalf("vBilling followed a redirect from a cluster's own Prometheus %d time(s)", followed)
	}
}
