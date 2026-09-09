package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/mentions"
)

const (
	maxMessageBytes    = 32 * 1024
	maxAttachmentBytes = 5 * 1024 * 1024
)

type postMessageReq struct {
	Body          string   `json:"body"`
	ThreadRootID  *string  `json:"thread_root_id"`
	AttachmentIDs []string `json:"attachment_ids"`
	// Broadcast only keeps old clients that send false working. A true value is
	// rejected: broadcasts are derived exclusively from a visible body mention.
	Broadcast bool `json:"broadcast"`
	// AllowUnknownMentions posts anyway when the body names a handle nobody
	// answers to. Without it an unknown @handle is a 422, so a typo cannot
	// silently address nobody.
	AllowUnknownMentions bool `json:"allow_unknown_mentions"`
}

// postMessageResp keeps the flat message shape clients already read and adds
// delivery warnings, e.g. a mention of somebody who is not in this channel.
type postMessageResp struct {
	models.Message
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	if ch.Archived {
		writeErr(w, http.StatusConflict, "channel is archived")
		return
	}

	var req postMessageReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" && len(req.AttachmentIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "message needs a body or attachments")
		return
	}
	if len(req.Body) > maxMessageBytes {
		writeErr(w, http.StatusBadRequest, "message too long (32KB max)")
		return
	}
	if req.Broadcast {
		writeErr(w, http.StatusBadRequest, "broadcast flag was removed; put a visible broadcast mention in the message body")
		return
	}
	if len(req.AttachmentIDs) > 10 {
		writeErr(w, http.StatusBadRequest, "too many attachments (10 max)")
		return
	}
	seenAtt := map[string]bool{}
	dedupedAtt := make([]string, 0, len(req.AttachmentIDs))
	for _, aid := range req.AttachmentIDs {
		if !isUUID(aid) {
			writeErr(w, http.StatusBadRequest, "invalid attachment id: "+aid)
			return
		}
		if seenAtt[aid] {
			continue
		}
		seenAtt[aid] = true
		dedupedAtt = append(dedupedAtt, aid)
	}
	req.AttachmentIDs = dedupedAtt

	// replies always attach to the thread root, never nest
	if req.ThreadRootID != nil {
		if !isUUID(*req.ThreadRootID) {
			writeErr(w, http.StatusNotFound, "thread root not found")
			return
		}
		root, err := s.store.MessageByID(r.Context(), p.RoomID, *req.ThreadRootID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if root.ChannelID != ch.ID {
			writeErr(w, http.StatusBadRequest, "thread root is in a different channel")
			return
		}
		if root.ThreadRootID != nil {
			req.ThreadRootID = root.ThreadRootID
		}
	}

	participants, err := s.store.ListParticipants(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	known := map[string]bool{}
	byName := map[string]string{}
	for _, part := range participants {
		known[part.Name] = true
		byName[part.Name] = part.ID
	}
	names, broadcast := mentions.Parse(req.Body, known)
	mentionIDs := make([]string, 0, len(names))
	for _, n := range names {
		mentionIDs = append(mentionIDs, byName[n])
	}

	if unknown := mentions.Unknown(req.Body, known); len(unknown) > 0 && !req.AllowUnknownMentions {
		// the roster rides along so a client can refresh its cache and retry
		// without a second round trip
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":            "unknown mentions: " + strings.Join(unknown, ", "),
			"unknown_mentions": unknown,
			"members":          toRoster(participants, nil),
			"hint":             "fetch GET /api/v1/members, or resend with allow_unknown_mentions: true",
		})
		return
	}

	// a mentioned room member who is not in this channel never sees the
	// message; the sender must learn that instead of waiting for a reply
	var warnings []string
	if len(names) > 0 {
		inChannel, err := s.channelMemberSet(r, p.RoomID, ch.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		for _, n := range names {
			if !inChannel[byName[n]] {
				warnings = append(warnings, "@"+n+" is not a member of #"+ch.Name+" and will not receive this message")
			}
		}
	}

	msg, err := s.store.CreateMessage(r.Context(), models.CreateMessageParams{
		RoomID:        p.RoomID,
		ChannelID:     ch.ID,
		ThreadRootID:  req.ThreadRootID,
		AuthorID:      p.ID,
		Body:          req.Body,
		IsBroadcast:   broadcast,
		AttachmentIDs: req.AttachmentIDs,
		MentionIDs:    mentionIDs,
	})
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, postMessageResp{Message: msg, Warnings: warnings})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var before *time.Time
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before must be RFC3339")
			return
		}
		before = &t
	}
	var beforeID *string
	var beforeAt *time.Time
	if bid := q.Get("before_id"); bid != "" {
		if !isUUID(bid) {
			writeErr(w, http.StatusNotFound, "before_id not found")
			return
		}
		cursor, err := s.store.MessageByID(r.Context(), p.RoomID, bid)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		beforeID = &cursor.ID
		beforeAt = &cursor.CreatedAt
	}
	msgs, err := s.store.ListChannelMessages(r.Context(), p.RoomID, ch.ID, before, beforeID, beforeAt, limit)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channel": ch, "messages": msgs})
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	msg, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, msg.ChannelID) {
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

type editMessageReq struct {
	Body string `json:"body"`
}

// handleEditMessage: authors edit their own messages. Edits never create an
// alert, but removing a visible broadcast mention clears its stored state.
func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	msg, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if msg.AuthorID != p.ID {
		writeErr(w, http.StatusForbidden, "only the author can edit a message")
		return
	}
	var req editMessageReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" || len(req.Body) > maxMessageBytes {
		writeErr(w, http.StatusBadRequest, "body must be 1 byte to 32KB")
		return
	}
	_, bodyBroadcast := mentions.Parse(req.Body, nil)
	if bodyBroadcast && !msg.IsBroadcast {
		writeErr(w, http.StatusBadRequest, "a broadcast mention cannot be added by editing; send a new message")
		return
	}
	updated, err := s.store.UpdateMessageBody(r.Context(), p.RoomID, id, req.Body, msg.IsBroadcast && bodyBroadcast)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteMessage: the author or an admin; deleting a thread root deletes its replies.
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	msg, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if msg.AuthorID != p.ID && !isAdmin(p) {
		writeErr(w, http.StatusForbidden, "only the author or an admin can delete a message")
		return
	}
	if err := s.store.DeleteMessage(r.Context(), p.RoomID, id); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// gate on the thread's channel before returning any of its messages
	root, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, root.ChannelID) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, err := s.store.ListThread(r.Context(), p.RoomID, id, limit)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// resolveThreadRoot maps a message id (root or reply) to the thread's root.
