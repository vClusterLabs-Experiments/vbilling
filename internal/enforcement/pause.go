package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// annPaused marks a pause made with `vcluster pause`. vBilling never sets
// it (see discovery.AnnotationPaused) and never lifts a pause it did not make.
const annPaused = discovery.AnnotationPaused

// TaintPaused is the NoSchedule taint vBilling puts on a paused cluster's
// own nodes: it keeps workloads off them until resume. vBilling stops the
// workloads itself; node infrastructure (GPU driver, device plugin, CNI)
// keeps running, so the cluster serves again soon after it resumes.
const TaintPaused = "vbilling.vcluster.com/paused"

// labelManagedBy marks the pods a vCluster synced to the control plane
// cluster (what `vcluster pause` deletes).
const labelManagedBy = "vcluster.loft.sh/managed-by"

var pausedTaint = corev1.Taint{Key: TaintPaused, Value: "true", Effect: corev1.TaintEffectNoSchedule}

// maxGrace caps the termination grace period, in seconds, of every pod a
// pause stops: no workload can stretch the drain past it. Whatever is left
// at the drain timeout is deleted with no grace period at all.
const maxGrace int64 = 30

// pauseOutcome is how far a pause got.
type pauseOutcome struct {
	// verified: vBilling confirmed nothing of the cluster runs any more,
	// and its pause span is open: capacity stops billing.
	verified bool
	// why it is not verified: the cluster's own nodes and whatever still
	// runs keep billing.
	why string
}

// pauseCluster pauses one cluster, in this order:
//
//  1. mark it paused (vbilling.vcluster.com/paused-by, -at, -replicas);
//  2. inside the cluster, block new pods and drain its own nodes;
//  3. save its inventory, to meter it from while its API is down;
//  4. scale its control plane to zero and stop the pods it synced to the
//     control plane cluster, as `vcluster pause` does;
//  5. verify that nothing of it runs any more: its own nodes were drained
//     and stayed empty while its API stopped, its synced pods are gone, and
//     its control plane is down.
//
// Billing stops only on that evidence: the pause span, which stops billing
// capacity, opens when step 5 succeeds. A pause that cannot be verified
// (no access to the cluster's API, pods that survive the drain or come
// back) still stops the control plane, a hard stop the tenant cannot undo
// and Guard keeps down, but opens no span: the cluster's own nodes and
// whatever survived keep billing, and the pause status says why.
//
// A drain the cluster's API refuses, or that cannot reach the API, keeps
// the control plane up and is retried within 15 seconds: once stopped, it
// could no longer stop the pods on the cluster's own nodes. After the
// drain timeout of failures it is stopped anyway, unverified.
//
// Every step is idempotent: a cluster resumed by anything else is paused
// again at the next pass.
func (e *Enforcer) pauseCluster(ctx context.Context, cl discovery.TenantCluster, ts TenantState) (pauseOutcome, error) {
	id := cl.ExternalID()
	if cl.External {
		return e.pauseExternal(ctx, cl, ts)
	}
	w, err := e.workload(ctx, cl)
	if err != nil {
		return pauseOutcome{}, err
	}
	ours := w.ann[discovery.AnnotationPausedBy] == pause.ByVBilling
	if w.replicas == 0 {
		e.sawStopped(id)
		if !ours {
			// Paused by someone else (vcluster pause, an operator): keep it
			// down. vBilling verified nothing of it, so it bills as a pause
			// it only observed: everything but its control plane.
			if _, err := e.stopSyncedPods(ctx, cl, false); err != nil {
				return pauseOutcome{}, err
			}
			return pauseOutcome{why: "paused outside vBilling: only its control plane stops billing"}, nil
		}
		return e.keepPaused(ctx, cl, w)
	}

	// It runs: a new pause, or a paused cluster something brought back
	// (scaled up by hand, woken by vCluster Platform). It bills from the
	// last time vBilling saw it stopped.
	e.endSpanAt(id, e.lastStopped(id))
	if !ours {
		if err := e.mark(ctx, cl, w.replicas); err != nil {
			return pauseOutcome{}, fmt.Errorf("mark paused: %w", err)
		}
	}
	own, err := e.drainOwn(ctx, cl, ts)
	if err != nil {
		if e.keepTrying(id) {
			// It still runs, so it bills; without the mark it also looks
			// like what it is, and Guard marks it again at the retry.
			if uerr := e.unmark(ctx, cl); uerr != nil {
				log.Printf("[enforcement] %s: lift pause mark after failed drain: %v", id, uerr)
			}
			return pauseOutcome{}, fmt.Errorf("drain its own nodes (retried within 15 seconds; the cluster keeps running and billing, and its control plane is stopped anyway after %s of failures): %w", e.drainTimeout(), err)
		}
		own = ownCheck{noAccess: true, why: fmt.Sprintf("its own nodes could not be drained for %s (%v)", e.drainTimeout(), err)}
	}
	e.clearFailing(id)
	if e.opts.Freeze != nil {
		if err := e.opts.Freeze(ctx, cl); err != nil {
			log.Printf("[enforcement] %s: save inventory before pause: %v", id, err)
		}
	}
	if own.noAccess {
		// Undrained: fine only if it has no nodes of its own that are billed
		// (Freeze read its nodes just now).
		why := own.why
		if own = e.ownWithoutAccess(cl); !own.ok && why != "" {
			own.why = why
		}
	}
	if err := e.patchWorkload(ctx, cl, map[string]any{"spec": map[string]any{"replicas": 0}}); err != nil {
		return pauseOutcome{}, fmt.Errorf("scale control plane down: %w", err)
	}
	if e.opts.OnPause != nil {
		e.opts.OnPause(id, true)
	}
	e.opts.Telemetry.Add("vbilling_clusters_paused_total", "Clusters vBilling paused.", nil, 1)
	synced, err := e.stopSyncedPods(ctx, cl, true)
	if err != nil {
		e.setOwnChecked(id, own)
		return pauseOutcome{why: fmt.Sprintf("its synced pods could not be stopped (%v)", err)}, nil
	}
	down := false
	if own.ok {
		var left []corev1.Pod
		left, down, err = e.confirmStopped(ctx, cl, own)
		switch {
		case err != nil:
			own = ownCheck{why: fmt.Sprintf("could not confirm its control plane stopped (%v)", err)}
		case len(left) > 0:
			own = ownCheck{why: stillThere(left, "started on its own nodes while its control plane stopped")}
		}
	}
	e.setOwnChecked(id, own)
	out := e.verdict(id, own, synced, down)
	if out.verified {
		log.Printf("[enforcement] %s: paused (%s), verified: capacity stops billing", id, ts.Reason)
	} else {
		log.Printf("[enforcement] %s: paused (%s), not verified: it keeps billing, %s", id, ts.Reason, out.why)
	}
	return out, nil
}

