package enforcement

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/kv"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

func int32p(v int32) *int32 { return &v }

func controlPlane(name, ns string, replicas int32, ann map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "vcluster"}, Annotations: ann},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32p(replicas)},
	}
}

func syncedPod(name, ns, cluster string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{labelManagedBy: cluster}}}
}

func podOn(name, ns, node string, tol ...corev1.Toleration) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{NodeName: node, Tolerations: tol}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func sts(t *testing.T, kube kubernetes.Interface, ns, name string) *appsv1.StatefulSet {
	t.Helper()
	s, err := kube.AppsV1().StatefulSets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func podNames(t *testing.T, kube kubernetes.Interface, ns string) map[string]bool {
	t.Helper()
	list, err := kube.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range list.Items {
		out[p.Name] = true
	}
	return out
}

type pauseLog struct {
	mu     sync.Mutex
	events []string
}

func (l *pauseLog) add(s string) { l.mu.Lock(); l.events = append(l.events, s); l.mu.Unlock() }
func (l *pauseLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func TestPauseStopsSharedClusterAndResumeRestoresIt(t *testing.T) {
	ns := "vc-team-a"
	host := fake.NewSimpleClientset(
		controlPlane("team-a", ns, 2, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		syncedPod("trainer-x-default-x-team-a", ns, "team-a"),
		syncedPod("server-x-default-x-team-a", ns, "team-a"),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "team-a-0", Namespace: ns, Labels: map[string]string{"app": "vcluster"}}},
	)
	cl := discovery.TenantCluster{Name: "team-a", Namespace: ns, Source: "statefulset"}
	spans := pause.Memory()
	var calls pauseLog
	// The pause worker reads the clock in the background, so the test's
	// clock is guarded.
	var clockMu sync.Mutex
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	e := New(host, Options{Mode: "enforce", Now: clock,
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		Pauses:   spans,
		OnPause: func(id string, paused bool) {
			if paused {
				calls.add("paused " + id)
			} else {
				calls.add("resumed " + id)
			}
		},
	})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "metronome", "alerts.low_remaining_credit_balance_reached"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	s := sts(t, host, ns, "team-a")
	if *s.Spec.Replicas != 0 || s.Annotations[annPaused] != "" || s.Annotations[discovery.AnnotationPausedReplicas] != "2" ||
		s.Annotations[discovery.AnnotationPausedBy] != "vbilling" {
		t.Fatalf("control plane after pause: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
	if pods := podNames(t, host, ns); len(pods) != 1 || !pods["team-a-0"] {
		t.Fatalf("pods after pause = %v, want only the control plane pod (scaled down by its StatefulSet)", pods)
	}
	if sp, open := spans.Current(cl.ExternalID()); !open || !sp.From.Equal(clock()) || sp.By != "vbilling" {
		t.Fatalf("pause span = %+v, open %t", sp, open)
	}
	if got := e.Pauses(); len(got) != 1 || got[0].Phase != "paused" {
		t.Fatalf("pause status = %+v", got)
	}

	// Durable: a resume outside vBilling (vcluster resume) is undone at the
	// next resync, and synced pods that came back are deleted again.
	if _, err := host.AppsV1().StatefulSets(ns).Update(ctx, controlPlane("team-a", ns, 2, nil), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.CoreV1().Pods(ns).Create(ctx, syncedPod("trainer-x-default-x-team-a", ns, "team-a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	e.Wait()
	if s := sts(t, host, ns, "team-a"); *s.Spec.Replicas != 0 || s.Annotations[discovery.AnnotationPausedReplicas] != "2" {
		t.Fatalf("resync did not pause again: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
	if pods := podNames(t, host, ns); pods["trainer-x-default-x-team-a"] {
		t.Fatal("a synced pod survived the pause")
	}

	// A payment clears the billing hold but never an abuse pause.
	if err := e.SetHold(ctx, "acme", "abuse", Paused, "crypto mining", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	clockMu.Lock()
	now = now.Add(time.Hour)
	clockMu.Unlock()
	if err := e.Set(ctx, "acme", Active, "top-up paid", "metronome", "payment_gate.payment_status"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if s := sts(t, host, ns, "team-a"); *s.Spec.Replicas != 0 {
		t.Fatal("a payment resumed a cluster paused for abuse")
	}
	if err := e.SetHold(ctx, "acme", "abuse", Active, "reviewed", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	s = sts(t, host, ns, "team-a")
	if *s.Spec.Replicas != 2 || s.Annotations[annPaused] != "" || s.Annotations[discovery.AnnotationPausedBy] != "" {
		t.Fatalf("control plane after resume: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
	got := spans.Spans(cl.ExternalID())
	if len(got) != 1 || !got[0].To.Equal(clock()) {
		t.Fatalf("pause spans = %+v, want one ending at the resume", got)
	}
	if ev := calls.list(); len(ev) < 2 || ev[len(ev)-1] != "resumed "+cl.ExternalID() {
		t.Fatalf("metering was not told: %v", ev)
	}

	// Pausing twice is safe, and a pause somebody else made is never undone.
	if err := e.Set(ctx, "acme", Active, "still fine", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.AppsV1().StatefulSets(ns).Update(ctx, controlPlane("team-a", ns, 0, map[string]string{annPaused: "true", "loft.sh/paused-replicas": "2"}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Resync(ctx)
	e.Wait()
	if s := sts(t, host, ns, "team-a"); *s.Spec.Replicas != 0 || s.Annotations[annPaused] != "true" {
		t.Fatal("vBilling resumed a cluster an operator paused")
	}
}

func TestPausePrivateNodesDrainsBeforeStoppingTheControlPlane(t *testing.T) {
	ns := "vc-private-a"
	host := fake.NewSimpleClientset(
		controlPlane("private-a", ns, 1, nil),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-worker-1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "cp-worker-1-uuid"}}},
	)
	exists := corev1.Toleration{Operator: corev1.TolerationOpExists}
	daemon := func(p *corev1.Pod) *corev1.Pod {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: p.Name}}
		return p
	}
	gpuDaemon := daemon(podOn("gpu-burner-x", "ml", "gpu-1"))
	gpuDaemon.Spec.Containers = []corev1.Container{{Name: "burn", Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}}}
	inside := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"}},
		// A control plane cluster node seen through the cluster (vCluster's
		// synced copy reports the same machine): never tainted.
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-worker-1"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "cp-worker-1-uuid"}}},
		podOn("trainer", "research", "gpu-1"),
		daemon(podOn("kube-proxy-abc", "kube-system", "gpu-1", exists)),
		daemon(podOn("nvidia-driver-daemonset-x", "gpu-operator", "gpu-1")), // infrastructure: keeps running
		gpuDaemon, // a DaemonSet that holds a GPU: stopped
		podOn("web", "default", "cp-worker-1"),
	)
	cl := discovery.TenantCluster{Name: "private-a", Namespace: ns, Source: "statefulset"}
	var order pauseLog
	e := New(host, Options{Mode: "enforce", Now: time.Now, DrainTimeout: 200 * time.Millisecond,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
		Pauses:       pause.Memory(),
		Freeze: func(ctx context.Context, c discovery.TenantCluster) error {
			s := sts(t, host, ns, "private-a")
			if *s.Spec.Replicas != 1 {
				order.add("freeze after scale-down")
			}
			if podNames(t, inside, "research")["trainer"] {
				order.add("freeze before drain")
			}
			order.add("freeze")
			return nil
		},
	})
	e.resumeEvery = 10 * time.Millisecond
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if got := order.list(); len(got) != 1 || got[0] != "freeze" {
		t.Fatalf("pause order: %v (want drain, then freeze, then scale down)", got)
	}
	gpu, _ := inside.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	cp, _ := inside.CoreV1().Nodes().Get(ctx, "cp-worker-1", metav1.GetOptions{})
	if !hasTaint(gpu, TaintPaused) || hasTaint(cp, TaintPaused) {
		t.Fatalf("taints: gpu-1 %v, cp-worker-1 %v", gpu.Spec.Taints, cp.Spec.Taints)
	}
	if podNames(t, inside, "research")["trainer"] || !podNames(t, inside, "kube-system")["kube-proxy-abc"] || !podNames(t, inside, "default")["web"] ||
		!podNames(t, inside, "gpu-operator")["nvidia-driver-daemonset-x"] || podNames(t, inside, "ml")["gpu-burner-x"] {
		t.Fatal("drain deleted the wrong pods: workloads and GPU-holding DaemonSet pods stop, node infrastructure keeps running")
	}
	for _, tn := range gpu.Spec.Taints {
		if tn.Key == TaintPaused && tn.Effect != corev1.TaintEffectNoSchedule {
			t.Fatalf("pause taint effect %s, want NoSchedule (NoExecute would evict the GPU driver too)", tn.Effect)
		}
	}
	if _, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{}); err != nil {
		t.Fatalf("no admission policy inside the paused cluster: %v", err)
	}
	if s := sts(t, host, ns, "private-a"); *s.Spec.Replicas != 0 {
		t.Fatal("control plane still running")
	}

	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	gpu, _ = inside.CoreV1().Nodes().Get(ctx, "gpu-1", metav1.GetOptions{})
	if hasTaint(gpu, TaintPaused) {
		t.Fatal("pause taint still on gpu-1 after resume")
	}
	if _, err := inside.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, PolicyName, metav1.GetOptions{}); err == nil {
		t.Fatal("admission policy left after resume")
	}
	if s := sts(t, host, ns, "private-a"); *s.Spec.Replicas != 1 {
		t.Fatal("control plane not scaled back up")
	}
	if st := e.Pauses(); len(st) != 1 || st[0].Phase != "running" || st[0].Error != "" {
		t.Fatalf("status after resume = %+v", st)
	}
}

func TestPausedIsTheMostSevereStateAndRulesAcceptIt(t *testing.T) {
	ts := effective("acme", map[string]Hold{
		"billing": {State: Suspended, Reason: "unpaid"},
		"abuse":   {State: Paused, Reason: "mining"},
	})
	if ts.State != Paused || ts.Hold != "abuse" {
		t.Fatalf("effective = %+v", ts)
	}
	rules, err := ParseRules("alerts.low_remaining_credit_balance_reached=paused")
	if err != nil || rules["alerts.low_remaining_credit_balance_reached"] != Paused {
		t.Fatalf("rules = %v, %v", rules, err)
	}
	// Annotate mode records and labels a pause but never stops anything.
	host := fake.NewSimpleClientset(controlPlane("a", "ns-a", 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-a"}})
	cl := discovery.TenantCluster{Name: "a", Namespace: "ns-a", Source: "statefulset"}
	e := New(host, Options{Mode: "annotate", Now: time.Now, Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	if err := e.Set(context.Background(), "acme", Paused, "x", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	if s := sts(t, host, "ns-a", "a"); *s.Spec.Replicas != 1 || s.Annotations[AnnotationState] != "paused" {
		t.Fatalf("annotate mode: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
}

func TestInterruptedPauseIsFinishedAtResync(t *testing.T) {
	// vBilling restarted after marking the cluster paused but before
	// stopping it: the next pass drains and stops it, keeping the original
	// pause time.
	ns := "vc-b"
	at := "2026-11-18T09:00:00.123456789Z"
	host := fake.NewSimpleClientset(
		controlPlane("b", ns, 1, map[string]string{discovery.AnnotationPausedReplicas: "1",
			discovery.AnnotationPausedBy: "vbilling", discovery.AnnotationPausedAt: at}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
	)
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-9"}}, podOn("job", "ml", "gpu-9"))
	cl := discovery.TenantCluster{Name: "b", Namespace: ns, Source: "statefulset"}
	state := kvWith(t, "acme", Paused)
	e := New(host, Options{Mode: "enforce", Now: time.Now, State: state, DrainTimeout: 200 * time.Millisecond,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
	})
	e.Resync(context.Background())
	e.Wait()
	s := sts(t, host, ns, "b")
	if *s.Spec.Replicas != 0 || s.Annotations[discovery.AnnotationPausedAt] != at {
		t.Fatalf("after resync: replicas %d, annotations %v", *s.Spec.Replicas, s.Annotations)
	}
	if podNames(t, inside, "ml")["job"] {
		t.Fatal("the interrupted pause did not drain the cluster's own node")
	}
}

// kvWith returns a state store in which tenant already has a billing hold.
func kvWith(t *testing.T, tenant string, s State) *kv.Store {
	t.Helper()
	st := kv.Memory()
	ts := TenantState{Tenant: tenant, State: s, Hold: HoldBilling, Since: time.Now().UTC(),
		Holds: map[string]Hold{HoldBilling: {State: s, Reason: "wallet empty", Since: time.Now().UTC()}}}
	b, _ := json.Marshal(ts)
	if err := st.Set("state/"+tenant, string(b)); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPauseExternalClusterKeepsOneSpan(t *testing.T) {
	inside := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "edge-1"}}, podOn("infer", "ml", "edge-1"))
	cl := discovery.TenantCluster{Name: "edge", External: true}
	spans := pause.Memory()
	notified := 0
	e := New(fake.NewSimpleClientset(), Options{Mode: "enforce", Now: time.Now, DrainTimeout: 200 * time.Millisecond, Pauses: spans,
		Clusters:     func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} },
		TenantClient: func(context.Context, discovery.TenantCluster) (kubernetes.Interface, error) { return inside, nil },
		OnPause:      func(string, bool) { notified++ },
	})
	ctx := context.Background()
	if err := e.Set(ctx, "acme", Paused, "wallet empty", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	e.Resync(ctx)
	e.Wait()
	if got := spans.Spans(cl.ExternalID()); len(got) != 1 || !got[0].Open() {
		t.Fatalf("spans = %+v, want one open span across resyncs", got)
	}
	n, _ := inside.CoreV1().Nodes().Get(ctx, "edge-1", metav1.GetOptions{})
	if !hasTaint(n, TaintPaused) || podNames(t, inside, "ml")["infer"] {
		t.Fatal("external cluster not drained")
	}
	if notified != 0 {
		t.Fatal("metering was told to read a saved inventory for a cluster whose API stays up")
	}
	if err := e.Set(ctx, "acme", Active, "paid", "api", "manual"); err != nil {
		t.Fatal(err)
	}
	e.Wait()
	n, _ = inside.CoreV1().Nodes().Get(ctx, "edge-1", metav1.GetOptions{})
	if got := spans.Spans(cl.ExternalID()); len(got) != 1 || got[0].Open() || hasTaint(n, TaintPaused) {
		t.Fatalf("after resume: spans %+v, taints %v", got, n.Spec.Taints)
	}
}

func TestGuardPausesAgainAtOnce(t *testing.T) {
	ns := "vc-g"
	host := fake.NewSimpleClientset(controlPlane("g", ns, 1, nil), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	cl := discovery.TenantCluster{Name: "g", Namespace: ns, Source: "statefulset"}
	e := New(host, Options{Mode: "enforce", Now: time.Now, State: kvWith(t, "acme", Paused),
		Clusters: func(string) []discovery.TenantCluster { return []discovery.TenantCluster{cl} }})
	ctx := context.Background()
	e.Guard(ctx) // a cluster created while its tenant is paused
	e.Wait()
	if s := sts(t, host, ns, "g"); *s.Spec.Replicas != 0 || s.Annotations[discovery.AnnotationPausedBy] != "vbilling" {
		t.Fatalf("guard did not pause a new cluster: %d %v", *s.Spec.Replicas, s.Annotations)
	}
	// Scaled back up by hand (or woken by the Platform): paused again.
	if _, err := host.AppsV1().StatefulSets(ns).Patch(ctx, "g", "application/merge-patch+json", []byte(`{"spec":{"replicas":1}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Guard(ctx)
	e.Wait()
	if s := sts(t, host, ns, "g"); *s.Spec.Replicas != 0 {
		t.Fatal("guard did not pause a cluster scaled back up")
	}
	// Nothing to do: no work started.
	e.Guard(ctx)
	e.Wait()
}
