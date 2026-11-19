package pricing

import (
	"context"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// UncountedReporter is implemented by price checkers whose billing platform
// never counts usage it receives before something that counts it exists:
// Metronome attributes usage only to billable metrics that existed when it
// arrived. Sending such usage is pointless, so its series is held in every
// PRICE_CHECK mode but off, released in order once the platform counts it,
// and released unpriced after the longest hold (pipeline.MaxUnpricedHold),
// like any held usage.
type UncountedReporter interface {
	// Uncounted reports whether a coverage answer says the platform would
	// not count the series' usage at all yet.
	Uncounted(c destinations.PriceCoverage) bool
}

// Gate returns the dispatcher gate: in hold mode for every destination that
// checks prices, in warn mode for those that report usage their platform
// would not count (nil when there are none, or with PRICE_CHECK=off).
func (s *Service) Gate() pipeline.Gate {
	if !s.Enabled() {
		return nil
	}
	switch s.opts.Mode {
	case ModeHold:
		return gate{s}
	case ModeWarn:
		for _, name := range s.order {
			if s.dests[name].uc != nil {
				return gate{s}
			}
		}
	}
	return nil
}

type gate struct{ s *Service }

// gated reports whether the gate decides for ds: every destination in hold
// mode, in warn mode only those that report uncounted usage.
func (s *Service) gated(ds *destState) bool {
	return s.opts.Mode == ModeHold || ds.uc != nil
}

// holds reports whether new events of a series must wait. In hold mode:
// their platform has no price for it, or no check has answered for it yet.
// A series a check could not decide (unknown, e.g. no rate card configured)
// is not held: the check cannot tell when to release it. In warn mode: only
// while the platform would not count its usage at all. s.mu held.
func (s *Service) holds(st *seriesState) bool {
	if s.opts.Mode == ModeHold {
		return st == nil || st.status == "" || st.status == destinations.PriceMissing
	}
	return st != nil && st.uncounted
}

// releasable reports whether a held series may ship. Held events need an
// answer that lets them go: in warn mode, a check must have found that the
// platform counts the usage now, so a restart (which forgets every answer)
// never sends uncounted usage before a check. A destination the gate does
// not decide for in warn mode holds nothing: what an earlier run in hold
// mode left ships. s.mu held.
func (s *Service) releasable(ds *destState, st *seriesState) bool {
	switch {
	case s.opts.Mode == ModeHold:
		return !s.holds(st)
	case ds.uc == nil:
		return true
	default:
		return st != nil && st.status != "" && !st.uncounted
	}
}

func reason(st *seriesState, lastErr string) string {
	switch {
	case st != nil && st.uncounted:
		r := "not counted yet: " + st.detail
		if st.hint != "" {
			r += " (hint: " + st.hint + ")"
		}
		return r
	case st != nil && st.status == destinations.PriceMissing:
		r := "unpriced: " + st.detail
		if st.hint != "" {
			r += " (hint: " + st.hint + ")"
		}
		return r
	case lastErr != "":
		return "price not confirmed yet: last check failed: " + lastErr
	default:
		return "price not confirmed yet"
	}
}

// Decide holds the events of series their destination has no price for (hold
// mode), or would not count (both modes). Series seen for the first time are
// checked before deciding, for up to DecideWait, so a new series that is
// priced already never waits.
func (g gate) Decide(ctx context.Context, dest string, events []usage.Event) []pipeline.Decision {
	s := g.s
	ds := s.dests[dest]
	if ds == nil || !s.gated(ds) {
		return nil
	}
	keys := make([]string, len(events))
	now := s.now()
	waitFor := false
	s.mu.Lock()
	for i := range events {
		if !priced(ds, events[i].Metric) {
			continue
		}
		key, _ := s.observeLocked(ds, &events[i], now)
		keys[i] = key
		if st := ds.series[key]; st.status == "" && !st.attempted {
			waitFor = true
		}
	}
	s.mu.Unlock()
	if waitFor {
		s.checkWithin(ctx, ds, s.opts.DecideWait)
	}

	out := make([]pipeline.Decision, len(events))
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, key := range keys {
		if key == "" {
			continue
		}
		st := ds.series[key]
		out[i] = pipeline.Decision{Series: key, Hold: s.holds(st)}
		if out[i].Hold {
			out[i].Reason = reason(st, ds.lastErr)
		}
	}
	return out
}

// Releasable reports whether a held series may ship. A series the service
// does not know (after a restart, or because the tenant's plan changed) is
// looked up by its sample event's current series, and checked soon.
func (g gate) Releasable(ctx context.Context, dest, series string, sample *usage.Event) bool {
	s := g.s
	ds := s.dests[dest]
	if ds == nil {
		return true // the destination no longer checks prices
	}
	s.mu.Lock()
	st := ds.series[series]
	isNew := false
	if st == nil {
		var key string
		key, isNew = s.observeLocked(ds, sample, s.now())
		st = ds.series[key]
	}
	s.mu.Unlock()
	if isNew {
		s.Kick(dest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releasable(ds, st)
}

// ReleasedUnpriced records held events released without a price after
// MaxHold, and turns the series' health entry failing at once.
func (g gate) ReleasedUnpriced(dest, series string, sample *usage.Event, n int) {
	s := g.s
	ds := s.dests[dest]
	if ds == nil {
		return
	}
	s.mu.Lock()
	st := ds.series[series]
	if st == nil {
		key, _ := s.observeLocked(ds, sample, s.now())
		st = ds.series[key]
	}
	st.releasedUnpriced += n
	st.releasedAt = s.now().UTC()
	s.mu.Unlock()
	s.report(ds)
}

// Changed is closed when a check finds that a series which had to wait may
// ship now.
func (g gate) Changed() <-chan struct{} {
	g.s.mu.Lock()
	defer g.s.mu.Unlock()
	return g.s.changed
}

// checkWithin runs a check of ds and waits for it up to d.
func (s *Service) checkWithin(ctx context.Context, ds *destState, d time.Duration) {
	s.mu.Lock()
	base := s.ctx
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.check(base, ds)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}
