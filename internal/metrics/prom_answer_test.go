package metrics

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// An answer larger than maxPromAnswer fails the read on both transports,
// whatever its status, instead of being read into memory.
func TestPrometheusAnswerPastTheBoundFailsTheRead(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(bytes.Repeat([]byte(" "), maxPromAnswer+1))
		}))
		kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range []promTransport{
			&httpTransport{baseURL: srv.URL, httpClient: srv.Client()},
			&kubeProxyTransport{kube: kube, namespace: "monitoring", service: "prometheus:9090"},
		} {
			_, _, err := tr.get(context.Background(), "/api/v1/query", url.Values{"query": {"up"}})
			if err == nil || !strings.Contains(err.Error(), "larger than 16 MiB") {
				t.Errorf("HTTP %d via %s: err = %v, want the read to fail past 16 MiB", status, tr, err)
			}
		}
		srv.Close()
	}
}

// A Prometheus inside a cluster is reached through the cluster API's
// service proxy, with its query parameters.
func TestTenantServicePrometheusIsQueriedThroughTheServiceProxy(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"model_name":"llama"},"value":[1790000000,"42"]}]}}`))
	}))
	defer srv.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, BearerToken: "reader-token"})
	if err != nil {
		t.Fatal(err)
	}
	p := &prometheusClient{t: &kubeProxyTransport{kube: kube, namespace: "monitoring", service: "prometheus:9090", prefix: "/prom"}}
	res, err := p.Query(context.Background(), `sum(x{model_name="llama"})`, time.Unix(1790000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Value != 42 || res[0].Labels["model_name"] != "llama" {
		t.Fatalf("result = %+v", res)
	}
	if got.URL.Path != "/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/prom/api/v1/query" ||
		got.URL.Query().Get("query") != `sum(x{model_name="llama"})` || got.URL.Query().Get("time") != "1790000000" {
		t.Fatalf("request = %s", got.URL)
	}
	if got.Header.Get("Authorization") != "Bearer reader-token" {
		t.Fatalf("the cluster API's credentials were not sent: %q", got.Header.Get("Authorization"))
	}
}
