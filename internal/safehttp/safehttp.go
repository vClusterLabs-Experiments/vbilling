// Package safehttp builds HTTP clients for endpoints a tenant can
// influence: a tenant's own cluster (directly, or through the vCluster
// Platform proxy), the Platform API, and URLs taken from cluster settings.
//
// Answers from those endpoints may redirect anywhere. Go's http.Client and
// client-go follow redirects, and client-go adds its bearer token again to
// the redirected request whatever its host, so following one could hand a
// credential to a host the tenant chose. These clients never follow a
// redirect: the 3xx answer is returned to the caller as is.
package safehttp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/client-go/rest"
)

// RefuseRedirect is an http.Client CheckRedirect that never follows a
// redirect: the client returns the 3xx response itself.
func RefuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// KubeHTTPClient returns the HTTP client client-go would build for cfg
// (TLS, credentials, user agent, timeout), except that it never follows a
// redirect. Pass it to the NewForConfigAndClient constructors.
func KubeHTTPClient(cfg *rest.Config) (*http.Client, error) {
	c := rest.CopyConfig(cfg)
	if c.UserAgent == "" {
		c.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	hc, err := rest.HTTPClientFor(c)
	if err != nil {
		return nil, err
	}
	out := *hc // HTTPClientFor may return http.DefaultClient, which must not change
	out.CheckRedirect = RefuseRedirect
	return &out, nil
}

// SameOrigin reports whether two URLs have the same scheme, host and port.
func SameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// errTooManyRedirects matches Go's default limit.
var errTooManyRedirects = errors.New("stopped after 10 redirects")

// KeepHeadersOnOrigin returns a CheckRedirect for requests that carry
// credentials in headers: redirects are followed, but once one leaves the
// origin of the first request, the headers named by headers (and
// Authorization, Cookie and Referer) are removed. Go itself only removes
// Authorization and Cookie, and keeps them for subdomains.
func KeepHeadersOnOrigin(headers func() []string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errTooManyRedirects
		}
		if SameOrigin(req.URL, via[0].URL) {
			return nil
		}
		for _, h := range headers() {
			req.Header.Del(h)
		}
		for _, h := range []string{"Authorization", "Cookie", "Referer"} {
			req.Header.Del(h)
		}
		return nil
	}
}
