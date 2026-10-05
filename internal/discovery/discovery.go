// Package discovery finds tenant clusters running on this control plane
// cluster and the billing metadata attached to them.
package discovery

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// AnnotationPrefix namespaces vBilling's labels and annotations. Set them on
// the tenant cluster's StatefulSet/Deployment, its namespace, or its
// VirtualClusterInstance (later sources win):
//
//	vbilling.vcluster.com/tenant               billing customer ID (several clusters can share one)
//	vbilling.vcluster.com/display-name         customer name on invoices
//	vbilling.vcluster.com/email                billing email
//	vbilling.vcluster.com/project              project / cost attribution
//	vbilling.vcluster.com/tenant-class         public, enterprise, government, dev, ...
//	vbilling.vcluster.com/plan                 plan code / rate card alias
//	vbilling.vcluster.com/currency             invoice currency
//	vbilling.vcluster.com/stripe-customer-id   pin an existing Stripe customer
//	vbilling.vcluster.com/metronome-customer-id pin an existing Metronome customer
//	vbilling.vcluster.com/exclude              "true" to skip metering
const AnnotationPrefix = "vbilling.vcluster.com/"

// TenantCluster is a discovered tenant cluster.
type TenantCluster struct {
	Name      string
	Namespace string
	UID       string
	CreatedAt time.Time
	// Ready is true while the control plane has a ready replica. Sleeping or
	// still-provisioning clusters are not charged instance hours.
	Ready   bool
	Source  string // statefulset | deployment | external
	Project string
	// Instance is the vCluster Platform instance name (the
	// vcluster_platform_instance label of fleet observability metrics).
	Instance string
	// External tenant clusters run elsewhere (vCluster Standalone, other
	// clusters) and are reached only through their own API.
	External    bool
	DisplayName string
	Labels      map[string]string
	Annotations map[string]string
}

// ExternalID is the stable tenant-cluster identifier (v0.1 compatible).
func (c *TenantCluster) ExternalID() string {
	if c.External {
		return "external-" + c.Name
	}
	return fmt.Sprintf("vcluster-%s-%s", c.Namespace, c.Name)
}

// Meta returns a vBilling setting from annotations, then labels.
func (c *TenantCluster) Meta(key string) string {
	if v := c.Annotations[AnnotationPrefix+key]; v != "" {
		return v
	}
	return c.Labels[AnnotationPrefix+key]
}

// TenantSource selects how tenant clusters map to billing customers.
type TenantSource string

const (
	TenantPerCluster TenantSource = "cluster" // default: one customer per tenant cluster
	TenantPerProject TenantSource = "project" // one customer per vCluster Platform project
)

// TenantID resolves the billing customer. An explicit tenant label always wins.
func (c *TenantCluster) TenantID(src TenantSource) string {
	if v := c.Meta("tenant"); v != "" {
		return v
	}
	if src == TenantPerProject && c.Project != "" {
		return "project-" + c.Project
	}
	return c.ExternalID()
}

// Discoverer finds tenant clusters.
type Discoverer struct {
	client          kubernetes.Interface
	dynamicClient   dynamic.Interface // optional: vCluster Platform enrichment
	namespaces      []string
	platformCluster string
	external        []TenantCluster
}

// WithExternal adds tenant clusters configured outside this control plane
// cluster (see TENANT_CLUSTERS_FILE).
func (d *Discoverer) WithExternal(clusters []TenantCluster) *Discoverer {
	d.external = clusters
	return d
}

func NewDiscoverer(client kubernetes.Interface, dynamicClient dynamic.Interface, namespaces []string, platformCluster string) *Discoverer {
	return &Discoverer{client: client, dynamicClient: dynamicClient, namespaces: namespaces, platformCluster: platformCluster}
}

var virtualClusterInstanceGVR = schema.GroupVersionResource{
	Group: "management.loft.sh", Version: "v1", Resource: "virtualclusterinstances",
}

// Discover returns the tenant clusters running here. Workloads are the
// source of truth for what runs; vCluster Platform instances, when the API
// is reachable, enrich them with project and display name. An error means
// "unknown", never "no clusters": callers must not offboard on it.
func (d *Discoverer) Discover(ctx context.Context) ([]TenantCluster, error) {
	clusters, err := d.fromWorkloads(ctx)
	if err != nil {
		return nil, err
	}
	nsMeta, err := d.namespaceMeta(ctx, clusters)
	if err != nil {
		return nil, err
	}
	for i := range clusters {
		c := &clusters[i]
		if ns, ok := nsMeta[c.Namespace]; ok {
			c.Labels = merge(ns.Labels, c.Labels)
			c.Annotations = merge(ns.Annotations, c.Annotations)
		}
	}
	if d.dynamicClient != nil {
		d.enrichFromPlatform(ctx, clusters)
	}
	clusters = append(clusters, d.external...)
	out := clusters[:0]
	for _, c := range clusters {
		if c.Instance == "" {
			c.Instance = c.Name
		}
		if v := c.Meta("project"); v != "" {
			c.Project = v // an explicit annotation beats the Platform project
		}
		if v := c.Meta("display-name"); v != "" {
			c.DisplayName = v
		}
		if c.Meta("exclude") == "true" {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExternalID() < out[j].ExternalID() })
	return out, nil
}

