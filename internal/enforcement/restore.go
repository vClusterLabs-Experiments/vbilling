package enforcement

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// A pause lowered to a suspension (the billing hold is paid while an abuse
// hold at suspended stays, or an operator lowers the hold): "suspended"
// means no new pods while what runs keeps running, but a paused cluster
// runs nothing, and the suspension refuses every pod its workload
// controllers create again. The cluster would be up and empty.
//
// So vBilling first restores the cluster as a resume does, and holds the
// suspension back (the admission policy inside the cluster, and the
// suspended label of its namespaces on the control plane cluster) until the
// workloads its pause stopped are back, or defaultRestoreTimeout has passed
// since the cluster's API answered again. Only then does it apply the
// suspension. While it is held back the cluster takes any pod, so the wait
// is kept short: it ends as soon as every controller whose pods the pause
// deleted has as many pods again.
//
// What a pause stopped, and a restore that is under way, are kept in the
// billing state store (DATA_DIR/billing-state.json), never on the cluster's
// objects: whoever writes vcluster.yaml writes those, and an annotation that
// held a suspension back would be a way around it. For the same reason a
// restore begins only for a pause that is on record there, once per pause,
// and its time, once it runs, is never set again. A restart continues the
// restore where it was, and never applies the suspension earlier.

// defaultRestoreTimeout bounds the wait for a cluster's workloads to come
// back before a suspension is applied to it, from the moment its API
// answers again. In the live run of v0.3.2 the Deployments of a resumed
// cluster were ready again 34 seconds after the resume, control plane start
// included; a pod only has to exist again, not to be ready, so two minutes
// are several times what a restore takes, and leave room for a slower
// control plane and for StatefulSets that start their pods one after the
// other. It is also the bound of every wait of a pause (the drain timeout).
const defaultRestoreTimeout = 2 * time.Minute

// annOwnerReferences is the annotation vCluster's syncer puts on a pod it
// syncs to the control plane cluster: the owner references of the pod
// inside the cluster, as JSON.
const annOwnerReferences = "vcluster.loft.sh/owner-references"

// pauseRecord is what vBilling remembers, across restarts, about a cluster
// it paused or started to pause.
type pauseRecord struct {
	Tenant string `json:"tenant"`
	// Stopped counts, per controller, the pods the pause deleted: "in/<UID>"
	// for the pods on the cluster's own nodes, "host/<UID>" for the pods it
	// had synced to the control plane cluster. Pods nothing creates again
	// (bare pods, static pods) are not counted.
	Stopped map[string]int `json:"stopped,omitempty"`
	// Untracked marks a restore of a pause this record did not follow (an
	// older version made it): nothing says what it stopped, so the restore
	// takes its full time.
	Untracked bool `json:"untracked,omitempty"`
	// RestoreSince is set while the cluster is restored before a suspension
	// is applied to it; RestoreLifted, once it could start pods again;
	// RestoreUntil is when the suspension is applied at the latest.
	RestoreSince  time.Time `json:"restore_since,omitzero"`
	RestoreLifted time.Time `json:"restore_lifted,omitzero"`
	RestoreUntil  time.Time `json:"restore_until,omitzero"`
}

const recordPrefix = "pause/"

// loadRecords reads the pause records the store holds.
func (e *Enforcer) loadRecords() {
	for id, raw := range e.opts.State.WithPrefix(recordPrefix) {
		var rec pauseRecord
		if json.Unmarshal([]byte(raw), &rec) == nil {
			e.records[id] = rec
		}
	}
}

// record returns a copy of a cluster's pause record.
func (e *Enforcer) record(id string) (pauseRecord, bool) {
	e.rmu.Lock()
	defer e.rmu.Unlock()
	rec, ok := e.records[id]
	if rec.Stopped != nil {
		stopped := make(map[string]int, len(rec.Stopped))
		for k, v := range rec.Stopped {
			stopped[k] = v
		}
		rec.Stopped = stopped
	}
	return rec, ok
}

// hasRecord reports whether vBilling paused a cluster, or started to, and
// has not finished lifting that pause.
func (e *Enforcer) hasRecord(id string) bool {
	_, ok := e.record(id)
	return ok
}

