package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
)

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request, p models.Participant) {
	list, err := s.store.ListChannelsUnread(r.Context(), p.RoomID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": list})
}

// handleBrowseChannels lists the public channels the caller can join but has
// not joined yet, with a live member count for each.
func (s *Server) handleBrowseChannels(w http.ResponseWriter, r *http.Request, p models.Participant) {
	list, err := s.store.BrowsableChannels(r.Context(), p.RoomID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": list})
}

// handleJoinChannel adds the caller to a channel. Idempotent: re-joining is a
// no-op that still returns 200 with the channel.
func (s *Server) handleJoinChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ch.Archived {
		writeErr(w, http.StatusConflict, "channel is archived")
		return
	}
	// Private channels are invite-only: you cannot add yourself, a member must add you.
	if ch.Private {
		writeErr(w, http.StatusForbidden, "this channel is invite-only; ask a member to add you")
		return
	}
	if _, err := s.store.JoinChannel(r.Context(), p.RoomID, ch.ID, p.ID, ch.Name, p.ID, p.Name); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// handleListChannelMembers lists a channel's members. Member-gated: who is in
// a channel (a private one especially) is itself channel content.
func (s *Server) handleListChannelMembers(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	list, err := s.store.ListChannelMembers(r.Context(), p.RoomID, ch.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": list})
}

type addMemberReq struct {
	Participant string `json:"participant"`
}

// handleAddChannelMember lets any current member add another participant to a
// channel. This is the only way into a private channel: a member adds you.
func (s *Server) handleAddChannelMember(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ch.Archived {
		writeErr(w, http.StatusConflict, "channel is archived")
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	var req addMemberReq
	if !readJSON(w, r, &req) {
		return
	}
	target, err := s.resolveParticipant(r, p, req.Participant)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if _, err := s.store.JoinChannel(r.Context(), p.RoomID, ch.ID, target.ID, ch.Name, p.ID, p.Name); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// handleRemoveChannelMember removes another participant from a channel. Admin
// only; #general is pinned so nobody can be removed from it.
func (s *Server) handleRemoveChannelMember(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ch.Name == "general" {
		writeErr(w, http.StatusConflict, "the general channel cannot be left")
		return
	}
	target, err := s.resolveParticipant(r, p, r.PathValue("pid"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if _, err := s.store.LeaveChannel(r.Context(), p.RoomID, ch.ID, target.ID, ch.Name, p.ID, p.Name); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "removed", "channel_id": ch.ID, "participant_id": target.ID,
	})
}

// handleLeaveChannel removes the caller from a channel. #general is pinned:
// nobody can leave it, so no participant is ever left with an empty sidebar.
func (s *Server) handleLeaveChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ch.Name == "general" {
		writeErr(w, http.StatusConflict, "the general channel cannot be left")
		return
	}
	if _, err := s.store.LeaveChannel(r.Context(), p.RoomID, ch.ID, p.ID, ch.Name, p.ID, p.Name); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "left", "channel_id": ch.ID})
}

// requireChannelMember gates a channel's contents by membership. A non-member
// gets 403 whether the channel is public or private. Content stays fully gated;
// a private channel is already hidden from the channel list, browse, and search,
// so a non-member has no path to its contents.
func (s *Server) requireChannelMember(w http.ResponseWriter, r *http.Request, p models.Participant, channelID string) bool {
	ok, err := s.store.IsChannelMember(r.Context(), channelID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "you are not a member of this channel")
		return false
	}
	return true
}

// handleMarkRead advances the caller's read marker for a channel to now.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	at, err := s.store.MarkChannelRead(r.Context(), p.ID, ch.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel_id": ch.ID, "last_read_at": at})
}

type channelMuteReq struct {
	Muted bool `json:"muted"`
}

// handleMuteChannel is the caller's own notification mute; membership is the
// gate, so a private channel you are not in stays a 404 like everywhere else.
func (s *Server) handleMuteChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req channelMuteReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.SetChannelMuted(r.Context(), p.ID, ch.ID, req.Muted); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel_id": ch.ID, "muted": req.Muted})
}

