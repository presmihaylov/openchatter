package models

import (
	"context"
	"encoding/json"
)

// SetAck records that participantID has taken message on, and emits one
// message.ack event carrying the message's full ack list. It is idempotent: a
// second ack from the same participant still succeeds and still emits, so a
// retry after a dropped response never errors.
func (s *Store) SetAck(ctx context.Context, roomID, messageID, participantID string) (AckEvent, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AckEvent{}, err
	}
	defer tx.Rollback(ctx)

	// advisory-first, like every event-writing tx (see CreateMessage)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return AckEvent{}, err
	}
	ev := AckEvent{MessageID: messageID, ParticipantID: participantID}
	err = tx.QueryRow(ctx,
		`SELECT m.channel_id, m.thread_root_id, m.author_id, a.name
		   FROM messages m JOIN participants a ON a.id = m.author_id
		  WHERE m.id = $1 AND m.room_id = $2`,
		messageID, roomID).Scan(&ev.ChannelID, &ev.ThreadRootID, &ev.AuthorID, &ev.AuthorName)
	if err != nil {
		return AckEvent{}, mapRowErr(err)
	}
	if err := tx.QueryRow(ctx, `SELECT name FROM participants WHERE id = $1`, participantID).
		Scan(&ev.ParticipantName); err != nil {
		return AckEvent{}, mapRowErr(err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO message_acks (message_id, participant_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, messageID, participantID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return AckEvent{}, ErrNotFound
		}
		return AckEvent{}, err
	}
	fresh := tag.RowsAffected() > 0

	rows, err := tx.Query(ctx,
		`SELECT ak.participant_id, ap.name, ak.created_at
		   FROM message_acks ak JOIN participants ap ON ap.id = ak.participant_id
		  WHERE ak.message_id = $1 ORDER BY ak.created_at`, messageID)
	if err != nil {
		return AckEvent{}, err
	}
	ev.AckedBy = []Ack{}
	for rows.Next() {
		var a Ack
		if err := rows.Scan(&a.ParticipantID, &a.Name, &a.AckedAt); err != nil {
			rows.Close()
			return AckEvent{}, err
		}
		ev.AckedBy = append(ev.AckedBy, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return AckEvent{}, err
	}

	// a repeat ack still answers with the list, but writes no second event: the
	// author would be woken again for a receipt that has not changed
	if fresh {
		payload, err := json.Marshal(ev)
		if err != nil {
			return AckEvent{}, err
		}
		if err := appendEventTx(ctx, tx, roomID, "message.ack", payload); err != nil {
			return AckEvent{}, err
		}
	}
	return ev, tx.Commit(ctx)
}

// PendingAcks lists the asks addressed to participantID that it has not acked,
// oldest first. An ask is a message that mentions the participant, or a reply
// under a thread root the participant wrote. Its own messages never count, and
// neither do system rows: an agent must not be nagged about its own timeline.
func (s *Store) PendingAcks(ctx context.Context, roomID, participantID string, limit int) ([]PendingAck, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.channel_id, c.name, m.author_id, a.name, m.created_at,
		        left(m.body, 200),
		        CASE WHEN mn.participant_id IS NOT NULL THEN 'mention' ELSE 'thread' END
		   FROM messages m
		   JOIN participants a ON a.id = m.author_id
		   JOIN channels c ON c.id = m.channel_id
		   LEFT JOIN mentions mn ON mn.message_id = m.id AND mn.participant_id = $2
		   LEFT JOIN messages root ON root.id = m.thread_root_id
		  WHERE m.room_id = $1
		    AND m.kind = 'message'
		    AND m.author_id <> $2
		    AND (mn.participant_id IS NOT NULL OR root.author_id = $2)
		    -- membership, not privacy, is the read gate everywhere else in the
		    -- room, and a mention is written even for a non-member. Without this
		    -- the pending list hands out excerpts you cannot read.
		    AND EXISTS (SELECT 1 FROM channel_members cm
		                 WHERE cm.channel_id = m.channel_id AND cm.participant_id = $2)
		    AND NOT EXISTS (SELECT 1 FROM message_acks ak
		                     WHERE ak.message_id = m.id AND ak.participant_id = $2)
		  ORDER BY m.created_at
		  LIMIT $3`, roomID, participantID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingAck{}
	for rows.Next() {
		var p PendingAck
		if err := rows.Scan(&p.MessageID, &p.ChannelID, &p.ChannelName, &p.AuthorID,
			&p.AuthorName, &p.CreatedAt, &p.Excerpt, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
