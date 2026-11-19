package safehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// recorder is a server that records the Authorization header it receives.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newRecorder(t *testing.T) *recorder {
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Header.Clone())
		r.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) requests() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.seen...)
}

// A cluster's API answers with a redirect to another host: client-go
// built with KubeHTTPClient returns the redirect and never sends the
// request, with its bearer token, to that host.
func TestKubeHTTPClientNeverFollowsARedirect(t *testing.T) {
	other := newRecorder(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer api.Close()

	cfg := &rest.Config{Host: api.URL, BearerToken: "SECRET-KEY"}
	hc, err := KubeHTTPClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Fatal("KubeHTTPClient changed http.DefaultClient")
	}
	kube, err := kubernetes.NewForConfigAndClient(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	err = kube.CoreV1().RESTClient().Get().AbsPath("/api/v1/namespaces/x/services/y:80/proxy/q").Do(context.Background()).Error()
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("the redirect must come back as the answer (an error naming status 302), got %v", err)
	}
	if got := other.requests(); len(got) != 0 {
		t.Fatalf("the client followed the redirect and sent %v", got[0].Get("Authorization"))
	}
}

// A pull that is redirected to another origin keeps following, without the
// headers that carry its credentials.
func TestKeepHeadersOnOriginDropsCredentialsAcrossOrigins(t *testing.T) {
	other := newRecorder(t)
	var originHits int
	var mu sync.Mutex
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		originHits++
		mu.Unlock()
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/moved", http.StatusFound) // same origin: headers kept
			return
		}
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("a same-origin redirect lost the pull's header")
		}
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	}))
	defer origin.Close()

	c := &http.Client{CheckRedirect: KeepHeadersOnOrigin(func() []string { return []string{"X-Api-Key"} })}
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/start?token=q", nil)
	req.Header.Set("X-Api-Key", "k")
	req.Header.Set("Authorization", "Bearer b")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := other.requests()
	if len(got) != 1 {
		t.Fatalf("the cross-origin redirect was not followed: %d requests", len(got))
	}
	for _, h := range []string{"X-Api-Key", "Authorization", "Referer"} {
		if v := got[0].Get(h); v != "" {
			t.Errorf("%s reached another origin: %q", h, v)
		}
	}
	if originHits != 2 {
		t.Errorf("origin hits = %d, want 2", originHits)
	}
}