// changeRecord applies change to a cluster's record and stores it. create
// says whether a missing record is created.
func (e *Enforcer) changeRecord(id string, create bool, change func(*pauseRecord) bool) {
	e.rmu.Lock()
	defer e.rmu.Unlock()
	rec, ok := e.records[id]
	if !ok && !create {
		return
	}
	if !change(&rec) && ok {
		return
	}
	b, err := json.Marshal(rec)
	if err == nil {
		err = e.opts.State.Set(recordPrefix+id, string(b))
	}
	if err != nil {
		// Kept in memory all the same: only a restart loses it.
		log.Printf("[enforcement] %s: store pause record: %v", id, err)
	}
	e.records[id] = rec
}

// dropRecord forgets a cluster's pause record: its pause is lifted for
// good, or the cluster is gone.
func (e *Enforcer) dropRecord(id string) {
	e.rmu.Lock()
	defer e.rmu.Unlock()
	if _, ok := e.records[id]; !ok {
		return
	}
	delete(e.records, id)
	if err := e.opts.State.Delete(recordPrefix + id); err != nil {
		log.Printf("[enforcement] %s: drop pause record: %v", id, err)
	}
}

// notePausing records that vBilling pauses a cluster, before the first step
// that changes anything. A pause that begins while the cluster is being
// restored ends that restore; what the earlier pause stopped stays
// recorded, so the next restore still waits for it. fromStart says the pass
// sees the pause begin: a record first made for a pause already under way
// (one an older version made) does not know what it stopped.
func (e *Enforcer) notePausing(id, tenant string, fromStart bool) {
	e.changeRecord(id, true, func(rec *pauseRecord) bool {
		changed := rec.Tenant != tenant || !rec.RestoreSince.IsZero()
		if rec.Tenant == "" && !fromStart {
			rec.Untracked = true
		}
		rec.Tenant = tenant
		rec.RestoreSince, rec.RestoreLifted, rec.RestoreUntil = time.Time{}, time.Time{}, time.Time{}
		return changed
	})
}

// noteStopped records the pods a pause is about to delete, by the
// controller that creates them again. synced says they are the pods the
// cluster synced to the control plane cluster. A later pass of the same
// pause finds fewer pods, so the largest count per controller is kept.
func (e *Enforcer) noteStopped(id string, pods []corev1.Pod, synced bool) {
	counts := stoppedBy(pods, synced)
	if len(counts) == 0 {
		return
	}
	e.changeRecord(id, false, func(rec *pauseRecord) bool {
		changed := false
		for key, n := range counts {
			if n > rec.Stopped[key] {
				if rec.Stopped == nil {
					rec.Stopped = map[string]int{}
				}
				rec.Stopped[key] = n
				changed = true
			}
		}
		return changed
	})
}

// stoppedBy counts pods by the controller that creates them again.
func stoppedBy(pods []corev1.Pod, synced bool) map[string]int {
	counts := map[string]int{}
	// vCluster's syncer annotates a synced pod with its owners. A syncer
	// that does not leaves no way to tell a controller's pods from bare
	// ones: all of them are then expected back, as one group.
	annotated := false
	for i := range pods {
		if _, ok := pods[i].Annotations[annOwnerReferences]; ok {
			annotated = true
		}
	}
	for i := range pods {
		p := &pods[i]
		key, ok := controllerKey(p, synced)
		switch {
		case ok:
			counts[key]++
		case synced && !annotated:
			counts[hostKey+"*"]++
		}
	}
	return counts
}

const (
	ownKey  = "in/"
	hostKey = "host/"
)

// controllerKey names the controller that creates a pod again when it is
// deleted (a ReplicaSet, StatefulSet, DaemonSet or Job), or reports that
// nothing does: a bare pod stays gone, and a static pod's mirror pod is the
// kubelet's.
func controllerKey(p *corev1.Pod, synced bool) (string, bool) {
	refs, prefix := p.OwnerReferences, ownKey
	if synced {
		prefix = hostKey
		refs = nil
		if raw, ok := p.Annotations[annOwnerReferences]; ok {
			if json.Unmarshal([]byte(raw), &refs) != nil {
				return "", false
			}
		}
	}
	for _, ref := range refs {
		if ref.Controller == nil || !*ref.Controller || ref.Kind == "Node" {
			continue
		}
		if ref.UID != "" {
			return prefix + string(ref.UID), true
		}
		return prefix + ref.Kind + "/" + p.Namespace + "/" + ref.Name, true
	}
	return "", false
}

