package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
	"github.com/presmihaylov/openchatter/pkg/slug"
)

type createRoomReq struct {
	Name string `json:"name"`
	// optional: the URL segment; derived from the name when empty
	Slug string `json:"slug"`
}

// handleCreateRoom: only a logged-in human creates a workspace; the creator
// lands in it as admin. The per-creator quota replaces the per-IP limit.
func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request, u models.User) {
	var req createRoomReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		writeErr(w, http.StatusBadRequest, "name must be 1-100 characters")
		return
	}

	req.Slug = strings.TrimSpace(req.Slug)
	if req.Slug == "" {
		req.Slug = slug.From(req.Name)
	}
	if !slug.Valid(req.Slug) {
		writeErrCode(w, http.StatusBadRequest, "slug_invalid", "the workspace URL needs lowercase letters, digits and hyphens, 1-60 characters")
		return
	}
	token := secrets.InviteCode()
	room, _, err := s.store.CreateRoomAs(r.Context(), req.Name, req.Slug, token, u)
	if errors.Is(err, models.ErrConflict) {
		writeErrCode(w, http.StatusConflict, "slug_taken", "that workspace URL is taken, pick another name or edit the slug")
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"room":     room,
		"join_url": s.cfg.PublicURL + "/r/" + room.Slug,
		"invite":   s.inviteURL(token),
		// bare token, kept one release for older clients
		"invite_code": token,
	})
}

type enterWorkspaceReq struct {
	Invite string `json:"invite"`
	// older clients sent the bare code under these names
	InviteCode string `json:"invite_code"`
	Secret     string `json:"secret"`
}

func (q enterWorkspaceReq) token() string {
	for _, v := range []string{q.Invite, q.InviteCode, q.Secret} {
		if v != "" {
			return inviteToken(v)
		}
	}
	return ""
}

// handleEnterWorkspace makes a logged-in user a member of the room behind
// {slug}. A live member is idempotent, a revoked one stays out, and a
// newcomer needs a link that opens this very room. Never adopts a row by name.
func (s *Server) handleEnterWorkspace(w http.ResponseWriter, r *http.Request, u models.User) {
	var req enterWorkspaceReq
	if !readJSON(w, r, &req) {
		return
	}
	room, err := s.store.RoomBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}

	p, err := s.store.ParticipantForUser(r.Context(), room.ID, u.ID)
	if err == nil && p.Revoked {
		writeWorkspaceForbidden(w, "revoked")
		return
	}
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"participant": p, "room": room})
		return
	}
	if !errors.Is(err, models.ErrNotFound) {
		writeStoreErr(w, err)
		return
	}

	inv, target, err := s.store.InviteByToken(r.Context(), req.token())
	if errors.Is(err, models.ErrNotFound) || (err == nil && target.ID != room.ID) {
		writeStoreErr(w, models.ErrInviteInvalid)
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if inv.OwnerID != nil {
		writeStoreErr(w, models.ErrInviteAgentsOnly)
		return
	}
	p, err = s.store.EnterRoom(r.Context(), room.ID, u, inv.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participant": p, "room": room})
}

func writeWorkspaceForbidden(w http.ResponseWriter, reason string) {
	msg := "you are not a member of this workspace"
	if reason == "revoked" {
		msg = "you were removed from this workspace"
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": msg, "code": "workspace_forbidden", "reason": reason})
}

type joinRoomReq struct {
	Invite string `json:"invite"`
	// older clients sent the bare code under these names
	InviteCode string `json:"invite_code"`
	Secret     string `json:"secret"`
	Name       string `json:"name"`
	// legacy emoji avatar: still parsed so an old client's join does not 4xx,
	// never stored, never rendered. Drop the field after one release.
	Avatar      string `json:"avatar"`
	Description string `json:"description"`
	IsHuman     bool   `json:"is_human"`
}

func (s *Server) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	var req joinRoomReq
	if !readJSON(w, r, &req) {
		return
	}
	token := enterWorkspaceReq{Invite: req.Invite, InviteCode: req.InviteCode, Secret: req.Secret}.token()
	if !validParticipantName(req.Name) {
		writeErr(w, http.StatusBadRequest, "name must be 2-32 chars: letters, digits, spaces, - or _; no leading/trailing/double spaces")
		return
	}
	if isReservedName(req.Name) {
		writeErr(w, http.StatusBadRequest, "that name is reserved")
		return
	}
	if len(req.Description) > 2000 {
		writeErr(w, http.StatusBadRequest, "description too long")
		return
	}

	inv, room, err := s.store.InviteByToken(r.Context(), token)
	if errors.Is(err, models.ErrNotFound) {
		err = models.ErrInviteInvalid
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if req.IsHuman && inv.OwnerID != nil {
		// humans are their own principal: a bound link never admits one
		writeStoreErr(w, models.ErrInviteAgentsOnly)
		return
	}
	// an agent belongs to a human (task 19): a bound link binds its owner on
	// create and reclaim alike; a plain link hands a NEW agent to the workspace
	// creator, while a reclaim on it keeps the owner the agent already has
	ownerID := inv.OwnerID
	newOwner := ownerID
	if newOwner == nil && !req.IsHuman {
		if newOwner, err = s.store.CreatorRow(r.Context(), room.ID); err != nil {
			writeStoreErr(w, err)
			return
		}
	}

	token, hash := secrets.NewToken()
	var p models.Participant
	// req.Avatar is read and dropped: an avatar is an image, uploaded after the
	// join, and every member without one shows the same seedling.
	p, err = s.store.CreateParticipant(r.Context(), room.ID, req.Name, models.DefaultAvatar, req.Description, req.IsHuman, hash, newOwner, nil, inv.ID)
	if errors.Is(err, models.ErrConflict) {
		// same name = same identity: re-claim it with a fresh token so a
		// restarted agent does not pile up orphan duplicates
		p, err = s.store.ReclaimParticipant(r.Context(), room.ID, req.Name, hash, ownerID)
		if errors.Is(err, models.ErrIdentityOnline) {
			writeErr(w, http.StatusConflict,
				"that name is taken by a participant that is online right now; wait for it to go offline (~90s idle) or pick another name")
			return
		}
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":       token,
			"participant": p,
			"room":        room,
			"reclaimed":   true,
		})
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":       token,
		"participant": p,
		"room":        room,
	})
}

