package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebhookDisabledEvent tells an agent, its owner and the admins that the
// agent's push wake was switched off after too many failed deliveries.
const WebhookDisabledEvent = "participant.webhook_disabled"

// liveHookSQL is true for a participant row p with a healthy push webhook: enabled
// and its last delivery succeeded (a success resets the failure streak), so the
// room can wake it without a heartbeat and presence counts it as online.
const liveHookSQL = `EXISTS (SELECT 1 FROM participant_webhooks lw
  WHERE lw.participant_id = p.id AND lw.disabled_at IS NULL AND lw.consecutive_failures = 0)`

// WebhookMaxFailures is how many failed attempts in a row disable a hook.
const WebhookMaxFailures = 10

// Webhook is an agent's push-wake endpoint. The secret never leaves the
// server through the API: SecretSet says one is stored.
type Webhook struct {
	URL                 string     `json:"url"`
	SecretSet           bool       `json:"secret_set"`
	AuthorizationSet    bool       `json:"authorization_set"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at"`
	LastError           *string    `json:"last_error"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// WebhookJob is one due push wake, with what the worker needs to send it.
type WebhookJob struct {
	ID            string
	RoomID        string
	RoomSlug      string
	EventSeq      int64
	ParticipantID string
	Reason        string
	Attempts      int
	URL           string
	Secret        string
	Authorization string
	Message       Message
}

// SetWebhook stores or replaces the agent's hook and re-enables it. An empty
// authorization stores none: a PUT replaces the whole hook.
func (s *Store) SetWebhook(ctx context.Context, participantID, url, secret, authorization string) (Webhook, error) {
	var auth *string
	if authorization != "" {
		auth = &authorization
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO participant_webhooks (participant_id, url, secret, authorization_value)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (participant_id) DO UPDATE SET
		   url = EXCLUDED.url, secret = EXCLUDED.secret, authorization_value = EXCLUDED.authorization_value,
		   consecutive_failures = 0, disabled_at = NULL, last_error = NULL, updated_at = now()
		 RETURNING url, authorization_value IS NOT NULL, consecutive_failures, disabled_at, last_error, updated_at`,
		participantID, url, secret, auth)
	w := Webhook{SecretSet: true}
	err := row.Scan(&w.URL, &w.AuthorizationSet, &w.ConsecutiveFailures, &w.DisabledAt, &w.LastError, &w.UpdatedAt)
	return w, err
}

// WebhookFor returns the agent's hook, or ErrNotFound.
func (s *Store) WebhookFor(ctx context.Context, participantID string) (Webhook, error) {
	w := Webhook{SecretSet: true}
	err := s.pool.QueryRow(ctx,
		`SELECT url, authorization_value IS NOT NULL, consecutive_failures, disabled_at, last_error, updated_at
		 FROM participant_webhooks WHERE participant_id = $1`, participantID,
	).Scan(&w.URL, &w.AuthorizationSet, &w.ConsecutiveFailures, &w.DisabledAt, &w.LastError, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, ErrNotFound
	}
	return w, err
}

// DeleteWebhook removes the agent's hook and drops its queued wakes.
func (s *Store) DeleteWebhook(ctx context.Context, participantID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM participant_webhooks WHERE participant_id = $1`, participantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM webhook_jobs WHERE participant_id = $1 AND state = 'pending'`, participantID)
	return err
}

