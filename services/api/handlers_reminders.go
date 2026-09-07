package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/schedule"
)

// reminderFiredEvent is appended by the scheduler when a reminder is due.
// Payload: reminder_id, participant_id, participant_name, owner_id, text,
// schedule, kind, fired_at, next_fire_at (null when completed), fire_count.
const reminderFiredEvent = "reminder.fired"

const maxReminderText = 4000

type reminderRequest struct {
	Text     *string `json:"text"`
	Schedule *string `json:"schedule"`
	TZ       *string `json:"tz"`
}

// reminderAgentID is the agent a fired reminder belongs to.
func reminderAgentID(e models.Event) string {
	var pl struct {
		ParticipantID string `json:"participant_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil {
		return ""
	}
	return pl.ParticipantID
}

// reminderVisible: the agent itself, its owner (task 19) and admins.
func reminderVisible(e models.Event, p models.Participant) bool {
	var pl struct {
		ParticipantID string  `json:"participant_id"`
		OwnerID       *string `json:"owner_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil {
		return false
	}
	if pl.ParticipantID == p.ID || isAdmin(p) {
		return true
	}
	return pl.OwnerID != nil && *pl.OwnerID == p.ID
}

// parseReminderSchedule resolves text in tz (IANA name, default UTC) and
// returns the schedule plus its first firing. A one-time moment in the past
// is an error, not a reminder that never fires.
func parseReminderSchedule(text, tz string, now time.Time) (schedule.Schedule, time.Time, string, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || strings.EqualFold(tz, "Local") {
		return schedule.Schedule{}, time.Time{}, "", errors.New("tz must be an IANA zone name like Europe/Sofia")
	}
	sc, err := schedule.Parse(text, loc, now)
	if err != nil {
		return schedule.Schedule{}, time.Time{}, "", err
	}
	next, ok := sc.Next(now)
	if !ok {
		return schedule.Schedule{}, time.Time{}, "", errors.New("that moment already passed")
	}
	return sc, next, tz, nil
}

func (s *Server) handleCreateReminder(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErr(w, http.StatusForbidden, "only agents set reminders for themselves")
		return
	}
	var req reminderRequest
	if !readJSON(w, r, &req) {
		return
	}
	text := ""
	if req.Text != nil {
		text = strings.TrimSpace(*req.Text)
	}
	if text == "" || len(text) > maxReminderText {
		writeErr(w, http.StatusBadRequest, "text is required, at most 4000 characters")
		return
	}
	if req.Schedule == nil || strings.TrimSpace(*req.Schedule) == "" {
		writeErr(w, http.StatusBadRequest, "schedule is required")
		return
	}
	tz := ""
	if req.TZ != nil {
		tz = *req.TZ
	}
	sc, next, _, err := parseReminderSchedule(*req.Schedule, tz, time.Now())
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "bad_schedule", err.Error())
		return
	}
	rem, err := s.store.CreateReminder(r.Context(), p.RoomID, p.ID, text, sc, next)
	if errors.Is(err, models.ErrTooManyReminders) {
		writeErrCode(w, http.StatusConflict, "too_many_reminders", "an agent holds at most 100 reminders")
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rem)
}

func (s *Server) handleListReminders(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErr(w, http.StatusForbidden, "humans see an agent's reminders on its profile")
		return
	}
	list, err := s.store.ListReminders(r.Context(), p.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": list})
}

func (s *Server) handleGetReminder(w http.ResponseWriter, r *http.Request, p models.Participant) {
	rem, err := s.store.ReminderByID(r.Context(), p.RoomID, r.PathValue("rid"))
	if err == nil && rem.ParticipantID != p.ID {
		err = models.ErrNotFound
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rem)
}

func (s *Server) handleUpdateReminder(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErr(w, http.StatusForbidden, "only agents edit their own reminders")
		return
	}
	var req reminderRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Text == nil && req.Schedule == nil && req.TZ == nil {
		writeErr(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if req.Text != nil {
		t := strings.TrimSpace(*req.Text)
		if t == "" || len(t) > maxReminderText {
			writeErr(w, http.StatusBadRequest, "text must be 1 to 4000 characters")
			return
		}
		req.Text = &t
	}
	var sc *schedule.Schedule
	var next *time.Time
	if req.Schedule != nil || req.TZ != nil {
		cur, err := s.store.ReminderByID(r.Context(), p.RoomID, r.PathValue("rid"))
		if err == nil && cur.ParticipantID != p.ID {
			err = models.ErrNotFound
		}
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		text, tz := cur.Schedule, cur.TZ
		if req.Schedule != nil {
			text = *req.Schedule
		}
		if req.TZ != nil {
			tz = *req.TZ
		}
		parsed, n, _, err := parseReminderSchedule(text, tz, time.Now())
		if err != nil {
			writeErrCode(w, http.StatusBadRequest, "bad_schedule", err.Error())
			return
		}
		sc, next = &parsed, &n
		// a tz-only change must not restart an "in 30m" countdown
		if req.Schedule == nil && cur.Kind == schedule.KindOnce && strings.HasPrefix(cur.Schedule, "in ") {
			next = cur.NextFireAt
		}
	}
	rem, err := s.store.UpdateReminder(r.Context(), p.ID, r.PathValue("rid"), req.Text, sc, next)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rem)
}

func (s *Server) handleDeleteReminder(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if err := s.store.DeleteReminder(r.Context(), p.ID, r.PathValue("rid")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownsAgent: the caller is the agent itself, its server-verified owner, or an admin.
func ownsAgent(target, p models.Participant) bool {
	owns := target.OwnerID != nil && *target.OwnerID == p.ID
	return owns || isAdmin(p) || target.ID == p.ID
}

// handleListParticipantReminders: the profile view for the agent's owner.
func (s *Server) handleListParticipantReminders(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !ownsAgent(target, p) {
		writeErr(w, http.StatusForbidden, "only the agent's owner or an admin sees its reminders")
		return
	}
	list, err := s.store.ListReminders(r.Context(), target.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reminders": list})
}

func (s *Server) handleDeleteParticipantReminder(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if !ownsAgent(target, p) {
		writeErr(w, http.StatusForbidden, "only the agent's owner or an admin deletes its reminders")
		return
	}
	if err := s.store.DeleteReminder(r.Context(), target.ID, r.PathValue("rid")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
