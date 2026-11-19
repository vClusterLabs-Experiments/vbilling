package metrics

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

type nodeHeartbeat struct {
	At       time.Time     `json:"at"`
	Interval time.Duration `json:"interval"`
}

// HeartbeatPending keeps a closed window out of the ledger while Kubernetes
// is still deciding whether a node whose Lease stopped is alive.
type HeartbeatPending struct {
	Nodes []string
	Until time.Time
}

func (e *HeartbeatPending) Error() string {
	return fmt.Sprintf("waiting for %d stale node heartbeat(s), at most until %s", len(e.Nodes), e.Until.UTC().Format(time.RFC3339))
}

// Read the node Leases in pages: a GET per healthy node would overwhelm a
// large fleet's Kubernetes API client rate limit at every window boundary.
func readNodeHeartbeats(ctx context.Context, kube kubernetes.Interface) (map[string]nodeHeartbeat, error) {
	l, err := discovery.ListPaged(ctx, metav1.ListOptions{}, kube.CoordinationV1().Leases(corev1.NamespaceNodeLease).List)
	if err != nil {
		return nil, err
	}
	out := make(map[string]nodeHeartbeat, len(l.Items))
	for _, lease := range l.Items {
		if lease.Spec.RenewTime == nil {
			continue
		}
		interval := 10 * time.Second
		if d := lease.Spec.LeaseDurationSeconds; d != nil && *d >= 4 {
			interval = time.Duration(*d) * time.Second / 4
		}
		out[lease.Name] = nodeHeartbeat{At: lease.Spec.RenewTime.Time, Interval: interval}
	}
	return out, nil
}

func heartbeatRenewals(proofs map[string]nodeHeartbeat) map[string]time.Time {
	out := make(map[string]time.Time, len(proofs))
	for node, proof := range proofs {
		out[node] = proof.At
	}
	return out
}

// settleHeartbeats holds only windows with stale Leases. A renewed Lease or
// Kubernetes' Unknown mark ends the wait. If Kubernetes itself remains slow,
// six renewal intervals (at least a minute) are the maximum wait: local
// metering then treats the node as unavailable from its last proof of life.
// This changes the read snapshot, never the Kubernetes Node.
func settleHeartbeats(nodes []corev1.Node, proofs map[string]nodeHeartbeat, windows []Window, now time.Time) *HeartbeatPending {
	if len(windows) == 0 {
		return nil
	}
	last := windows[len(windows)-1]
	var pending *HeartbeatPending
	for i := range nodes {
		proof, ok := proofs[nodes[i].Name]
		if !ok || proof.At.IsZero() {
			continue
		}
		for j := range nodes[i].Status.Conditions {
			cond := &nodes[i].Status.Conditions[j]
			if cond.Type != corev1.NodeReady || cond.Status != corev1.ConditionTrue {
				continue
			}
			at := proof.At
			if cond.LastHeartbeatTime.After(at) {
				at = cond.LastHeartbeatTime.Time
			}
			if now.Sub(at) <= 2*proof.Interval || !at.Before(last.End) {
				continue
			}
			until := at.Add(max(time.Minute, 6*proof.Interval))
			if cap := last.End.Add(last.End.Sub(last.Start)); until.After(cap) {
				until = cap
			}
			if now.Before(until) {
				if pending == nil {
					pending = &HeartbeatPending{Until: until}
				}
				pending.Nodes = append(pending.Nodes, nodes[i].Name)
				if until.Before(pending.Until) {
					pending.Until = until
				}
				continue
			}
			cond.Status = corev1.ConditionUnknown
			cond.Reason = "VBillingHeartbeatExpired"
			cond.LastHeartbeatTime = metav1.NewTime(at)
			cond.LastTransitionTime = metav1.NewTime(at)
		}
	}
	return pending
}
