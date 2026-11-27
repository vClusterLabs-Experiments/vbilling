package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A pause lowered to a suspension. In the live validation of v0.3.2 the
// control plane came back and the suspension policy stayed, so it refused
// every pod the workload controllers created again: the cluster ran, and
// billed, with nothing in it. "Suspended" means no new pods while what runs
// keeps running, so the cluster is restored first and suspended after.

func nsSuspended(t *testing.T, kube *fake.Clientset, ns string) bool {
	t.Helper()
	got, err := kube.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got.Labels[LabelSuspended] == "true"
}

// pausedForBillingAndSuspendedForAbuse pauses a cluster with a ReplicaSet's
// two pods and a bare pod on its own node gpu-1, for a tenant that is also
// suspended for abuse: paying the billing hold lowers the pause to that
// suspension.
func pausedForBillingAndSuspendedForAbuse(t *testing.T, mutate ...func(*Options)) (discovery.TenantCluster, *fake.Clientset, *fake.Clientset, *Enforcer) {
	t.Helper()
	cl, host, inside := privateCluster(
		ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"),
		ownedPod("web-2", "shop", "gpu-1", "ReplicaSet", "web"),
		podOn("debug", "shop", "gpu-1"))
	stopsWithItsControlPlane(host, inside)
	opts := Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)}
	for _, m := range mutate {
		m(&opts)
	}
	e := New(host, opts)
	e.resumeEvery, e.restoreEvery = 5*time.Millisecond, 5*time.Millisecond
	ctx := context.Background()
	if err := e.SetHold(ctx, "acme", "abuse", Suspended, "outbound spam reported", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if st := onlyStatus(t, e); !st.Verified || storedPods(t, inside, "shop") != 0 {
		t.Fatalf("test setup: status %+v, %d pod(s); want a verified pause with nothing running", st, storedPods(t, inside, "shop"))
	}
	if !storedPolicy(inside, PolicyName) || !storedPolicy(inside, PausedPolicyName) || !nsSuspended(t, host, cl.Namespace) {
		t.Fatal("test setup: a paused cluster carries both policies and its namespace is labeled suspended")
	}
	return cl, host, inside, e
}

