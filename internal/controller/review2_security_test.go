package controller

// Second independent review (v0.3.0). Every TestReview2* test here fails on
// v0.3.0 and describes the expected behavior.

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/config"
	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/spool"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Discovery marks a cluster paused from its control plane workload's
// annotations alone (loft.sh/paused=true, or vbilling.vcluster.com/paused-by
// =vbilling with a paused-at of the tenant's choosing), whatever its
// replicas. observePauses then opens a pause span, and metering clips the
// control plane hours, the pods and the dedicated or private nodes of a
// cluster that is up and serving. Whoever writes vcluster.yaml sets those
// annotations (controlPlane.statefulSet.annotations), and in Platform mode
// those people are exactly the ones the docs say must not choose who pays
// or at what price. vBilling never lifts a loft.sh/paused it did not set, so
// the cluster runs unbilled indefinitely, in every enforcement mode.
func TestReview2TenantSetPauseAnnotationStopsBillingOfARunningCluster(t *testing.T) {
	// Control: the same running cluster without the annotation bills.
	if gpu, cp, _ := review2Bill(t, nil); gpu == 0 || cp == 0 {
		t.Fatalf("test setup: an unannotated running cluster bills %v GPU-hours, %v control plane hours", gpu, cp)
	}
	gpu, cp, span := review2Bill(t, map[string]string{discovery.AnnotationPaused: "true"}) // from controlPlane.statefulSet.annotations
	if gpu == 0 || cp == 0 {
		t.Fatalf("a ready control plane with replicas=1 and an 8-GPU pod running bills %v GPU-hours and %v control plane hours: "+
			"an annotation from vcluster.yaml opened pause span %+v", gpu, cp, span)
	}
}

// review2Bill meters one window of a running cluster whose StatefulSet
// carries ann, through real discovery, controller and collector.
func review2Bill(t *testing.T, ann map[string]string) (gpuHours, cpHours float64, span pause.Span) {
	t.Helper()
	ctx := context.Background()
	created := metav1.NewTime(t0.Add(-24 * time.Hour))
	one := int32(1)
	kube := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu", Namespace: "team-a", Labels: map[string]string{"app": "vcluster"}, CreationTimestamp: created,
				Annotations: ann},
			Spec:   appsv1.StatefulSetSpec{Replicas: &one},
			Status: appsv1.StatefulSetStatus{Replicas: 1, ReadyReplicas: 1}, // up and serving
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "h100-1", Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100"}, CreationTimestamp: created},
			Status: corev1.NodeStatus{
				Capacity:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64"), corev1.ResourceMemory: resource.MustParse("512Gi"), "nvidia.com/gpu": resource.MustParse("8")},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "trainer-x-default-x-gpu", Namespace: "team-a", Labels: map[string]string{metrics.LabelManagedBy: "gpu"}},
			Spec: corev1.PodSpec{NodeName: "h100-1", Containers: []corev1.Container{{Name: "main", Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "main",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: created}}}}},
		},
	)
	cfg := &config.Config{CollectionInterval: time.Minute, ReconcileInterval: 30 * time.Second, MaxBackfill: time.Hour,
		OffboardGrace: time.Hour, Region: "ap-southeast-2", TenantSource: "cluster", Adapters: []string{"fake"}}
	sp, err := spool.Open(t.TempDir(), spool.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	clock := t0
	coll := metrics.NewCollector(kube, nil, metrics.Options{Region: cfg.Region, GPUResources: []string{"nvidia.com/gpu"}, Basis: "requests",
		PauseKeep: mustKeep(t, pause.DefaultKeep), Now: func() time.Time { return clock }})
	spans := pause.Memory()
	coll.UsePauses(spans)
	c := New(cfg, discovery.NewDiscoverer(kube, nil, nil, ""), coll, sp, pipeline.NewTenantRegistry(), []destinations.Destination{&fakeDest{}}, nil)
	c.now = func() time.Time { return clock }
	c.UsePauses(spans)

	c.Reconcile(ctx)
	clock = t0.Add(3 * time.Minute)
	c.CollectDue(ctx)
	_ = sp.Scan(t0.Add(-time.Hour), t0.Add(time.Hour), func(ev *usage.Event) error {
		switch ev.Metric {
		case usage.MetricGPUHours:
			gpuHours += ev.Quantity
		case usage.MetricInstanceHours:
			cpHours += ev.Quantity
		}
		return nil
	})
	span, _ = spans.Current("vcluster-team-a-gpu")
	return gpuHours, cpHours, span
}

// Prior finding 3 was fixed with c.tenants.Get(alias): an alias that is a
// tenant's ID is not another tenant's gateway identity, and the tenant's ID
// wins in ResolveTenant. The registry only holds tenants discovered from
// clusters. A tenant that only buys tokens (the case the gateway block is
// for: "Tenants that only buy tokens have no cluster") is not in it, so an
// alias equal to its ID still captures it: its records bill to the other
// tenant, suspending the other tenant blocks its team, and suspending it
// blocks nothing (main.go drops identities that bill elsewhere).
func TestReview2AliasMustNotCaptureATokenOnlyTenant(t *testing.T) {
	h := newHarness(t)
	h.disc.clusters = []discovery.TenantCluster{
		cluster("acme", "vc", map[string]string{discovery.AnnotationPrefix + "tenant": "acme", discovery.AnnotationPrefix + "aliases": "tokens-co"}),
	}
	h.c.Reconcile(context.Background())
	// tokens-co has no cluster: it reaches vBilling through a bound ingest
	// token or records, and Stripe or Metronome webhooks name it.
	got, _ := h.c.ResolveTenant("tokens-co")
	ids := h.c.GatewayIdentities("acme")
	for _, id := range ids {
		if id == "tokens-co" || got != "tokens-co" {
			t.Fatalf("token-only tenant tokens-co is captured by acme's alias: its usage bills to %q and GatewayIdentities(acme) = %v", got, ids)
		}
	}
}

func mustKeep(t *testing.T, spec string) pause.Keep {
	t.Helper()
	k, err := pause.ParseKeep(spec)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
