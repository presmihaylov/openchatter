package api

import (
	"net/http"
	"strconv"

	"github.com/presmihaylov/openchatter/models"
)

// handleInbox drains the caller's unacked receipts: the missed batch, in seq
// order, marked delivered on the way out (peek=1 only looks). Safe to call
// twice at once; see Store.Inbox.
func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request, p models.Participant) {
	q := r.URL.Query()
	peek := q.Get("peek") == "1" || q.Get("peek") == "true"
	limit, _ := strconv.Atoi(q.Get("limit"))
	events, receipts, err := s.store.Inbox(r.Context(), p.RoomID, p.ID, peek, limit)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "receipts": receipts, "peek": peek})
}

// handleAck: the agent confirms it acted on the event. Idempotent; 404 when
// the caller was never a recipient of that seq.
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request, p models.Participant) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq <= 0 {
		writeErr(w, http.StatusBadRequest, "seq must be a positive integer")
		return
	}
	if err := s.store.AckDelivery(r.Context(), p.RoomID, p.ID, seq); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeliveryStats: per-agent counts for the agent's server-verified owner,
// an admin, or the agent itself.
func (s *Server) handleDeliveryStats(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	owns := target.OwnerID != nil && *target.OwnerID == p.ID
	if !owns && !isAdmin(p) && target.ID != p.ID {
		writeErr(w, http.StatusForbidden, "only the agent's owner or an admin sees its delivery stats")
		return
	}
	st, err := s.store.DeliveryStatsFor(r.Context(), p.RoomID, target.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
