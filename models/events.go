package models

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// lockRoomEvents serializes event-writing transactions per room. It is the
// FIRST lock any such tx takes; a tx that grabs strong row locks before
// calling appendEventTx must call this up front to keep one global lock order.
func lockRoomEvents(ctx context.Context, tx pgx.Tx, roomID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomID)
	return err
}

func appendEventTx(ctx context.Context, tx pgx.Tx, roomID, typ string, payload json.RawMessage) error {
	_, err := appendEventSeqTx(ctx, tx, roomID, typ, payload)
	return err
}

// appendEventSeqTx is appendEventTx returning the new seq, for callers that
// hang per-recipient rows (deliveries) off the event in the same transaction.
func appendEventSeqTx(ctx context.Context, tx pgx.Tx, roomID, typ string, payload json.RawMessage) (int64, error) {
	// Serialize event-writing transactions per room so seq order matches commit
	// order — otherwise a long-poller's cursor can jump past a seq that commits
	// late and that event is lost to every tailing client. (Reentrant, so txs
	// that already called lockRoomEvents are fine.)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return 0, err
	}
	var seq int64
	err := tx.QueryRow(ctx,
		`INSERT INTO events (room_id, type, payload) VALUES ($1, $2, $3) RETURNING seq`,
		roomID, typ, payload).Scan(&seq)
	return seq, err
}

// AppendEvent records a standalone event (mutations done in their own tx append inline).
func (s *Store) AppendEvent(ctx context.Context, roomID, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := appendEventTx(ctx, tx, roomID, typ, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListEvents returns events with seq > afterSeq, oldest first.
func (s *Store) ListEvents(ctx context.Context, roomID string, afterSeq int64, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT seq, room_id, type, payload, created_at
		 FROM events WHERE room_id = $1 AND seq > $2
		 ORDER BY seq ASC LIMIT $3`,
		roomID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.RoomID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ParticipatedThreadRoots reports which rootIDs the participant authored,
// replied in, or was mentioned in. Used by the events relevance filter.
func (s *Store) ParticipatedThreadRoots(ctx context.Context, roomID, participantID string, rootIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(rootIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT COALESCE(m.thread_root_id, m.id) FROM messages m
		 LEFT JOIN mentions mn ON mn.message_id = m.id AND mn.participant_id = $2
		 WHERE m.room_id = $1 AND (m.author_id = $2 OR mn.participant_id IS NOT NULL) AND m.kind <> 'system'
		   AND COALESCE(m.thread_root_id, m.id) = ANY($3::uuid[])
		   AND NOT EXISTS (SELECT 1 FROM thread_states ts
		                   WHERE ts.root_id = COALESCE(m.thread_root_id, m.id)
		                     AND ts.participant_id = $2 AND ts.left_at IS NOT NULL)`,
		roomID, participantID, rootIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// LatestSeq returns the highest event seq for a room (0 if none).
func (s *Store) LatestSeq(ctx context.Context, roomID string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE room_id = $1`, roomID,
	).Scan(&seq)
	return seq, err
}
