package enforcement

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// A cluster that is deleted while it is paused left its pause entry behind:
// GET /api/v1/billing-states still listed it as paused and verified, for as
// long as vBilling ran. dropGone removes what the enforcer keeps about it:
// its pause status, its pause record, and the refusal of its API, if one
// was reported.
//
// Its pause span is not the enforcer's to end. The controller closes the
// span of a deleted cluster once the cluster's last window is committed, so
// that window is still metered against the open span; a span ended here, in
// the middle of that window, would leave the rest of the window unclipped.
// The saved inventory and the stop time metering reads stay with the pause
// store for the same reason.
//
// A cluster counts as gone only when both hold: metering no longer has it
// (it is not among the clusters of the last successful discovery), and its
// control plane is no longer on the control plane cluster (no vCluster
// StatefulSet or Deployment has its ID). A cluster that is excluded from
// metering, outside the watched namespaces, or held back until vCluster
// Platform names its owner is still there, and still paused: its record is
// what a later restore goes by. And while discovery fails, metering keeps
// its last view, so nothing is dropped under a cluster it still meters.

// dropGone forgets what the enforcer keeps about clusters that were
// deleted.
func (e *Enforcer) dropGone(ctx context.Context) {
	if e.kube == nil || e.opts.AllClusters == nil {
		return
	}
	clusters, ready := e.opts.AllClusters()
	if !ready {
		return // nothing discovered yet: nothing can be gone
	}
	metered := map[string]bool{}
	for i := range clusters {
		metered[clusters[i].ExternalID()] = true
	}
	suspects := map[string]bool{}
	note := func(id string) {
		// A cluster reached only through its own API has no control plane
		// here to look for: its list comes from TENANT_CLUSTERS_FILE.
		if !metered[id] && !strings.HasPrefix(id, "external-") {
			suspects[id] = true
		}
	}
	e.wmu.Lock()
	for id := range e.status {
		note(id)
	}
	e.wmu.Unlock()
	e.rmu.Lock()
	for id := range e.records {
		note(id)
	}
	e.rmu.Unlock()
	e.omu.Lock()
	for id := range e.refused {
		note(id) // its API refused vBilling: reported for as long as it exists
	}
	e.omu.Unlock()
	if len(suspects) == 0 {
		return
	}
	there, err := e.controlPlanes(ctx)
	if err != nil {
		e.omu.Lock()
		warned := e.goneWarned
		e.goneWarned = true
		e.omu.Unlock()
		if !warned {
			log.Printf("[enforcement] cannot tell whether %d cluster(s) with a pause on record were deleted: %v", len(suspects), err)
		}
		return
	}
	e.omu.Lock()
	e.goneWarned = false
	e.omu.Unlock()
	ids := make([]string, 0, len(suspects))
	for id := range suspects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if there[id] || e.working(id) {
			continue
		}
		e.forget(id)
		e.dropRecord(id)
		e.forgetObserved(id)
		e.wmu.Lock()
		_, listed := e.status[id]
		delete(e.status, id)
		e.wmu.Unlock()
		if listed {
			log.Printf("[enforcement] %s: the cluster was deleted: its pause is no longer listed", id)
		}
	}
}

// controlPlanes returns the IDs of every vCluster control plane on the
// control plane cluster, found as discovery finds them (app=vcluster), in
// every namespace, metered or not.
func (e *Enforcer) controlPlanes(ctx context.Context) (map[string]bool, error) {
	selector := metav1.ListOptions{LabelSelector: labels.Set{"app": "vcluster"}.String()}
	out := map[string]bool{}
	sts, err := e.kube.AppsV1().StatefulSets(corev1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("list statefulsets: %w", err)
	}
	for i := range sts.Items {
		cl := discovery.TenantCluster{Name: sts.Items[i].Name, Namespace: sts.Items[i].Namespace}
		out[cl.ExternalID()] = true
	}
	deps, err := e.kube.AppsV1().Deployments(corev1.NamespaceAll).List(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	for i := range deps.Items {
		cl := discovery.TenantCluster{Name: deps.Items[i].Name, Namespace: deps.Items[i].Namespace}
		out[cl.ExternalID()] = true
	}
	return out, nil
}
