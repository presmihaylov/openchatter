package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/imgvariant"
)

func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request, p models.Participant) {
	me, err := s.store.ParticipantByID(r.Context(), p.RoomID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// a linked human learns their account through the users join, whichever
	// credential they used; agents see today's shape
	writeJSON(w, http.StatusOK, me)
}

type updateMeReq struct {
	Name *string `json:"name"`
	// Legacy emoji avatar. Read and ignored for one release so an old client
	// does not start getting 4xx; an avatar is an image, set with
	// POST /api/v1/me/avatar. Drop the field after that.
	Avatar      *string `json:"avatar"`
	Description *string `json:"description"`
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req updateMeReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name != nil && !validParticipantName(*req.Name) {
		writeErr(w, http.StatusBadRequest, "name must be 2-32 chars: letters, digits, spaces, - or _; no leading/trailing/double spaces")
		return
	}
	if req.Name != nil && isReservedName(*req.Name) {
		writeErr(w, http.StatusBadRequest, "that name is reserved")
		return
	}
	if req.Description != nil && len(*req.Description) > 2000 {
		writeErr(w, http.StatusBadRequest, "description too long")
		return
	}
	me, err := s.store.UpdateProfile(r.Context(), p.RoomID, p.ID, req.Name, nil, req.Description)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

// handleSetAvatar uploads a profile image (stored like any attachment) and
// points the caller's avatar at it. Without one the member shows the seedling.
func (s *Server) handleSetAvatar(w http.ResponseWriter, r *http.Request, p models.Participant) {
	meta, ok := s.readAvatarUpload(w, r, p)
	if !ok {
		return
	}
	me, err := s.store.SetAvatarAttachment(r.Context(), p.RoomID, p.ID, &meta.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

// readAvatarUpload stores the multipart "file" as an attachment when it is an
// image under the size cap; on any failure the error is already written.
func (s *Server) readAvatarUpload(w http.ResponseWriter, r *http.Request, p models.Participant) (models.AttachmentMeta, bool) {
	var none models.AttachmentMeta
	if !s.uploadLimit.Allow("up:" + p.ID) {
		writeErr(w, http.StatusTooManyRequests, "too many uploads, slow down")
		return none, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+64*1024)
	if err := r.ParseMultipartForm(maxAttachmentBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid multipart form (5MB max): "+err.Error())
		return none, false
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, `multipart field "file" is required`)
		return none, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
	if err != nil || len(data) > maxAttachmentBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "avatar image exceeds 5MB")
		return none, false
	}
	// sniff, don't trust the client header — avatars render as <img> for everyone
	contentType := http.DetectContentType(data)
	if !strings.HasPrefix(contentType, "image/") {
		writeErr(w, http.StatusBadRequest, "avatar must be an image (png, jpeg, gif, webp)")
		return none, false
	}
	filename := header.Filename
	if filename == "" {
		filename = "avatar"
	}
	meta, err := s.store.CreateAttachment(r.Context(), p.RoomID, p.ID, filename, contentType, data)
	if err != nil {
		writeStoreErr(w, err)
		return none, false
	}
	// the chrome draws this at 20 to 96px: store the small copies now, and
	// mark an upload that will not resize so the original serves every size
	v, verr := imgvariant.Make(data, contentType)
	if verr != nil {
		_ = s.store.SetAttachmentVariants(r.Context(), meta.ID, nil, nil, "none")
		return meta, true
	}
	if err := s.store.SetAttachmentVariants(r.Context(), meta.ID, v.Small, v.Large, v.ContentType); err != nil {
		slog.Warn("avatar variants not stored", "id", meta.ID, "err", err)
	}
	return meta, true
}

func (s *Server) handleRemoveAvatar(w http.ResponseWriter, r *http.Request, p models.Participant) {
	me, err := s.store.SetAvatarAttachment(r.Context(), p.RoomID, p.ID, nil)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (s *Server) handleGoOffline(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if err := s.store.GoOffline(r.Context(), p.RoomID, p.ID); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "offline"})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, p models.Participant) {
	// authed() already touched presence; a declared-offline agent stays offline
	writeJSON(w, http.StatusOK, map[string]string{"status": presenceAfterTouch(p)})
}

func presenceAfterTouch(p models.Participant) string {
	if p.DeclaredOffline {
		return "offline"
	}
	return "online"
}

// presenceCatchupCap bounds one online batch; the rest stays behind the
// cursor and the next poll or inbox drain picks it up.
const presenceCatchupCap = 500

// handlePresence (task 21): an agent declares itself offline or online.
// Offline is sticky: polls hold and hand out nothing, mentions queue as
// deferred receipts, the roster shows a grey dot. Online returns the relevant
// message events (mentions, its threads, root broadcasts) since it went
// offline, past the caller's own cursor when it sends one, so a watcher
// never hears an event twice. A second online returns an empty batch.

func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErrCode(w, http.StatusForbidden, "agents_only", "presence is declared by agents; a human is online while the app is open")
		return
	}
	var req struct {
		Status string `json:"status"`
		After  *int64 `json:"after"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	switch req.Status {
	case "offline":
		if err := s.store.DeclareOffline(r.Context(), p.RoomID, p.ID); err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "offline"})
	case "online":
		since, wasOffline, err := s.store.DeclareOnline(r.Context(), p.RoomID, p.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		latest, err := s.store.LatestSeq(r.Context(), p.RoomID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if !wasOffline {
			// Nothing to hand out: it never left, or another online call already took
			// the batch. Echo the caller's cursor so a stale cursor file is never
			// pushed past events the poll still has to replay.
			cursor := latest
			if req.After != nil {
				cursor = *req.After
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "online", "events": []models.Event{}, "cursor": cursor, "was_offline": false})
			return
		}
		// The caller's cursor is the truth about what it has heard; the offline
		// mark only stands in when no cursor is sent.
		from := since
		if req.After != nil {
			from = *req.After
		}
		events, cursor, err := s.catchup(r, p, from)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "online", "events": events, "cursor": cursor, "was_offline": true})
	default:
		writeErr(w, http.StatusBadRequest, `status must be "online" or "offline"`)
	}
}

// catchup lists the relevant message events after from, the same set a
// relevant=true poll would have handed out, and marks them delivered.
func (s *Server) catchup(r *http.Request, p models.Participant, from int64) ([]models.Event, int64, error) {
	memberIDs, err := s.store.ParticipantChannelIDs(r.Context(), p.ID)
	if err != nil {
		return nil, 0, err
	}
	members := map[string]bool{}
	for _, id := range memberIDs {
		members[id] = true
	}
	types := map[string]bool{"message.created": true, reminderFiredEvent: true}
	kept := []models.Event{}
	cursor := from
	for len(kept) < presenceCatchupCap {
		page, err := s.store.ListEvents(r.Context(), p.RoomID, cursor, 500)
		if err != nil {
			return nil, 0, err
		}
		if len(page) == 0 {
			break
		}
		got, err := s.filterEvents(r.Context(), page, p, members, types, nil, true)
		if err != nil {
			return nil, 0, err
		}
		kept = append(kept, got...)
		cursor = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	if len(kept) > presenceCatchupCap {
		kept = kept[:presenceCatchupCap]
		cursor = kept[len(kept)-1].Seq
	}
	seqs := []int64{}
	for _, e := range kept {
		seqs = append(seqs, e.Seq)
	}
	if err := s.store.MarkDelivered(r.Context(), p.RoomID, p.ID, seqs); err != nil {
		return nil, 0, err
	}
	return kept, cursor, nil
}

func (s *Server) handleListParticipants(w http.ResponseWriter, r *http.Request, p models.Participant) {
	list, err := s.store.ListParticipants(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"participants": list})
}

// resolveParticipant accepts a participant id, a name, or "me".
func (s *Server) resolveParticipant(r *http.Request, me models.Participant, ref string) (models.Participant, error) {
	if ref == "me" || ref == me.ID {
		return s.store.ParticipantByID(r.Context(), me.RoomID, me.ID)
	}
	p, err := s.store.ParticipantByName(r.Context(), me.RoomID, ref)
	if err == nil {
		return p, nil
	}
	// only a clean miss falls through to id lookup; other errors must surface
	if !errors.Is(err, models.ErrNotFound) {
		return models.Participant{}, err
	}
	if !isUUID(ref) {
		return models.Participant{}, models.ErrNotFound
	}
	return s.store.ParticipantByID(r.Context(), me.RoomID, ref)
}

func (s *Server) handleGetParticipant(w http.ResponseWriter, r *http.Request, p models.Participant) {
	got, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}

type addTagReq struct {
	Tag string `json:"tag"`
}

func (s *Server) handleAddTag(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req addTagReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Tag = strings.ToLower(strings.TrimSpace(req.Tag))
	if req.Tag == "" || len(req.Tag) > 50 {
		writeErr(w, http.StatusBadRequest, "tag must be 1-50 characters")
		return
	}
	if err := s.store.AddTag(r.Context(), p.RoomID, target.ID, req.Tag, p.ID); err != nil {
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

func (s *Server) handleRemoveTag(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	tag := strings.ToLower(strings.TrimSpace(r.PathValue("tag")))
	if err := s.store.RemoveTag(r.Context(), p.RoomID, target.ID, tag); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (s *Server) handleGetNotifyPrefs(w http.ResponseWriter, r *http.Request, p models.Participant) {
	np, err := s.store.NotifyPrefs(r.Context(), p.RoomID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, np)
}

type notifyPrefsReq struct {
	Enabled          *bool `json:"enabled"`
	Sound            *bool `json:"sound"`
	ArchiveAfterSecs *int  `json:"archive_after_secs"`
}

func (s *Server) handleSetNotifyPrefs(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req notifyPrefsReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil && req.Sound == nil && req.ArchiveAfterSecs == nil {
		writeErr(w, http.StatusBadRequest, "nothing to change: send enabled, sound and/or archive_after_secs")
		return
	}
	if req.ArchiveAfterSecs != nil && *req.ArchiveAfterSecs < 0 {
		writeErr(w, http.StatusBadRequest, "archive_after_secs must be 0 (never) or a positive number of seconds")
		return
	}
	np, err := s.store.SetNotifyPrefs(r.Context(), p.RoomID, p.ID, req.Enabled, req.Sound, req.ArchiveAfterSecs)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, np)
}
