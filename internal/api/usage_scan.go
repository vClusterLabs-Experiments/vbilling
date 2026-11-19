package api

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// sameWindowSpan is how far behind the newest window of the scan a window
// still holds the IDs of its events from windowed sources (see usageScan).
// A second copy of such an event has the same window and is committed
// within MAX_BACKFILL (6h) of the first.
const sameWindowSpan = 8 * time.Hour

// usageScan finds the event IDs a usage scan has seen already, in memory
// that does not grow with the range:
//
//   - Sources that append through the ingest index (ingest:<client>, and
//     tenant-api-fill) can store an ID again once the index forgot it (48h):
//     their IDs are kept for the whole scan.
//   - Every other source commits each window once (the ledger refuses a
//     second commit of a window), so its own IDs never repeat. Another
//     source can hold the same ID only for the same window (IDs derive
//     from it): as an earlier version did when a metric moved between the
//     collector and Prometheus, or when a fill or an ingested event took
//     the ID of a measured one, before IDs were namespaced. Those IDs are
//     kept for the windows of the last sameWindowSpan of the scan.
//   - Records (per-request usage, aggregated) carry no resource ID, so
//     their IDs never meet the collector's; they are checked, not kept.
//
// IDs are kept as 8 bytes of their SHA-256.
type usageScan struct {
	repeatable map[uint64]struct{}
	windowed   map[uint64]struct{}
	byEnd      map[time.Time][]uint64 // windowed IDs by window end, to forget old windows
	newest     time.Time
	peak       int // most IDs kept at once
}

func newUsageScan() *usageScan {
	return &usageScan{repeatable: map[uint64]struct{}{}, windowed: map[uint64]struct{}{}, byEnd: map[time.Time][]uint64{}}
}

// repeatableSource reports whether a ledger source can store an event ID
// again: it appends through the ingest index, which remembers IDs for 48
// hours.
func repeatableSource(source string) bool {
	return strings.HasPrefix(source, "ingest:") || source == "tenant-api-fill"
}

// aggregatedSource reports whether a source holds records aggregated per
// window: no resource ID, never an ID of another source.
func aggregatedSource(source string) bool {
	return source == "records" || source == "records-late"
}

// seen reports whether an earlier event of the scan had e's ID, and
// remembers e's ID as the class of its source requires.
func (u *usageScan) seen(e *usage.Event) bool {
	sum := sha256.Sum256([]byte(e.ID))
	k := binary.BigEndian.Uint64(sum[:8])
	if _, ok := u.repeatable[k]; ok {
		return true
	}
	if _, ok := u.windowed[k]; ok {
		return true
	}
	switch {
	case repeatableSource(e.Source):
		u.repeatable[k] = struct{}{}
	case aggregatedSource(e.Source):
		return false
	default:
		end := e.WindowEnd.UTC()
		if end.Before(u.newest.Add(-sameWindowSpan)) {
			return false // a window long gone: its copies were committed long ago
		}
		u.windowed[k] = struct{}{}
		u.byEnd[end] = append(u.byEnd[end], k)
		if end.After(u.newest) {
			u.newest = end
			u.forget()
		}
	}
	if n := len(u.repeatable) + len(u.windowed); n > u.peak {
		u.peak = n
	}
	return false
}

// forget drops the windowed IDs of windows older than sameWindowSpan
// before the newest.
func (u *usageScan) forget() {
	cutoff := u.newest.Add(-sameWindowSpan)
	for end, ids := range u.byEnd {
		if end.Before(cutoff) {
			for _, k := range ids {
				delete(u.windowed, k)
			}
			delete(u.byEnd, end)
		}
	}
}

// scanUsage visits the usage events whose window overlaps [from, to), the
// archived ones included, each event ID once: the first copy, which
// Stripe, Metronome and Lago (on Postgres) keep. A second copy of an ID is
// billed nowhere, so it is not usage. It returns how many it skipped.
//
// Its memory grows with the events of sources that can repeat an ID
// (ingest, fills) in the range and with the last sameWindowSpan of the
// others, not with a month of collector windows (see usageScan).
func (s *Server) scanUsage(from, to time.Time, fn func(*usage.Event) error) (int, error) {
	dups, _, err := s.scanUsageStats(from, to, fn)
	return dups, err
}

// scanUsageStats is scanUsage that also reports the most IDs it kept at
// once.
func (s *Server) scanUsageStats(from, to time.Time, fn func(*usage.Event) error) (dups, kept int, err error) {
	u := newUsageScan()
	err = s.spool.ScanAll(from, to, func(e *usage.Event) error {
		if u.seen(e) {
			dups++
			return nil
		}
		return fn(e)
	})
	return dups, u.peak, err
}

// undelivered is usage in the ledger that a destination does not bill (yet):
// dead-lettered (refused for good) or held at the pricing gate.
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
