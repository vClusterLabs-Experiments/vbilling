package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Destination marks.
//
// Besides its cursor (cursors.json), the ledger remembers three things about
// a destination, in <dir>/destinations.json:
//
//   - where it started: the sequence number its cursor was created at (the
//     ledger head of that moment) or was last positioned at by an operator.
//     Reconciliation compares only usage recorded after it.
//   - whether an operator paused its delivery, since when and why.
//   - whether it is detached: vBilling started without it in ADAPTERS while
//     it had a cursor. A detached destination that is configured again with
//     a backlog delivers nothing until an operator decides where it starts.
//
// The file is a JSON object keyed by destination name:
//
//	{"metronome": {"start":    {"seq": 3857, "at": "...", "how": "created"},
//	               "paused":   {"at": "...", "reason": "sandbox cleanup"},
//	               "detached": {"at": "...", "committed": 3857, "awaiting_start": true}}}
//
// A destination with no entry (every cursor an earlier version wrote) has no
// recorded start, is not paused and is not detached.

// How a destination's start was recorded.
const (
	// StartCreated: the destination was seen for the first time and its
	// cursor was created at the ledger head. It never received anything
	// recorded before.
	StartCreated = "created"
	// StartPositioned: an operator positioned the cursor (a rewind, a skip or
	// a start decision). What was recorded before may or may not have been
	// delivered in the destination's earlier life.
	StartPositioned = "positioned"
)

const destinationsFile = "destinations.json"

// DestinationStart is where a destination's delivery starts: it is meant to
// receive every record after Seq.
type DestinationStart struct {
	Seq uint64    `json:"seq"`
	At  time.Time `json:"at"`
	How string    `json:"how"` // StartCreated or StartPositioned
}

// DestinationPause is an operator's pause of a destination's delivery.
type DestinationPause struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// DestinationDetach marks a destination that has a cursor but was not
// configured when vBilling started.
type DestinationDetach struct {
	// At is when vBilling first started without the destination, and
	// Committed the ledger head of that moment.
	At        time.Time `json:"at"`
	Committed uint64    `json:"committed"`
	// AwaitingStart: the destination is configured again and has a backlog;
	// it delivers nothing until an operator decides where it starts.
	AwaitingStart bool      `json:"awaiting_start,omitempty"`
	AwaitingSince time.Time `json:"awaiting_since,omitzero"`
	// Decided: an operator positioned the cursor while the destination was
	// detached. That is the decision: it starts there once it is configured.
	Decided bool `json:"decided,omitempty"`
}

// DestinationState is what the ledger remembers about a destination besides
// its cursor.
type DestinationState struct {
	Start    *DestinationStart  `json:"start,omitempty"`
	Paused   *DestinationPause  `json:"paused,omitempty"`
	Detached *DestinationDetach `json:"detached,omitempty"`
}

func (st *DestinationState) empty() bool {
	return st == nil || (st.Start == nil && st.Paused == nil && st.Detached == nil)
}

func (st *DestinationState) clone() DestinationState {
	var out DestinationState
	if st == nil {
		return out
	}
	if st.Start != nil {
		v := *st.Start
		out.Start = &v
	}
	if st.Paused != nil {
		v := *st.Paused
		out.Paused = &v
	}
	if st.Detached != nil {
		v := *st.Detached
		out.Detached = &v
	}
	return out
}

