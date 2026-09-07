package api

import (
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
)

// Channel groups are a purely personal sidebar layout: every endpoint here is
// scoped to the caller, holds no room state, and emits no events.

func (s *Server) handleListChannelGroups(w http.ResponseWriter, r *http.Request, p models.Participant) {
	layout, err := s.store.ListChannelLayout(r.Context(), p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, layout)
}

type defaultSectionReq struct {
	Collapsed *bool `json:"collapsed"`
}

// handleUpdateDefaultSection carries the collapsed flag of the default section.
// It has no row in channel_groups, so it cannot go through the {id} route.
func (s *Server) handleUpdateDefaultSection(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req defaultSectionReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Collapsed == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if err := s.store.SetDefaultSectionCollapsed(r.Context(), p.ID, *req.Collapsed); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

type channelGroupReq struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateChannelGroup(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req channelGroupReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 60 {
		writeErr(w, http.StatusBadRequest, "section name must be 1-60 characters")
		return
	}
	g, err := s.store.CreateChannelGroup(r.Context(), p.ID, req.Name)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

type updateChannelGroupReq struct {
	Name      *string `json:"name"`
	Collapsed *bool   `json:"collapsed"`
	Position  *int    `json:"position"`
}

func (s *Server) handleUpdateChannelGroup(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req updateChannelGroupReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == nil && req.Collapsed == nil && req.Position == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" || len(trimmed) > 60 {
			writeErr(w, http.StatusBadRequest, "section name must be 1-60 characters")
			return
		}
		req.Name = &trimmed
	}
	if err := s.store.UpdateChannelGroup(r.Context(), p.ID, r.PathValue("id"), req.Name, req.Collapsed, req.Position); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) handleDeleteChannelGroup(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if err := s.store.DeleteChannelGroup(r.Context(), p.ID, r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type setChannelGroupReq struct {
	// GroupID null/empty removes the channel from any section (ungrouped).
	GroupID  *string `json:"group_id"`
	Position int     `json:"position"`
}

// handleSetChannelGroup moves a channel into one of the caller's sections, or
// out of any section when group_id is null. The caller must be a member of the
// channel; you only organize channels you can see.
func (s *Server) handleSetChannelGroup(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	var req setChannelGroupReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.GroupID != nil && strings.TrimSpace(*req.GroupID) == "" {
		req.GroupID = nil
	}
	if err := s.store.SetChannelGroup(r.Context(), p.ID, ch.ID, req.GroupID, req.Position); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel_id": ch.ID, "group_id": req.GroupID})
}
