package models

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Delivery states. accepted -> delivered -> acked is the happy path; deferred
// is accepted-while-offline; failed is retries_exhausted or dead_letter.
const (
	DeliveryAccepted  = "accepted"
	DeliveryDeferred  = "deferred"
	DeliveryDelivered = "delivered"
	DeliveryAcked     = "acked"
	DeliveryFailed    = "failed"
)

// DeliveryBroadcastCap bounds how many agents one root broadcast fans out to.
const DeliveryBroadcastCap = 200 // mentioned agents sort first, so the cap only ever trims broadcast members

// DeliveryPruneAfter is how long acked and failed receipts are kept.
const DeliveryPruneAfter = 30 * 24 * time.Hour

// DeliveryLease is how long an inbox drain owns the rows it handed out: a
// second drain inside the window (concurrent, or a quick retry) gets nothing
// twice; a drain after it, say a restarted watcher, replays what was not acked.
const DeliveryLease = 60 * time.Second

// DeliveryStats is what an owner or admin sees on an agent's profile.
type DeliveryStats struct {
	Accepted  int `json:"accepted"`
	Deferred  int `json:"deferred"`
	Delivered int `json:"delivered"`
	Acked     int `json:"acked"`
	Failed    int `json:"failed"`
	// Pending is every receipt not yet acked or failed (accepted + deferred + delivered).
	Pending         int        `json:"pending"`
	OldestUnackedAt *time.Time `json:"oldest_unacked_at"`
}

// Delivery is one receipt, as listed by Inbox.
type Delivery struct {
	EventSeq    int64      `json:"event_seq"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
}

// createDeliveriesTx hangs one receipt per addressed agent off a fresh
// message.created event: every mentioned agent; on a HUMAN reply, every agent
// still participating in the thread (authored, replied, or was mentioned and
// did not leave); and for a root broadcast every agent member of the channel,
// capped. An untagged agent reply creates no thread receipts. The author never
// gets one. Online agents start accepted, offline ones deferred.
func createDeliveriesTx(ctx context.Context, tx pgx.Tx, p CreateMessageParams, seq int64) error {
	broadcastRoot := p.IsBroadcast && p.ThreadRootID == nil
	mentionIDs := p.MentionIDs
	if mentionIDs == nil {
		mentionIDs = []string{}
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO deliveries (room_id, event_seq, recipient_id, state)
		 SELECT $1, $2, r.id, CASE WHEN r.presence_online THEN 'accepted' ELSE 'deferred' END
		 FROM (
		   SELECT pa.id, pa.presence_online FROM participants pa
		   WHERE pa.room_id = $1 AND NOT pa.is_human AND NOT pa.revoked AND pa.id <> $3
		     AND (
		       pa.id = ANY($4::uuid[])
		       OR ($5::uuid IS NOT NULL
		           AND EXISTS (SELECT 1 FROM participants author WHERE author.id = $3 AND author.is_human)
		           AND EXISTS (
		             SELECT 1 FROM messages m
		             LEFT JOIN mentions mn ON mn.message_id = m.id AND mn.participant_id = pa.id
		             WHERE m.room_id = $1 AND COALESCE(m.thread_root_id, m.id) = $5
		               AND (m.author_id = pa.id OR mn.participant_id IS NOT NULL) AND m.kind <> 'system'
		               AND NOT EXISTS (SELECT 1 FROM thread_states ts
		                               WHERE ts.root_id = $5 AND ts.participant_id = pa.id AND ts.left_at IS NOT NULL)))
		       OR ($6 AND EXISTS (SELECT 1 FROM channel_members cm WHERE cm.channel_id = $7 AND cm.participant_id = pa.id))
		     )
		   ORDER BY (pa.id = ANY($4::uuid[])) DESC, pa.created_at, pa.id
		   LIMIT $8
		 ) r
		 ON CONFLICT DO NOTHING`,
		p.RoomID, seq, p.AuthorID, mentionIDs, p.ThreadRootID, broadcastRoot, p.ChannelID, DeliveryBroadcastCap)
	return err
}

// markDeliveredSQL: every hand-out is an attempt; past the room's max the
// receipt fails as retries_exhausted (the event is still returned, only the
// receipt gives up). Idempotent on state and delivered_at.
const markDeliveredSQL = `
	UPDATE deliveries d SET
	  attempts = d.attempts + 1,
	  delivered_at = COALESCE(d.delivered_at, now()),
	  state = CASE WHEN d.attempts + 1 > r.delivery_max_attempts THEN 'failed' ELSE 'delivered' END,
	  failed_at = CASE WHEN d.attempts + 1 > r.delivery_max_attempts THEN now() ELSE d.failed_at END,
	  failed_reason = CASE WHEN d.attempts + 1 > r.delivery_max_attempts THEN 'retries_exhausted' ELSE d.failed_reason END
	FROM rooms r
	WHERE r.id = d.room_id AND d.room_id = $1 AND d.recipient_id = $2 AND d.event_seq = ANY($3::bigint[])
	  AND d.state IN ('accepted', 'deferred', 'delivered')`

// MarkDelivered records that a poll handed these event seqs to the recipient.
func (s *Store) MarkDelivered(ctx context.Context, roomID, recipientID string, seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, markDeliveredSQL, roomID, recipientID, seqs)
	return err
}

