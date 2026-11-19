package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/discovery"
	"github.com/vclusterlabs-experiments/vbilling/internal/metrics"
	"github.com/vclusterlabs-experiments/vbilling/internal/safeguard"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// scriptedTenantAPI answers Conn calls with a scripted sequence of outcomes
// (true = the cluster's API answers), then answers from then on.
type scriptedTenantAPI struct {
	mu     sync.Mutex
	script []bool
	calls  int
	conn   *metrics.TenantConn
}

func (s *scriptedTenantAPI) Conn(context.Context, discovery.TenantCluster) (*metrics.TenantConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	up := true
	if s.calls < len(s.script) {
		up = s.script[s.calls]
	}
	s.calls++
	if !up {
		return nil, errors.New("connection refused")
	}
	return s.conn, nil
}

// A flaky cluster API (a Platform proxy that times out now and then) fails,
// answers, fails again. The window in between is committed with the
// cluster's own GPU node, and its fill attempt in that same pass fails, so
// the gap stays. The next failure extends the gap over that committed
// window (fillTenantGaps keeps the gap's first window and moves its last).
// When the API answers again, the fill re-meters the committed window too,
// and since v0.3.1 salts fill IDs, billing platforms no longer drop it as a
// duplicate: the GPU node is billed twice for that window.
func TestReview3FlakyTenantAPIFillNeverBillsACommittedWindowTwice(t *testing.T) {
	h := newHarness(t)
	created := metav1.NewTime(t0.Add(-time.Hour))
	gpuNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-h100-1", CreationTimestamp: created,
		Labels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"}},
		Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("96"),
			corev1.ResourceMemory: resource.MustParse("1024Gi"), "nvidia.com/gpu": resource.MustParse("8")},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	// Calls: W1 main (fails); W2 main (answers), W2's fill of W1 (fails);
	// W3 main (fails); W4 main and W4's fill: the API answers again.
	api := &scriptedTenantAPI{script: []bool{false, true, false, false},
		conn: &metrics.TenantConn{Kube: fake.NewSimpleClientset(gpuNode)}}
	coll := metrics.NewCollector(fake.NewSimpleClientset(), nil, metrics.Options{GPUResources: []string{"nvidia.com/gpu"},
		Basis: "requests", Region: "ap-southeast-2", Now: func() time.Time { return h.now }})
	coll.UseTenantAPI(api)
	h.c.coll = coll
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "gpu", nil)}
	h.c.Reconcile(ctx)

	for i := 0; i < 4; i++ { // windows ending 10:00, 10:01, 10:02, 10:03
		h.c.CollectDue(ctx)
		h.now = h.now.Add(time.Minute)
	}

	byWindow := map[time.Time][]usage.Event{}
	nodeHours := map[time.Time]float64{}
	recs, err := h.sp.Read(0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Event == nil {
			continue
		}
		e := *r.Event
		byWindow[e.WindowEnd] = append(byWindow[e.WindowEnd], e)
		if e.Metric == usage.MetricPrivateNodeHours {
			nodeHours[e.WindowEnd] += e.Quantity
		}
	}
	w2 := t0.Truncate(time.Minute).Add(time.Minute) // 10:01, the window collected while the API answered
	if len(nodeHours) == 0 {
		t.Fatalf("no private node-hours in the ledger: %+v", recs)
	}
	for end, h := range nodeHours {
		if h > 1.0/60+1e-6 {
			t.Errorf("window ending %s: GPU node billed %.6f node-hours (window is %.6f): committed once and filled again under a salted ID",
				end.Format("15:04"), h, 1.0/60)
		}
	}
	if findings, _ := safeguard.Check(w2.Add(-time.Minute), w2, byWindow[w2]); len(findings) > 0 {
		t.Errorf("vBilling's own safeguard flags the window: %v", findings)
	}
}

// A tenant deletes its last cluster but keeps calling the AI Cloud's
// inference API, so gateway records keep arriving under its ID. Offboarding
// only looks at clusters: an hour later every destination's RemoveTenant
// runs (Lago terminates the subscription and ignores the tenant's later
// events, "Events for subscriptions that have been terminated are
// ingested, but ignored"), and the tenant leaves the registry, although its
// usage is still flowing. v0.3.1 made offboarding wait for the tenant's last
// ledger event to be delivered, which a tenant with ongoing usage passes
// within seconds of every window.
func TestReview3TenantWithOngoingRecordsIsNotOffboarded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.disc.clusters = []discovery.TenantCluster{cluster("team-a", "train", map[string]string{discovery.AnnotationPrefix + "tenant": "acme"})}
	h.c.Reconcile(ctx)
	h.sp.Cursor("fake")
	h.disc.clusters = nil // the last cluster is deleted
	h.c.Reconcile(ctx)
	for i := 1; i <= 61; i++ {
		h.now = h.now.Add(time.Minute)
		end := h.now.Truncate(time.Minute)
		// Gateway usage committed by the records aggregator (a token metric in
		// production; any billable metric shows the same).
		ev := usage.Event{Tenant: "acme", Metric: usage.MetricNetworkEgressGB, Quantity: 1.5, WindowStart: end.Add(-time.Minute), WindowEnd: end, SKU: "llama-3.1-70b"}
		ev.Finalize()
		if err := h.sp.AppendWindow("records", end, []usage.Event{ev}, ""); err != nil {
			t.Fatal(err)
		}
		if err := h.sp.Ack("fake", h.sp.Committed()); err != nil { // the dispatcher delivers it within seconds
			t.Fatal(err)
		}
		h.c.Reconcile(ctx)
	}
	if len(h.dest.removed) > 0 {
		t.Errorf("tenant %v offboarded (subscription terminated in Lago) while its gateway usage still arrives every minute", h.dest.removed)
	}
}