// keepPaused keeps a cluster vBilling stopped down, and finishes verifying
// its pause if an earlier pass could not (synced pods still stopping, a
// restart in between).
//
// A span an older version opened (v0.3.0, at its mark, before its drain and
// whatever the drain left running) verified nothing: it ends now, and the
// pause is verified afresh, or keeps billing.
func (e *Enforcer) keepPaused(ctx context.Context, cl discovery.TenantCluster, w workloadState) (pauseOutcome, error) {
	id := cl.ExternalID()
	left, err := e.stopSyncedPods(ctx, cl, false)
	if err != nil {
		return pauseOutcome{}, err
	}
	if sp, open := e.currentSpan(id); open {
		if sp.Verified() {
			return pauseOutcome{verified: true}, nil
		}
		if sp.Unproven() {
			log.Printf("[enforcement] %s: its pause span was opened by an older version without verifying the drain: it stops billing nothing from now", id)
			e.endSpanAt(id, e.opts.Now().UTC())
			e.noteOlder(id)
		}
	}
	own, ok := e.ownChecked(id)
	if !ok {
		own = e.ownSinceRestart(ctx, cl)
	}
	out := e.verdict(id, own, left, w.down())
	if !out.verified && e.openedByOlder(id) {
		out.why = olderPause + ": " + out.why
	}
	return out, nil
}

// olderPause explains the status of a pause an older version opened.
const olderPause = "an older version opened this pause"

// verdict opens the pause span when every check passed.
func (e *Enforcer) verdict(id string, own ownCheck, synced []corev1.Pod, down bool) pauseOutcome {
	switch {
	case !own.ok:
		return pauseOutcome{why: own.why}
	case len(synced) > 0:
		return pauseOutcome{why: stillThere(synced, "synced to the control plane cluster")}
	case !down:
		return pauseOutcome{why: "its control plane has not stopped yet"}
	}
	e.openSpan(id)
	e.sawStopped(id)
	return pauseOutcome{verified: true}
}

// ownCheck is what a pause established about the cluster's own nodes.
type ownCheck struct {
	// ok: no workload runs on the cluster's own nodes, or vBilling bills
	// none of them.
	ok  bool
	why string // why not
	// noAccess: vBilling cannot look inside (decided after Freeze).
	noAccess bool
	// list returns the workload pods on the cluster's own nodes, to check
	// again while its control plane stops; nil when it has none.
	list func(context.Context) ([]corev1.Pod, error)
}

// drainOwn blocks new pods inside the cluster and drains its own nodes. An
// error means the drain failed (its API refused a call or could not be
// reached) and may succeed if retried.
func (e *Enforcer) drainOwn(ctx context.Context, cl discovery.TenantCluster, ts TenantState) (ownCheck, error) {
	if e.opts.TenantClient == nil {
		return ownCheck{noAccess: true}, nil
	}
	kube, err := e.opts.TenantClient(ctx, cl)
	if err != nil {
		return ownCheck{}, fmt.Errorf("reach the cluster's API: %w", err)
	}
	if kube == nil {
		// No credentials for this cluster: metering cannot read inside it
		// either, so nothing on its own nodes is billed.
		return ownCheck{ok: true}, nil
	}
	if err := e.policyInside(ctx, kube, ts); err != nil {
		log.Printf("[enforcement] %s: admission policy before pause: %v", cl.ExternalID(), err)
	}
	d, err := e.drain(ctx, kube, false)
	if err != nil {
		return ownCheck{}, err
	}
	if len(d.left) > 0 {
		return ownCheck{why: stillThere(d.left, "still on its own nodes after the drain")}, nil
	}
	return ownCheck{ok: true, list: d.list}, nil
}

