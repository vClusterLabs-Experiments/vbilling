package discovery

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// VCluster represents a discovered vCluster instance on the host cluster.
type VCluster struct {
	Name      string
	Namespace string
	UID       string
	CreatedAt time.Time
	// Labels from the StatefulSet for additional metadata
	Labels map[string]string
}

// ExternalID returns a stable identifier for Lago customer mapping.
func (v *VCluster) ExternalID() string {
	return fmt.Sprintf("vcluster-%s-%s", v.Namespace, v.Name)
}

// SubscriptionID returns a stable subscription identifier.
func (v *VCluster) SubscriptionID() string {
	return fmt.Sprintf("sub-vcluster-%s-%s", v.Namespace, v.Name)
}

// DisplayName returns a human-readable name for the billing customer.
func (v *VCluster) DisplayName() string {
	return fmt.Sprintf("vCluster: %s (%s)", v.Name, v.Namespace)
}

// Discoverer finds vCluster instances in a Kubernetes cluster.
type Discoverer struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface // optional, for Platform API discovery
	namespaces    []string          // empty = all namespaces
}

func NewDiscoverer(client kubernetes.Interface, dynamicClient dynamic.Interface, namespaces []string) *Discoverer {
	return &Discoverer{
		client:        client,
		dynamicClient: dynamicClient,
		namespaces:    namespaces,
	}
}

// virtualClusterInstanceGVR is the GroupVersionResource for the vCluster Platform
// management API's VirtualClusterInstance custom resource.
var virtualClusterInstanceGVR = schema.GroupVersionResource{
	Group:    "management.loft.sh",
	Version:  "v1",
	Resource: "virtualclusterinstances",
}

// Discover lists all vCluster instances across the cluster.
// It tries Platform API discovery first (if a dynamic client is available),
// then falls back to StatefulSet/Deployment scanning.
func (d *Discoverer) Discover(ctx context.Context) ([]VCluster, error) {
	// Try Platform API first if dynamic client is available
	if d.dynamicClient != nil {
		vclusters, err := d.DiscoverFromPlatform(ctx)
		if err == nil && len(vclusters) > 0 {
			log.Printf("[discovery] found %d vCluster(s) via Platform API", len(vclusters))
			return vclusters, nil
		}
		if err != nil {
			log.Printf("[discovery] Platform API not available, falling back to StatefulSet scanning: %v", err)
		} else {
			log.Printf("[discovery] Platform API returned 0 results, falling back to StatefulSet scanning")
		}
	}

	// Fall back to StatefulSet/Deployment scanning
	return d.discoverFromWorkloads(ctx)
}

// DiscoverFromPlatform uses the Kubernetes dynamic client to list
// VirtualClusterInstance resources from the management.loft.sh API group.
// This is the preferred discovery method when the vCluster Platform is installed.
// It falls back gracefully if the CRD does not exist.
func (d *Discoverer) DiscoverFromPlatform(ctx context.Context) ([]VCluster, error) {
	if d.dynamicClient == nil {
		return nil, fmt.Errorf("dynamic client not configured")
	}

	var vclusters []VCluster

	namespaces := d.namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""} // empty string = all namespaces
	}

	for _, ns := range namespaces {
		var items []map[string]interface{}

		if ns == "" {
			// Cluster-wide list
			result, err := d.dynamicClient.Resource(virtualClusterInstanceGVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list virtualclusterinstances (all namespaces): %w", err)
			}
			for _, item := range result.Items {
				items = append(items, item.Object)
			}
		} else {
			result, err := d.dynamicClient.Resource(virtualClusterInstanceGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				log.Printf("[discovery] error listing virtualclusterinstances in namespace %q: %v", ns, err)
				continue
			}
			for _, item := range result.Items {
				items = append(items, item.Object)
			}
		}

		for _, obj := range items {
			name := nestedString(obj, "metadata", "name")
			namespace := nestedString(obj, "metadata", "namespace")
			uid := nestedString(obj, "metadata", "uid")
			creationStr := nestedString(obj, "metadata", "creationTimestamp")

			createdAt := time.Time{}
			if creationStr != "" {
				if t, parseErr := time.Parse(time.RFC3339, creationStr); parseErr == nil {
					createdAt = t
				}
			}

			// Extract labels from the resource
			lbls := make(map[string]string)
			if metadata, ok := obj["metadata"].(map[string]interface{}); ok {
				if labelsRaw, ok := metadata["labels"].(map[string]interface{}); ok {
					for k, v := range labelsRaw {
						if s, ok := v.(string); ok {
							lbls[k] = s
						}
					}
				}
			}

			vclusters = append(vclusters, VCluster{
				Name:      name,
				Namespace: namespace,
				UID:       uid,
				CreatedAt: createdAt,
				Labels:    lbls,
			})
		}
	}

	return vclusters, nil
}

// discoverFromWorkloads finds vClusters by scanning for StatefulSets and
// Deployments with the app=vcluster label. This is the fallback discovery
// method when the Platform API is not available.
func (d *Discoverer) discoverFromWorkloads(ctx context.Context) ([]VCluster, error) {
	selector := labels.Set{"app": "vcluster"}.String()
	var vclusters []VCluster

	namespaces := d.namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""} // empty string = all namespaces
	}

	for _, ns := range namespaces {
		// Check StatefulSets (default vCluster deployment)
		stsList, err := d.client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			log.Printf("[discovery] error listing StatefulSets in namespace %q: %v", ns, err)
			continue
		}
		for _, sts := range stsList.Items {
			vclusters = append(vclusters, VCluster{
				Name:      sts.Name,
				Namespace: sts.Namespace,
				UID:       string(sts.UID),
				CreatedAt: sts.CreationTimestamp.Time,
				Labels:    sts.Labels,
			})
		}

		// Also check Deployments (some vCluster configurations use Deployments)
		depList, err := d.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			log.Printf("[discovery] error listing Deployments in namespace %q: %v", ns, err)
			continue
		}
		// Deduplicate: only add if not already found as StatefulSet
		seen := make(map[string]bool)
		for _, vc := range vclusters {
			seen[vc.Namespace+"/"+vc.Name] = true
		}
		for _, dep := range depList.Items {
			key := dep.Namespace + "/" + dep.Name
			if seen[key] {
				continue
			}
			vclusters = append(vclusters, VCluster{
				Name:      dep.Name,
				Namespace: dep.Namespace,
				UID:       string(dep.UID),
				CreatedAt: dep.CreationTimestamp.Time,
				Labels:    dep.Labels,
			})
		}
	}

	log.Printf("[discovery] found %d vCluster(s)", len(vclusters))
	return vclusters, nil
}

// nestedString extracts a string value from a nested map using the given path
// of keys. Returns empty string if the path does not exist or the value is not
// a string.
func nestedString(obj map[string]interface{}, fields ...string) string {
	var current interface{} = obj
	for _, field := range fields {
		m, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = m[field]
		if !ok {
			return ""
		}
	}
	s, _ := current.(string)
	return s
}
