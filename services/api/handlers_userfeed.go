package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// feedEvent is a room event tagged with the workspace it came from.
type feedEvent struct {
	models.Event
	Workspace string `json:"workspace"`
}

// feedRoom is one member workspace inside a user feed poll.
type feedRoom struct {
	slug    string
	p       models.Participant
	members map[string]bool
	after   int64
}

// handleUserEvents is the browser's one long-poll for every workspace the
// account belongs to (task 23). cursors=<slug>:<seq>,... names where each
// workspace was left; a member workspace without a cursor answers with its
// latest seq and no events, so the client learns it and picks it up next poll.
// Each room is scanned and gated exactly like /api/v1/events; humans hold no
// delivery receipts, so nothing is marked delivered.
func (s *Server) handleUserEvents(w http.ResponseWriter, r *http.Request, u models.User) {
	q := r.URL.Query()
	cursors := map[string]int64{}
	for _, part := range strings.Split(q.Get("cursors"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		slug, seqText, ok := strings.Cut(part, ":")
		seq, err := strconv.ParseInt(seqText, 10, 64)
		if !ok || slug == "" || err != nil || seq < 0 {
			writeErr(w, http.StatusBadRequest, "cursors must be slug:seq pairs")
			return
		}
		cursors[slug] = seq
	}
	exclude := map[string]bool{}
	for _, t := range strings.Split(q.Get("exclude"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			exclude[t] = true
		}
	}
	wait := time.Duration(0)
	if ws := q.Get("wait"); ws != "" {
		sec, err := strconv.Atoi(ws)
		if err != nil || sec < 0 {
			writeErr(w, http.StatusBadRequest, "wait must be seconds")
			return
		}
		wait = min(time.Duration(sec)*time.Second, maxEventWait)
	}

	rooms, err := s.store.RoomsByUser(r.Context(), u.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	out := map[string]int64{}
	scan := []*feedRoom{}
	for _, ur := range rooms {
		after, known := cursors[ur.Slug]
		if !known {
			seq, err := s.store.LatestSeq(r.Context(), ur.ID)
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			out[ur.Slug] = seq
			continue
		}
		p, err := s.store.ParticipantForUser(r.Context(), ur.ID, u.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		memberIDs, err := s.store.ParticipantChannelIDs(r.Context(), p.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		fr := &feedRoom{slug: ur.Slug, p: p, members: map[string]bool{}, after: after}
		for _, id := range memberIDs {
			fr.members[id] = true
		}
		out[ur.Slug] = after
		scan = append(scan, fr)
	}

	deadline := time.Now().Add(wait)
	for {
		kept := []feedEvent{}
		for _, fr := range scan {
			events, err := s.store.ListEvents(r.Context(), fr.p.RoomID, fr.after, 0)
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			if len(events) == 0 {
				continue
			}
			fr.after = events[len(events)-1].Seq
			out[fr.slug] = fr.after
			got, err := s.filterEvents(r.Context(), events, fr.p, fr.members, map[string]bool{}, exclude, false)
			if err != nil {
				writeStoreErr(w, err)
				return
			}
			for _, e := range got {
				kept = append(kept, feedEvent{Event: e, Workspace: fr.slug})
			}
		}
		if len(kept) > 0 || time.Now().After(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"events": kept, "cursors": out})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(eventPollTick):
		}
	}
}
