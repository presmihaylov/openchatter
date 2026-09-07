package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/presmihaylov/openchatter/pkg/schedule"
)

// Reminder is one agent-owned reminder. NextFireAt is nil once a one-time
// reminder completed.
type Reminder struct {
	ID            string     `json:"id"`
	RoomID        string     `json:"room_id"`
	ParticipantID string     `json:"participant_id"`
	Text          string     `json:"text"`
	Schedule      string     `json:"schedule"`
	Kind          string     `json:"kind"`
	TZ            string     `json:"tz"`
	NextFireAt    *time.Time `json:"next_fire_at"`
	LastFiredAt   *time.Time `json:"last_fired_at"`
	FireCount     int        `json:"fire_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Recurring reports whether the reminder reschedules itself after a fire.
func (r Reminder) Recurring() bool { return r.Kind != schedule.KindOnce }

// MaxRemindersPerAgent caps what one agent may hold at once.
const MaxRemindersPerAgent = 100

// ErrTooManyReminders is returned when an agent is at the cap.
var ErrTooManyReminders = errors.New("too many reminders")

const reminderCols = `id, room_id, participant_id, text, schedule, kind, tz, next_fire_at, last_fired_at, fire_count, created_at, updated_at`

func scanReminder(row pgx.Row) (Reminder, error) {
	var r Reminder
	err := row.Scan(&r.ID, &r.RoomID, &r.ParticipantID, &r.Text, &r.Schedule, &r.Kind, &r.TZ,
		&r.NextFireAt, &r.LastFiredAt, &r.FireCount, &r.CreatedAt, &r.UpdatedAt)
	return r, mapRowErr(err)
}

// CreateReminder stores a parsed schedule for participantID. next_fire_at is
// the schedule's first firing after now; a one-time schedule already in the
// past is rejected by the caller (Next returns false).
func (s *Store) CreateReminder(ctx context.Context, roomID, participantID, text string, sc schedule.Schedule, next time.Time) (Reminder, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM reminders WHERE participant_id = $1 AND next_fire_at IS NOT NULL`, participantID).Scan(&n); err != nil {
		return Reminder{}, err
	}
	if n >= MaxRemindersPerAgent {
		return Reminder{}, ErrTooManyReminders
	}
	return scanReminder(s.pool.QueryRow(ctx,
		`INSERT INTO reminders (room_id, participant_id, text, schedule, kind, tz, next_fire_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+reminderCols,
		roomID, participantID, text, sc.Text, sc.Kind, sc.Location.String(), next))
}

// UpdateReminder replaces the text and/or the schedule of one reminder. A nil
// field keeps the stored value. It returns ErrNotFound when the reminder is
// not participantID's.
func (s *Store) UpdateReminder(ctx context.Context, participantID, id string, text *string, sc *schedule.Schedule, next *time.Time) (Reminder, error) {
	if sc == nil {
		return scanReminder(s.pool.QueryRow(ctx,
			`UPDATE reminders SET text = COALESCE($3, text), updated_at = now()
			 WHERE id = $1 AND participant_id = $2 RETURNING `+reminderCols, id, participantID, text))
	}
	return scanReminder(s.pool.QueryRow(ctx,
		`UPDATE reminders SET text = COALESCE($3, text), schedule = $4, kind = $5, tz = $6, next_fire_at = $7, updated_at = now()
		 WHERE id = $1 AND participant_id = $2 RETURNING `+reminderCols,
		id, participantID, text, sc.Text, sc.Kind, sc.Location.String(), next))
}

// DeleteReminder removes one of participantID's reminders.
func (s *Store) DeleteReminder(ctx context.Context, participantID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM reminders WHERE id = $1 AND participant_id = $2`, id, participantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReminderByID fetches one reminder in a room.
func (s *Store) ReminderByID(ctx context.Context, roomID, id string) (Reminder, error) {
	return scanReminder(s.pool.QueryRow(ctx,
		`SELECT `+reminderCols+` FROM reminders WHERE room_id = $1 AND id = $2`, roomID, id))
}

// ListReminders returns participantID's reminders, oldest first.
func (s *Store) ListReminders(ctx context.Context, participantID string) ([]Reminder, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+reminderCols+` FROM reminders WHERE participant_id = $1 ORDER BY created_at, id`, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FireDueReminders fires every reminder whose next_fire_at is at or before
// now, one transaction each, and returns how many fired. Each tx takes the
// room event lock first, then re-checks the row under FOR UPDATE, so two
// ticks (or a tick racing a boot) cannot fire the same moment twice. A
// recurring reminder moves to its next firing after now; a one-time one
// completes (next_fire_at NULL). Fires missed while the server was down
// collapse into one.
func (s *Store) FireDueReminders(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.room_id FROM reminders r JOIN participants p ON p.id = r.participant_id
		 WHERE NOT p.revoked AND r.next_fire_at IS NOT NULL AND r.next_fire_at <= $1
		 ORDER BY r.next_fire_at, r.id LIMIT 500`, now)
	if err != nil {
		return 0, err
	}
	type due struct{ id, room string }
	var list []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.room); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// one bad row must not hold the whole batch: keep going, report the first error
	fired := 0
	var firstErr error
	for _, d := range list {
		ok, err := s.fireReminder(ctx, d.room, d.id, now)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("reminder %s: %w", d.id, err)
		}
		if ok {
			fired++
		}
	}
	return fired, firstErr
}

func (s *Store) fireReminder(ctx context.Context, roomID, id string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return false, err
	}
	r, err := scanReminder(tx.QueryRow(ctx,
		`SELECT `+reminderCols+` FROM reminders WHERE id = $1 AND next_fire_at IS NOT NULL AND next_fire_at <= $2 FOR UPDATE`, id, now))
	if errors.Is(err, ErrNotFound) {
		return false, nil // someone else fired it, or it was deleted
	}
	if err != nil {
		return false, err
	}
	var next *time.Time
	if r.Recurring() {
		loc, lerr := time.LoadLocation(r.TZ)
		if lerr != nil {
			loc = time.UTC
		}
		sc, perr := schedule.Parse(r.Schedule, loc, now)
		if perr != nil {
			return false, perr
		}
		next = nextAfter(sc, *r.NextFireAt, now)
	}
	var name string
	var ownerID *string
	var online bool
	err = tx.QueryRow(ctx,
		`SELECT name, owner_id, presence_online FROM participants WHERE id = $1`, r.ParticipantID).Scan(&name, &ownerID, &online)
	if err != nil {
		return false, mapRowErr(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"reminder_id":      r.ID,
		"participant_id":   r.ParticipantID,
		"participant_name": name,
		"owner_id":         ownerID,
		"text":             r.Text,
		"schedule":         r.Schedule,
		"kind":             r.Kind,
		"fired_at":         now.UTC(),
		"next_fire_at":     next,
		"fire_count":       r.FireCount + 1,
	})
	seq, err := appendEventSeqTx(ctx, tx, roomID, "reminder.fired", payload)
	if err != nil {
		return false, err
	}
	state := "deferred"
	if online {
		state = "accepted"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO deliveries (room_id, event_seq, recipient_id, state) VALUES ($1, $2, $3, $4)`,
		roomID, seq, r.ParticipantID, state); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE reminders SET next_fire_at = $2, last_fired_at = $3, fire_count = fire_count + 1, updated_at = now() WHERE id = $1`,
		id, next, now); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// nextAfter advances from the due time, not the tick, so "every 2h" keeps its
// grid instead of drifting by the tick delay; fires missed while the server
// was down collapse into one. A wild backlog falls back to counting from now.
func nextAfter(sc schedule.Schedule, due, now time.Time) *time.Time {
	n, ok := sc.Next(due)
	for i := 0; ok && !n.After(now); i++ {
		if i > 10000 {
			n, ok = sc.Next(now)
			break
		}
		n, ok = sc.Next(n)
	}
	if !ok {
		return nil
	}
	return &n
}
