package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vclusterlabs-experiments/vbilling/internal/pipeline"
)

// The operator's controls over a destination's delivery (see
// pipeline/valve.go): pause and resume, and the start decision of a
// destination that was removed from ADAPTERS and is configured again.

// maxPauseReason bounds the reason an operator gives for a pause.
const maxPauseReason = 500

// valveFailed answers a failed valve call and reports whether it did:
// 404 for a destination nothing is known about, 409 when the call does not
// apply to the destination's state or the destination refuses a rewind.
func (s *Server) valveFailed(w http.ResponseWriter, name string, err error) bool {
	var state *pipeline.StateError
	var replay *pipeline.ReplayError
	switch {
	case err == nil:
		return false
	case errors.Is(err, pipeline.ErrUnknownDestination):
		writeErr(w, http.StatusNotFound, "no destination %q", name)
	case errors.As(err, &state), errors.As(err, &replay):
		writeErr(w, http.StatusConflict, "%v", err)
	default:
		writeErr(w, http.StatusInternalServerError, "%v", err)
	}
	return true
}

// writeValve answers a valve call with the destination as GET
// /api/v1/destinations lists it, its name also as "destination", and what
// the call did.
func (s *Server) writeValve(w http.ResponseWriter, st pipeline.Status, did map[string]any) {
	out := map[string]any{}
	if b, err := json.Marshal(s.destinationRow(st)); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber() // ledger sequence numbers stay exact
		_ = dec.Decode(&out)
	}
	out["destination"] = st.Name
	for k, v := range did {
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

// pauseDestination stops a destination's delivery until it is resumed. The
// body is optional: {"reason": "..."}.
func (s *Server) pauseDestination(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Reason string `json:"reason"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, `body must be empty or {"reason": "<why>"}`)
			return
		}
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len(req.Reason) > maxPauseReason {
		writeErr(w, http.StatusBadRequest, "reason must be at most %d bytes", maxPauseReason)
		return
	}
	st, stopped, err := s.disp.Pause(name, req.Reason)
	if s.valveFailed(w, name, err) {
		return
	}
	log.Printf("[api] operator paused delivery to %s at cursor %d (reason: %q)", name, st.Cursor, req.Reason)
	did := map[string]any{}
	if !stopped {
		// A batch that was being sent when the pause arrived has not finished
		// yet: it completes, and nothing follows it.
		did["in_flight"] = true
	}
	s.writeValve(w, st, did)
}

// resumeDestination lifts a pause: delivery continues at the cursor.
func (s *Server) resumeDestination(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	st, was, err := s.disp.Resume(name)
	if s.valveFailed(w, name, err) {
		return
	}
	did := map[string]any{"resumed": was != nil}
	if was != nil {
		did["was_paused"] = was
		log.Printf("[api] operator resumed delivery to %s after %s: %d ledger record(s) to catch up from cursor %d",
			name, s.now().Sub(was.At).Round(time.Second), st.LagRecords, st.Cursor)
	}
	s.writeValve(w, st, did)
}

// startDestination is the start decision of a destination that awaits its
// start (it was removed from ADAPTERS, is configured again and has a
// backlog). The body says where it starts:
//
//	{"from": "cursor"}                deliver everything recorded while it was removed, held events included
//	{"from": "head"}                  skip the backlog: start at the ledger head
//	{"from": "2026-11-26T00:00:00Z"}  start with what the ledger committed since that time
//
// From the head or a time, the events held for it that were recorded before
// the new position move to its dead letters, where they stay replayable.
func (s *Server) startDestination(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	before, known := s.disp.Status(name)
	if !known {
		writeErr(w, http.StatusNotFound, "no destination %q", name)
		return
	}
	var req struct {
		From string `json:"from"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.From == "" {
		writeErr(w, http.StatusBadRequest, `body must be {"from": "cursor"}, {"from": "head"} or {"from": "<RFC 3339 time>"}`)
		return
	}
	// "position" is where the destination starts; "cursor" is its cursor
	// now, which may have moved on already.
	did := map[string]any{"from": req.From, "previous_cursor": before.Cursor, "position": before.Cursor}
	if before.Backlog != nil {
		did["backlog"] = before.Backlog // what the decision was about
	}
	var st pipeline.Status
	switch req.From {
	case "cursor":
		var err error
		if st, err = s.disp.Start(name); s.valveFailed(w, name, err) {
			return
		}
		did["skipped_held_events"] = 0
		if b := before.Backlog; b != nil && b.Compacted > 0 {
			did["warning"] = "some of the backlog was compacted away while the destination was removed and cannot be delivered from the ledger (see backlog.compacted_records)"
		}
	default:
		seq := s.spool.Committed()
		if req.From != "head" {
			at, err := time.Parse(time.RFC3339Nano, req.From)
			if err != nil {
				writeErr(w, http.StatusBadRequest, `from must be "cursor", "head" or an RFC 3339 time: %v`, err)
				return
			}
			if seq, err = s.firstSeqSince(at); err != nil {
				writeErr(w, http.StatusInternalServerError, "%v", err)
				return
			}
		}
		res, err := s.disp.Position(r.Context(), name, seq, true)
		if s.valveFailed(w, name, err) {
			return
		}
		st = res.Status
		did["position"], did["previous_cursor"], did["skipped_held_events"] = res.To, res.From, res.SkippedHeld
	}
	log.Printf("[api] operator started destination %s from %s: cursor %d (was %d), %v held event(s) moved to its dead letters",
		name, req.From, st.Cursor, before.Cursor, did["skipped_held_events"])
	s.writeValve(w, st, did)
}