func (s *Server) resolveThreadRoot(r *http.Request, p models.Participant) (string, error) {
	id := r.PathValue("id")
	if !isUUID(id) {
		return "", models.ErrNotFound
	}
	m, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		return "", err
	}
	if m.ThreadRootID != nil {
		return *m.ThreadRootID, nil
	}
	return m.ID, nil
}

// handleListThreads: the caller's thread tree for one channel — threads they
// started, replied in, or were mentioned in.
func (s *Server) handleListThreads(w http.ResponseWriter, r *http.Request, p models.Participant) {
	ch, err := s.resolveChannel(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, ch.ID) {
		return
	}
	list, err := s.store.ListInvolvedThreads(r.Context(), p.RoomID, ch.ID, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": list})
}

// handleListRoomThreads: the caller's whole thread tree across the room, each
// thread tagged with its channel_id so the sidebar can nest it under its
// parent channel.
func (s *Server) handleListRoomThreads(w http.ResponseWriter, r *http.Request, p models.Participant) {
	// ?include_archived=1 is the web sidebar's view; agents keep the default
	list, err := s.store.ListInvolvedThreadsRoom(r.Context(), p.RoomID, p.ID)
	if r.URL.Query().Get("include_archived") == "1" {
		list, err = s.store.ListInvolvedThreadsRoomAll(r.Context(), p.RoomID, p.ID)
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": list})
}

func (s *Server) handleThreadRead(w http.ResponseWriter, r *http.Request, p models.Participant) {
	root, err := s.resolveThreadRoot(r, p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	at, err := s.store.MarkThreadRead(r.Context(), p.ID, root)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root_id": root, "last_read_at": at})
}

type threadMuteReq struct {
	Muted bool `json:"muted"`
}

func (s *Server) handleThreadMute(w http.ResponseWriter, r *http.Request, p models.Participant) {
	root, err := s.resolveThreadRoot(r, p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req threadMuteReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.SetThreadMuted(r.Context(), p.ID, root, req.Muted); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root_id": root, "muted": req.Muted})
}

type threadSubscribeReq struct {
	Subscribed bool `json:"subscribed"`
}

func (s *Server) handleThreadSubscribe(w http.ResponseWriter, r *http.Request, p models.Participant) {
	root, err := s.resolveThreadRoot(r, p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req threadSubscribeReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.SetThreadSubscribed(r.Context(), p.ID, root, req.Subscribed); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root_id": root, "subscribed": req.Subscribed})
}

type threadLeaveReq struct {
	Left bool `json:"left"`
}

func (s *Server) handleThreadLeave(w http.ResponseWriter, r *http.Request, p models.Participant) {
	root, err := s.resolveThreadRoot(r, p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req threadLeaveReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.SetThreadLeft(r.Context(), p.RoomID, p.ID, root, req.Left); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root_id": root, "left": req.Left})
}

type threadResolveReq struct {
	Resolved bool `json:"resolved"`
}

func (s *Server) handleThreadResolve(w http.ResponseWriter, r *http.Request, p models.Participant) {
	root, err := s.resolveThreadRoot(r, p)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var req threadResolveReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.SetThreadResolved(r.Context(), p.ID, root, req.Resolved); err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root_id": root, "resolved": req.Resolved})
}

const maxReactionBytes = 64

type reactionReq struct {
	Emoji string `json:"emoji"`
}

// validReaction accepts a unicode emoji or a :shortcode:; it rejects blanks,
// whitespace, and anything long enough to be a sentence rather than a symbol.
func validReaction(e string) bool {
	if e == "" || len(e) > maxReactionBytes {
		return false
	}
	for _, r := range e {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// handleAddReaction adds the caller's emoji to a message (idempotent) and
// returns the message's reactions afterwards.
func (s *Server) handleAddReaction(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req reactionReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Emoji = strings.TrimSpace(req.Emoji)
	if !validReaction(req.Emoji) {
		writeErr(w, http.StatusBadRequest, "emoji must be a single emoji or :shortcode: (64 bytes max)")
		return
	}
	ev, err := s.store.SetReaction(r.Context(), p.RoomID, id, p.ID, req.Emoji, true)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reactions": ev.Reactions})
}

// handleRemoveReaction removes the caller's emoji from a message (idempotent).
func (s *Server) handleRemoveReaction(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	emoji := strings.TrimSpace(r.PathValue("emoji"))
	if !validReaction(emoji) {
		writeErr(w, http.StatusBadRequest, "emoji must be a single emoji or :shortcode: (64 bytes max)")
		return
	}
	ev, err := s.store.SetReaction(r.Context(), p.RoomID, id, p.ID, emoji, false)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reactions": ev.Reactions})
}

type replaceReactionsReq struct {
	Emojis []string `json:"emojis"`
}

// handleReplaceReactions makes the caller's reactions on a message exactly the
// given list (others' reactions untouched); an invalid entry refuses the whole
// request so a typo never half-applies.
func (s *Server) handleReplaceReactions(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req replaceReactionsReq
	if !readJSON(w, r, &req) {
		return
	}
	emojis := make([]string, 0, len(req.Emojis))
	for _, e := range req.Emojis {
		e = strings.TrimSpace(e)
		if !validReaction(e) {
			writeErr(w, http.StatusBadRequest, "every emoji must be a single emoji or :shortcode: (64 bytes max)")
			return
		}
		emojis = append(emojis, e)
	}
	reactions, err := s.store.ReplaceReactions(r.Context(), p.RoomID, id, p.ID, emojis)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reactions": reactions})
}

func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if !s.uploadLimit.Allow("up:" + p.ID) {
		writeErr(w, http.StatusTooManyRequests, "too many uploads, slow down")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+64*1024)
	if err := r.ParseMultipartForm(maxAttachmentBytes); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "attachment exceeds 5MB")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid multipart form (5MB max): "+err.Error())
		return
	}
	// ParseMultipartForm spills parts over the memory limit to temp files on
	// disk; without this they leak until process exit
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, `multipart field "file" is required`)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxAttachmentBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed reading upload")
		return
	}
	if len(data) > maxAttachmentBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "attachment exceeds 5MB")
		return
	}

	filename := header.Filename
	if filename == "" {
		filename = "attachment"
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	meta, err := s.store.CreateAttachment(r.Context(), p.RoomID, p.ID, filename, contentType, data)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, meta)
}

func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// ?size=128|512 serves the resized copy an avatar or logo got on upload;
	// an upload that has none (older, non-image) falls back to the original
	size := 0
	switch r.URL.Query().Get("size") {
	case "":
	case "128":
		size = models.VariantSmall
	case "512":
		size = models.VariantLarge
	default:
		writeErr(w, http.StatusBadRequest, "size must be 128 or 512")
		return
	}
	// an attachment never changes under its id (a new avatar is a new id), so
	// the browser may keep it for good; private keeps shared caches out
	// (the byte count is in the tag so a later backfill invalidates a client
	// that cached the original under ?size= before the variant existed)
	a, err := s.store.AttachmentSized(r.Context(), p.RoomID, id, size)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	etag := `"` + id + `-` + strconv.Itoa(size) + `-` + strconv.Itoa(len(a.Data)) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// never let uploaded content execute in the UI's origin
	w.Header().Set("Content-Type", safeContentType(a.ContentType))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(a.Filename)+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Data)
}

