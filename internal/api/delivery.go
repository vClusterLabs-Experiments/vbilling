package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/vclusterlabs-experiments/vbilling/internal/destinations"
)

func (s *Server) deliveryController(w http.ResponseWriter, r *http.Request) destinations.DeliveryController {
	d := s.destination(r.PathValue("name"))
	if d == nil {
		writeErr(w, http.StatusNotFound, "unknown destination")
		return nil
	}
	c, ok := d.(destinations.DeliveryController)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "destination has no delivery receipt controls")
		return nil
	}
	return c
}

func (s *Server) deliveryReceipt(w http.ResponseWriter, r *http.Request) {
	c := s.deliveryController(w, r)
	if c == nil {
		return
	}
	receipt, ok, err := c.DeliveryReceipt(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no outstanding receipt for this event")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "receipt": receipt})
}

func (s *Server) resolveDelivery(w http.ResponseWriter, r *http.Request) {
	c := s.deliveryController(w, r)
	if c == nil {
		return
	}
	var req struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must contain outcome and reconciliation reason")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "body must contain exactly one JSON object")
		return
	}
	if err := c.ResolveDelivery(r.PathValue("id"), req.Outcome, req.Reason); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	log.Printf("[api] operator resolved delivery receipt for destination %s event %s as %s", r.PathValue("name"), r.PathValue("id"), req.Outcome)
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "outcome": req.Outcome})
}
