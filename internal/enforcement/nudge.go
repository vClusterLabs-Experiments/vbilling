package enforcement

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// While an admission policy of vBilling's refuses a cluster's new pods, its
// workload controllers keep failing to create them, and each one waits
// longer before every new try: kube-controller-manager doubles the wait per
// object, from 5 milliseconds up to 1000 seconds. After six minutes of
// suspension a ReplicaSet tries again only five and a half minutes later,
// so lifting the suspension left Deployments down for minutes (a Job's wait
// is capped at a minute, and a control plane that restarts starts afresh).
//
// A controller looks at an object again as soon as the object changes. So
// when vBilling lifts one of its policies from a cluster, it writes one
// annotation on every ReplicaSet, StatefulSet, DaemonSet and Job that has
// fewer pods than it wants: metadata only, never the pod template, so
// nothing is rolled out or restarted.

// AnnotationUnblockedAt is the annotation vBilling writes on a workload
// controller's object that was waiting for pods when a suspension or pause
// was lifted, with the time: the change makes the controller try again at
// once.
const AnnotationUnblockedAt = "vbilling.vcluster.com/unblocked-at"

const (
	// nudgeTimeout bounds one nudge of a cluster.
	nudgeTimeout = 30 * time.Second
	// nudgeMax is the most objects one nudge patches; the rest come back
	// when their controllers' own waits end.
	nudgeMax = 500
)

// nudgeAccess is what the nudge needs inside a cluster, beyond what
// suspending and pausing need.
const nudgeAccess = "list and patch on replicasets, statefulsets and daemonsets (apps) and on jobs (batch)"

// nudge makes a cluster's workload controllers create their missing pods
// now instead of when their waits end. It is best effort: an error is
// logged once per cluster and changes nothing else.
func (e *Enforcer) nudge(ctx context.Context, kube kubernetes.Interface, cl discovery.TenantCluster) {
	ctx, cancel := context.WithTimeout(ctx, nudgeTimeout)
	defer cancel()
	id := cl.ExternalID()
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		AnnotationUnblockedAt: e.opts.Now().UTC().Format(time.RFC3339),
	}}})
	opts := metav1.PatchOptions{}
	done, skipped := 0, 0
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// touch patches one object, unless the limit is reached or a patch of
	// its kind was refused already (the others of that kind would be too).
	touch := func(refused *bool, patchIt func() error) {
		if *refused || done >= nudgeMax {
			skipped++
			return
		}
		switch err := patchIt(); {
		case err == nil:
			done++
		case apierrors.IsNotFound(err): // deleted meanwhile
		default:
			fail(err)
			*refused = true
			skipped++
		}
	}

	if list, err := kube.AppsV1().ReplicaSets(corev1.NamespaceAll).List(ctx, metav1.ListOptions{}); err != nil {
		fail(fmt.Errorf("list replicasets: %w", err))
	} else {
		refused := false
		for i := range list.Items {
			o := &list.Items[i]
			if o.DeletionTimestamp == nil && o.Status.ReadyReplicas < wanted(o.Spec.Replicas) {
				touch(&refused, func() error {
					_, err := kube.AppsV1().ReplicaSets(o.Namespace).Patch(ctx, o.Name, types.MergePatchType, patch, opts)
					return err
				})
			}
		}
	}
	if list, err := kube.AppsV1().StatefulSets(corev1.NamespaceAll).List(ctx, metav1.ListOptions{}); err != nil {
		fail(fmt.Errorf("list statefulsets: %w", err))
	} else {
		refused := false
		for i := range list.Items {
			o := &list.Items[i]
			if o.DeletionTimestamp == nil && o.Status.ReadyReplicas < wanted(o.Spec.Replicas) {
				touch(&refused, func() error {
					_, err := kube.AppsV1().StatefulSets(o.Namespace).Patch(ctx, o.Name, types.MergePatchType, patch, opts)
					return err
				})
			}
		}
	}
	if list, err := kube.AppsV1().DaemonSets(corev1.NamespaceAll).List(ctx, metav1.ListOptions{}); err != nil {
		fail(fmt.Errorf("list daemonsets: %w", err))
	} else {
		refused := false
		for i := range list.Items {
			o := &list.Items[i]
			if o.DeletionTimestamp == nil && o.Status.NumberReady < o.Status.DesiredNumberScheduled {
				touch(&refused, func() error {
					_, err := kube.AppsV1().DaemonSets(o.Namespace).Patch(ctx, o.Name, types.MergePatchType, patch, opts)
					return err
				})
			}
		}
	}
	if list, err := kube.BatchV1().Jobs(corev1.NamespaceAll).List(ctx, metav1.ListOptions{}); err != nil {
		fail(fmt.Errorf("list jobs: %w", err))
	} else {
		refused := false
		for i := range list.Items {
			o := &list.Items[i]
			if o.DeletionTimestamp == nil && jobWaits(o) {
				touch(&refused, func() error {
					_, err := kube.BatchV1().Jobs(o.Namespace).Patch(ctx, o.Name, types.MergePatchType, patch, opts)
					return err
				})
			}
		}
	}

	e.omu.Lock()
	warned := e.nudgeWarned[id]
	if firstErr != nil {
		e.nudgeWarned[id] = true
	} else {
		delete(e.nudgeWarned, id)
	}
	e.omu.Unlock()
	if done > 0 {
		log.Printf("[enforcement] %s: asked the controllers of %d workload(s) that wait for pods to create them now", id, done)
	}
	switch {
	case firstErr != nil && !warned:
		log.Printf("[enforcement] %s: could not ask every workload controller to create its pods now (%v): the others retry by themselves, which can take up to about 16 minutes. "+
			"Grant vBilling %s inside the cluster", id, firstErr, nudgeAccess)
	case firstErr == nil && skipped > 0:
		log.Printf("[enforcement] %s: %d more workload(s) wait for pods; their controllers retry by themselves", id, skipped)
	}
}

// wanted is a replica count as its controller reads it: one when unset.
func wanted(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// jobWaits reports whether a Job that still runs has fewer pods than it
// wants.
func jobWaits(j *batchv1.Job) bool {
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		return false
	}
	for _, c := range j.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return false
		}
	}
	want := wanted(j.Spec.Parallelism)
	switch {
	case j.Spec.Completions != nil:
		want = min(want, *j.Spec.Completions-j.Status.Succeeded)
	case j.Status.Succeeded > 0:
		want = 0 // a work queue Job that has a success starts no more pods
	}
	return j.Status.Active < want
}