// The billing hold is paid and the abuse hold at suspended stays. The
// cluster comes back as after any resume, and takes pods again: the
// suspension policy and the suspended label are held back, at the state
// change and at every resync, until the ReplicaSet has its two pods again.
// Then the suspension applies, long before the time limit. The bare pod is
// not waited for: nothing creates it again.
func TestPauseLoweredToSuspendedRestoresTheClusterBeforeSuspendingIt(t *testing.T) {
	cl, host, inside, e := pausedForBillingAndSuspendedForAbuse(t)
	e.restoreTimeout = time.Minute // only the workloads coming back end the restore in time
	ctx := context.Background()
	logs := captureLog(t)
	began := time.Now()
	if err := e.Set(ctx, "acme", Active, "top-up paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if st := e.StateOf("acme"); st.State != Suspended || st.Hold != "abuse" {
		t.Fatalf("test setup: state %+v, want the abuse suspension", st)
	}
	eventually(t, "the control plane is back and the pause is lifted inside the cluster", func() bool {
		rec, _ := e.record(cl.ExternalID())
		return !rec.RestoreLifted.IsZero()
	})
	check := func(when string) {
		t.Helper()
		if *sts(t, host, cl.Namespace, cl.Name).Spec.Replicas != 1 {
			t.Fatalf("%s: the control plane is not back", when)
		}
		if hasPolicy(t, inside, PausedPolicyName) || tainted(t, inside, "gpu-1") {
			t.Fatalf("%s: the pause policy or the taint is still there", when)
		}
		if hasPolicy(t, inside, PolicyName) {
			t.Fatalf("%s: the suspension policy refuses the pods the ReplicaSet creates again: the cluster is up and empty", when)
		}
		if nsSuspended(t, host, cl.Namespace) {
			t.Fatalf("%s: the namespace is labeled suspended, so the control plane cluster refuses the pods it syncs again", when)
		}
	}
	check("after the hold was lowered")
	if st := onlyStatus(t, e); st.Phase != "resuming" || st.RestoringUntil == nil || st.RestoringUntil.Before(time.Now()) {
		t.Fatalf("status = %+v, want resuming, with the time the suspension applies at the latest", st)
	}
	e.Resync(ctx)
	e.Guard(ctx)
	check("after a resync and a guard pass")

	// The ReplicaSet controller creates its pods again.
	for _, name := range []string{"web-3", "web-4"} {
		if _, err := inside.CoreV1().Pods("shop").Create(ctx, ownedPod(name, "shop", "gpu-1", "ReplicaSet", "web"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if took := time.Since(began); took > 30*time.Second {
		t.Fatalf("the suspension applied after %s: it waited for its time limit, not for the workloads", took)
	}
	p, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{})
	if err != nil || p.Spec.Validations[0].Message != suspendedMessage(e.StateOf("acme")) {
		t.Fatalf("suspension policy after the restore: %+v, %v", p, err)
	}
	if !nsSuspended(t, host, cl.Namespace) {
		t.Fatal("the namespace is not labeled suspended after the restore")
	}
	if len(podNames(t, inside, "shop")) != 2 {
		t.Fatalf("pods after the restore: %v", podNames(t, inside, "shop"))
	}
	if st := onlyStatus(t, e); st.Phase != "running" || st.RestoringUntil != nil || st.Error != "" {
		t.Fatalf("status = %+v, want running", st)
	}
	if e.hasRecord(cl.ExternalID()) {
		t.Fatal("the restore is still on record after it ended")
	}
	// The log says what happened: never that workloads can run again while
	// the suspension it keeps refuses new pods.
	if logs.count(cl.ExternalID(), "workloads can run again") != 0 {
		t.Fatalf("the log says workloads can run again for a suspended tenant:\n%s", logs)
	}
	if logs.count(cl.ExternalID(), "restored first") != 1 || logs.count(cl.ExternalID(), "new pods are blocked again", "what ran before the pause is back") != 1 {
		t.Fatalf("the log does not tell the restore and the suspension:\n%s", logs)
	}
}

// The billing hold is paid while the pause is still draining, and the abuse
// suspension stays: the pause stops where it is, and the pod it had already
// deleted gets the same restore before the suspension applies.
func TestPauseLiftedMidDrainIntoASuspensionRestoresWhatItStopped(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster(
		ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"),
		ownedPod("web-2", "shop", "gpu-1", "ReplicaSet", "web"))
	stopped := scaleDowns(host)
	var e *Enforcer
	var mu sync.Mutex
	deleted := 0
	inside.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		deleted++
		first := deleted == 1
		mu.Unlock()
		if first {
			if err := e.storeHold("acme", Active); err != nil {
				t.Errorf("lift the billing hold: %v", err)
			}
		}
		return false, nil, nil
	})
	e = New(host, Options{Mode: "enforce", DrainTimeout: 5 * time.Second, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery, e.restoreEvery, e.restoreTimeout = 5*time.Millisecond, 5*time.Millisecond, time.Minute
	if err := e.SetHold(ctx, "acme", "abuse", Suspended, "outbound spam reported", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pause stops and the cluster is restored", func() bool {
		rec, _ := e.record(cl.ExternalID())
		return !rec.RestoreLifted.IsZero()
	})
	if n := stopped(); n != 0 {
		t.Fatalf("the control plane was scaled to zero %d time(s) after the hold was lowered", n)
	}
	if hasPolicy(t, inside, PolicyName) || hasPolicy(t, inside, PausedPolicyName) || tainted(t, inside, "gpu-1") {
		t.Fatal("a policy or the taint keeps the ReplicaSet from creating the deleted pod again")
	}
	if len(podNames(t, inside, "shop")) != 1 {
		t.Fatalf("pods: %v, want the one the drain had not reached", podNames(t, inside, "shop"))
	}
	if _, err := inside.CoreV1().Pods("shop").Create(ctx, ownedPod("web-3", "shop", "gpu-1", "ReplicaSet", "web"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if !hasPolicy(t, inside, PolicyName) || e.hasRecord(cl.ExternalID()) {
		t.Fatal("the suspension did not apply once the deleted pod was back")
	}
	if st := onlyStatus(t, e); st.Phase != "running" || st.Error != "" {
		t.Fatalf("status = %+v, want running", st)
	}
}

// Workloads that do not come back (a controller that was deleted, a
// quota that is used up) do not hold the suspension back for good: it
// applies when the time limit is up, and not a second before.
func TestRestoreEndsAtItsTimeLimit(t *testing.T) {
	clock := newClock()
	cl, _, inside, e := pausedForBillingAndSuspendedForAbuse(t, func(o *Options) { o.Now = clock.Now })
	ctx := context.Background()
	logs := captureLog(t)
	if err := e.Set(ctx, "acme", Active, "top-up paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pause is lifted inside the cluster", func() bool {
		rec, _ := e.record(cl.ExternalID())
		return !rec.RestoreLifted.IsZero()
	})
	if until := onlyStatus(t, e).RestoringUntil; until == nil || !until.Equal(clock.Now().Add(defaultRestoreTimeout)) {
		t.Fatalf("restoring until %v, want %s after the cluster's API answered", until, defaultRestoreTimeout)
	}
	clock.add(defaultRestoreTimeout - time.Second)
	time.Sleep(50 * time.Millisecond) // the restore looks every 5 ms
	if hasPolicy(t, inside, PolicyName) {
		t.Fatal("the suspension applied a second before the time limit of the restore")
	}
	clock.add(2 * time.Second)
	e.Wait()
	if !hasPolicy(t, inside, PolicyName) {
		t.Fatal("the suspension did not apply when the time limit was up")
	}
	if logs.count(cl.ExternalID(), "new pods are blocked again", "0 of the 1 workload(s)") != 1 {
		t.Fatalf("the log does not say what was missing when the restore ended:\n%s", logs)
	}
}

// A restart in the middle of a restore: the new process finds the restore
// in the billing state store, holds the suspension back until its time,
// never earlier (not at its first resync, not at its guard passes), and
// applies it then.
func TestRestoreSurvivesARestartAndIsNeverCutShort(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster() // resumed already: running, nothing left of the pause
	clock := newClock()
	state := kv.Memory()
	ts := effective("acme", map[string]Hold{"abuse": {State: Suspended, Reason: "outbound spam reported", Source: "api", EventType: "manual", Since: clock.Now()}})
	ts.Since = clock.Now()
	raw, _ := json.Marshal(ts)
	if err := state.Set("state/acme", string(raw)); err != nil {
		t.Fatal(err)
	}
	// What the process that restarted had stored: the pause had stopped a
	// ReplicaSet's two pods, and the restore runs for another minute.
	rec, _ := json.Marshal(pauseRecord{Tenant: "acme", Stopped: map[string]int{ownKey + "ReplicaSet-web": 2},
		RestoreSince: clock.Now().Add(-time.Minute), RestoreLifted: clock.Now().Add(-time.Minute), RestoreUntil: clock.Now().Add(time.Minute)})
	if err := state.Set(recordPrefix+cl.ExternalID(), string(rec)); err != nil {
		t.Fatal(err)
	}
	e := New(host, Options{Mode: "enforce", State: state, Now: clock.Now, DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery, e.restoreEvery = 5*time.Millisecond, 5*time.Millisecond

	e.Guard(ctx)  // 15 seconds after the start
	e.Resync(ctx) // two minutes after the start
	time.Sleep(50 * time.Millisecond)
	if hasPolicy(t, inside, PolicyName) || nsSuspended(t, host, cl.Namespace) {
		t.Fatalf("after a restart the suspension applied a minute early: policy %t, namespace label %t", hasPolicy(t, inside, PolicyName), nsSuspended(t, host, cl.Namespace))
	}
	if st := e.Pauses(); len(st) != 1 || st[0].RestoringUntil == nil || !st[0].RestoringUntil.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("status after the restart = %+v, want the restore and its time", st)
	}
	clock.add(61 * time.Second)
	e.Wait() // the worker the guard pass started applies it
	if !hasPolicy(t, inside, PolicyName) || !nsSuspended(t, host, cl.Namespace) {
		t.Fatalf("the suspension did not apply once its time was up: policy %t, namespace label %t", hasPolicy(t, inside, PolicyName), nsSuspended(t, host, cl.Namespace))
	}
	if _, kept := state.Get(recordPrefix + cl.ExternalID()); kept {
		t.Fatal("the restore is still in the store after it ended")
	}
}

// Whoever writes vcluster.yaml writes the control plane's annotations: none
// of them holds a suspension back. Only vBilling's own record does.
func TestNoAnnotationHoldsASuspensionBack(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster()
	s := sts(t, host, cl.Namespace, cl.Name)
	far := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	s.Annotations = map[string]string{"vbilling.vcluster.com/restore-until": far, "vbilling.vcluster.com/restoring": "true", "vbilling.vcluster.com/restoring-until": far}
	if _, err := host.AppsV1().StatefulSets(cl.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e := New(host, Options{Mode: "enforce", Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if !hasPolicy(t, inside, PolicyName) || !nsSuspended(t, host, cl.Namespace) {
		t.Fatal("annotations on the control plane held the suspension back")
	}
}

// The pause mark is an annotation too. A tenant that plants vBilling's mark
// on its running control plane while it is suspended gets the mark removed,
// and nothing else: only a pause vBilling has on record starts a restore, so
// the suspension is not held back for a moment.
func TestPauseMarkPlantedOnASuspendedClusterHoldsNothingBack(t *testing.T) {
	ctx := context.Background()
	cl, host, inside := privateCluster()
	var mu sync.Mutex
	lifted := 0
	inside.PrependReactor("delete", "validatingadmissionpolicies", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.(ktesting.DeleteActionImpl).Name == PolicyName {
			mu.Lock()
			lifted++
			mu.Unlock()
		}
		return false, nil, nil
	})
	e := New(host, Options{Mode: "enforce", Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery, e.restoreEvery = 5*time.Millisecond, 5*time.Millisecond
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		mark := `{"metadata":{"annotations":{"` + discovery.AnnotationPausedBy + `":"vbilling","` + discovery.AnnotationPausedReplicas + `":"1"}}}`
		if _, err := host.AppsV1().StatefulSets(cl.Namespace).Patch(ctx, cl.Name, "application/merge-patch+json", []byte(mark), metav1.PatchOptions{}); err != nil {
			t.Fatal(err)
		}
		e.Resync(ctx)
		e.Wait()
		if !hasPolicy(t, inside, PolicyName) || !nsSuspended(t, host, cl.Namespace) {
			t.Fatalf("round %d: a planted pause mark lifted the suspension: policy %t, namespace label %t", round, hasPolicy(t, inside, PolicyName), nsSuspended(t, host, cl.Namespace))
		}
		if e.restoring(cl.ExternalID()) || e.hasRecord(cl.ExternalID()) {
			t.Fatalf("round %d: a planted pause mark started a restore", round)
		}
		if s := sts(t, host, cl.Namespace, cl.Name); s.Annotations[discovery.AnnotationPausedBy] != "" || *s.Spec.Replicas != 1 {
			t.Fatalf("round %d: control plane annotations %v, replicas %d", round, s.Annotations, *s.Spec.Replicas)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if lifted != 0 {
		t.Fatalf("the suspension policy was removed %d time(s) on the strength of a planted mark", lifted)
	}
}

// A cluster that refuses what follows the lifting of the policies (its
// tenant's admins took vBilling's access to nodes away, so the taint stays)
// does not stretch the restore: its time runs from the moment its API
// answered, and the suspension applies when it is up, whatever else failed.
func TestSuspensionIsNotHeldBackByAClusterThatRefusesTheRest(t *testing.T) {
	var mu sync.Mutex
	deny := false
	clock := newClock()
	cl, _, inside, e := pausedForBillingAndSuspendedForAbuse(t, func(o *Options) { o.Now = clock.Now })
	inside.PrependReactor("update", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if deny {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "gpu-1", errors.New("RBAC: access denied"))
		}
		return false, nil, nil
	})
	mu.Lock()
	deny = true
	mu.Unlock()
	if err := e.Set(context.Background(), "acme", Active, "top-up paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the policies are lifted for the restore", func() bool {
		rec, _ := e.record(cl.ExternalID())
		return !rec.RestoreLifted.IsZero() && !storedPolicy(inside, PolicyName)
	})
	if until := onlyStatus(t, e).RestoringUntil; until == nil || !until.Equal(clock.Now().Add(defaultRestoreTimeout)) {
		t.Fatalf("restoring until %v: the refused step stretched the restore beyond its %s", until, defaultRestoreTimeout)
	}
	clock.add(defaultRestoreTimeout + time.Second)
	eventually(t, "the suspension applies although the taint cannot be lifted", func() bool { return storedPolicy(inside, PolicyName) })
	if !tainted(t, inside, "gpu-1") {
		t.Fatal("test setup: the taint was lifted")
	}
	mu.Lock()
	deny = false // the access is back: the pass finishes
	mu.Unlock()
	e.Wait()
	if !hasPolicy(t, inside, PolicyName) || tainted(t, inside, "gpu-1") {
		t.Fatalf("after the pass: suspension policy %t, taint %t; want the suspension in place and the taint lifted", hasPolicy(t, inside, PolicyName), tainted(t, inside, "gpu-1"))
	}
}

// On shared nodes the pause deletes the pods the cluster synced to the
// control plane cluster, and its syncer creates them again after the
// resume. The control plane cluster's policy refuses pods in a namespace
// labeled suspended, so the label waits for the restore too.
func TestPauseLoweredToSuspendedLetsSyncedPodsComeBack(t *testing.T) {
	ctx := context.Background()
	ns := "vc-team"
	cp := controlPlane("team", ns, 1, nil)
	cp.Status.ReadyReplicas = 1
	synced := func(name string) *corev1.Pod {
		p := syncedPod(name, ns, "team")
		p.Annotations = map[string]string{annOwnerReferences: `[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"api","uid":"rs-api","controller":true}]`}
		return p
	}
	host := fake.NewSimpleClientset(cp, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		synced("api-1-x-default-x-team"), synced("api-2-x-default-x-team"), syncedPod("debug-x-default-x-team", ns, "team"))
	cl := discovery.TenantCluster{Name: "team", Namespace: ns, Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	e.resumeEvery, e.restoreEvery, e.restoreTimeout = 5*time.Millisecond, 5*time.Millisecond, time.Minute
	if err := e.SetHold(ctx, "acme", "abuse", Suspended, "outbound spam reported", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if len(podNames(t, host, ns)) != 0 || !nsSuspended(t, host, ns) {
		t.Fatalf("test setup: synced pods %v, suspended label %t", podNames(t, host, ns), nsSuspended(t, host, ns))
	}
	if err := e.Set(ctx, "acme", Active, "top-up paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the restore runs", func() bool {
		rec, _ := e.record(cl.ExternalID())
		return !rec.RestoreLifted.IsZero()
	})
	e.Resync(ctx)
	if nsSuspended(t, host, ns) {
		t.Fatal("the namespace is labeled suspended while the cluster syncs its pods again: the control plane cluster refuses them")
	}
	for _, name := range []string{"api-3-x-default-x-team", "api-4-x-default-x-team"} {
		if _, err := host.CoreV1().Pods(ns).Create(ctx, synced(name), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.Wait()
	if !nsSuspended(t, host, ns) {
		t.Fatal("the namespace is not labeled suspended after the restore")
	}
}

// A pause that is lifted altogether needs no restore: nothing is held back,
// and the log says workloads can run again.
func TestResumeToActiveHoldsNothingBack(t *testing.T) {
	cl, host, inside := privateCluster(ownedPod("web-1", "shop", "gpu-1", "ReplicaSet", "web"))
	stopsWithItsControlPlane(host, inside)
	e := New(host, Options{Mode: "enforce", DrainTimeout: 20 * time.Millisecond, Pauses: pause.Memory(),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	e.resumeEvery = 5 * time.Millisecond
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	logs := captureLog(t)
	if err := e.Set(ctx, "acme", Active, "top-up paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if e.hasRecord(cl.ExternalID()) || onlyStatus(t, e).RestoringUntil != nil {
		t.Fatal("a resume to active left a restore behind")
	}
	if logs.count(cl.ExternalID(), "workloads can run again") != 1 {
		t.Fatalf("the log does not say workloads can run again:\n%s", logs)
	}
}