// ownWithoutAccess decides about the own nodes of a cluster vBilling could
// not drain (no write access to its API, or a drain that kept failing):
// the pause is verified only if metering bills none of them.
func (e *Enforcer) ownWithoutAccess(cl discovery.TenantCluster) ownCheck {
	if e.opts.Freeze == nil && e.opts.TenantClient == nil {
		return ownCheck{ok: true} // metering never reads clusters' own APIs: nothing on their own nodes is billed
	}
	if e.opts.OwnNodes != nil {
		if has, known := e.opts.OwnNodes(cl); known && !has {
			return ownCheck{ok: true}
		}
	}
	return ownCheck{why: "vBilling cannot drain its own nodes: it has no write access to the cluster's API (ENFORCEMENT_IN_CLUSTER is off)"}
}

// ownSinceRestart decides about the own nodes of a cluster whose pause pass
// ran before vBilling restarted: its API is down now, so they count as
// checked only if nothing on them is billed.
func (e *Enforcer) ownSinceRestart(ctx context.Context, cl discovery.TenantCluster) ownCheck {
	if e.opts.TenantClient != nil {
		if kube, err := e.opts.TenantClient(ctx, cl); err == nil && kube == nil {
			return ownCheck{ok: true}
		}
	}
	o := e.ownWithoutAccess(cl)
	if !o.ok && e.opts.TenantClient != nil {
		o.why = "its own nodes were not verified empty before its control plane stopped (vBilling restarted since)"
	}
	return o
}

// confirmStopped waits for a stopped control plane to go down. While the
// cluster's API still answers, it checks no workload came back on the
// cluster's own nodes: a pod started in the last moments of the API would
// run on with nothing left to stop it.
func (e *Enforcer) confirmStopped(ctx context.Context, cl discovery.TenantCluster, own ownCheck) ([]corev1.Pod, bool, error) {
	deadline := time.Now().Add(e.drainTimeout())
	every := min(time.Second, e.drainTimeout()/10+time.Millisecond)
	answering := own.list != nil
	for {
		w, err := e.workload(ctx, cl)
		if err != nil {
			return nil, false, err
		}
		if answering {
			pods, err := own.list(ctx)
			switch {
			case err != nil:
				answering = false // its API went away with the control plane
			case len(pods) > 0:
				return pods, w.down(), nil
			}
		}
		if w.down() && !answering || !time.Now().Before(deadline) {
			return nil, w.down(), nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(every):
		}
	}
}

// pauseExternal pauses a cluster vBilling reaches only through its own API
// (vCluster Standalone, clusters elsewhere): its control plane is not
// vBilling's to stop, so the pause is the in-cluster policy plus drained
// nodes, applied again at every pass. The pause span opens only once a
// pass confirmed nothing runs on its nodes; a pass that finds workloads
// again (a cluster admin inside can undo the policy and the taint) ends it
// at the last pass that found none, and a failed pass drops what it cannot
// vouch for.
func (e *Enforcer) pauseExternal(ctx context.Context, cl discovery.TenantCluster, ts TenantState) (pauseOutcome, error) {
	id := cl.ExternalID()
	fail := func(why string, err error) (pauseOutcome, error) {
		e.endSpanAt(id, e.lastVerified(id))
		return pauseOutcome{why: why}, err
	}
	if e.opts.TenantClient == nil {
		return fail("", errors.New("pausing a cluster outside the control plane cluster needs ENFORCEMENT_IN_CLUSTER"))
	}
	kube, err := e.opts.TenantClient(ctx, cl)
	if err == nil && kube == nil {
		err = errors.New("no credentials for the cluster's API")
	}
	if err != nil {
		return fail("", fmt.Errorf("reach the cluster's API: %w", err))
	}
	if err := e.policyInside(ctx, kube, ts); err != nil {
		log.Printf("[enforcement] %s: admission policy: %v", id, err)
	}
	d, err := e.drain(ctx, kube, true)
	if err != nil {
		return fail("", fmt.Errorf("drain its nodes: %w", err))
	}
	if len(d.left) > 0 {
		return fail(stillThere(d.left, "still on its nodes after the drain"), nil)
	}
	if !d.clean {
		// Something ran since the last pass: nothing after that pass stays
		// clipped.
		e.endSpanAt(id, e.lastVerified(id))
	}
	e.openSpan(id)
	e.noteVerified(id)
	return pauseOutcome{verified: true}, nil
}

// openSpan records that a cluster's pause is verified from now: its
// capacity stops billing.
func (e *Enforcer) openSpan(id string) {
	if e.opts.Pauses == nil {
		return
	}
	now := e.opts.Now().UTC()
	if sp, open := e.opts.Pauses.Current(id); open && !sp.Verified() {
		// Seen paused before vBilling verified it: billed as such until now.
		if _, err := e.opts.Pauses.End(id, now); err != nil {
			log.Printf("[enforcement] %s: end observed pause: %v", id, err)
		}
	}
	if _, err := e.opts.Pauses.BeginVerified(id, now); err != nil {
		log.Printf("[enforcement] %s: record pause: %v", id, err)
	}
}

