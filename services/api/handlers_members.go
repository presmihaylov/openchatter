package api

import (
	"net/http"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// dormantAfter marks a member whose token has not connected in this long. It is
// far longer than the online window: dormant means "probably gone", not "away".
const dormantAfter = 14 * 24 * time.Hour

// rosterMember is the authoritative handle list clients validate against before
// they send a mention. It is deliberately smaller than the participant shape:
// identity and liveness only, so nothing here goes stale in a client cache.
type rosterMember struct {
	ID         string    `json:"id"`
	Handle     string    `json:"handle"`
	IsHuman    bool      `json:"is_human"`
	Role       string    `json:"role"`
	Online     bool      `json:"online"`
	Dormant    bool      `json:"dormant"`
	LastSeenAt time.Time `json:"last_seen_at"`
	// a logged-in human's account; agents and cli humans have neither
	UserID   *string `json:"user_id,omitempty"`
	Username *string `json:"username,omitempty"`
	// InChannel is set only when the caller passed ?channel=; a mention of a
	// member with in_channel false never reaches them.
	InChannel *bool `json:"in_channel,omitempty"`
}

func toRoster(list []models.Participant, inChannel map[string]bool) []rosterMember {
	out := make([]rosterMember, 0, len(list))
	cutoff := time.Now().Add(-dormantAfter)
	for _, p := range list {
		m := rosterMember{
			ID:         p.ID,
			Handle:     p.Name,
			IsHuman:    p.IsHuman,
			Role:       p.Role,
			Online:     p.Online,
			Dormant:    p.LastSeenAt.Before(cutoff),
			LastSeenAt: p.LastSeenAt,
			UserID:     p.UserID,
			Username:   p.Username,
		}
		if inChannel != nil {
			member := inChannel[p.ID]
			m.InChannel = &member
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, p models.Participant) {
	list, err := s.store.ListParticipants(r.Context(), p.RoomID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	var inChannel map[string]bool
	if ref := r.URL.Query().Get("channel"); ref != "" {
		ch, err := s.resolveChannel(r, p, ref)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		inChannel, err = s.channelMemberSet(r, p.RoomID, ch.ID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": toRoster(list, inChannel)})
}

func (s *Server) channelMemberSet(r *http.Request, roomID, channelID string) (map[string]bool, error) {
	members, err := s.store.ListChannelMembers(r.Context(), roomID, channelID)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, m := range members {
		set[m.ID] = true
	}
	return set, nil
}
