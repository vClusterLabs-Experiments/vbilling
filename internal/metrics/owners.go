package metrics

import (
	"sort"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pause"
)

// A cluster that changes owner inside a window (a Project handed to another
// Tenant, a cluster moved to another project, a tenant label changed) is
// metered as one target per owner. Each target carries the times the
// cluster belonged to the other owners in Target.Elsewhere, so each owner
// is billed for its own part of the window only, and the parts add up to
// what the window would have billed unsplit:
//
//   - object lifetimes (pods, nodes, volumes, load balancers, control plane
//     hours) are clipped to the owner's part, the way exclusions clip them;
//   - a Prometheus counter's increase goes to the owner at the time of the
//     sample that shows it;
//   - a query, an estimate over the whole window, is prorated by the
//     owner's share of the window.
//
// What is read from such a cluster is kept for every owner or for none
// (see ownerReads and the collector's own-API reads).

// NotBilled returns the times nothing of the cluster is billed to this
// target: it was excluded from metering, deleted, or billed to another
// owner.
func (t Target) NotBilled() []pause.Span { return pause.Union(t.Excluded, t.Elsewhere) }

// severalOwners returns the clusters (by external ID) that more than one
// target stands for: one per owner, in a window whose owner changed.
func severalOwners(targets []Target) map[string]bool {
	n := map[string]int{}
	for i := range targets {
		n[targets[i].Cluster.ExternalID()]++
	}
	var out map[string]bool
	for id, c := range n {
		if c > 1 {
			if out == nil {
				out = map[string]bool{}
			}
			out[id] = true
		}
	}
	return out
}

// ownAPIReads are the reads of a cluster's own API for each of its owners
// in one collection, kept only if none failed.
type ownAPIReads struct {
	accs   []map[time.Time]*accumulator
	failed bool
}

func newAccumulators(windows []Window) map[time.Time]*accumulator {
	out := make(map[time.Time]*accumulator, len(windows))
	for _, w := range windows {
		out[w.End] = newAccumulator(w)
	}
	return out
}

// mergeObserved adds what from accumulated to into, readings of zero
// included (an idle GPU's utilization is a reading).
func mergeObserved(from, into map[time.Time]*accumulator) {
	for end, a := range from {
		for _, k := range a.order {
			ev := *a.events[k]
			qty, props := ev.Quantity, ev.Properties
			ev.Quantity, ev.Properties = 0, nil
			into[end].observe(ev, qty, props)
		}
	}
}

func blankPromWindows(windows []Window) []PromWindow {
	out := make([]PromWindow, len(windows))
	for i, w := range windows {
		out[i] = PromWindow{Window: w, updates: map[string]counterSample{}, seen: map[string]time.Time{}, shared: map[string]time.Time{}}
	}
	return out
}

// mergePromWindows adds what one owner's read left in from to into. The
// owners of a cluster read the same counter series from the same
// baselines, so their baseline updates agree.
func mergePromWindows(from, into []PromWindow) {
	for i := range from {
		for k, v := range from[i].updates {
			into[i].updates[k] = v
		}
		for k, v := range from[i].seen {
			into[i].seen[k] = v
		}
		for k, v := range from[i].shared {
			into[i].shared[k] = v
		}
		into[i].unshared = append(into[i].unshared, from[i].unshared...)
		into[i].Late += from[i].Late
	}
}

// ownerReads holds, for the clusters with several owners in a run, each
// owner's read of each metric until all of them are known. A counter's
// baselines are the cluster's, shared by its owners: if one owner's read
// failed while another's advanced them, the failed owner's part would
// never be billed. So a metric's reads are kept for every owner of a
// cluster, or, when any of them failed, for none (and the counter catches
// up in a later window, as after any failed read).
type ownerReads struct {
	windows []Window
	several map[string]bool
	reads   map[string][]*ownerRead // by metric code and cluster
	order   []string
}

// ownerRead is one owner's read of one metric of a cluster.
type ownerRead struct {
	out     []PromWindow
	accs    map[time.Time]*accumulator
	n, late int
	err     error
	report  func(n, late int, err error)
}

func newOwnerReads(targets []Target, windows []Window) *ownerReads {
	return &ownerReads{windows: windows, several: severalOwners(targets), reads: map[string][]*ownerRead{}}
}

// start returns where one target's read of a metric goes: out and accs
// themselves for a cluster with one owner (read is nil), else a copy.
func (s *ownerReads) start(cluster, code string, out []PromWindow, accs map[time.Time]*accumulator) ([]PromWindow, map[time.Time]*accumulator, *ownerRead) {
	if !s.several[cluster] {
		return out, accs, nil
	}
	r := &ownerRead{out: blankPromWindows(s.windows), accs: newAccumulators(s.windows)}
	key := code + "|" + cluster
	if _, known := s.reads[key]; !known {
		s.order = append(s.order, key)
	}
	s.reads[key] = append(s.reads[key], r)
	return r.out, r.accs, r
}

// finish keeps the reads of every metric whose owners all succeeded,
// reports each read, and returns how many reads count as failed.
func (s *ownerReads) finish(out []PromWindow, accs map[time.Time]*accumulator) (failed int) {
	keys := append([]string(nil), s.order...)
	sort.Strings(keys)
	for _, key := range keys {
		reads := s.reads[key]
		var err error
		for _, r := range reads {
			if r.err != nil && err == nil {
				err = r.err
			}
		}
		for _, r := range reads {
			if err != nil {
				failed++
				if r.report != nil {
					r.report(0, 0, err)
				}
				continue
			}
			mergePromWindows(r.out, out)
			mergeObserved(r.accs, accs)
			if r.report != nil {
				r.report(r.n, r.late, nil)
			}
		}
	}
	return failed
}