// endSpanAt ends a cluster's open pause span at the given time, never
// later than now, or drops it when that is not after its start (a zero
// time drops it): no time vBilling cannot vouch for stays unbilled.
func (e *Enforcer) endSpanAt(id string, at time.Time) {
	if e.opts.Pauses == nil {
		return
	}
	sp, open := e.opts.Pauses.Current(id)
	if !open {
		return
	}
	if now := e.opts.Now().UTC(); at.After(now) {
		at = now
	}
	var err error
	if at.After(sp.From) {
		_, err = e.opts.Pauses.End(id, at)
	} else {
		_, err = e.opts.Pauses.Discard(id)
	}
	if err != nil {
		log.Printf("[enforcement] %s: end pause span: %v", id, err)
	}
}

func (e *Enforcer) currentSpan(id string) (pause.Span, bool) {
	if e.opts.Pauses == nil {
		return pause.Span{}, false
	}
	return e.opts.Pauses.Current(id)
}

// mark records on the control plane that vBilling paused it, and the
// replica count to resume it with. The mark says who paused the cluster;
// it never decides billing (see discovery.TenantCluster.Paused).
func (e *Enforcer) mark(ctx context.Context, cl discovery.TenantCluster, replicas int) error {
	return e.patchWorkload(ctx, cl, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		discovery.AnnotationPausedBy:       pause.ByVBilling,
		discovery.AnnotationPausedAt:       e.opts.Now().UTC().Format(time.RFC3339Nano),
		discovery.AnnotationPausedReplicas: strconv.Itoa(max(replicas, 1)),
	}}})
}

// unmark lifts vBilling's pause mark from a cluster that still runs.
func (e *Enforcer) unmark(ctx context.Context, cl discovery.TenantCluster) error {
	return e.patchWorkload(ctx, cl, map[string]any{"metadata": map[string]any{"annotations": map[string]any{
		discovery.AnnotationPausedBy: nil, discovery.AnnotationPausedAt: nil, discovery.AnnotationPausedReplicas: nil,
	}}})
}

// Per-cluster pause bookkeeping, in memory: a restart forgets it, and the
// next pass then decides from what it can still see.

// keepTrying records a failed drain and reports whether to keep the
// control plane up and retry: for the drain timeout after the first
// failure of a streak.
func (e *Enforcer) keepTrying(id string) bool {
	now := e.opts.Now()
	e.wmu.Lock()
	defer e.wmu.Unlock()
	first, ok := e.failing[id]
	if !ok {
		e.failing[id] = now
		return true
	}
	return now.Sub(first) < e.drainTimeout()
}

func (e *Enforcer) clearFailing(id string) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	delete(e.failing, id)
}

func (e *Enforcer) setOwnChecked(id string, own ownCheck) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.checked[id] = ownCheck{ok: own.ok, why: own.why}
}

func (e *Enforcer) ownChecked(id string) (ownCheck, bool) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	o, ok := e.checked[id]
	return o, ok
}

// sawStopped records that a cluster's control plane was stopped now: a
// cluster found running later bills from its last sighting.
func (e *Enforcer) sawStopped(id string) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.stopped[id] = e.opts.Now().UTC()
}

// lastStopped returns when vBilling last saw a cluster stopped, or the zero
// time if it has not since it started (Guard looks every 15 seconds).
func (e *Enforcer) lastStopped(id string) time.Time {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.stopped[id]
}

func (e *Enforcer) noteVerified(id string) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.verifiedAt[id] = e.opts.Now().UTC()
}

// lastVerified returns when a pass last confirmed nothing runs in an
// external cluster, or the zero time.
func (e *Enforcer) lastVerified(id string) time.Time {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.verifiedAt[id]
}

// noteOlder records that a cluster's current pause was opened by an older
// version, so its status says so.
func (e *Enforcer) noteOlder(id string) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.older[id] = true
}

func (e *Enforcer) openedByOlder(id string) bool {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	return e.older[id]
}

// forget drops a cluster's pause bookkeeping once it runs again.
func (e *Enforcer) forget(id string) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	delete(e.failing, id)
	delete(e.checked, id)
	delete(e.stopped, id)
	delete(e.verifiedAt, id)
	delete(e.older, id)
}

// resumeCluster resumes a cluster vBilling paused. Clusters paused by
// anyone else are left alone. It reports whether it scaled a control plane
// back up; the rest (lifting the taint, the admission policy) follows once
// that control plane answers.
func (e *Enforcer) resumeCluster(ctx context.Context, cl discovery.TenantCluster, ts TenantState) (bool, error) {
	id := cl.ExternalID()
	if cl.External {
		if sp, open := e.currentSpan(id); open && sp.By != pause.ByExternal {
			if _, err := e.opts.Pauses.End(id, e.opts.Now().UTC()); err != nil {
				log.Printf("[enforcement] %s: record resume: %v", id, err)
			}
		}
		e.forget(id)
		return false, nil // reflectInside lifts the taint
	}
	w, err := e.workload(ctx, cl)
	if err != nil {
		return false, err
	}
	if w.ann[discovery.AnnotationPausedBy] != pause.ByVBilling {
		return false, nil
	}
	replicas, err := strconv.Atoi(w.ann[discovery.AnnotationPausedReplicas])
	if err != nil || replicas < 1 {
		replicas = 1
	}
	if w.replicas > 0 {
		replicas = w.replicas // already scaled back up by someone else: keep that
	}
	now := e.opts.Now().UTC()
	if err := e.patchWorkload(ctx, cl, map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			discovery.AnnotationPausedBy: nil, discovery.AnnotationPausedAt: nil, discovery.AnnotationPausedReplicas: nil,
		}},
		"spec": map[string]any{"replicas": replicas},
	}); err != nil {
		return false, fmt.Errorf("scale control plane up: %w", err)
	}
	if e.opts.Pauses != nil {
		if _, err := e.opts.Pauses.End(id, now); err != nil {
			log.Printf("[enforcement] %s: record resume: %v", id, err)
		}
	}
	if e.opts.OnPause != nil {
		e.opts.OnPause(id, false)
	}
	e.forget(id)
	log.Printf("[enforcement] %s: resumed (%d control plane replica(s))", id, replicas)
	e.opts.Telemetry.Add("vbilling_clusters_resumed_total", "Clusters vBilling resumed.", nil, 1)
	return true, nil
}

