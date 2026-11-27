package spool

import (
	"fmt"
	"io"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// ScanAllSeq is ScanAll with each event's ledger sequence number: it visits
// the committed usage events whose window overlaps [from, to), first those of
// segments that compaction moved to ArchiveDir, then the retained ledger's,
// in sequence order. Reconciliation uses the sequence number to tell usage
// recorded before a destination started from usage it was meant to receive.
// Like ScanAll, it fails rather than skip when archived records are missing.
func (s *Spool) ScanAllSeq(from, to time.Time, fn func(ev *usage.Event, seq uint64) error) error {
	retainedScan := func() error {
		return s.ScanRecordsSeq(from, to, func(ev *usage.Event, _ time.Time, seq uint64) error { return fn(ev, seq) })
	}
	if s.opts.ArchiveDir == "" {
		return retainedScan()
	}
	archiveMu.Lock() // no segment moves from the ledger to the archive meanwhile
	defer archiveMu.Unlock()
	idx, err := cachedArchiveIndex(s.opts.ArchiveDir)
	if err != nil {
		return err
	}
	retained := s.FirstSeq()
	var sr segReader
	var next uint64 // seq the next archived record must carry, 0 before the first
	for _, e := range idx.entries {
		if retained != 0 && e.First >= retained {
			break // still in the ledger: read there
		}
		if next != 0 && e.First != next {
			return fmt.Errorf("%s: expected seq %d, found %d: archived records are missing", e.Name, next, e.First)
		}
		if e.LastAt.Before(from.Add(-scanSkew)) {
			next = e.Last + 1 // written before the range: holds no event in it (see Scan)
			continue
		}
		zr, err := sr.openArchived(s.opts.ArchiveDir, e)
		if err != nil {
			return fmt.Errorf("%s: %w", e.File(), err)
		}
		br := sr.lines(zr)
		next = e.First
		for {
			line, err := readLine(br, &sr.line)
			if err != nil && err != io.EOF {
				return fmt.Errorf("%s: %w", e.File(), err)
			}
			if len(line) == 0 {
				break
			}
			if line[len(line)-1] != '\n' {
				return fmt.Errorf("%s: torn line in an archived segment", e.File())
			}
			seq, ok := peekSeq(line)
			if !ok {
				return fmt.Errorf("%s: malformed line", e.File())
			}
			if seq != next {
				return fmt.Errorf("%s: expected seq %d, found %d: archived records are missing", e.File(), next, seq)
			}
			next = seq + 1
			if isCommitLine(line) {
				continue
			}
			ws, we, ok := peekWindow(line)
			if !ok || !we.After(from) || !ws.Before(to) {
				continue
			}
			rec, _, err := parseLine(line)
			if err != nil {
				return fmt.Errorf("%s: seq %d: %w", e.File(), seq, err)
			}
			if ev := rec.Event; rec.Type == TypeEvent && ev != nil && ev.WindowEnd.After(from) && ev.WindowStart.Before(to) {
				if err := fn(ev, seq); err != nil {
					return err
				}
			}
		}
		if next != e.Last+1 {
			return fmt.Errorf("%s ends at seq %d, %s says %d", e.File(), next-1, e.source(), e.Last)
		}
	}
	return retainedScan()
}