// beginRestore starts holding a suspension back for a cluster whose pause
// vBilling lifts while its tenant stays suspended. It does so only for a
// cluster with a pause on record, and once per pause: a restore that is
// under way is not started again, so nothing can stretch it. Until the
// cluster can start pods again (restoreLifted), the latest time is far
// enough to cover its control plane's start.
func (e *Enforcer) beginRestore(id, tenant string) {
	now := e.opts.Now().UTC()
	e.changeRecord(id, false, func(rec *pauseRecord) bool {
		if !rec.RestoreSince.IsZero() {
			return false // already under way: a restart, or a second pass
		}
		rec.Tenant = tenant
		rec.RestoreSince = now
		rec.RestoreLifted = time.Time{}
		rec.RestoreUntil = now.Add(e.resumeTimeout + e.restoreTimeout)
		return true
	})
}

// restoreLifted records that a cluster being restored can start pods again
// (its API answers, so the policies that refused them can be lifted): the
// suspension is applied restoreTimeout from now at the latest.
func (e *Enforcer) restoreLifted(id string) {
	now := e.opts.Now().UTC()
	e.changeRecord(id, false, func(rec *pauseRecord) bool {
		if rec.RestoreSince.IsZero() || !rec.RestoreLifted.IsZero() {
			return false
		}
		rec.RestoreLifted = now
		if until := now.Add(e.restoreTimeout); until.Before(rec.RestoreUntil) {
			rec.RestoreUntil = until
		}
		return true
	})
}

// restoring reports whether a suspension is held back for a cluster.
func (e *Enforcer) restoring(id string) bool {
	rec, ok := e.record(id)
	return ok && !rec.RestoreSince.IsZero() && e.opts.Now().Before(rec.RestoreUntil)
}

// restoreUntil returns when the suspension of a cluster being restored is
// applied at the latest (zero when none is held back).
func (e *Enforcer) restoreUntil(id string) time.Time {
	if rec, ok := e.record(id); ok && !rec.RestoreSince.IsZero() && e.opts.Now().Before(rec.RestoreUntil) {
		return rec.RestoreUntil
	}
	return time.Time{}
}

// enforced reports whether a cluster's own state blocks its new pods now:
// suspended or paused in enforce mode, unless its suspension is held back
// while it is restored. A pause is never held back.
func (e *Enforcer) enforced(id string, s State) bool {
	if e.opts.Mode != "enforce" || !s.blocks() {
		return false
	}
	return s == Paused || !e.restoring(id)
}

// restored counts the controllers whose pods a cluster's pause deleted
// (want) and those that have as many pods again (back). A pod counts as
// soon as it exists: the suspension only refuses pods that are created.
func (e *Enforcer) restored(ctx context.Context, cl discovery.TenantCluster) (back, want int, err error) {
	rec, _ := e.record(cl.ExternalID())
	want = len(rec.Stopped)
	if rec.Untracked {
		return 0, 1, nil // unknown: only the time limit ends the restore
	}
	if want == 0 {
		return 0, 0, nil
	}
	var inside, synced bool
	for key := range rec.Stopped {
		if len(key) >= len(hostKey) && key[:len(hostKey)] == hostKey {
			synced = true
		} else {
			inside = true
		}
	}
	counts := map[string]int{}
	if inside && e.opts.TenantClient != nil {
		kube, err := e.opts.TenantClient(ctx, cl)
		if err != nil {
			return 0, want, err
		}
		if kube != nil {
			pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				return 0, want, fmt.Errorf("list pods: %w", err)
			}
			for key, n := range stoppedBy(living(pods.Items), false) {
				counts[key] += n
			}
		}
	}
	if synced && !cl.External && e.kube != nil {
		pods, err := e.syncedPods(ctx, cl)
		if err != nil {
			return 0, want, fmt.Errorf("list pods the cluster synced: %w", err)
		}
		for key, n := range stoppedBy(living(pods), true) {
			counts[key] += n
		}
	}
	for key, n := range rec.Stopped {
		if counts[key] >= n {
			back++
		}
	}
	return back, want, nil
}