// finishResume waits for a resumed cluster's API, then lifts the pause
// taint from its own nodes and sets the admission policy for its tenant's
// current state. Resync does the same every few minutes if this gives up.
func (e *Enforcer) finishResume(ctx context.Context, tenant string, cl discovery.TenantCluster) error {
	ctx, cancel := context.WithTimeout(ctx, e.resumeTimeout)
	defer cancel()
	tick := time.NewTicker(e.resumeEvery)
	defer tick.Stop()
	cl.Paused = false
	for {
		ts := e.StateOf(tenant)
		if ts.State == Paused {
			return nil // paused again meanwhile: the worker loops
		}
		err := e.reflectInside(ctx, cl, ts)
		if err == nil {
			log.Printf("[enforcement] %s: workloads can run again", cl.ExternalID())
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("resumed control plane not reachable yet (%v); resync retries", err)
		case <-tick.C:
		}
	}
}

// PauseStatus is the progress of pausing or resuming one cluster.
type PauseStatus struct {
	Cluster string `json:"cluster"`
	Tenant  string `json:"tenant"`
	Phase   string `json:"phase"` // pausing | paused | resuming | running
	// Verified is true once vBilling confirmed nothing of the paused
	// cluster runs any more: its capacity stops billing from then.
	Verified bool `json:"verified,omitempty"`
	// Unverified says why a paused cluster could not be confirmed stopped:
	// its own nodes and anything still running keep billing.
	Unverified string    `json:"unverified,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

type worker struct{ running, dirty bool }

// kick converges a cluster's pause state in the background. Pausing waits
// for workloads to stop, so it never holds up an API call or a webhook.
// One worker per cluster serializes pauses and resumes: a resume never
// races a pause still in flight.
func (e *Enforcer) kick(tenant string, cl discovery.TenantCluster) {
	id := cl.ExternalID()
	e.wmu.Lock()
	w := e.workers[id]
	if w == nil {
		w = &worker{}
		e.workers[id] = w
	}
	if w.running {
		w.dirty = true
		e.wmu.Unlock()
		return
	}
	w.running = true
	e.wmu.Unlock()
	e.bg.Add(1)
	go func() {
		defer e.bg.Done()
		for {
			e.work(tenant, cl)
			e.wmu.Lock()
			if !w.dirty {
				w.running = false
				e.wmu.Unlock()
				return
			}
			w.dirty = false
			e.wmu.Unlock()
		}
	}()
}

func (e *Enforcer) work(tenant string, cl discovery.TenantCluster) {
	id := cl.ExternalID()
	// A pause drains, stops synced pods and confirms the stop, each bounded
	// by the drain timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 4*e.drainTimeout()+e.resumeTimeout+time.Minute)
	defer cancel()
	ts := e.StateOf(tenant)
	var err error
	if ts.State == Paused {
		e.setStatus(id, tenant, "pausing", nil)
		var out pauseOutcome
		if out, err = e.pauseCluster(ctx, cl, ts); err == nil {
			e.setPaused(id, tenant, out)
			return
		}
		// The pass failed: the status shows only what still holds.
		sp, open := e.currentSpan(id)
		e.wmu.Lock()
		st := e.status[id]
		st.Verified = open && sp.Verified()
		e.status[id] = st
		e.wmu.Unlock()
	} else {
		var resumed bool
		resumed, err = e.resumeCluster(ctx, cl, ts)
		if err == nil && resumed {
			e.setStatus(id, tenant, "resuming", nil)
			if e.opts.TenantClient != nil {
				err = e.finishResume(ctx, tenant, cl)
			}
		} else if err == nil && e.opts.TenantClient != nil && !cl.Paused {
			err = e.reflectInside(ctx, cl, ts)
		}
		if err == nil {
			e.setStatus(id, tenant, "running", nil)
			return
		}
	}
	if e.setStatus(id, tenant, "", err) {
		log.Printf("[enforcement] %s: %v", id, err) // once per distinct error, not every pass
	}
}

// setStatus records a cluster's phase (unchanged when empty) and error, and
// reports whether the error is new.
func (e *Enforcer) setStatus(id, tenant, phase string, err error) bool {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	st := e.status[id]
	prev := st.Error
	st.Cluster, st.Tenant, st.At = id, tenant, e.opts.Now().UTC()
	if phase != "" && phase != "pausing" {
		st.Verified, st.Unverified = false, ""
	}
	if phase != "" {
		st.Phase = phase
	}
	st.Error = ""
	if err != nil {
		st.Error = err.Error()
	}
	e.status[id] = st
	return st.Error != prev
}

// setPaused records how far a pause got.
func (e *Enforcer) setPaused(id, tenant string, out pauseOutcome) {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	st := e.status[id]
	st.Cluster, st.Tenant, st.At = id, tenant, e.opts.Now().UTC()
	st.Phase, st.Error = "paused", ""
	st.Verified, st.Unverified = out.verified, out.why
	if out.verified {
		st.Unverified = ""
	}
	e.status[id] = st
}

// Pauses returns the pause progress of clusters vBilling paused or resumed.
func (e *Enforcer) Pauses() []PauseStatus {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	out := make([]PauseStatus, 0, len(e.status))
	for _, st := range e.status {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return out
}

// needsWorker reports whether a cluster has a pause to make or undo.
func (e *Enforcer) needsWorker(ctx context.Context, cl discovery.TenantCluster, ts TenantState) bool {
	if ts.State == Paused {
		return true
	}
	id := cl.ExternalID()
	e.wmu.Lock()
	w := e.workers[id]
	busy := w != nil && w.running
	e.wmu.Unlock()
	if busy {
		return true
	}
	if cl.External {
		sp, open := e.currentSpan(id)
		return open && sp.By != pause.ByExternal // vBilling's, verified or opened by an older version
	}
	wl, err := e.workload(ctx, cl)
	return err == nil && wl.ann[discovery.AnnotationPausedBy] == pause.ByVBilling
}

func (e *Enforcer) drainTimeout() time.Duration {
	if e.opts.DrainTimeout > 0 {
		return e.opts.DrainTimeout
	}
	return 2 * time.Minute
}

// Guard pauses again, at once, any cluster of a paused tenant that runs:
// one scaled back up by hand, woken by vCluster Platform, or created while
// the tenant is paused. It checks external clusters again too, since a
// cluster admin inside one can undo its pause. It is cheap (one read per
// cluster of a paused tenant) and runs every few seconds; Resync covers
// the rest.
func (e *Enforcer) Guard(ctx context.Context) {
	if e.opts.Mode != "enforce" || e.kube == nil || e.opts.Clusters == nil {
		return
	}
	for _, ts := range e.States() {
		if ts.State != Paused {
			continue
		}
		for _, cl := range e.opts.Clusters(ts.Tenant) {
			if cl.External {
				e.kick(ts.Tenant, cl)
				continue
			}
			w, err := e.workload(ctx, cl)
			if err != nil {
				continue
			}
			if w.replicas == 0 {
				e.sawStopped(cl.ExternalID())
			}
			if w.replicas > 0 || w.ann[discovery.AnnotationPausedBy] != pause.ByVBilling && w.ann[annPaused] != "true" {
				log.Printf("[enforcement] %s: running while its tenant is paused, pausing it again", cl.ExternalID())
				e.kick(ts.Tenant, cl)
			}
		}
	}
}

// Wait waits for background pauses and resumes to finish (tests, shutdown).
func (e *Enforcer) Wait() { e.bg.Wait() }

// drained is what draining a cluster's own nodes left.
type drained struct {
	// list returns the workload pods on the cluster's own nodes (nil when
	// it has none).
	list  func(context.Context) ([]corev1.Pod, error)
	left  []corev1.Pod // still there at the end of the drain
	clean bool         // there was nothing to stop
}

// drain taints the cluster's own nodes NoSchedule, then stops every
// workload pod on them, whatever it tolerates (stopPods): only node
// infrastructure (keptOnPause) and nodes of the control plane cluster
// (shared and dedicated nodes, whose pods are the synced ones) are left
// alone. The node rules are metering's: a node vBilling bills as the
// cluster's own is one the drain empties.
func (e *Enforcer) drain(ctx context.Context, kube kubernetes.Interface, external bool) (drained, error) {
	host := map[string]bool{}
	if !external && e.kube != nil {
		nodes, err := e.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return drained{}, fmt.Errorf("list control plane cluster nodes: %w", err)
		}
		for _, n := range nodes.Items {
			host[n.Name] = true
		}
	}
	isOwn := func(n *corev1.Node) bool { return !host[n.Name] && n.Labels["vcluster.loft.sh/fake-node"] != "true" }
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return drained{}, fmt.Errorf("list nodes: %w", err)
	}
	hasOwn := false
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !isOwn(n) {
			continue
		}
		hasOwn = true
		if hasTaint(n, TaintPaused) {
			continue
		}
		if err := e.updateNode(ctx, kube, n.Name, func(n *corev1.Node) bool {
			if hasTaint(n, TaintPaused) {
				return false
			}
			n.Spec.Taints = append(n.Spec.Taints, pausedTaint)
			return true
		}); err != nil {
			return drained{}, fmt.Errorf("taint node %s: %w", n.Name, err)
		}
	}
	if !hasOwn {
		return drained{clean: true}, nil
	}
	// Nodes are listed each time: one that joins meanwhile counts too.
	list := func(ctx context.Context) ([]corev1.Pod, error) {
		nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list nodes: %w", err)
		}
		own := map[string]bool{}
		for i := range nodes.Items {
			if isOwn(&nodes.Items[i]) {
				own[nodes.Items[i].Name] = true
			}
		}
		pods, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list pods: %w", err)
		}
		var out []corev1.Pod
		for i := range pods.Items {
			if p := &pods.Items[i]; own[p.Spec.NodeName] && !e.keptOnPause(p) {
				out = append(out, *p)
			}
		}
		return out, nil
	}
	first, err := list(ctx)
	if err != nil {
		return drained{}, err
	}
	if len(first) == 0 {
		return drained{list: list, clean: true}, nil
	}
	left, err := e.stopPods(ctx, kube, list, true)
	if err != nil {
		return drained{}, err
	}
	return drained{list: list, left: left}, nil
}

// stopSyncedPods stops the pods a cluster synced to the control plane
// cluster, as `vcluster pause` deletes them. They come back on resume: the
// cluster still has them and syncs them again. With wait, it waits for
// them to be gone (stopPods); it returns those still there.
func (e *Enforcer) stopSyncedPods(ctx context.Context, cl discovery.TenantCluster, wait bool) ([]corev1.Pod, error) {
	list := func(ctx context.Context) ([]corev1.Pod, error) {
		pods, err := e.kube.CoreV1().Pods(cl.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + cl.Name})
		if err != nil {
			return nil, fmt.Errorf("list synced pods: %w", err)
		}
		return pods.Items, nil
	}
	return e.stopPods(ctx, e.kube, list, wait)
}

// stopPods deletes the pods list returns, each with its termination grace
// period capped at maxGrace. With wait, it lists them again until none is
// left, up to the drain timeout, then deletes what is left with no grace
// period and lists once more. A pod counts until it is gone, whether it is
// terminating or not. It returns the pods still there.
func (e *Enforcer) stopPods(ctx context.Context, kube kubernetes.Interface, list func(context.Context) ([]corev1.Pod, error), wait bool) ([]corev1.Pod, error) {
	timeout := e.drainTimeout()
	deadline := time.Now().Add(timeout)
	every := min(2*time.Second, timeout/10+time.Millisecond)
	forced := false
	for {
		pods, err := list(ctx)
		if err != nil {
			return nil, err
		}
		if len(pods) == 0 || forced {
			return pods, nil
		}
		forced = wait && !time.Now().Before(deadline)
		for i := range pods {
			if err := deletePod(ctx, kube, &pods[i], forced); err != nil {
				return nil, err
			}
		}
		if !wait {
			return pods, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// deletePod deletes a pod with its grace period capped at maxGrace, or
// with none when forced. A pod already stopping within the cap is left to
// finish.
func deletePod(ctx context.Context, kube kubernetes.Interface, p *corev1.Pod, force bool) error {
	grace := maxGrace
	if g := p.Spec.TerminationGracePeriodSeconds; g != nil && *g < grace {
		grace = max(*g, 0)
	}
	if force {
		grace = 0
	}
	if p.DeletionTimestamp != nil && (p.DeletionGracePeriodSeconds == nil || *p.DeletionGracePeriodSeconds <= grace) {
		return nil
	}
	err := kube.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s/%s: %w", p.Namespace, p.Name, err)
	}
	return nil
}

// stillThere describes pods a pause could not stop.
func stillThere(pods []corev1.Pod, where string) string {
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, pods[i].Namespace+"/"+pods[i].Name)
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = append(names[:5], fmt.Sprintf("and %d more", len(names)-5))
	}
	return fmt.Sprintf("%d pod(s) %s: %s", len(pods), where, strings.Join(names, ", "))
}

// untaint lifts the pause taint from every node of a cluster.
func (e *Enforcer) untaint(ctx context.Context, kube kubernetes.Interface) error {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	for i := range nodes.Items {
		if !hasTaint(&nodes.Items[i], TaintPaused) {
			continue
		}
		if err := e.updateNode(ctx, kube, nodes.Items[i].Name, func(n *corev1.Node) bool {
			kept := n.Spec.Taints[:0]
			for _, t := range n.Spec.Taints {
				if t.Key != TaintPaused {
					kept = append(kept, t)
				}
			}
			changed := len(kept) != len(n.Spec.Taints)
			n.Spec.Taints = kept
			return changed
		}); err != nil {
			return fmt.Errorf("lift pause taint from node %s: %w", nodes.Items[i].Name, err)
		}
	}
	return nil
}

// updateNode applies change to a node, retrying on conflicts.
func (e *Enforcer) updateNode(ctx context.Context, kube kubernetes.Interface, name string, change func(*corev1.Node) bool) error {
	for attempt := 0; ; attempt++ {
		n, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !change(n) {
			return nil
		}
		_, err = kube.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		if err == nil || !apierrors.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

// DefaultKeepNamespaces are the namespaces whose DaemonSets a pause leaves
// running: node infrastructure (GPU driver, device plugin, CNI, kube-proxy).
var DefaultKeepNamespaces = []string{"kube-system", "kube-flannel", "calico-system", "tigera-operator", "cilium",
	"gpu-operator", "nvidia-gpu-operator", "network-operator", "nvidia-network-operator"}

// gpuVisibility are the environment variables through which a container
// runtime or a GPU library hands a container devices it did not request.
var gpuVisibility = map[string]bool{"NVIDIA_VISIBLE_DEVICES": true, "CUDA_VISIBLE_DEVICES": true,
	"AMD_VISIBLE_DEVICES": true, "ROCR_VISIBLE_DEVICES": true, "HIP_VISIBLE_DEVICES": true}

// keptOnPause reports whether a pod may keep running on a paused cluster's
// own nodes: a DaemonSet pod of node infrastructure (GPU driver, CNI) in a
// PAUSE_KEEP_NAMESPACES namespace that requests no accelerator or other
// extended resource and sees no GPU through its environment, so the
// cluster serves again soon after it resumes. Every other pod, static pods
// included, is a workload: the drain counts as verified only once it is
// gone. The namespace and the pod spec are the tenant's to write, so a
// tenant with cluster-admin can still reach a node's GPUs from a kept
// namespace (a privileged container, for one): for untrusted tenants set
// PAUSE_KEEP_NAMESPACES=none, or power paused nodes off.
func (e *Enforcer) keptOnPause(p *corev1.Pod) bool {
	daemon := false
	for _, ref := range p.OwnerReferences {
		daemon = daemon || ref.Kind == "DaemonSet"
	}
	return daemon && e.keepNamespace(p.Namespace) && !requestsExtendedResources(p) && !seesGPUs(p)
}

func (e *Enforcer) keepNamespace(ns string) bool {
	keep := e.opts.KeepNamespaces
	if keep == nil {
		keep = DefaultKeepNamespaces
	}
	for _, k := range keep {
		if k == ns {
			return true
		}
	}
	return false
}

func requestsExtendedResources(p *corev1.Pod) bool {
	if len(p.Spec.ResourceClaims) > 0 {
		return true // DRA: devices allocated by a driver
	}
	for _, c := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
			for name := range list {
				switch {
				case name == corev1.ResourceCPU, name == corev1.ResourceMemory, name == corev1.ResourceEphemeralStorage,
					strings.HasPrefix(string(name), corev1.ResourceHugePagesPrefix):
				default:
					return true
				}
			}
		}
	}
	return false
}

// seesGPUs reports whether any container of a pod sets a GPU visibility
// variable to anything but empty, void or none. A value vBilling cannot
// read in the pod (from a ConfigMap, a Secret, envFrom) counts as seeing
// GPUs.
func seesGPUs(p *corev1.Pod) bool {
	sees := func(env []corev1.EnvVar, from []corev1.EnvFromSource) bool {
		if len(from) > 0 {
			return true
		}
		for _, v := range env {
			if !gpuVisibility[v.Name] {
				continue
			}
			if v.ValueFrom != nil {
				return true
			}
			switch strings.ToLower(strings.TrimSpace(v.Value)) {
			case "", "void", "none":
			default:
				return true
			}
		}
		return false
	}
	for _, c := range p.Spec.InitContainers {
		if sees(c.Env, c.EnvFrom) {
			return true
		}
	}
	for _, c := range p.Spec.Containers {
		if sees(c.Env, c.EnvFrom) {
			return true
		}
	}
	for _, c := range p.Spec.EphemeralContainers {
		if sees(c.Env, c.EnvFrom) {
			return true
		}
	}
	return false
}

func hasTaint(n *corev1.Node, key string) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

// workloadState is what a pause needs to know of a cluster's control plane
// StatefulSet or Deployment.
type workloadState struct {
	ann      map[string]string
	replicas int // wanted (one when unset)
	ready    int
}

// down reports whether the control plane is really stopped: no replicas
// wanted and none ready (discovery's definition of paused).
func (w workloadState) down() bool { return w.replicas == 0 && w.ready == 0 }

// workload returns the state of a cluster's control plane StatefulSet or
// Deployment.
func (e *Enforcer) workload(ctx context.Context, cl discovery.TenantCluster) (workloadState, error) {
	replicas := func(r *int32) int {
		if r == nil {
			return 1
		}
		return int(*r)
	}
	if cl.Source == "deployment" {
		d, err := e.kube.AppsV1().Deployments(cl.Namespace).Get(ctx, cl.Name, metav1.GetOptions{})
		if err != nil {
			return workloadState{}, fmt.Errorf("get deployment: %w", err)
		}
		return workloadState{ann: d.Annotations, replicas: replicas(d.Spec.Replicas), ready: int(d.Status.ReadyReplicas)}, nil
	}
	s, err := e.kube.AppsV1().StatefulSets(cl.Namespace).Get(ctx, cl.Name, metav1.GetOptions{})
	if err != nil {
		return workloadState{}, fmt.Errorf("get statefulset: %w", err)
	}
	return workloadState{ann: s.Annotations, replicas: replicas(s.Spec.Replicas), ready: int(s.Status.ReadyReplicas)}, nil
}

func (e *Enforcer) patchWorkload(ctx context.Context, cl discovery.TenantCluster, patch map[string]any) error {
	b, _ := json.Marshal(patch)
	var err error
	if cl.Source == "deployment" {
		_, err = e.kube.AppsV1().Deployments(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, b, metav1.PatchOptions{})
	} else {
		_, err = e.kube.AppsV1().StatefulSets(cl.Namespace).Patch(ctx, cl.Name, types.MergePatchType, b, metav1.PatchOptions{})
	}
	return err
}