type createInviteReq struct {
	ExpiresInSeconds int  `json:"expires_in_seconds"`
	BindOwner        bool `json:"bind_owner"`
}

// handleCreateInvite mints a link. An agent's link always binds joiners to
// the agent's own principal, so ownership stays server-verified; an admin's
// link binds only on request (the "agent instructions" flow). A plain human
// member can only mint links bound to themselves: that is how they add an
// agent of their own, and a bound link admits no humans at all.
func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req createInviteReq
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	if p.IsHuman && !isAdmin(p) && !req.BindOwner {
		writeErr(w, http.StatusForbidden, "members can only create invite links bound to themselves (bind_owner)")
		return
	}
	if req.ExpiresInSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "expires_in_seconds must be positive")
		return
	}
	var ownerID *string
	switch {
	case !p.IsHuman:
		ownerID = principalOf(p)
	case req.BindOwner:
		ownerID = &p.ID
	}
	// ten years: past that time.Duration overflows and the link is born expired
	if req.ExpiresInSeconds > 10*365*24*3600 {
		writeErr(w, http.StatusBadRequest, "expires_in_seconds is too large")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresInSeconds > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInSeconds) * time.Second)
		expiresAt = &t
	}
	inv, err := s.store.CreateInvite(r.Context(), p.RoomID, secrets.InviteCode(), &p.ID, ownerID, expiresAt)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	out := map[string]any{
		"invite":   s.inviteJSON(inv),
		"join_url": s.inviteURL(inv.Token),
	}
	// behind Cloudflare Access a bare curl to /skill gets the login page, so the
	// invite text has to carry the service token; the caller is already a member
	if s.cfg.AccessClientID != "" {
		out["access"] = map[string]string{
			"client_id":     s.cfg.AccessClientID,
			"client_secret": s.cfg.AccessClientSecret,
		}
	}
	writeJSON(w, http.StatusCreated, out)
}

// principalOf is the identity an agent's invitees get bound to: its owner,
// or itself when nobody owns it.
func principalOf(p models.Participant) *string {
	if p.OwnerID != nil {
		return p.OwnerID
	}
	return &p.ID
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	list, err := s.store.ListInvites(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	out := make([]models.Invite, 0, len(list))
	for _, v := range list {
		out = append(out, s.inviteJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !requireAdmin(w, p) {
		return
	}
	if err := s.store.RevokeInvite(r.Context(), p.RoomID, r.PathValue("id")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePeekInvite shows what a link opens before the visitor has a session.
// A dead link still answers, with its status, so the page can say why.
func (s *Server) handlePeekInvite(w http.ResponseWriter, r *http.Request) {
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	inv, room, err := s.store.InviteByToken(r.Context(), inviteToken(r.URL.Query().Get("token")))
	status := inv.Status
	switch {
	case errors.Is(err, models.ErrInviteRevoked):
		status = "revoked"
	case errors.Is(err, models.ErrInviteExpired):
		status = "expired"
	case err != nil:
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": room.Name, "color": room.Color, "slug": room.Slug, "status": status})
}

// handlePeekRoom shows the room name behind a join URL. The slug is not a
// secret; the name is the only thing it reveals.
func (s *Server) handlePeekRoom(w http.ResponseWriter, r *http.Request) {
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "slow down")
		return
	}
	room, err := s.store.RoomBySlug(r.Context(), strings.TrimSpace(r.URL.Query().Get("slug")))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// the image needs membership to fetch, so the peek carries only the initials colour
	writeJSON(w, http.StatusOK, map[string]any{"name": room.Name, "created_at": room.CreatedAt, "color": room.Color})
}

func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request, p models.Participant) {
	room, err := s.store.RoomByID(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	channels, err := s.store.ListChannelsUnread(r.Context(), p.RoomID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	participants, err := s.store.ListParticipants(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// invite links are secrets that live behind GET /api/v1/invites (admins
	// only); this payload just says whether the caller may manage them
	writeJSON(w, http.StatusOK, map[string]any{
		"room":         room,
		"join_url":     s.cfg.PublicURL + "/r/" + room.Slug,
		"admin":        isAdmin(p),
		"channels":     channels,
		"participants": participants,
	})
}

func (s *Server) inviteURL(token string) string {
	return s.cfg.PublicURL + "/join/" + token
}

// inviteJSON fills the link's URL; the token alone never leaves the store.
func (s *Server) inviteJSON(v models.Invite) models.Invite {
	v.URL = s.inviteURL(v.Token)
	return v
}

// inviteToken accepts a full link or a bare token, with stray whitespace and
// slashes around either.
func inviteToken(s string) string {
	s = strings.Trim(strings.TrimSpace(s), "/ ")
	if i := strings.LastIndex(s, "/join/"); i >= 0 {
		s = s[i+len("/join/"):]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Trim(s, "/ ")
}