// living returns the pods that are not being deleted.
func living(pods []corev1.Pod) []corev1.Pod {
	out := pods[:0:0]
	for i := range pods {
		if pods[i].DeletionTimestamp == nil {
			out = append(out, pods[i])
		}
	}
	return out
}

// awaitRestore holds a cluster's suspension back while the workloads its
// pause stopped come back, then applies it. It returns early, applying
// nothing, when the tenant's state changes: paused again (the worker pauses
// the cluster), or no longer blocked (there is nothing left to apply).
func (e *Enforcer) awaitRestore(ctx context.Context, tenant string, cl discovery.TenantCluster) error {
	id := cl.ExternalID()
	log.Printf("[enforcement] %s: its pause is lifted and its tenant stays suspended: what ran before the pause is restored first; new pods are blocked again once it is back, at %s at the latest",
		id, e.restoreUntil(id).Format(time.RFC3339))
	tick := time.NewTicker(e.restoreEvery)
	defer tick.Stop()
	for {
		ts := e.StateOf(tenant)
		switch {
		case ts.State == Paused:
			return nil
		case !ts.State.blocks():
			e.dropRecord(id)
			log.Printf("[enforcement] %s: workloads can run again", id)
			return nil
		}
		rec, _ := e.record(id)
		back, want, err := e.restored(ctx, cl)
		if done := err == nil && back >= want; done || !e.restoring(id) {
			return e.closeRestore(ctx, cl, ts, back, want, done, rec.Untracked)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("restoring before the suspension applies: %w (resync continues)", ctx.Err())
		case <-tick.C:
		}
	}
}

// closeRestore applies the suspension that was held back for a cluster.
func (e *Enforcer) closeRestore(ctx context.Context, cl discovery.TenantCluster, ts TenantState, back, want int, done, untracked bool) error {
	id := cl.ExternalID()
	e.dropRecord(id)
	var errs []error
	if err := e.reflect(ctx, cl, ts, false); err != nil {
		errs = append(errs, err)
	}
	if e.opts.TenantClient != nil {
		if err := e.reflectInside(ctx, cl, ts); err != nil {
			errs = append(errs, fmt.Errorf("inside the cluster: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("apply the suspension after the restore (resync retries): %v", errs)
	}
	what := "what ran before the pause is back"
	switch {
	case untracked:
		what = fmt.Sprintf("an older version paused it without recording what it stopped, so it had the full %s to come back", e.restoreTimeout)
	case want == 0:
		what = "its pause had stopped nothing that comes back"
	case !done:
		what = fmt.Sprintf("%d of the %d workload(s) its pause stopped were back when the %s to restore them ended", back, want, e.restoreTimeout)
	}
	log.Printf("[enforcement] %s: suspended (%s): new pods are blocked again; %s", id, ts.Reason, what)
	return nil
}

// finishRestores continues, after a restart, the restores no worker is
// waiting on: the worker applies the suspension when the workloads are back
// or the time is up, never earlier.
func (e *Enforcer) finishRestores() {
	if e.opts.Mode != "enforce" || e.kube == nil || e.opts.Clusters == nil {
		return
	}
	e.rmu.Lock()
	pending := map[string]string{}
	for id, rec := range e.records {
		if !rec.RestoreSince.IsZero() {
			pending[id] = rec.Tenant
		}
	}
	e.rmu.Unlock()
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if e.working(id) {
			continue
		}
		tenant := pending[id]
		if e.StateOf(tenant).State == Paused {
			continue // Guard pauses it
		}
		for _, cl := range e.opts.Clusters(tenant) {
			if cl.ExternalID() == id {
				e.kick(tenant, cl)
			}
		}
	}
}
