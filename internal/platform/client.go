// Package platform takes billing identity from vCluster Platform objects
// (TENANT_SOURCE=platform): which Project holds the VirtualClusterInstance
// of each cluster, and which Tenant owns that project. It also reaches
// clusters' own API servers through the Platform proxy, with the same
// access key, the way `vcluster platform connect vcluster` does.
//
// API shapes come from github.com/loft-sh/api (management.loft.sh/v1 and
// storage.loft.sh/v1) and the vcluster CLI (pkg/platform):
//
//	management API   <PLATFORM_URL>/kubernetes/management/apis/management.loft.sh/v1/...
//	cluster proxy    <PLATFORM_URL>/kubernetes/project/<project>/virtualcluster/<instance>
//
// both with the access key as bearer token. Tenants exist from Platform
// 4.12; on older versions API discovery does not list them and the project
// is the billing account.
package platform

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/vclusterlabs-experiments/vbilling/internal/safehttp"
)

// Management API resources (management.loft.sh/v1).
var (
	projectsGVR  = schema.GroupVersionResource{Group: "management.loft.sh", Version: "v1", Resource: "projects"}
	tenantsGVR   = schema.GroupVersionResource{Group: "management.loft.sh", Version: "v1", Resource: "tenants"}
	instancesGVR = schema.GroupVersionResource{Group: "management.loft.sh", Version: "v1", Resource: "virtualclusterinstances"}
	selvesGVR    = schema.GroupVersionResource{Group: "management.loft.sh", Version: "v1", Resource: "selves"}
)

// ProjectNamespaceAnnotation on a Project names the namespace that holds its
// instances (p-<project> by default).
const ProjectNamespaceAnnotation = "loft.sh/project-namespace"

// Labels that bind a resource to a Tenant, newest first: Platform 4.13 and
// later stamp tenant.vcluster.com/owner on what a tenant creates and let an
// operator assign its own resources with tenant.vcluster.com/exclusive-to;
// Platform 4.12 used tenant.platform.vcluster.com/name.
const (
	TenantOwnerLabel       = "tenant.vcluster.com/owner"
	TenantExclusiveLabel   = "tenant.vcluster.com/exclusive-to"
	TenantLegacyLabel      = "tenant.platform.vcluster.com/name"
	instancePhaseSleeping  = "Sleeping"
	legacyNamespacePrefix  = "loft-p-"
	defaultNamespacePrefix = "p-"
)

// Object is the part of a Project, Tenant or VirtualClusterInstance vBilling
// reads. Field names follow loft-sh/api.
type Object struct {
	metav1.ObjectMeta `json:"metadata"`
	Spec              struct {
		DisplayName string `json:"displayName,omitempty"`
		ClusterRef  struct {
			Cluster        string `json:"cluster,omitempty"`
			Namespace      string `json:"namespace,omitempty"`
			VirtualCluster string `json:"virtualCluster,omitempty"`
		} `json:"clusterRef"`
		Standalone bool `json:"standalone,omitempty"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase,omitempty"`
		// IgnoreReconciliation marks the old instance while the Platform
		// migrates a cluster to another project.
		IgnoreReconciliation bool `json:"ignoreReconciliation,omitempty"`
	} `json:"status"`
}

// ClientOptions configure the Platform client.
type ClientOptions struct {
	URL       string
	AccessKey string
	CAFile    string
	Insecure  bool
	Timeout   time.Duration
	UserAgent string
}

// Client talks to the Platform management API.
type Client struct {
	base *rest.Config
	mgmt dynamic.Interface
	disc discovery.DiscoveryInterface
	// watch has no overall request timeout: a watch stays open until the
	// Platform ends it (watchTimeout).
	watch dynamic.Interface
}

// NewClient validates opts and returns a client.
func NewClient(opts ClientOptions) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(opts.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("PLATFORM_URL %q must be an http(s) URL", opts.URL)
	}
	if opts.AccessKey == "" {
		return nil, fmt.Errorf("PLATFORM_ACCESS_KEY is empty")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}
	base := &rest.Config{
		Host:            u.String(),
		BearerToken:     opts.AccessKey,
		Timeout:         opts.Timeout,
		UserAgent:       opts.UserAgent,
		TLSClientConfig: rest.TLSClientConfig{CAFile: opts.CAFile, Insecure: opts.Insecure},
	}
	mc := rest.CopyConfig(base)
	mc.Host += "/kubernetes/management"
	mc.QPS, mc.Burst = 20, 40 // a refresh is five requests; reads forced by new clusters add more
	// The access key is sent with every request, so it never follows a
	// redirect to another host (clients for the proxy are built the same
	// way in internal/tenantapi).
	hc, err := safehttp.KubeHTTPClient(mc)
	if err != nil {
		return nil, err
	}
	mgmt, err := dynamic.NewForConfigAndClient(mc, hc)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfigAndClient(mc, hc)
	if err != nil {
		return nil, err
	}
	wc := rest.CopyConfig(mc)
	wc.Timeout = 0
	whc, err := safehttp.KubeHTTPClient(wc)
	if err != nil {
		return nil, err
	}
	watch, err := dynamic.NewForConfigAndClient(wc, whc)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, mgmt: mgmt, disc: disc, watch: watch}, nil
}

