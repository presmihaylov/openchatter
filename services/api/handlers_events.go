package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

const (
	maxEventWait  = 30 * time.Second
	eventPollTick = 700 * time.Millisecond
)

// handleEvents returns events after a cursor, long-polling up to wait seconds.
// With no "after" param it returns the current cursor so clients can start tailing.
// Filters: types=a,b limits event types; relevant=true keeps only message
// events that are broadcasts, mention the caller, or belong to threads the
// caller wrote in. The cursor always advances past filtered-out events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, p models.Participant) {
	q := r.URL.Query()

	if q.Get("after") == "" {
		seq, err := s.store.LatestSeq(r.Context(), p.RoomID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": []models.Event{}, "cursor": seq})
		return
	}

	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeErr(w, http.StatusBadRequest, "after must be a non-negative integer")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))

	types := map[string]bool{}
	for _, t := range strings.Split(q.Get("types"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			types[t] = true
		}
	}
	// exclude drops whole event types server-side so a watcher that never
	// wants reactions does not pay to receive and discard them
	exclude := map[string]bool{}
	for _, t := range strings.Split(q.Get("exclude"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			exclude[t] = true
		}
	}
	relevant := q.Get("relevant") == "true"

	// snapshot the caller's channel membership once per poll; filterEvents
	// keeps it current from the caller's own member_joined/left events, so a
	// mid-poll add is neither dropped nor lost (the cursor moves past it).
	memberIDs, err := s.store.ParticipantChannelIDs(r.Context(), p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	members := map[string]bool{}
	for _, id := range memberIDs {
		members[id] = true
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

	deadline := time.Now().Add(wait)
	// a declared-offline agent (task 21) hears nothing: hold for the wait,
	// then hand back an empty batch at the same cursor, so nothing is skipped
	if p.DeclaredOffline && !p.IsHuman {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(wait):
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": []models.Event{}, "cursor": after, "presence": "offline"})
		return
	}
	for {
		events, err := s.store.ListEvents(r.Context(), p.RoomID, after, limit)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		scanned := after
		if len(events) > 0 {
			scanned = events[len(events)-1].Seq
		}
		kept, err := s.filterEvents(r.Context(), events, p, members, types, exclude, relevant)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if len(kept) > 0 || time.Now().After(deadline) {
			// a message this poll hands to an agent counts as delivered for its
			// receipt, whatever filter the agent asked for; humans hold no receipts
			if !p.IsHuman {
				seqs := []int64{}
				for _, e := range kept {
					if e.Type == "message.created" || e.Type == reminderFiredEvent {
						seqs = append(seqs, e.Seq)
					}
				}
				if err := s.store.MarkDelivered(r.Context(), p.RoomID, p.ID, seqs); err != nil {
					writeStoreErr(w, err)
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"events": kept, "cursor": scanned})
			return
		}
		// everything in this batch was filtered out — advance past it so the
		// long-poll keeps waiting for something relevant instead of returning empty
		after = scanned
		select {
		case <-r.Context().Done():
			return
		case <-time.After(eventPollTick):
		}
	}
}

// eventMessage is the slice of a message payload the relevance filter needs.
type eventMessage struct {
	ID           string   `json:"id"`
	ThreadRootID *string  `json:"thread_root_id"`
	IsBroadcast  bool     `json:"is_broadcast"`
	Kind         string   `json:"kind"`
	Mentions     []string `json:"mentions"`
}

// gatedChannel returns the channel an event belongs to and whether membership
// gates its delivery. Content events (message.created/edited) and membership
// events (channel.member_joined/left) reach only that channel's members.
// Everything else — participant.*, channel.created/archived/deleted, and
// message.deleted (which carries only a message id, no channel_id) — is
// delivered to everyone as before.
func gatedChannel(e models.Event) (string, bool) {
	switch e.Type {
	case "message.created", "message.edited", "message.reaction", "message.ack",
		"channel.member_joined", "channel.member_left",
		"channel.privacy_changed", "channel.renamed":
		var pl struct {
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(e.Payload, &pl); err != nil || pl.ChannelID == "" {
			return "", false
		}
		return pl.ChannelID, true
	}
	return "", false
}

// ownMembership reports whether the event is this participant's own
// channel.member_joined/left. Both must bypass the membership gate: the
// snapshot predates a mid-poll add, and the removal arrives after the
// participant is already gone. The bool is joined (true) or left (false).
func ownMembership(e models.Event, participantID string) (chID string, joined, ok bool) {
	if e.Type != "channel.member_joined" && e.Type != "channel.member_left" {
		return "", false, false
	}
	var pl struct {
		ChannelID     string `json:"channel_id"`
		ParticipantID string `json:"participant_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil || pl.ParticipantID != participantID {
		return "", false, false
	}
	return pl.ChannelID, e.Type == "channel.member_joined", true
}

func (s *Server) filterEvents(ctx context.Context, events []models.Event, p models.Participant, members, types, exclude map[string]bool, relevant bool) ([]models.Event, error) {
	kept := []models.Event{}
	// message events whose fate depends on thread participation, keyed by root
	pending := map[string][]models.Event{}
	rootIDs := []string{}

	for _, e := range events {
		// membership gate first, so it runs on every path including the web UI
		// firehose (types empty, relevant=false): a non-member of a channel
		// never receives its messages or its membership events. Exception:
		// your own join or removal, which also updates the snapshot so the
		// rest of this batch is gated by your new membership.
		ownCh, joined, own := ownMembership(e, p.ID)
		if own {
			members[ownCh] = joined
		}
		if chID, gated := gatedChannel(e); gated && !members[chID] && !own {
			continue
		}
		// a fired reminder is private: only the agent, its server-verified
		// owner and admins ever see it, on every path including the firehose
		if e.Type == reminderFiredEvent && !reminderVisible(e, p) {
			continue
		}
		if len(types) > 0 && !types[e.Type] {
			continue
		}
		if exclude[e.Type] {
			continue
		}
		if !relevant {
			kept = append(kept, e)
			continue
		}
		// a reaction is relevant to the author of the message it landed on;
		// your own reactions are not news to you
		if e.Type == "message.reaction" {
			var rx models.ReactionEvent
			if err := json.Unmarshal(e.Payload, &rx); err == nil && rx.AuthorID == p.ID && rx.ParticipantID != p.ID {
				kept = append(kept, e)
			}
			continue
		}
		// an ack on your ask is news to you; your own ack is not. Reactions
		// never wake an agent and neither does this: it is a receipt.
		if e.Type == "message.ack" {
			var ak models.AckEvent
			if err := json.Unmarshal(e.Payload, &ak); err == nil && ak.AuthorID == p.ID && ak.ParticipantID != p.ID {
				kept = append(kept, e)
			}
			continue
		}
		// a fired reminder is news to the agent it belongs to, not its owner
		if e.Type == reminderFiredEvent {
			if reminderAgentID(e) == p.ID {
				kept = append(kept, e)
			}
			continue
		}
		// a capability call is news to its target, a result to its caller
		if e.Type == capabilityCallEvent || e.Type == capabilityResult {
			if capabilityRelevant(e, p.ID) {
				kept = append(kept, e)
			}
			continue
		}
		// relevant=true only ever passes message payloads — other types carry
		// no addressing info to judge relevance by
		if e.Type != "message.created" && e.Type != "message.edited" {
			continue
		}
		var m eventMessage
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			continue
		}
		// timeline entries ("x left this thread") are never news to anyone
		if m.Kind == "system" {
			continue
		}
		if m.IsBroadcast || slices.Contains(m.Mentions, p.Name) {
			kept = append(kept, e)
			continue
		}
		if m.ThreadRootID != nil {
			if _, seen := pending[*m.ThreadRootID]; !seen {
				rootIDs = append(rootIDs, *m.ThreadRootID)
			}
			pending[*m.ThreadRootID] = append(pending[*m.ThreadRootID], e)
		}
	}

	if len(rootIDs) > 0 {
		mine, err := s.store.ParticipatedThreadRoots(ctx, p.RoomID, p.ID, rootIDs)
		if err != nil {
			return nil, err
		}
		for root, evs := range pending {
			if mine[root] {
				kept = append(kept, evs...)
			}
		}
		// participation check regroups by thread — restore log order
		slices.SortFunc(kept, func(a, b models.Event) int { return int(a.Seq - b.Seq) })
	}
	return kept, nil
}