func (s *Spool) loadDestinations() error {
	b, err := os.ReadFile(filepath.Join(s.dir, destinationsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// A file that cannot be read must stop the start: treating it as empty
	// would silently resume a paused destination and unfence a detached one.
	if err := json.Unmarshal(b, &s.dests); err != nil {
		return fmt.Errorf("parse %s: %w", destinationsFile, err)
	}
	if s.dests == nil {
		s.dests = map[string]*DestinationState{}
	}
	for name, st := range s.dests {
		if st.empty() {
			delete(s.dests, name)
			continue
		}
		if st.Start != nil && st.Start.Seq > s.committed { // recorded ahead of a truncated tail
			st.Start.Seq = s.committed
		}
	}
	return nil
}

func (s *Spool) writeDestinationsLocked() error {
	return writeJSONAtomic(filepath.Join(s.dir, destinationsFile), s.dests, s.opts.NoSync)
}

// Whether a change to the marks stays in memory when it cannot be written.
// A change that stops delivery (a pause, a detach mark, a wait for a start
// decision) stays: until the next restart the destination is at least held
// back as asked. A change that lets delivery go on is undone: a destination
// never starts delivering on a decision that a restart would forget.
const (
	keepUnwritten = true
	undoUnwritten = false
)

// changeDestLocked applies change to name's marks and writes the marks.
func (s *Spool) changeDestLocked(name string, keep bool, change func(*DestinationState)) error {
	prev, had := s.dests[name]
	next := prev.clone()
	change(&next)
	if next.empty() {
		delete(s.dests, name)
	} else {
		s.dests[name] = &next
	}
	err := s.writeDestinationsLocked()
	if err != nil && !keep {
		if had {
			s.dests[name] = prev
		} else {
			delete(s.dests, name)
		}
	}
	return err
}

func (s *Spool) setStartLocked(name string, seq uint64, how string) error {
	return s.changeDestLocked(name, keepUnwritten, func(st *DestinationState) {
		st.Start = &DestinationStart{Seq: seq, At: s.opts.Now().UTC(), How: how}
	})
}

// DestinationState returns a copy of what the ledger remembers about a
// destination besides its cursor.
func (s *Spool) DestinationState(name string) DestinationState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dests[name].clone()
}

// Delivery returns, as of one moment, a destination's cursor, the cursor's
// generation (for AckAt) and its marks. Like Cursor, it creates the cursor at
// the ledger head for a destination never seen before. A delivery loop reads
// all three together, so a pause or a positioning that completed before the
// call is never missed.
func (s *Spool) Delivery(name string) (cursor, gen uint64, st DestinationState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor = s.cursorLocked(name)
	return cursor, s.cursorGen[name], s.dests[name].clone()
}

// CursorOf returns a destination's cursor without creating one: ok is false
// for a destination this ledger has never seen.
func (s *Spool) CursorOf(name string) (cursor uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor, ok = s.cursors[name]
	return cursor, ok
}

// KnownDestinations lists every destination the ledger has a cursor or a
// mark for, configured or not, sorted by name.
func (s *Spool) KnownDestinations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for name := range s.cursors {
		seen[name] = true
		out = append(out, name)
	}
	for name := range s.dests {
		if !seen[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// PauseDestination records an operator's pause. Pausing a destination that
// is paused already keeps the first pause's time and replaces the reason
// only when a new one is given.
func (s *Spool) PauseDestination(name, reason string) (DestinationPause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p DestinationPause
	err := s.changeDestLocked(name, keepUnwritten, func(st *DestinationState) {
		if st.Paused == nil {
			st.Paused = &DestinationPause{At: s.opts.Now().UTC(), Reason: reason}
		} else if reason != "" {
			st.Paused.Reason = reason
		}
		p = *st.Paused
	})
	return p, err
}

// ResumeDestination lifts a pause and returns it (nil if the destination was
// not paused).
func (s *Spool) ResumeDestination(name string) (*DestinationPause, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.dests[name]
	if st == nil || st.Paused == nil {
		return nil, nil
	}
	p := *st.Paused
	if err := s.changeDestLocked(name, undoUnwritten, func(st *DestinationState) { st.Paused = nil }); err != nil {
		return nil, err
	}
	return &p, nil
}

// MarkDetached marks a destination that has a cursor but is not configured.
// The first mark keeps its time and ledger head: marking again only ends a
// wait for a start decision, which a destination that is gone again no
// longer needs. It reports whether the mark is new.
func (s *Spool) MarkDetached(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.dests[name]
	if st != nil && st.Detached != nil {
		if !st.Detached.AwaitingStart {
			return false, nil
		}
		return false, s.changeDestLocked(name, keepUnwritten, func(st *DestinationState) {
			st.Detached.AwaitingStart, st.Detached.AwaitingSince = false, time.Time{}
		})
	}
	return true, s.changeDestLocked(name, keepUnwritten, func(st *DestinationState) {
		st.Detached = &DestinationDetach{At: s.opts.Now().UTC(), Committed: s.committed}
	})
}

// AwaitStart makes a detached destination that is configured again wait for
// an operator's start decision. It reports whether the wait is new.
func (s *Spool) AwaitStart(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.dests[name]
	if st == nil || st.Detached == nil || st.Detached.AwaitingStart {
		return false, nil
	}
	return true, s.changeDestLocked(name, keepUnwritten, func(st *DestinationState) {
		st.Detached.AwaitingStart, st.Detached.AwaitingSince = true, s.opts.Now().UTC()
	})
}

// DecideDetached records that an operator positioned a destination while it
// was detached: it starts at its cursor once it is configured again.
func (s *Spool) DecideDetached(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.dests[name]
	if st == nil || st.Detached == nil || st.Detached.Decided {
		return nil
	}
	return s.changeDestLocked(name, undoUnwritten, func(st *DestinationState) { st.Detached.Decided = true })
}

// ClearDetached removes a destination's detached mark: it is configured
// again and either has no backlog or an operator decided where it starts.
func (s *Spool) ClearDetached(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.dests[name]
	if st == nil || st.Detached == nil {
		return nil
	}
	return s.changeDestLocked(name, undoUnwritten, func(st *DestinationState) { st.Detached = nil })
}

// AddDeadLetters parks several events for operator review with one durable
// write (AddDeadLetter writes and syncs each event on its own, which is too
// slow for the thousands of held events a start decision can skip). Like
// AddDeadLetter, it parks each event ID once; it returns how many were new.
// A letter without a time gets the current one.
func (s *Spool) AddDeadLetters(dest string, letters []DeadLetter) (added int, err error) {
	if len(letters) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadIDs == nil {
		s.deadIDs = map[string]map[string]bool{}
	}
	ids := s.deadIDs[dest]
	if ids == nil {
		dls, err := s.deadLettersLocked(dest)
		if err != nil {
			return 0, err
		}
		ids = map[string]bool{}
		for _, dl := range dls {
			ids[dl.Event.ID] = true
		}
		s.deadIDs[dest] = ids
	}
	now := s.opts.Now().UTC()
	var buf []byte
	var fresh []string
	batch := map[string]bool{}
	for _, dl := range letters {
		if ids[dl.Event.ID] || batch[dl.Event.ID] {
			continue
		}
		if dl.At.IsZero() {
			dl.At = now
		}
		b, err := json.Marshal(dl)
		if err != nil {
			return 0, err
		}
		buf = append(append(buf, b...), '\n')
		batch[dl.Event.ID] = true
		fresh = append(fresh, dl.Event.ID)
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	f, err := os.OpenFile(s.deadLetterPath(dest), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
			return 0, err
		}
		if last[0] != '\n' {
			// A crash may have left an incomplete final line: keep it apart
			// from the first event written now (see AddDeadLetter).
			buf = append([]byte{'\n'}, buf...)
		}
	}
	if n, err := f.Write(buf); err != nil {
		return 0, err
	} else if n != len(buf) {
		return 0, io.ErrShortWrite
	}
	if !s.opts.NoSync {
		if err := f.Sync(); err != nil {
			return 0, err
		}
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if !s.opts.NoSync {
		if err := syncDir(s.dir); err != nil {
			return 0, err
		}
	}
	for _, id := range fresh {
		ids[id] = true
	}
	return len(fresh), nil
}