// Inbox returns every event with an unacked receipt for the recipient, in
// seq order, and (unless peek) marks each one delivered and leases it for
// DeliveryLease. Two drains never return the same event twice inside the
// lease: rows are locked SKIP LOCKED while the drain runs, and a leased row
// is skipped after it. A drain past the lease replays anything still
// unacked, until the agent acks it or the attempts run out. peek lists
// everything unacked, leased or not, and marks nothing.
func (s *Store) Inbox(ctx context.Context, roomID, recipientID string, peek bool, limit int) ([]Event, []Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	lock := " AND (leased_until IS NULL OR leased_until < now()) ORDER BY event_seq LIMIT $3 FOR UPDATE SKIP LOCKED"
	if peek {
		lock = " ORDER BY event_seq LIMIT $3"
	}
	rows, err := tx.Query(ctx,
		`SELECT event_seq, state, attempts, created_at, delivered_at FROM deliveries
		 WHERE room_id = $1 AND recipient_id = $2 AND state IN ('accepted', 'deferred', 'delivered')`+lock,
		roomID, recipientID, limit)
	if err != nil {
		return nil, nil, err
	}
	receipts := []Delivery{}
	seqs := []int64{}
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.EventSeq, &d.State, &d.Attempts, &d.CreatedAt, &d.DeliveredAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		receipts = append(receipts, d)
		seqs = append(seqs, d.EventSeq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	events := []Event{}
	if len(seqs) == 0 {
		return events, receipts, tx.Commit(ctx)
	}
	erows, err := tx.Query(ctx,
		`SELECT seq, room_id, type, payload, created_at FROM events
		 WHERE room_id = $1 AND seq = ANY($2::bigint[]) ORDER BY seq`,
		roomID, seqs)
	if err != nil {
		return nil, nil, err
	}
	for erows.Next() {
		var e Event
		if err := erows.Scan(&e.Seq, &e.RoomID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
			erows.Close()
			return nil, nil, err
		}
		events = append(events, e)
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return nil, nil, err
	}
	if !peek {
		if _, err := tx.Exec(ctx, markDeliveredSQL, roomID, recipientID, seqs); err != nil {
			return nil, nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE deliveries SET leased_until = now() + $4::interval
			 WHERE room_id = $1 AND recipient_id = $2 AND event_seq = ANY($3::bigint[])`,
			roomID, recipientID, seqs, DeliveryLease.String()); err != nil {
			return nil, nil, err
		}
	}
	return events, receipts, tx.Commit(ctx)
}

// AckDelivery moves the recipient's receipt for the event to acked. A repeat
// ack is a no-op; a seq with no receipt for this recipient is ErrNotFound.
func (s *Store) AckDelivery(ctx context.Context, roomID, recipientID string, seq int64) error {
	var state string
	err := s.pool.QueryRow(ctx,
		`UPDATE deliveries SET state = 'acked', acked_at = COALESCE(acked_at, now())
		 WHERE room_id = $1 AND recipient_id = $2 AND event_seq = $3
		 RETURNING state`,
		roomID, recipientID, seq).Scan(&state)
	return mapRowErr(err)
}

// DeliveryStatsFor summarises one agent's receipts.
func (s *Store) DeliveryStatsFor(ctx context.Context, roomID, recipientID string) (DeliveryStats, error) {
	var st DeliveryStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE state = 'accepted'),
		   count(*) FILTER (WHERE state = 'deferred'),
		   count(*) FILTER (WHERE state = 'delivered'),
		   count(*) FILTER (WHERE state = 'acked'),
		   count(*) FILTER (WHERE state = 'failed'),
		   count(*) FILTER (WHERE state IN ('accepted', 'deferred', 'delivered')),
		   min(created_at) FILTER (WHERE state IN ('accepted', 'deferred', 'delivered'))
		 FROM deliveries WHERE room_id = $1 AND recipient_id = $2`,
		roomID, recipientID).Scan(&st.Accepted, &st.Deferred, &st.Delivered, &st.Acked, &st.Failed, &st.Pending, &st.OldestUnackedAt)
	return st, err
}

// SetDeliveryPolicy sets the room's dead-letter age and retry cap.
func (s *Store) SetDeliveryPolicy(ctx context.Context, roomID string, deadLetterDays, maxAttempts int) (Room, error) {
	var r Room
	err := scanRoom(s.pool.QueryRow(ctx,
		`UPDATE rooms SET delivery_dead_letter_days = $2, delivery_max_attempts = $3
		 WHERE id = $1 RETURNING `+roomColumns,
		roomID, deadLetterDays, maxAttempts), &r)
	return r, mapRowErr(err)
}

// SweepDeliveries dead-letters receipts older than the room's limit that
// nobody acked, and prunes acked and failed receipts past DeliveryPruneAfter.
// Meant to run on a ticker next to SweepPresence. Returns how many it
// dead-lettered.
func (s *Store) SweepDeliveries(ctx context.Context) (int, error) {
	res, err := s.pool.Exec(ctx,
		`UPDATE deliveries d SET state = 'failed', failed_at = now(), failed_reason = 'dead_letter'
		 FROM rooms r
		 WHERE r.id = d.room_id AND d.state IN ('accepted', 'deferred', 'delivered')
		   AND d.created_at < now() - make_interval(days => r.delivery_dead_letter_days)`)
	if err != nil {
		return 0, err
	}
	_, err = s.pool.Exec(ctx,
		`DELETE FROM deliveries
		 WHERE (state = 'acked' AND acked_at < now() - $1::interval)
		    OR (state = 'failed' AND failed_at < now() - $1::interval)`,
		DeliveryPruneAfter.String())
	return int(res.RowsAffected()), err
}

// BackdateDeliveries ages a recipient's receipts, leases included (tests:
// lease expiry, dead-letter and prune).
func (s *Store) BackdateDeliveries(ctx context.Context, recipientID string, by time.Duration) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE deliveries SET created_at = created_at - $2::interval,
		   acked_at = acked_at - $2::interval, failed_at = failed_at - $2::interval,
		   leased_until = leased_until - $2::interval
		 WHERE recipient_id = $1`,
		recipientID, by.String())
	return err
}