// safeContentType downgrades types browsers would execute or render as HTML.
func safeContentType(ct string) string {
	lower := strings.ToLower(ct)
	for _, bad := range []string{"text/html", "application/xhtml", "image/svg"} {
		if strings.HasPrefix(lower, bad) {
			return "application/octet-stream"
		}
	}
	return ct
}

func sanitizeFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return '_'
		}
		return r
	}, name)
}

// handleAckMessage records that the caller has taken this ask on. Idempotent:
// acking twice succeeds, so a retry after a dropped response never errors.
func (s *Server) handleAckMessage(w http.ResponseWriter, r *http.Request, p models.Participant) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// you can only ack what you could read: membership is the read gate here as
	// everywhere else, and an ack paints your name into other people's UI
	msg, err := s.store.MessageByID(r.Context(), p.RoomID, id)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !s.requireChannelMember(w, r, p, msg.ChannelID) {
		return
	}
	ev, err := s.store.SetAck(r.Context(), p.RoomID, id, p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acked_by": ev.AckedBy})
}

// handlePendingAcks lists the asks addressed to the caller that it has not
// acked. This is what the watcher nags from, so it is scoped to the caller
// only: no participant can read another's pending list.
func (s *Server) handlePendingAcks(w http.ResponseWriter, r *http.Request, p models.Participant) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	pending, err := s.store.PendingAcks(r.Context(), p.RoomID, p.ID, limit)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": pending})
}
