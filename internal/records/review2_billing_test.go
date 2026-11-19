package records

import (
	"fmt"
	"testing"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/health"
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

// TestReview2TwoReplicasSplitRecordsKeepTheFullTotal: the README says two
// replicas (each with its own PVC) are safe because both compute the same
// deterministic IDs. That holds for Kubernetes usage, which both replicas
// measure in full. Records are pushed to the Service, so each replica
// receives a share of a window's requests. Each replica's aggregator then
// commits a base event with the same ID (the ID excludes the quantity) but
// only its own share as the quantity. Stripe (identifier), Metronome and
// Lago (transaction_id) keep the first copy of an ID, so a share is never
// invoiced.
func TestReview2TwoReplicasSplitRecordsKeepTheFullTotal(t *testing.T) {
	a := newRig(t, Options{})
	defer a.close()
	b := newRig(t, Options{})
	defer b.close()

	// 10 requests for one tenant in the window 10:00-10:01, 100 output
	// tokens each. The Service load-balances the gateway's posts: requests
	// 0-2 reach replica A, 3-9 reach replica B.
	for i := 0; i < 10; i++ {
		r := a
		if i >= 3 {
			r = b
		}
		r.clk.set(t0.Add(time.Duration(i+1) * time.Second))
		r.post(t, Native, native(fmt.Sprintf("req-%d", i), "tenant-a", t0.Add(time.Duration(i)*time.Second), 0, 100))
	}
	// Both replicas commit the window once it settles.
	for _, r := range []*rig{a, b} {
		r.clk.set(t0.Add(2 * time.Minute))
		if err := r.agg.Tick(); err != nil {
			t.Fatal(err)
		}
	}

	// A billing platform deduplicates on the event ID and keeps the first
	// copy it receives: replica A delivers first.
	kept := map[string]usage.Event{}
	for _, r := range []*rig{a, b} {
		for id, e := range r.ledgerEvents(t) {
			if e.Metric != "tokens_out" {
				continue
			}
			if prev, dup := kept[id]; dup {
				if prev.Quantity != e.Quantity {
					t.Errorf("replicas committed event %s with different quantities (%v and %v): a billing platform keeps only the first", id, prev.Quantity, e.Quantity)
				}
				continue
			}
			kept[id] = e
		}
	}
	if got := total(kept, "tokens_out"); got != 1000 {
		t.Errorf("billed %v output tokens after the platform's dedupe, want 1000 (10 requests x 100)", got)
	}
}