func (d *Discoverer) scope() []string {
	if len(d.namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return d.namespaces
}

// fromWorkloads finds tenant clusters by their app=vcluster StatefulSets and
// Deployments. A failed list is an error: a partial view must never look
// like deleted clusters.
func (d *Discoverer) fromWorkloads(ctx context.Context) ([]TenantCluster, error) {
	selector := labels.Set{"app": "vcluster"}.String()
	seen := map[string]bool{}
	var out []TenantCluster
	for _, ns := range d.scope() {
		sts, err := d.client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, fmt.Errorf("list statefulsets in %q: %w", ns, err)
		}
		for _, s := range sts.Items {
			key := s.Namespace + "/" + s.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, TenantCluster{
				Name: s.Name, Namespace: s.Namespace, UID: string(s.UID), CreatedAt: s.CreationTimestamp.Time,
				Ready: s.Status.ReadyReplicas > 0, Source: "statefulset",
				Labels: copyMap(s.Labels), Annotations: copyMap(s.Annotations),
			})
		}
		deps, err := d.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, fmt.Errorf("list deployments in %q: %w", ns, err)
		}
		for _, dep := range deps.Items {
			key := dep.Namespace + "/" + dep.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, TenantCluster{
				Name: dep.Name, Namespace: dep.Namespace, UID: string(dep.UID), CreatedAt: dep.CreationTimestamp.Time,
				Ready: dep.Status.ReadyReplicas > 0, Source: "deployment",
				Labels: copyMap(dep.Labels), Annotations: copyMap(dep.Annotations),
			})
		}
	}
	return out, nil
}

func (d *Discoverer) namespaceMeta(ctx context.Context, clusters []TenantCluster) (map[string]*corev1.Namespace, error) {
	out := map[string]*corev1.Namespace{}
	for _, c := range clusters {
		if _, ok := out[c.Namespace]; ok {
			continue
		}
		ns, err := d.client.CoreV1().Namespaces().Get(ctx, c.Namespace, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get namespace %s: %w", c.Namespace, err)
		}
		out[c.Namespace] = ns
	}
	return out, nil
}

// enrichFromPlatform maps VirtualClusterInstances onto running workloads via
// spec.clusterRef.{namespace,virtualCluster}. The instance's own namespace
// is the project namespace (p-<project>). Errors are non-fatal: the
// Platform API is optional and only adds metadata.
func (d *Discoverer) enrichFromPlatform(ctx context.Context, clusters []TenantCluster) {
	list, err := d.dynamicClient.Resource(virtualClusterInstanceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return // Platform not installed or not reachable from here
	}
	byKey := map[string]int{}
	for i, c := range clusters {
		byKey[c.Namespace+"/"+c.Name] = i
	}
	enriched := 0
	for _, item := range list.Items {
		obj := item.Object
		cluster := nestedString(obj, "spec", "clusterRef", "cluster")
		if d.platformCluster != "" && cluster != "" && cluster != d.platformCluster {
			continue // runs on another control plane cluster
		}
		ns := nestedString(obj, "spec", "clusterRef", "namespace")
		name := nestedString(obj, "spec", "clusterRef", "virtualCluster")
		if name == "" {
			name = item.GetName()
		}
		i, ok := byKey[ns+"/"+name]
		if !ok {
			continue
		}
		c := &clusters[i]
		c.Instance = item.GetName()
		if p := strings.TrimPrefix(item.GetNamespace(), "p-"); p != item.GetNamespace() {
			c.Project = p
		}
		if dn := nestedString(obj, "spec", "displayName"); dn != "" {
			c.DisplayName = dn
		}
		c.Labels = merge(c.Labels, item.GetLabels())
		c.Annotations = merge(c.Annotations, item.GetAnnotations())
		enriched++
	}
	if enriched > 0 {
		log.Printf("[discovery] enriched %d tenant cluster(s) from vCluster Platform", enriched)
	}
}

func merge(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func copyMap(m map[string]string) map[string]string { return merge(nil, m) }

// nestedString extracts a string from a nested map, or "".
func nestedString(obj map[string]interface{}, fields ...string) string {
	var current interface{} = obj
	for _, field := range fields {
		m, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		if current, ok = m[field]; !ok {
			return ""
		}
	}
	s, _ := current.(string)
	return s
}
