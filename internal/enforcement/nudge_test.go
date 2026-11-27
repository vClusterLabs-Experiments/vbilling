package enforcement

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
)

// After a suspension was cleared in the live validation of v0.3.2, the
// Deployments of the cluster stayed down for 5 minutes 26 seconds: their
// ReplicaSet controller had failed against the policy for six minutes and
// waited that long before it tried again. vBilling now touches the objects
// that wait for pods, so their controllers try again at once.

// waitingWorkloads is a cluster with one of every kind of workload that
// waits for pods, and one of every kind that does not.
func waitingWorkloads() []runtime.Object {
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "shop"} }
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:1"}}}}
	done := batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}
	suspended := true
	return []runtime.Object{
		&appsv1.ReplicaSet{ObjectMeta: meta("web-waits"), Spec: appsv1.ReplicaSetSpec{Replicas: int32p(2), Template: template}},
		&appsv1.ReplicaSet{ObjectMeta: meta("web-ready"), Spec: appsv1.ReplicaSetSpec{Replicas: int32p(2), Template: template}, Status: appsv1.ReplicaSetStatus{ReadyReplicas: 2}},
		&appsv1.ReplicaSet{ObjectMeta: meta("web-old-revision"), Spec: appsv1.ReplicaSetSpec{Replicas: int32p(0), Template: template}},
		&appsv1.StatefulSet{ObjectMeta: meta("db-waits"), Spec: appsv1.StatefulSetSpec{Replicas: int32p(3), Template: template}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}},
		&appsv1.StatefulSet{ObjectMeta: meta("db-ready"), Spec: appsv1.StatefulSetSpec{Template: template}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}},
		&appsv1.DaemonSet{ObjectMeta: meta("agent-waits"), Spec: appsv1.DaemonSetSpec{Template: template}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1}},
		&appsv1.DaemonSet{ObjectMeta: meta("agent-ready"), Spec: appsv1.DaemonSetSpec{Template: template}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2}},
		&batchv1.Job{ObjectMeta: meta("train-waits"), Spec: batchv1.JobSpec{Template: template}},
		&batchv1.Job{ObjectMeta: meta("train-runs"), Spec: batchv1.JobSpec{Template: template}, Status: batchv1.JobStatus{Active: 1}},
		&batchv1.Job{ObjectMeta: meta("train-done"), Spec: batchv1.JobSpec{Template: template}, Status: done},
		&batchv1.Job{ObjectMeta: meta("train-suspended"), Spec: batchv1.JobSpec{Template: template, Suspend: &suspended}},
	}
}

// patches records the patches a cluster's API gets for workload objects.
type patches struct {
	mu   sync.Mutex
	seen map[string]string // resource/name -> patch
}

func recordPatches(inside *fake.Clientset) *patches {
	p := &patches{seen: map[string]string{}}
	inside.PrependReactor("patch", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa := a.(ktesting.PatchActionImpl)
		p.mu.Lock()
		p.seen[pa.GetResource().Resource+"/"+pa.Name] = string(pa.Patch)
		p.mu.Unlock()
		return false, nil, nil
	})
	return p
}

