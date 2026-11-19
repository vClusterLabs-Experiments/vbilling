package metronome

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/usage"
)

// Usage waits for a billable metric that counts it. Metronome does not
// attribute usage to a billable metric created after it: "When using
// streaming billable metrics, the billable metric must be defined in
// Metronome before any usage can be associated with it. If usage is sent
// before a streaming metric is defined, it isn't attributed to the metric
// by default" (docs.metronome.com, guides/implement-metronome/core-concepts/
// create-billable-metrics; a reflow needs Metronome support), and "billable
// metrics only match usage events sent after the billable metric is
// created" (guides/get-started/developer-sdks).
//
// So without METRONOME_PROVISION, where Finance adds the billable metrics,
// the events of a type no active billable metric sums wait here, whatever
// PRICE_CHECK says, until one does; price coverage reports the series
// unpriced meanwhile. They are kept durably, per event type, in
// DATA_DIR/metronome-waiting, because the dispatcher's cursor moves past
// them once SendEvents accepts them. Events that wait longer than
// PRICE_CHECK_MAX_HOLD (by their window start) are sent anyway, with a
// warning, before Metronome's 34-day backdating limit: Metronome keeps raw
// events, so its support can still reflow them into a billable metric.

const (
	// metricsRefresh is how often the billable metrics are read again while
	// SendEvents needs them.
	metricsRefresh = time.Minute
	// waitMax is how long usage waits for its billable metric when the
	// configuration does not say (PRICE_CHECK_MAX_HOLD).
	waitMax = 28 * 24 * time.Hour
	// noMetricHint is the hint price coverage gives for such usage.
	noMetricHint = "no billable metric counts this event type yet: add it (vbilling metronome billable-metrics prints it)"
)

// countedView is what the adapter last read of Metronome's billable
// metrics.
type countedView struct {
	bms []BillableMetric
	at  time.Time
	ok  bool
}

// counts reports whether an active billable metric sums an event type.
func (v countedView) counts(code string) bool { return len(summing(v.bms, code)) > 0 }

// billableMetricsView returns Metronome's billable metrics, read again once
// the last read is metricsRefresh old. A failed read falls back to the last
// one; with none, it is an error.
func (a *Adapter) billableMetricsView(ctx context.Context) (countedView, error) {
	a.mu.Lock()
	v := a.counted
	a.mu.Unlock()
	if v.ok && a.now().Sub(v.at) < metricsRefresh {
		return v, nil
	}
	bms, err := a.c.ListBillableMetrics(ctx)
	if err != nil {
		if v.ok {
			log.Printf("[metronome] could not read the billable metrics again (%v); using those read at %s", err, v.at.UTC().Format(time.RFC3339))
			return v, nil
		}
		return v, fmt.Errorf("list billable metrics (usage waits until a billable metric counts it): %w", err)
	}
	v = countedView{bms: bms, at: a.now(), ok: true}
	a.mu.Lock()
	a.counted = v
	a.mu.Unlock()
	return v, nil
}

// waitStore keeps the waiting events, per event type, in order.
type waitStore struct {
	dir string // "" keeps them in memory

	mu     sync.Mutex
	loaded bool
	types  map[string]time.Time     // event types with waiting events -> the oldest window start
	mem    map[string][]usage.Event // memory mode
	logged map[string]time.Time     // last "waiting" log per type
}

func newWaitStore(dataDir string) *waitStore {
	w := &waitStore{types: map[string]time.Time{}, mem: map[string][]usage.Event{}, logged: map[string]time.Time{}}
	if dataDir != "" {
		w.dir = filepath.Join(dataDir, "metronome-waiting")
	}
	return w
}

var safeType = regexp.MustCompile(`^[a-z][a-z0-9_]{0,99}$`)

// file is the file of an event type's waiting events.
func (w *waitStore) file(code string) string {
	name := code
	if !safeType.MatchString(code) {
		sum := sha256.Sum256([]byte(code))
		name = "x" + hex.EncodeToString(sum[:8])
	}
	return filepath.Join(w.dir, name+".jsonl")
}

// loadLocked indexes the event types that have waiting events on disk.
func (w *waitStore) loadLocked() error {
	if w.loaded || w.dir == "" {
		w.loaded = true
		return nil
	}
	entries, err := os.ReadDir(w.dir)
	if errors.Is(err, os.ErrNotExist) {
		w.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".jsonl") {
			continue
		}
		var code string
		var oldest time.Time
		err := scanEvents(filepath.Join(w.dir, en.Name()), func(e usage.Event) bool {
			code = e.Metric
			if oldest.IsZero() || e.WindowStart.Before(oldest) {
				oldest = e.WindowStart
			}
			return true
		})
		if err != nil {
			return err
		}
		if code != "" {
			w.types[code] = oldest
		}
	}
	w.loaded = true
	return nil
}

// pending returns the event types with waiting events, with the oldest
// window start of each.
func (w *waitStore) pending() (map[string]time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.loadLocked(); err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(w.types))
	for t, oldest := range w.types {
		out[t] = oldest
	}
	return out, nil
}

