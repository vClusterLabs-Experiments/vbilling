package pipeline

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// HeldStore parks events a destination must not receive yet (the billing
// platform would not count them yet, or with PRICE_CHECK=hold has no price
// for their series), durably, until they are released. Each destination and
// series has its own append-only file, in ledger order:
//
//	<dir>/<destination>/<sha256(series)[:16]>.jsonl
//
// An event line is a HeldEntry; {"released_through": N} marks every entry up
// to sequence N as delivered. A drained file is deleted. An event is written
// here (and fsynced) before the destination's cursor moves past it, so a held
// event is never lost; after a crash, at worst a released batch is sent
// again, which destinations deduplicate on event IDs.
type HeldStore struct {
	dir    string
	noSync bool
	now    func() time.Time

	mu    sync.Mutex
	dests map[string]map[string]*heldSeries // destination -> series key ->
}

// HeldEntry is one parked event.
type HeldEntry struct {
	Seq    uint64      `json:"seq"`
	At     time.Time   `json:"at"`
	Series string      `json:"series"`
	Reason string      `json:"reason"`
	Event  usage.Event `json:"event"`

	end int64 // file offset after this entry (set by Peek)
}

// HeldSummary describes the held events of one series.
type HeldSummary struct {
	Series   string      `json:"-"`
	Sample   usage.Event `json:"-"` // the oldest held event
	Count    int         `json:"held_events"`
	FirstSeq uint64      `json:"first_seq"`
	LastSeq  uint64      `json:"last_seq"`
	Since    time.Time   `json:"held_since"`    // when the oldest one was parked
	Oldest   time.Time   `json:"oldest_window"` // window start of the oldest one
	Reason   string      `json:"reason"`
}

type heldSeries struct {
	HeldSummary
	path     string
	offset   int64  // file offset of the first entry not yet released
	released uint64 // highest released sequence number
}

type heldLine struct {
	HeldEntry
	ReleasedThrough uint64 `json:"released_through,omitempty"`
}

// OpenHeld opens (or creates) a held store and indexes what it holds.
func OpenHeld(dir string, noSync bool) (*HeldStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	h := &HeldStore{dir: dir, noSync: noSync, now: time.Now, dests: map[string]map[string]*heldSeries{}}
	dests, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, d := range dests {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(dir, d.Name(), f.Name())
			s, err := h.load(path)
			if err != nil {
				return nil, fmt.Errorf("held events %s: %w", path, err)
			}
			if s == nil {
				os.Remove(path) // fully released before a crash
				continue
			}
			if h.dests[d.Name()] == nil {
				h.dests[d.Name()] = map[string]*heldSeries{}
			}
			h.dests[d.Name()][s.Series] = s
		}
	}
	return h, nil
}

// load indexes one series file, cutting off a torn last line.
func (h *HeldStore) load(path string) (*heldSeries, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	var (
		s     heldSeries
		off   int64
		good  int64
		lines []struct {
			seq uint64
			off int64
			e   HeldEntry
		}
	)
	s.path = path
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 || err != nil && !errors.Is(err, io.EOF) {
			break
		}
		if line[len(line)-1] != '\n' {
			break // torn write: never acknowledged, the dispatcher holds it again
		}
		var l heldLine
		if json.Unmarshal(line, &l) != nil {
			break
		}
		if l.ReleasedThrough > 0 {
			s.released = max(s.released, l.ReleasedThrough)
		} else {
			lines = append(lines, struct {
				seq uint64
				off int64
				e   HeldEntry
			}{l.Seq, off, l.HeldEntry})
		}
		off += int64(len(line))
		good = off
	}
	if st, err := f.Stat(); err == nil && st.Size() > good {
		if err := f.Truncate(good); err != nil {
			return nil, err
		}
	}
	s.offset = good
	for _, l := range lines {
		if l.seq <= s.released {
			continue
		}
		if s.Count == 0 {
			s.offset = l.off
			s.Series, s.Sample, s.FirstSeq = l.e.Series, l.e.Event, l.seq
			s.Since, s.Oldest, s.Reason = l.e.At, l.e.Event.WindowStart, l.e.Reason
		}
		s.Count++
		s.LastSeq = max(s.LastSeq, l.seq)
		if l.e.Event.WindowStart.Before(s.Oldest) {
			s.Oldest = l.e.Event.WindowStart
		}
	}
	if s.Count == 0 {
		return nil, nil
	}
	return &s, nil
}

func seriesFile(series string) string {
	sum := sha256.Sum256([]byte(series))
	return hex.EncodeToString(sum[:8]) + ".jsonl"
}

// Has reports whether a series has events held for dest.
func (h *HeldStore) Has(dest, series string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dests[dest][series] != nil
}

// Count returns how many events are held for dest.
func (h *HeldStore) Count(dest string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.dests[dest] {
		n += s.Count
	}
	return n
}

// Series lists dest's held series, oldest first.
func (h *HeldStore) Series(dest string) []HeldSummary {
	h.mu.Lock()
	out := make([]HeldSummary, 0, len(h.dests[dest]))
	for _, s := range h.dests[dest] {
		out = append(out, s.HeldSummary)
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeq < out[j].FirstSeq })
	return out
}