func (p *patches) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k := range p.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lifting a suspension touches exactly the ReplicaSets, StatefulSets,
// DaemonSets and Jobs that wait for pods, with one annotation: metadata
// only, so nothing is rolled out.
func TestLiftingASuspensionMakesWaitingControllersTryAgainAtOnce(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	inside := fake.NewSimpleClientset(waitingWorkloads()...)
	seen := recordPatches(inside)
	at := time.Date(2026, 11, 24, 18, 31, 3, 0, time.UTC)
	e := New(host, Options{Mode: "enforce", Now: func() time.Time { return at },
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
	if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	if got := seen.names(); len(got) != 0 {
		t.Fatalf("workloads touched while the tenant is suspended: %v", got)
	}
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	want := []string{"daemonsets/agent-waits", "jobs/train-waits", "replicasets/web-waits", "statefulsets/db-waits"}
	if got := seen.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("touched %v, want exactly the workloads that wait for pods: %v", got, want)
	}
	seen.mu.Lock()
	for name, patch := range seen.seen {
		var body map[string]map[string]map[string]string
		if err := json.Unmarshal([]byte(patch), &body); err != nil || len(body) != 1 || len(body["metadata"]) != 1 ||
			len(body["metadata"]["annotations"]) != 1 || body["metadata"]["annotations"][AnnotationUnblockedAt] != "2026-11-24T18:31:03Z" {
			t.Errorf("%s was patched with %s: want one annotation, %s with the time, and nothing else", name, patch, AnnotationUnblockedAt)
		}
	}
	seen.mu.Unlock()
	rs, err := inside.AppsV1().ReplicaSets("shop").Get(ctx, "web-waits", metav1.GetOptions{})
	if err != nil || rs.Annotations[AnnotationUnblockedAt] == "" || len(rs.Spec.Template.Annotations) != 0 || rs.Spec.Template.Spec.Containers[0].Image != "web:1" {
		t.Fatalf("ReplicaSet after the nudge: %+v, %v; want the annotation on the object and its pod template untouched", rs, err)
	}

	// Nothing was lifted at the next pass: nothing is touched again.
	seen.mu.Lock()
	seen.seen = map[string]string{}
	seen.mu.Unlock()
	e.Resync(ctx)
	if got := seen.names(); len(got) != 0 {
		t.Fatalf("workloads touched again by a resync that lifted nothing: %v", got)
	}
}

// Without the access the nudge needs (list and patch on the workload
// objects, which the rule for suspending and pausing does not grant), the
// suspension is lifted all the same: the nudge is best effort, and the log
// names what to grant, once.
func TestNudgeThatIsRefusedDoesNotKeepASuspension(t *testing.T) {
	ctx := context.Background()
	cl := discovery.TenantCluster{Name: "private", Namespace: "tenant", Source: "statefulset"}
	host := fake.NewSimpleClientset(controlPlane(cl.Name, cl.Namespace, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cl.Namespace}})
	for _, tc := range []struct{ name, verb string }{{"may not list them", "list"}, {"may not patch them", "patch"}} {
		t.Run(tc.name, func(t *testing.T) {
			inside := fake.NewSimpleClientset(waitingWorkloads()...)
			for _, resource := range []string{"replicasets", "statefulsets", "daemonsets", "jobs"} {
				inside.PrependReactor(tc.verb, resource, func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: a.GetResource().Group, Resource: a.GetResource().Resource}, "", errors.New("RBAC: access denied"))
				})
			}
			e := New(host, Options{Mode: "enforce",
				Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }, TenantClient: clientFor(inside)})
			logs := captureLog(t)
			for round := 0; round < 2; round++ {
				if err := e.Set(ctx, "acme", Suspended, "unpaid", "api", "manual"); err != nil {
					t.Fatal(err)
				}
				if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
					t.Fatalf("lifting the suspension failed because the nudge was refused: %v", err)
				}
				if hasPolicy(t, inside, PolicyName) {
					t.Fatal("the suspension policy stayed")
				}
			}
			if n := logs.count(cl.ExternalID(), "replicasets, statefulsets and daemonsets", "jobs"); n != 1 {
				t.Fatalf("the log names the missing access %d time(s), want once:\n%s", n, logs)
			}
		})
	}
}

// A Job counts as waiting only while it still runs and has fewer pods than
// it wants.
func TestJobWaitsForPods(t *testing.T) {
	two, yes := int32(2), true
	for _, tc := range []struct {
		name  string
		job   batchv1.Job
		waits bool
	}{
		{"no pod yet", batchv1.Job{}, true},
		{"its pod runs", batchv1.Job{Status: batchv1.JobStatus{Active: 1}}, false},
		{"one of two parallel pods", batchv1.Job{Spec: batchv1.JobSpec{Parallelism: &two}, Status: batchv1.JobStatus{Active: 1}}, true},
		{"one completion left", batchv1.Job{Spec: batchv1.JobSpec{Parallelism: &two, Completions: &two}, Status: batchv1.JobStatus{Succeeded: 1, Active: 1}}, false},
		{"all completions done", batchv1.Job{Spec: batchv1.JobSpec{Completions: &two}, Status: batchv1.JobStatus{Succeeded: 2}}, false},
		{"a work queue with a success", batchv1.Job{Status: batchv1.JobStatus{Succeeded: 1}}, false},
		{"suspended", batchv1.Job{Spec: batchv1.JobSpec{Suspend: &yes}}, false},
		{"failed", batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}, false},
	} {
		if got := jobWaits(&tc.job); got != tc.waits {
			t.Errorf("%s: waits = %t, want %t", tc.name, got, tc.waits)
		}
	}
}