// add appends events (all of one type) durably.
func (w *waitStore) add(code string, events []usage.Event) error {
	if len(events) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.loadLocked(); err != nil {
		return err
	}
	oldest, held := w.types[code]
	for i := range events {
		if !held || events[i].WindowStart.Before(oldest) {
			oldest, held = events[i].WindowStart, true
		}
	}
	if w.dir == "" {
		w.mem[code] = append(w.mem[code], events...)
		w.types[code] = oldest
		return nil
	}
	var buf strings.Builder
	for i := range events {
		b, err := json.Marshal(events[i])
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(w.dir, 0o750); err != nil {
		return err
	}
	path := w.file(code)
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := syncDir(w.dir); err != nil {
			return err
		}
	}
	w.types[code] = oldest
	return nil
}

// take calls send with an event type's waiting events, oldest first, in
// slices, and keeps those keep says to keep (all when keep is nil, none).
// What send accepted is gone once take returns nil.
func (w *waitStore) take(code string, keep func(usage.Event) bool, send func([]usage.Event) error) (sent, kept int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var events []usage.Event
	if w.dir == "" {
		events = w.mem[code]
	} else if err := scanEvents(w.file(code), func(e usage.Event) bool { events = append(events, e); return true }); err != nil {
		return 0, 0, err
	}
	var out, rest []usage.Event
	for _, e := range events {
		if keep != nil && keep(e) {
			rest = append(rest, e)
		} else {
			out = append(out, e)
		}
	}
	for i := 0; i < len(out); i += 100 {
		if err := send(out[i:min(i+100, len(out))]); err != nil {
			return 0, 0, err
		}
	}
	if len(out) == 0 {
		return 0, len(rest), nil
	}
	if w.dir == "" {
		w.mem[code] = rest
	} else if err := w.rewrite(code, rest); err != nil {
		return 0, 0, err
	}
	if len(rest) == 0 {
		delete(w.types, code)
		delete(w.mem, code)
	} else {
		oldest := rest[0].WindowStart
		for _, e := range rest[1:] {
			if e.WindowStart.Before(oldest) {
				oldest = e.WindowStart
			}
		}
		w.types[code] = oldest
	}
	return len(out), len(rest), nil
}

// rewrite replaces an event type's file with the events left, or removes
// it.
func (w *waitStore) rewrite(code string, rest []usage.Event) error {
	path := w.file(code)
	if len(rest) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDir(w.dir)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	for i := range rest {
		b, err := json.Marshal(rest[i])
		if err != nil {
			f.Close()
			return err
		}
		bw.Write(b)
		bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(w.dir)
}

// scanEvents calls fn with each event in a file until it returns false. A
// last line cut short by a crash is skipped.
func scanEvents(path string, fn func(usage.Event) bool) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e usage.Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue // torn by a crash: it was never acknowledged
		}
		if !fn(e) {
			return nil
		}
	}
	return sc.Err()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// releaseWaiting sends the waiting events of every event type a billable
// metric counts now, and those that waited longer than the configured
// maximum (by window start) whatever their type, with a warning. Events
// Metronome would refuse as too old are dropped, with an error log.
func (a *Adapter) releaseWaiting(ctx context.Context, view countedView) error {
	types, err := a.wait.pending()
	if err != nil {
		return fmt.Errorf("waiting usage: %w", err)
	}
	limit := a.cfg.PriceCheckMaxHold
	if limit <= 0 {
		limit = waitMax
	}
	now := a.now()
	codes := make([]string, 0, len(types))
	for code := range types {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		counted := view.counts(code)
		var keep func(usage.Event) bool
		if !counted {
			if now.Sub(types[code]) <= limit {
				continue // nothing waited too long yet
			}
			keep = func(e usage.Event) bool { return now.Sub(e.WindowStart) <= limit }
		}
		var tooOld int
		sent, kept, err := a.wait.take(code, keep, func(evs []usage.Event) error {
			batch := make([]IngestEvent, 0, len(evs))
			for i := range evs {
				if now.Sub(evs[i].WindowStart) > maxEventAge {
					tooOld++
					continue
				}
				batch = append(batch, a.ToIngest(&evs[i]))
			}
			if len(batch) == 0 {
				return nil
			}
			return a.c.Ingest(ctx, batch)
		})
		if err != nil {
			return fmt.Errorf("release waiting usage of %s: %w", code, err)
		}
		switch {
		case sent == 0:
		case counted:
			log.Printf("[metronome] a billable metric counts %s now: sent the %d event(s) that waited for it", code, sent)
		default:
			log.Printf("[metronome] WARNING: sent %d event(s) of %s WITHOUT a billable metric that counts them: they waited longer than %s. "+
				"Metronome keeps them but bills none (its support can reflow them into a billable metric added later); %d still wait",
				sent, code, limit, kept)
		}
		if tooOld > 0 {
			log.Printf("[metronome] ERROR: dropped %d waiting event(s) of %s older than Metronome's 34-day ingest window", tooOld, code)
		}
	}
	return nil
}

// noteWaiting logs, at most once an hour per event type, that its usage
// waits for a billable metric.
func (a *Adapter) noteWaiting(code string, n int) {
	now := a.now()
	a.wait.mu.Lock()
	last, seen := a.wait.logged[code]
	if seen && now.Sub(last) < time.Hour {
		a.wait.mu.Unlock()
		return
	}
	a.wait.logged[code] = now
	a.wait.mu.Unlock()
	log.Printf("[metronome] holding %d event(s) of %s: %s; they are sent once one does", n, code, noMetricHint)
}
