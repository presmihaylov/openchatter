package api

import (
	"log"
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
)

func isAdmin(p models.Participant) bool { return p.Role == "admin" }

func requireAdmin(w http.ResponseWriter, p models.Participant) bool {
	if !isAdmin(p) {
		writeErr(w, http.StatusForbidden, "admin role required")
		return false
	}
	return true
}

type renameRoomReq struct {
	Name string `json:"name"`
	// delivery policy (task 25); either may come alone, without a rename
	DeliveryDeadLetterDays *int `json:"delivery_dead_letter_days"`
	DeliveryMaxAttempts    *int `json:"delivery_max_attempts"`
}

func (s *Server) handleRenameRoom(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	var req renameRoomReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	policy := req.DeliveryDeadLetterDays != nil || req.DeliveryMaxAttempts != nil
	if req.Name == "" && !policy {
		writeErr(w, http.StatusBadRequest, "name must be 1-100 characters")
		return
	}
	if req.Name != "" && len(req.Name) > 100 {
		writeErr(w, http.StatusBadRequest, "name must be 1-100 characters")
		return
	}
	var room models.Room
	var err error
	if policy {
		room, err = s.store.RoomByID(r.Context(), p.RoomID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		days, attempts := room.DeliveryDeadLetterDays, room.DeliveryMaxAttempts
		if req.DeliveryDeadLetterDays != nil {
			days = *req.DeliveryDeadLetterDays
		}
		if req.DeliveryMaxAttempts != nil {
			attempts = *req.DeliveryMaxAttempts
		}
		if days < 1 || days > 365 || attempts < 1 || attempts > 100 {
			writeErr(w, http.StatusBadRequest, "delivery_dead_letter_days must be 1-365 and delivery_max_attempts 1-100")
			return
		}
		room, err = s.store.SetDeliveryPolicy(r.Context(), p.RoomID, days, attempts)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	if req.Name != "" {
		room, err = s.store.RenameRoom(r.Context(), p.RoomID, req.Name, p.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, room)
}

type deleteRoomReq struct {
	Name string `json:"name"`
}

// Only the owner (the user who created the room) may delete it; the typed
// name is the confirmation. Agents have no user, so they never qualify.
func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	room, err := s.store.RoomByID(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if p.UserID == nil || room.CreatedByUserID == nil || *p.UserID != *room.CreatedByUserID {
		writeErrCode(w, http.StatusForbidden, "owner_required", "only the workspace owner can delete it")
		return
	}
	var req deleteRoomReq
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) != room.Name {
		writeErrCode(w, http.StatusBadRequest, "name_mismatch", "type the workspace name exactly to confirm")
		return
	}
	if err := s.store.DeleteRoom(r.Context(), room.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	log.Printf("room %s (%s) deleted by user %s", room.ID, room.Slug, *p.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "slug": room.Slug})
}

type setRoleReq struct {
	Role string `json:"role"`
}

func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req setRoleReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		writeErr(w, http.StatusBadRequest, `role must be "admin" or "member"`)
		return
	}
	// the owner stays admin: a demoted owner could neither leave nor delete
	if owner, err := s.isRoomOwner(r, target); err != nil {
		writeStoreErr(w, err)
		return
	} else if owner {
		writeErrCode(w, http.StatusForbidden, "owner_protected", "the workspace owner's role cannot change")
		return
	}
	if err := s.store.SetRole(r.Context(), p.RoomID, target.ID, req.Role); err != nil {
		writeStoreErr(w, err)
		return
	}
	got, err := s.store.ParticipantByID(r.Context(), p.RoomID, target.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

// isRoomOwner: the target's account created the room
func (s *Server) isRoomOwner(r *http.Request, target models.Participant) (bool, error) {
	if target.UserID == nil {
		return false, nil
	}
	room, err := s.store.RoomByID(r.Context(), target.RoomID)
	if err != nil {
		return false, err
	}
	return room.CreatedByUserID != nil && *target.UserID == *room.CreatedByUserID, nil
}

// handleRevokeParticipant: admins kick anyone (Slack-style deactivation), a
// human may delete an agent they own, and everyone may remove themself (leave).
func (s *Server) handleRevokeParticipant(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ownsAgent := p.IsHuman && !target.IsHuman && target.OwnerID != nil && *target.OwnerID == p.ID
	if !isAdmin(p) && target.ID != p.ID && !ownsAgent {
		writeErr(w, http.StatusForbidden, "only admins can remove other participants; humans can remove agents they own")
		return
	}
	owner, err := s.isRoomOwner(r, target)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// the creator is the one row nobody removes: not an admin, not the creator
	// themself. Their agents would go with them (task 19), so this is 409 with
	// a reason, never a silent no-op
	if owner {
		if target.ID == p.ID {
			writeErrCode(w, http.StatusConflict, "owner_cannot_leave", "the workspace creator cannot leave it; delete the workspace instead")
			return
		}
		writeErrCode(w, http.StatusConflict, "owner_protected", "the workspace creator cannot be removed; their agents would go with them")
		return
	}
	if err := s.store.Revoke(r.Context(), p.RoomID, target.ID, p.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleSetRoomAvatar: the workspace image, admins only. Members see it through
// GET /room and the switcher; the initials fallback returns on DELETE.
func (s *Server) handleSetRoomAvatar(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	meta, ok := s.readAvatarUpload(w, r, p)
	if !ok {
		return
	}
	room, err := s.store.SetRoomAvatar(r.Context(), p.RoomID, &meta.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRemoveRoomAvatar(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	room, err := s.store.SetRoomAvatar(r.Context(), p.RoomID, nil)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

type setOwnerReq struct {
	OwnerID string `json:"owner_id"`
}

// handleSetOwner rebinds an agent to another human member (admins only):
// a wrong owner from the migration is one request, not a migration.
func (s *Server) handleSetOwner(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if target.IsHuman {
		writeErr(w, http.StatusBadRequest, "only an agent has an owner")
		return
	}
	var req setOwnerReq
	if !readJSON(w, r, &req) {
		return
	}
	if !isUUID(req.OwnerID) {
		writeErr(w, http.StatusBadRequest, "owner_id must be a participant id")
		return
	}
	if err := s.store.SetOwner(r.Context(), p.RoomID, target.ID, req.OwnerID); err != nil {
		writeStoreErr(w, err)
		return
	}
	fresh, err := s.store.ParticipantByID(r.Context(), p.RoomID, target.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participant": fresh})
}
