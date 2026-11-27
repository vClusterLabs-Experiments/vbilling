package metrics

import (
	"math"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Nothing of a cluster is billed for the time it was excluded from
// metering, whatever keeps billing while paused; the rest of the window is.
// Before, a cluster was dropped from metering the moment it was excluded,
// and the whole window was lost on both sides of the exclusion.
func TestExcludedTimeIsNotBilledAndTheRestOfTheWindowIs(t *testing.T) {
	class := "fast"
	objs := []runtime.Object{
		node("n1", nil, 0, true),
		pod("web", "n1", withRequests("2", "4Gi")),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("30Gi")}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "lb", Namespace: ns, Labels: map[string]string{LabelManagedBy: "gpu"}, CreationTimestamp: metav1.NewTime(ws.Add(-time.Hour))},
			Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
			Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}}},
		},
	}
	for _, tc := range []struct {
		name     string
		excluded []pause.Span
		billed   float64 // seconds of the window that are billed
	}{
		{"not excluded", nil, 60},
		{"excluded 15 seconds before the window closed", []pause.Span{{From: ws.Add(45 * time.Second), By: pause.ByExcluded}}, 45},
		{"metered again 44 seconds into the window", []pause.Span{{From: ws.Add(-10 * time.Minute), To: ws.Add(44 * time.Second), By: pause.ByExcluded}}, 16},
		{"excluded for 20 seconds in the middle", []pause.Span{{From: ws.Add(20 * time.Second), To: ws.Add(40 * time.Second), By: pause.ByExcluded}}, 40},
		{"excluded throughout", []pause.Span{{From: ws.Add(-time.Hour), By: pause.ByExcluded}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := target("gpu", true)
			tg.Excluded = tc.excluded
			evs := collect(t, Options{}, objs, latest(), tg)[we]
			h := tc.billed / 3600
			for metric, want := range map[string]float64{
				usage.MetricCPUCoreHours:   2 * h,
				usage.MetricMemoryGBHours:  4 * h,
				usage.MetricStorageGBHours: 30 * h,
				usage.MetricLBHours:        h,
				usage.MetricInstanceHours:  h,
			} {
				if got, _ := find(evs, metric); math.Abs(got-want) > 1e-6 {
					t.Errorf("%s = %v, want %v (%v s billed)", metric, got, want, tc.billed)
				}
			}
		})
	}
}

// A counter must not bill what was counted while its cluster was excluded
// from metering. Before, the cluster was simply not read meanwhile, and the
// first read afterwards billed the whole increase since the last one as
// one lump (24 excluded minutes of requests in one window).
func TestCounterDoesNotBillWhatWasCountedWhileExcluded(t *testing.T) {
	labels := map[string]string{"namespace": ns, "model": "a"}
	fp := &fakeProm{series: []fakeSeries{
		{name: "app_requests_total", labels: labels, samples: every(ws.Add(-10*time.Minute+45*time.Second), ws.Add(40*time.Minute), time.Minute, 60)},
	}}
	srv := httptest.NewServer(fp.handler(t))
	defer srv.Close()
	_, p, _ := promSourceFor(t, srv.URL, "", nil, requests)
	tg := Target{Cluster: discovery.TenantCluster{Name: "gpu", Namespace: ns}, Tenant: "acme"}

	// Read for two windows, then excluded at 10:02:10 for about 24 minutes.
	before := readEach(t, p, tg, windowsFrom(ws, 2))
	if before[ws] != 60 || before[ws.Add(time.Minute)] != 60 {
		t.Fatalf("before the exclusion: %v", before)
	}
	excludedFrom, excludedTo := ws.Add(2*time.Minute+10*time.Second), ws.Add(26*time.Minute+33*time.Second)

	// The window it is excluded in: its step was counted at 10:02:45, after
	// the exclusion began.
	tg.Excluded = []pause.Span{{From: excludedFrom, By: pause.ByExcluded}}
	if got := readEach(t, p, tg, []Window{{Start: ws.Add(2 * time.Minute), End: ws.Add(3 * time.Minute), Latest: true}}); got[ws.Add(2*time.Minute)] != 0 {
		t.Errorf("the window it was excluded in billed %v, want 0: its step was counted after the exclusion began", got[ws.Add(2*time.Minute)])
	}

	// Not read while excluded. Metered again from 10:26:33.
	tg.Excluded = []pause.Span{{From: excludedFrom, To: excludedTo, By: pause.ByExcluded}}
	after := readEach(t, p, tg, []Window{
		{Start: ws.Add(26 * time.Minute), End: ws.Add(27 * time.Minute)},
		{Start: ws.Add(27 * time.Minute), End: ws.Add(28 * time.Minute), Latest: true},
	})
	// 10:26:45 is the first sample after the exclusion; the step before it
	// (10:25:45 to 10:26:45) began while excluded, so it is not billed.
	if got := after[ws.Add(26*time.Minute)]; got != 0 {
		t.Errorf("the first window after the exclusion billed %v, want 0 (before: 1440, the whole excluded time as one lump)", got)
	}
	if got := after[ws.Add(27*time.Minute)]; got != 60 {
		t.Errorf("the second window after the exclusion billed %v, want one step of 60", got)
	}
}
