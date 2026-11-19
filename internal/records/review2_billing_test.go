package records

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
	"github.com/vclusterlabs-experiments/vbilling/internal/lease"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// TestReview2RecordsSurviveACollectionIntervalChange: records are bucketed
// at ingest into windows aligned to the epoch (usage.Window), but the
// aggregator walks base windows from its watermark in steps of the current
// COLLECTION_INTERVAL and looks records up by exact window end. After the
// interval changes (60s to 5m here, a documented setting with no warning),
// a watermark that is not a multiple of the new size (10:03) makes every
// step (10:08, 10:13, ...) miss every record window (10:05, 10:10, ...):
// each window is committed empty and on-time records are never billed, for
// as long as vBilling runs.
func TestReview2RecordsSurviveACollectionIntervalChange(t *testing.T) {
	r := newRig(t, Options{})
	defer r.close()
	// Running with 60s windows; the base watermark reaches 10:03.
	r.clk.set(t0.Add(10 * time.Second))
	r.post(t, Native, native("req-0", "tenant-a", t0.Add(5*time.Second), 0, 100))
	r.clk.set(t0.Add(3*time.Minute + 30*time.Second))
	if err := r.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	if wm, _ := r.ledger.Watermark(SourceBase); !wm.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("setup: base watermark %s", wm)
	}
	// Restarted with COLLECTION_INTERVAL=5m.
	o := Options{Now: r.clk.now, Window: 5 * time.Minute}
	formats, err := LoadFormats("", true)
	if err != nil {
		t.Fatal(err)
	}
	h := health.New(nil)
	h.SetLogger(func(string, ...any) {})
	in := NewIngester(r.records, formats, o, nil, h, nil)
	agg := NewAggregator(r.records, r.ledger, o, h, nil)

	r.clk.set(t0.Add(6 * time.Minute))
	if _, err := in.Ingest(Native, Native, "", []byte(native("req-1", "tenant-a", t0.Add(6*time.Minute), 0, 100))); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Duration{11, 16, 21, 26} {
		r.clk.set(t0.Add(at * time.Minute))
		if err := agg.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if got := total(r.ledgerEvents(t), "tokens_out"); got != 200 {
		t.Fatalf("billed %v output tokens, want 200: 100 before and 100 (the request at 10:06) after the interval change", got)
	}
}

// TestReview2TwoReplicasSplitRecordsKeepTheFullTotal: the review ran two
// replicas, each with its own ledger, behind one Service. The gateway's
// posts split between them, both committed their share of a window under
// the same event ID (the ID excludes the quantity), and billing platforms
// keep one copy of an ID: a share was never invoiced.
//
// v0.3.1 does not tolerate a split, it prevents one. vBilling runs as one
// instance: a process takes the vbilling Lease before it takes records,
// aggregates, delivers or enforces (cmd/vbilling holdLease), a second one
// waits without being Ready, and the chart refuses replicaCount above 1.
// This runs two processes the way the binary does: B starts while A holds
// the Lease and never aggregates or commits anything, every request of the
// window reaches A, and A bills all 1000 tokens. B takes over only once A
// released the Lease.
func TestReview2TwoReplicasSplitRecordsKeepTheFullTotal(t *testing.T) {
	kube := fake.NewSimpleClientset()
	leaseOpts := func(id string) lease.Options {
		return lease.Options{Namespace: "vbilling-system", Identity: id, LeaseDuration: time.Second,
			RenewDeadline: 600 * time.Millisecond, RetryPeriod: 100 * time.Millisecond, Logf: func(string, ...any) {}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newRig(t, Options{})
	defer a.close()
	b := newRig(t, Options{})
	defer b.close()

	heldA, err := lease.Acquire(ctx, kube, leaseOpts("a"))
	if err != nil {
		t.Fatal(err)
	}
	// B starts. Like cmd/vbilling, it runs its aggregator (and its API,
	// which takes records) only once it holds the Lease.
	bRunning, bDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(bDone)
		heldB, err := lease.Acquire(ctx, kube, leaseOpts("b"))
		if err != nil {
			return
		}
		defer heldB.Release()
		close(bRunning)
		b.clk.set(t0.Add(2 * time.Minute))
		if err := b.agg.Tick(); err != nil {
			t.Error(err)
		}
	}()

	// 10 requests for one tenant in the window 10:00-10:01, 100 output
	// tokens each. B is not Ready, so the Service sends every post to A.
	for i := 0; i < 10; i++ {
		a.clk.set(t0.Add(time.Duration(i+1) * time.Second))
		a.post(t, Native, native(fmt.Sprintf("req-%d", i), "tenant-a", t0.Add(time.Duration(i)*time.Second), 0, 100))
	}
	a.clk.set(t0.Add(2 * time.Minute))
	if err := a.agg.Tick(); err != nil {
		t.Fatal(err)
	}
	// Longer than two lease durations: A renews, B keeps waiting.
	select {
	case <-bRunning:
		t.Fatal("a second instance started while the first holds the Lease")
	case <-time.After(2500 * time.Millisecond):
	}
	if wm, ok := b.ledger.Watermark(SourceBase); ok || b.ledger.Committed() != 0 {
		t.Fatalf("the waiting instance committed to its ledger (watermark %s, seq %d)", wm, b.ledger.Committed())
	}
	if got := total(a.ledgerEvents(t), "tokens_out"); got != 1000 {
		t.Fatalf("the instance holding the Lease billed %v output tokens, want 1000 (10 requests x 100)", got)
	}

	// A stops; only now does B take over.
	heldA.Release()
	select {
	case <-bRunning:
	case <-time.After(5 * time.Second):
		t.Fatal("the second instance did not take over the released Lease")
	}
	<-bDone
	// What billing platforms keep after deduplicating on event IDs: one
	// instance committed the window, in full.
	kept := map[string]usage.Event{}
	for _, r := range []*rig{a, b} {
		for id, e := range r.ledgerEvents(t) {
			if prev, dup := kept[id]; dup {
				if prev.Quantity != e.Quantity {
					t.Errorf("both instances committed event %s (%v and %v)", id, prev.Quantity, e.Quantity)
				}
				continue
			}
			kept[id] = e
		}
	}
	if got := total(kept, "tokens_out"); got != 1000 {
		t.Errorf("billed %v output tokens after the platform's dedupe, want 1000", got)
	}
}