type createChannelReq struct {
	Name    string `json:"name"`
	Topic   string `json:"topic"`
	Private bool   `json:"private"`
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req createChannelReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(req.Name, "#")))
	if !validName(req.Name) {
		writeErr(w, http.StatusBadRequest, "channel name must match ^[a-z0-9][a-z0-9_-]{1,31}$")
		return
	}
	if len(req.Topic) > 500 {
		writeErr(w, http.StatusBadRequest, "topic too long")
		return
	}
	ch, err := s.store.CreateChannel(r.Context(), p.RoomID, req.Name, req.Topic, p.ID, req.Private)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ch)
}

type updateChannelReq struct {
	Archived *bool   `json:"archived"`
	Private  *bool   `json:"private"`
	Name     *string `json:"name"`
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req updateChannelReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Archived == nil && req.Private == nil && req.Name == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	// Slack-style: general is pinned public and unarchived; only admins or the
	// creator manage a channel's archive and privacy state
	if ch.Name == "general" {
		writeErr(w, http.StatusConflict, "the general channel cannot be changed")
		return
	}
	isCreator := ch.CreatedBy != nil && *ch.CreatedBy == p.ID
	if !isAdmin(p) && !isCreator {
		writeErr(w, http.StatusForbidden, "only admins or the channel creator can update it")
		return
	}
	if req.Name != nil {
		// a rename rewrites every "#name" reference people hold, so it is admin-only
		if !isAdmin(p) {
			writeErr(w, http.StatusForbidden, "only admins can rename a channel")
			return
		}
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(*req.Name, "#")))
		if !validName(name) {
			writeErr(w, http.StatusBadRequest, "channel name must match ^[a-z0-9][a-z0-9_-]{1,31}$")
			return
		}
		if _, err := s.store.RenameChannel(r.Context(), p.RoomID, ch.ID, name, p.ID, p.Name); err != nil {
			if errors.Is(err, models.ErrNameTaken) {
				writeErrCode(w, http.StatusConflict, "name_taken", err.Error())
				return
			}
			writeStoreErr(w, err)
			return
		}
	}
	if req.Private != nil {
		// one-way: making a private channel public would expose history that
		// was shared under an expectation of privacy. An empty one has none.
		if !*req.Private && ch.Private {
			if err := s.store.SetChannelPublicIfEmpty(r.Context(), p.RoomID, ch.ID); err != nil {
				writeStoreErr(w, err)
				return
			}
		}
		if *req.Private && !ch.Private {
			if err := s.store.SetChannelPrivate(r.Context(), p.RoomID, ch.ID); err != nil {
				writeStoreErr(w, err)
				return
			}
		}
	}
	if req.Archived != nil {
		if err := s.store.SetChannelArchived(r.Context(), p.RoomID, ch.ID, *req.Archived); err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	ch, err = s.store.ChannelByID(r.Context(), p.RoomID, ch.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

// handleDeleteChannel: admin only; deleting a channel deletes its messages.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if ch.Name == "general" {
		writeErr(w, http.StatusConflict, "the general channel cannot be deleted")
		return
	}
	if err := s.store.DeleteChannel(r.Context(), p.RoomID, ch.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// resolveChannel accepts a channel id or a name (with or without '#').
func (s *Server) resolveChannel(r *http.Request, p models.Participant, ref string) (models.Channel, error) {
	ref = strings.TrimPrefix(ref, "#")
	ch, err := s.store.ChannelByName(r.Context(), p.RoomID, ref)
	if err == nil {
		return ch, nil
	}
	// only a clean miss falls through to id lookup; other errors must surface
	if !errors.Is(err, models.ErrNotFound) {
		return models.Channel{}, err
	}
	if !isUUID(ref) {
		return models.Channel{}, models.ErrNotFound
	}
	return s.store.ChannelByID(r.Context(), p.RoomID, ref)
}