// resources returns the verbs of each management resource, from API
// discovery: a key that may not read a resource gets 403 whether or not the
// resource exists, so listing cannot tell.
func (c *Client) resources() (map[string][]string, error) {
	l, err := c.disc.ServerResourcesForGroupVersion(projectsGVR.GroupVersion().String())
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(l.APIResources))
	for _, r := range l.APIResources {
		out[r.Name] = r.Verbs
	}
	return out, nil
}

// servesTenants reports whether this Platform has the Tenant resource
// (4.12 and later).
func (c *Client) servesTenants() (bool, error) {
	res, err := c.resources()
	if err != nil {
		return false, err
	}
	_, ok := res[tenantsGVR.Resource]
	return ok, nil
}

// Host is the Platform URL.
func (c *Client) Host() string { return c.base.Host }

// ProxyConfig reaches a cluster's API through the Platform proxy, as
// `vcluster platform connect vcluster` does: the same URL, the same key.
func (c *Client) ProxyConfig(project, instance string) *rest.Config {
	cfg := rest.CopyConfig(c.base)
	cfg.Host = c.base.Host + "/kubernetes/project/" + url.PathEscape(project) + "/virtualcluster/" + url.PathEscape(instance)
	return cfg
}

// Self is who the access key belongs to and how project namespaces are named.
type Self struct {
	Subject         string
	NamespacePrefix string // empty when the Platform does not report it
	Scoped          bool   // the key is limited to an access key scope
}

// Self reads management.loft.sh/v1 Self (a create, like the CLI does).
func (c *Client) Self(ctx context.Context) (Self, error) {
	req := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "management.loft.sh/v1", "kind": "Self"}}
	res, err := c.mgmt.Resource(selvesGVR).Create(ctx, req, metav1.CreateOptions{})
	if err != nil {
		return Self{}, err
	}
	s := Self{}
	s.NamespacePrefix, _, _ = unstructured.NestedString(res.Object, "status", "projectNamespacePrefix")
	for _, path := range [][]string{{"status", "user", "name"}, {"status", "team", "name"}, {"status", "subject"}} {
		if v, _, _ := unstructured.NestedString(res.Object, path...); v != "" {
			s.Subject = v
			break
		}
	}
	_, s.Scoped, _ = unstructured.NestedMap(res.Object, "status", "accessKeyScope")
	return s, nil
}

// list reads every page of a management resource.
func (c *Client) list(ctx context.Context, gvr schema.GroupVersionResource) ([]Object, error) {
	out, _, err := c.listAt(ctx, gvr)
	return out, err
}

// listAt reads every page of a management resource, and the resource
// version of the list: a watch started from it misses nothing after it.
func (c *Client) listAt(ctx context.Context, gvr schema.GroupVersionResource) ([]Object, string, error) {
	var out []Object
	rv := ""
	opts := metav1.ListOptions{Limit: 500}
	for {
		l, err := c.mgmt.Resource(gvr).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if rv == "" {
			rv = l.GetResourceVersion() // the pages of one list share it
		}
		for _, it := range l.Items {
			o, err := decode(gvr, &it)
			if err != nil {
				return nil, "", err
			}
			out = append(out, o)
		}
		if l.GetContinue() == "" {
			return out, rv, nil
		}
		opts.Continue = l.GetContinue()
	}
}

func decode(gvr schema.GroupVersionResource, it *unstructured.Unstructured) (Object, error) {
	var o Object
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(it.Object, &o); err != nil {
		return o, fmt.Errorf("decode %s %s: %w", gvr.Resource, it.GetName(), err)
	}
	return o, nil
}

// watchTimeout is how long the Platform keeps one watch open before it
// ends it and vBilling starts the next one.
const watchTimeout = 5 * time.Minute

// watchFrom watches a management resource from resource version rv (all
// of it, as a new watch reports, when rv is empty).
func (c *Client) watchFrom(ctx context.Context, gvr schema.GroupVersionResource, rv string) (watch.Interface, error) {
	secs := int64(watchTimeout / time.Second)
	return c.watch.Resource(gvr).Watch(ctx, metav1.ListOptions{ResourceVersion: rv, AllowWatchBookmarks: true, TimeoutSeconds: &secs})
}
