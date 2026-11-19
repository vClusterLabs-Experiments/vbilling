package api

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// scanUsage visits the usage events whose window overlaps [from, to), the
// archived ones included, each event ID once: the first copy, which
// Stripe, Metronome and Lago (on Postgres) keep. A second copy of an ID is
// billed nowhere, so it is not usage. It returns how many it skipped.
//
// It remembers 16 bytes per event in the range, so a month of a large
// installation takes memory in proportion to its events.
func (s *Server) scanUsage(from, to time.Time, fn func(*usage.Event) error) (int, error) {
	seen := map[[16]byte]struct{}{}
	dups := 0
	err := s.spool.ScanAll(from, to, func(e *usage.Event) error {
		sum := sha256.Sum256([]byte(e.ID))
		k := [16]byte(sum[:16])
		if _, ok := seen[k]; ok {
			dups++
			return nil
		}
		seen[k] = struct{}{}
		return fn(e)
	})
	return dups, err
}

// undelivered is usage in the ledger that a destination does not bill (yet):
// dead-lettered (refused for good) or held until priced.
type undelivered struct {
	dead map[string]map[string]float64 // group key -> destination -> quantity
	held map[string]map[string]float64
}

// undeliveredUsage sums, per group key, the dead-lettered and held events of
// every destination that pass keep and start in [from, to).
func (s *Server) undeliveredUsage(from, to time.Time, keep func(*usage.Event) bool, groupKey func(*usage.Event) string) undelivered {
	out := undelivered{dead: map[string]map[string]float64{}, held: map[string]map[string]float64{}}
	if s.disp == nil {
		return out
	}
	add := func(into map[string]map[string]float64, dest string, e *usage.Event) {
		if e.WindowStart.Before(from) || !e.WindowStart.Before(to) || !keep(e) {
			return
		}
		k := groupKey(e)
		if into[k] == nil {
			into[k] = map[string]float64{}
		}
		into[k][dest] += e.Quantity
	}
	held := s.disp.Held()
	for _, d := range s.disp.Destinations() {
		name := d.Name()
		if dls, err := s.spool.DeadLetters(name); err == nil {
			for i := range dls {
				add(out.dead, name, &dls[i].Event)
			}
		}
		if held != nil {
			_ = held.Each(name, func(h pipeline.HeldEntry) bool {
				add(out.held, name, &h.Event)
				return true
			})
		}
	}
	return out
}

// byDestination rounds per-destination quantities for a response.
func byDestination(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for d, q := range m {
		out[d] = usage.Round(q)
	}
	return out
}

// destinationsCSV renders per-destination quantities as "lago=1;stripe=2".
func destinationsCSV(m map[string]float64) string {
	parts := make([]string, 0, len(m))
	for d, q := range m {
		parts = append(parts, d+"="+strconv.FormatFloat(usage.Round(q), 'f', -1, 64))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}