// Destinations lists the destinations that have held events.
func (h *HeldStore) Destinations() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for d, m := range h.dests {
		if len(m) > 0 {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// Add parks entries (in ledger order) durably. Entries at or below the
// newest sequence already held for their series are skipped: they are the
// same batch read again after a failed delivery.
func (h *HeldStore) Add(dest string, entries []HeldEntry) (added int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.ContainsAny(dest, `/\`) || dest == "" || dest == "." || dest == ".." {
		return 0, fmt.Errorf("invalid destination name %q", dest)
	}
	bySeries := map[string][]HeldEntry{}
	var order []string
	for _, e := range entries {
		if _, ok := bySeries[e.Series]; !ok {
			order = append(order, e.Series)
		}
		bySeries[e.Series] = append(bySeries[e.Series], e)
	}
	if h.dests[dest] == nil {
		h.dests[dest] = map[string]*heldSeries{}
	}
	for _, key := range order {
		s := h.dests[dest][key]
		var buf bytes.Buffer
		var keep []HeldEntry
		for _, e := range bySeries[key] {
			if s != nil && e.Seq <= s.LastSeq {
				continue
			}
			b, err := json.Marshal(e)
			if err != nil {
				return added, err
			}
			buf.Write(b)
			buf.WriteByte('\n')
			keep = append(keep, e)
		}
		if len(keep) == 0 {
			continue
		}
		dir := filepath.Join(h.dir, dest)
		created := s == nil
		if created {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return added, err
			}
			s = &heldSeries{path: filepath.Join(dir, seriesFile(key))}
			s.Series, s.Sample, s.FirstSeq = key, keep[0].Event, keep[0].Seq
			s.Since, s.Oldest, s.Reason = keep[0].At, keep[0].Event.WindowStart, keep[0].Reason
		}
		if err := h.appendSync(s.path, buf.Bytes(), created); err != nil {
			return added, err
		}
		for _, e := range keep {
			s.Count++
			s.LastSeq = e.Seq
			if e.Event.WindowStart.Before(s.Oldest) {
				s.Oldest = e.Event.WindowStart
			}
		}
		s.Reason = keep[len(keep)-1].Reason
		h.dests[dest][key] = s
		added += len(keep)
	}
	return added, nil
}

func (h *HeldStore) appendSync(path string, b []byte, newFile bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if newFile {
		flags |= os.O_TRUNC // whatever a drained series left behind was delivered
	}
	f, err := os.OpenFile(path, flags, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if !h.noSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if newFile && !h.noSync {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Each calls fn with every event held for dest, series by series and in
// ledger order within a series, until fn returns false.
func (h *HeldStore) Each(dest string, fn func(HeldEntry) bool) error {
	h.mu.Lock()
	series := make([]string, 0, len(h.dests[dest]))
	for k := range h.dests[dest] {
		series = append(series, k)
	}
	h.mu.Unlock()
	sort.Strings(series)
	for _, s := range series {
		stop := false
		_, err := h.scan(dest, s, func(e HeldEntry) bool {
			stop = !fn(e)
			return !stop
		})
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

// Peek returns up to max of a series' held events, oldest first, and the
// file offset after the last one (for Release).
func (h *HeldStore) Peek(dest, series string, max int) ([]HeldEntry, int64, error) {
	var out []HeldEntry
	off, err := h.scan(dest, series, func(e HeldEntry) bool {
		out = append(out, e)
		return len(out) < max
	})
	if err != nil {
		return nil, 0, err
	}
	return out, off, nil
}

// ExpiredThrough returns the highest sequence number among a series' held
// events whose window started before cutoff (0 if none). Held events are in
// ledger order, not window order: a late adjustment for an old window can
// sit behind the current minute's usage.
func (h *HeldStore) ExpiredThrough(dest, series string, cutoff time.Time) (uint64, error) {
	var seq uint64
	_, err := h.scan(dest, series, func(e HeldEntry) bool {
		if e.Event.WindowStart.Before(cutoff) {
			seq = e.Seq
		}
		return true
	})
	return seq, err
}

// scan calls fn with a series' unreleased entries in order until it
// returns false, and returns the file offset after the last one read.
func (h *HeldStore) scan(dest, series string, fn func(HeldEntry) bool) (int64, error) {
	h.mu.Lock()
	s := h.dests[dest][series]
	if s == nil {
		h.mu.Unlock()
		return 0, nil
	}
	path, off, released := s.path, s.offset, s.released
	h.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		var l heldLine
		if jerr := json.Unmarshal(line, &l); jerr != nil {
			return 0, fmt.Errorf("held events %s: %w", path, jerr)
		}
		off += int64(len(line))
		if l.ReleasedThrough == 0 && l.Seq > released {
			e := l.HeldEntry
			e.end = off
			if !fn(e) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return off, nil
}

// Release marks a series' held events up to seq as delivered (n of them,
// ending at file offset next, as Peek returned). A drained series' file is
// deleted.
func (h *HeldStore) Release(dest, series string, seq uint64, n int, next int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.dests[dest][series]
	if s == nil {
		return nil
	}
	if seq >= s.LastSeq {
		delete(h.dests[dest], series)
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !h.noSync {
			return syncDir(filepath.Dir(s.path))
		}
		return nil
	}
	b, _ := json.Marshal(map[string]uint64{"released_through": seq})
	if err := h.appendSync(s.path, append(b, '\n'), false); err != nil {
		return err
	}
	s.released, s.offset = seq, next
	s.Count -= n
	// The next entry becomes the first; the oldest window may be any of
	// the rest (ledger order is not window order).
	h.mu.Unlock()
	first := true
	var oldest time.Time
	var head HeldEntry
	_, err := h.scan(dest, series, func(e HeldEntry) bool {
		if first {
			head, oldest, first = e, e.Event.WindowStart, false
		} else if e.Event.WindowStart.Before(oldest) {
			oldest = e.Event.WindowStart
		}
		return true
	})
	h.mu.Lock()
	if err == nil && !first {
		s.FirstSeq, s.Sample, s.Since = head.Seq, head.Event, head.At
		s.Oldest = oldest
	}
	return nil
}
