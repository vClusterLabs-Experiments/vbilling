package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// With vcluster.yaml sync.toHost.namespaces, vCluster syncs a cluster's
// pods to namespaces of the control plane cluster of their own, labeled
// vcluster.loft.sh/managed-by with the cluster's name. A pause stops them
// there, and a suspension labels those namespaces too, when the name says
// whose they are (discovery.TenantCluster.SyncedHere), as metering bills
// them.

// AnnotationSyncedFrom marks a namespace labeled because a cluster syncs
// pods to it: its value is the cluster's ID, so the labels are lifted there
// too, even once no pod is left.
const AnnotationSyncedFrom = "vbilling.vcluster.com/synced-from"

// syncedPods lists the pods a cluster synced to the control plane cluster:
// in its namespace, and in the other namespaces its name says are its.
func syncedPods(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster) ([]corev1.Pod, error) {
	pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + cl.Name})
	if err != nil {
		return nil, fmt.Errorf("list synced pods: %w", err)
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		if p.Labels[labelManagedBy] == cl.Name && cl.SyncedHere(p.Namespace) {
			out = append(out, p)
		}
	}
	return out, nil
}

// reflectSynced labels the other namespaces a cluster's pods run in as its
// own namespace is labeled (reflect), so the admission policy blocks its
// new pods there too while its tenant is blocked, and lifts the labels
// once the tenant is active. A namespace a cluster runs in is left to that
// cluster's reflect.
func (e *Enforcer) reflectSynced(ctx context.Context, cl discovery.TenantCluster, ts TenantState) error {
	id := cl.ExternalID()
	targets := map[string]bool{}
	if !cl.NameShared() {
		pods, err := syncedPods(ctx, e.kube, cl)
		if err != nil {
			return err
		}
		for _, p := range pods {
			if p.Namespace != cl.Namespace {
				targets[p.Namespace] = true
			}
		}
	}
	marked, err := e.kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelState})
	if err != nil {
		return fmt.Errorf("list labeled namespaces: %w", err)
	}
	for _, n := range marked.Items {
		if n.Annotations[AnnotationSyncedFrom] == id {
			targets[n.Name] = true
		}
	}
	blocked := e.opts.Mode == "enforce" && ts.State.blocks()
	var errs []error
	for ns := range targets {
		if ns == cl.Namespace || e.opts.Neighbors != nil && len(e.opts.Neighbors(ns)) > 0 {
			continue
		}
		labels := map[string]any{LabelState: string(ts.State), LabelSuspended: nil} // merge patch: nil removes
		ann := map[string]any{AnnotationSyncedFrom: id}
		if blocked {
			labels[LabelSuspended] = "true"
		}
		if ts.State == Active {
			labels[LabelState], ann[AnnotationSyncedFrom] = nil, nil
		}
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels, "annotations": ann}})
		if _, err := e.kube.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("label namespace %s: %w", ns, err))
		}
	}
	return errors.Join(errs...)
}