// enqueueWebhooksTx queues one push wake per receipt the message just created,
// for every recipient with a live hook. The reason mirrors why the receipt
// exists: a mention wins over a broadcast, and anything else is a thread reply.
func enqueueWebhooksTx(ctx context.Context, tx pgx.Tx, p CreateMessageParams, seq int64) error {
	mentionIDs := p.MentionIDs
	if mentionIDs == nil {
		mentionIDs = []string{}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO webhook_jobs (room_id, event_seq, participant_id, reason)
		 SELECT d.room_id, d.event_seq, d.recipient_id,
		   CASE WHEN d.recipient_id = ANY($3::uuid[]) THEN 'mention'
		        WHEN $4 THEN 'broadcast'
		        ELSE 'thread_reply' END
		 FROM deliveries d
		 JOIN participant_webhooks w ON w.participant_id = d.recipient_id AND w.disabled_at IS NULL
		 WHERE d.room_id = $1 AND d.event_seq = $2
		 ON CONFLICT DO NOTHING`,
		p.RoomID, seq, mentionIDs, p.IsBroadcast && p.ThreadRootID == nil)
	return err
}

// ClaimWebhookJobs leases up to limit due jobs: their next attempt moves out
// by lease, so a second worker, or this one after a crash, retries them only
// once the lease has passed. A job whose hook is gone or disabled is dropped.
func (s *Store) ClaimWebhookJobs(ctx context.Context, limit int, lease time.Duration) ([]WebhookJob, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE webhook_jobs j SET state = 'failed', last_error = 'webhook removed or disabled'
		 WHERE j.state = 'pending' AND NOT EXISTS (
		   SELECT 1 FROM participant_webhooks w WHERE w.participant_id = j.participant_id AND w.disabled_at IS NULL)`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`WITH due AS (
		   SELECT id FROM webhook_jobs
		   WHERE state = 'pending' AND next_attempt_at <= now()
		   ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		 UPDATE webhook_jobs j SET next_attempt_at = now() + $2::interval
		 FROM due, participant_webhooks w, rooms r, events e
		 WHERE j.id = due.id AND w.participant_id = j.participant_id AND r.id = j.room_id
		   AND e.room_id = j.room_id AND e.seq = j.event_seq
		 RETURNING j.id, j.room_id, r.slug, j.event_seq, j.participant_id, j.reason, j.attempts, w.url, w.secret, COALESCE(w.authorization_value, ''), e.payload`,
		limit, lease.String())
	if err != nil {
		return nil, err
	}
	var jobs []WebhookJob
	for rows.Next() {
		var j WebhookJob
		var payload json.RawMessage
		if err := rows.Scan(&j.ID, &j.RoomID, &j.RoomSlug, &j.EventSeq, &j.ParticipantID, &j.Reason, &j.Attempts, &j.URL, &j.Secret, &j.Authorization, &payload); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(payload, &j.Message)
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, tx.Commit(ctx)
}

// WebhookSent marks a job delivered and resets the hook's failure streak.
func (s *Store) WebhookSent(ctx context.Context, jobID, participantID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE webhook_jobs SET state = 'sent', attempts = attempts + 1, last_error = NULL WHERE id = $1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE participant_webhooks SET consecutive_failures = 0, last_error = NULL WHERE participant_id = $1`, participantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WebhookFailed records a failed attempt, and a nil retryAt gives the job up. At
// WebhookMaxFailures in a row it disables the hook, drops its pending jobs, emits one
// event and returns true.
func (s *Store) WebhookFailed(ctx context.Context, job WebhookJob, reason string, retryAt *time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// the room lock first, as every event-writing transaction must
	if err := lockRoomEvents(ctx, tx, job.RoomID); err != nil {
		return false, err
	}
	var failures int
	var disabled *time.Time
	err = tx.QueryRow(ctx,
		`UPDATE participant_webhooks SET consecutive_failures = consecutive_failures + 1, last_error = $2,
		   disabled_at = CASE WHEN disabled_at IS NULL AND consecutive_failures + 1 >= $3 THEN now() ELSE disabled_at END
		 WHERE participant_id = $1 RETURNING consecutive_failures, disabled_at`,
		job.ParticipantID, reason, WebhookMaxFailures).Scan(&failures, &disabled)
	gone := errors.Is(err, pgx.ErrNoRows) // the hook was deleted meanwhile
	if err != nil && !gone {
		return false, err
	}
	// a job retries only while its hook is live; a hook switched off by an
	// earlier failure in this burst takes this job down with it
	state, next := "pending", time.Now()
	if retryAt == nil || gone || disabled != nil {
		state = "failed"
	} else {
		next = *retryAt
	}
	if _, err := tx.Exec(ctx,
		`UPDATE webhook_jobs SET attempts = attempts + 1, last_error = $2, state = $3, next_attempt_at = $4 WHERE id = $1`,
		job.ID, reason, state, next); err != nil {
		return false, err
	}
	if gone {
		return false, tx.Commit(ctx)
	}
	justDisabled := disabled != nil && failures == WebhookMaxFailures
	if justDisabled {
		if _, err := tx.Exec(ctx,
			`UPDATE webhook_jobs SET state = 'failed', last_error = 'webhook disabled'
			 WHERE participant_id = $1 AND state = 'pending'`, job.ParticipantID); err != nil {
			return false, err
		}
		var name string
		var ownerID *string
		if err := tx.QueryRow(ctx, `SELECT name, owner_id FROM participants WHERE id = $1`, job.ParticipantID).Scan(&name, &ownerID); err != nil {
			return false, err
		}
		payload, _ := json.Marshal(map[string]any{
			"participant_id": job.ParticipantID, "participant_name": name, "owner_id": ownerID,
			"failures": failures, "last_error": reason, "disabled_at": disabled,
		})
		if err := appendEventTx(ctx, tx, job.RoomID, WebhookDisabledEvent, payload); err != nil {
			return false, err
		}
	}
	return justDisabled, tx.Commit(ctx)
}
