package models

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type CreateMessageParams struct {
	RoomID        string
	ChannelID     string
	ThreadRootID  *string
	AuthorID      string
	Body          string
	IsBroadcast   bool
	AttachmentIDs []string
	MentionIDs    []string
}

// messageColumns/messageFrom hydrate messages with author name, attachments,
// mentions and reply counts. Split so searches can append a score column.
const messageColumns = `
	       m.id, m.room_id, m.channel_id, m.thread_root_id, m.author_id, a.name,
	       m.body, m.is_broadcast, m.kind, m.created_at, m.edited_at,
	       (SELECT count(*) FROM messages r WHERE r.thread_root_id = m.id) AS reply_count,
	       (SELECT max(r.created_at) FROM messages r WHERE r.thread_root_id = m.id) AS last_reply_at,
	       COALESCE(
	           (SELECT json_agg(x.author_id ORDER BY x.last_at DESC)
	            FROM (SELECT r.author_id, max(r.created_at) AS last_at
	                  FROM messages r WHERE r.thread_root_id = m.id
	                  GROUP BY r.author_id ORDER BY last_at DESC LIMIT 3) x),
	           '[]'::json) AS replier_ids,
	       COALESCE(
	           (SELECT json_agg(json_build_object(
	                'id', att.id, 'filename', att.filename, 'content_type', att.content_type,
	                'size_bytes', att.size_bytes, 'created_at', att.created_at) ORDER BY att.created_at)
	            FROM message_attachments ma JOIN attachments att ON att.id = ma.attachment_id
	            WHERE ma.message_id = m.id),
	           '[]'::json) AS attachments,
	       COALESCE(
	           (SELECT json_agg(mp.name ORDER BY mp.name)
	            FROM mentions mn JOIN participants mp ON mp.id = mn.participant_id
	            WHERE mn.message_id = m.id),
	           '[]'::json) AS mentions,
	       COALESCE(
	           (SELECT json_agg(json_build_object(
	                'emoji', g.emoji, 'count', g.n,
	                'participant_ids', g.ids, 'names', g.names) ORDER BY g.first_at)
	            FROM (SELECT mr.emoji, count(*) AS n, min(mr.created_at) AS first_at,
	                         json_agg(mr.participant_id ORDER BY mr.created_at) AS ids,
	                         json_agg(rp.name ORDER BY mr.created_at) AS names
	                    FROM message_reactions mr JOIN participants rp ON rp.id = mr.participant_id
	                   WHERE mr.message_id = m.id GROUP BY mr.emoji) g),
	           '[]'::json) AS reactions,
	       COALESCE(
	           (SELECT json_agg(json_build_object(
	                'participant_id', ak.participant_id, 'name', ap.name,
	                'acked_at', ak.created_at) ORDER BY ak.created_at)
	            FROM message_acks ak JOIN participants ap ON ap.id = ak.participant_id
	            WHERE ak.message_id = m.id),
	           '[]'::json) AS acked_by`

const messageFrom = `
	FROM messages m
	JOIN participants a ON a.id = m.author_id`

const messageSelect = "SELECT" + messageColumns + messageFrom

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	var attJSON, menJSON, repJSON, rxnJSON, ackJSON []byte
	err := row.Scan(&m.ID, &m.RoomID, &m.ChannelID, &m.ThreadRootID, &m.AuthorID, &m.AuthorName,
		&m.Body, &m.IsBroadcast, &m.Kind, &m.CreatedAt, &m.EditedAt, &m.ReplyCount, &m.LastReplyAt,
		&repJSON, &attJSON, &menJSON, &rxnJSON, &ackJSON)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(repJSON, &m.ReplierIDs); err != nil {
		return m, err
	}
	if err := json.Unmarshal(attJSON, &m.Attachments); err != nil {
		return m, err
	}
	if err := json.Unmarshal(menJSON, &m.Mentions); err != nil {
		return m, err
	}
	m.ReplyToID = m.ReplyTo()
	if err := json.Unmarshal(rxnJSON, &m.Reactions); err != nil {
		return m, err
	}
	if err := json.Unmarshal(ackJSON, &m.AckedBy); err != nil {
		return m, err
	}
	return m, nil
}

func (s *Store) CreateMessage(ctx context.Context, p CreateMessageParams) (Message, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)

	// advisory lock first: this tx takes FK row locks (FOR KEY SHARE on rooms via
	// the messages insert) before appendEventTx, so without locking advisory-first
	// it would deadlock AB-BA against a rooms-row UPDATE such as RenameRoom.
	if err := lockRoomEvents(ctx, tx, p.RoomID); err != nil {
		return Message{}, err
	}

	// archived check inside the tx so a concurrent archive can't race past
	// the handler's pre-check; FOR SHARE blocks the archiver until we commit
	var archived bool
	err = tx.QueryRow(ctx,
		`SELECT archived FROM channels WHERE id = $1 AND room_id = $2 FOR SHARE`,
		p.ChannelID, p.RoomID).Scan(&archived)
	if err != nil {
		return Message{}, mapRowErr(err)
	}
	if archived {
		return Message{}, ErrArchived
	}

	// system timeline entries take no replies
	if p.ThreadRootID != nil {
		var rootKind string
		err = tx.QueryRow(ctx,
			`SELECT kind FROM messages WHERE id = $1 AND room_id = $2`,
			*p.ThreadRootID, p.RoomID).Scan(&rootKind)
		if err != nil {
			return Message{}, mapRowErr(err)
		}
		if rootKind == "system" {
			return Message{}, ErrConflict
		}
	}

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO messages (room_id, channel_id, thread_root_id, author_id, body, is_broadcast)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		p.RoomID, p.ChannelID, p.ThreadRootID, p.AuthorID, p.Body, p.IsBroadcast,
	).Scan(&id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return Message{}, ErrNotFound
		}
		return Message{}, err
	}

	for _, attID := range p.AttachmentIDs {
		// join through attachments to guarantee same-room ownership
		res, err := tx.Exec(ctx,
			`INSERT INTO message_attachments (message_id, attachment_id)
			 SELECT $1, id FROM attachments WHERE id = $2 AND room_id = $3`,
			id, attID, p.RoomID)
		if err != nil {
			return Message{}, err
		}
		if res.RowsAffected() == 0 {
			return Message{}, ErrNotFound
		}
	}
	for _, pid := range p.MentionIDs {
		res, err := tx.Exec(ctx,
			`INSERT INTO mentions (message_id, participant_id)
			 SELECT $1, id FROM participants WHERE id = $2 AND room_id = $3
			 ON CONFLICT DO NOTHING`,
			id, pid, p.RoomID)
		if err != nil {
			return Message{}, err
		}
		if res.RowsAffected() == 0 {
			return Message{}, ErrNotFound
		}
		// a direct @mention breaks a thread mute, un-resolves it and pulls a
		// participant who left back in, so the tagged person hears the thread again
		if p.ThreadRootID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE thread_states SET muted = false, resolved_at = NULL, left_at = NULL
				 WHERE root_id = $1 AND participant_id = $2
				   AND (muted OR resolved_at IS NOT NULL OR left_at IS NOT NULL)`,
				*p.ThreadRootID, pid); err != nil {
				return Message{}, err
			}
		}
	}

	// Writing in a thread again is rejoining it
	if p.ThreadRootID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE thread_states SET left_at = NULL
			 WHERE root_id = $1 AND participant_id = $2 AND left_at IS NOT NULL`,
			*p.ThreadRootID, p.AuthorID); err != nil {
			return Message{}, err
		}
	}

	// A reply revives the thread for everyone who archived it by hand: the
	// sidebar pin comes back, and the inactivity clock restarts from this message.
	if p.ThreadRootID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE thread_states SET resolved_at = NULL
			 WHERE root_id = $1 AND resolved_at IS NOT NULL`,
			*p.ThreadRootID); err != nil {
			return Message{}, err
		}
	}

	msg, err := scanMessage(tx.QueryRow(ctx, messageSelect+` WHERE m.id = $1`, id))
	if err != nil {
		return Message{}, err
	}

	// thread_participants rides on the event only: a firehose watcher has no
	// other way to tell "a reply in a thread I wrote in" from a stranger's thread
	names, err := threadParticipantNamesTx(ctx, tx, p.RoomID, msg.ReplyTo())
	if err != nil {
		return Message{}, err
	}
	payload, err := json.Marshal(messageEvent{Message: msg, ThreadParticipants: names})
	if err != nil {
		return Message{}, err
	}
	seq, err := appendEventSeqTx(ctx, tx, p.RoomID, "message.created", payload)
	if err != nil {
		return Message{}, err
	}
	if err := createDeliveriesTx(ctx, tx, p, seq); err != nil {
		return Message{}, err
	}
	return msg, tx.Commit(ctx)
}

func (s *Store) MessageByID(ctx context.Context, roomID, id string) (Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx,
		messageSelect+` WHERE m.id = $1 AND m.room_id = $2`, id, roomID))
	return m, mapRowErr(err)
}

// ListChannelMessages returns top-level messages, oldest first, at most limit.
// before filters strictly older; beforeID (paired with beforeAt) is a tuple
// cursor so pages don't skip or repeat messages sharing a timestamp.
func (s *Store) ListChannelMessages(ctx context.Context, roomID, channelID string, before *time.Time, beforeID *string, beforeAt *time.Time, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	// clamp, don't reset: a too-big limit collapsing to 50 makes paginating
	// clients think they hit the end of history
	if limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		messageSelect+`
		 WHERE m.room_id = $1 AND m.channel_id = $2 AND m.thread_root_id IS NULL
		   AND ($3::timestamptz IS NULL OR m.created_at < $3)
		   AND ($4::uuid IS NULL OR (m.created_at, m.id) < ($5::timestamptz, $4::uuid))
		 ORDER BY m.created_at DESC, m.id DESC LIMIT $6`,
		roomID, channelID, before, beforeID, beforeAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, err := collectMessages(rows)
	if err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}

// ListThread returns the root message followed by the newest limit replies,
// oldest first. Bounded so a huge autonomous-agent thread can't make one GET
// serialize the whole conversation.
func (s *Store) ListThread(ctx context.Context, roomID, rootID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx,
		`SELECT * FROM (
		    SELECT`+messageColumns+messageFrom+`
		     WHERE m.room_id = $1 AND (m.id = $2 OR m.thread_root_id = $2)
		     ORDER BY (m.id = $2) DESC, m.created_at DESC, m.id DESC LIMIT $3
		 ) latest ORDER BY created_at ASC, id ASC`,
		roomID, rootID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out, err := collectMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func collectMessages(rows pgx.Rows) ([]Message, error) {
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateMessageBody edits a message in place; the edit also re-queues embedding.
// assertChannelWritable resolves a message's channel and fails if it is
// archived. FOR SHARE OF the channel row blocks a concurrent archive until the
// caller's tx commits, closing the same race CreateMessage guards. Returns
// ErrNotFound when the message does not exist, ErrArchived when it is read-only.
func assertChannelWritable(ctx context.Context, tx pgx.Tx, roomID, messageID string) error {
	var archived bool
	err := tx.QueryRow(ctx,
		`SELECT c.archived FROM messages m JOIN channels c ON c.id = m.channel_id
		 WHERE m.id = $1 AND m.room_id = $2 FOR SHARE OF c`,
		messageID, roomID).Scan(&archived)
	if err != nil {
		return mapRowErr(err)
	}
	if archived {
		return ErrArchived
	}
	return nil
}

func (s *Store) UpdateMessageBody(ctx context.Context, roomID, id, body string, isBroadcast bool) (Message, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)

	// advisory-first, like CreateMessage: the UPDATE takes an FK key-share lock on
	// rooms before appendEventTx, else it deadlocks AB-BA against RenameRoom.
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return Message{}, err
	}
	// archived channels are read-only: block edits too, not just new posts. FOR
	// SHARE OF the channel blocks a concurrent archive until we commit.
	if err := assertChannelWritable(ctx, tx, roomID, id); err != nil {
		return Message{}, err
	}

	res, err := tx.Exec(ctx,
		`UPDATE messages SET body = $3, is_broadcast = $4, edited_at = now(), embed_status = 'pending', embed_attempts = 0
		 WHERE room_id = $1 AND id = $2`,
		roomID, id, body, isBroadcast)
	if err != nil {
		return Message{}, err
	}
	if res.RowsAffected() == 0 {
		return Message{}, ErrNotFound
	}

	msg, err := scanMessage(tx.QueryRow(ctx, messageSelect+` WHERE m.id = $1`, id))
	if err != nil {
		return Message{}, err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return Message{}, err
	}
	if err := appendEventTx(ctx, tx, roomID, "message.edited", payload); err != nil {
		return Message{}, err
	}
	return msg, tx.Commit(ctx)
}

// DeleteMessage removes a message; deleting a thread root deletes its replies (FK cascade).
func (s *Store) DeleteMessage(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// advisory-first, like CreateMessage, to avoid the AB-BA deadlock vs RenameRoom.
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	// archived channels are read-only: block deletes too.
	if err := assertChannelWritable(ctx, tx, roomID, id); err != nil {
		return err
	}

	// the root id rides along so a client can drop the deleted reply out of its
	// cached root footer; without it the "N replies" line stays high until a reload
	var rootID *string
	err = tx.QueryRow(ctx,
		`DELETE FROM messages WHERE room_id = $1 AND id = $2 RETURNING thread_root_id`,
		roomID, id).Scan(&rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	fields := map[string]string{"message_id": id}
	if rootID != nil {
		fields["thread_root_id"] = *rootID
	}
	payload, _ := json.Marshal(fields)
	if err := appendEventTx(ctx, tx, roomID, "message.deleted", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func reverse(ms []Message) {
	for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
		ms[i], ms[j] = ms[j], ms[i]
	}
}

// messageEvent is the message.created payload: the message plus the distinct
// author names in its thread (root author first, then repliers), this one included.
type messageEvent struct {
	Message
	ThreadParticipants []string `json:"thread_participants"`
}

func threadParticipantNamesTx(ctx context.Context, tx pgx.Tx, roomID, rootID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT p.name FROM messages m JOIN participants p ON p.id = m.author_id
		 WHERE m.room_id = $1 AND COALESCE(m.thread_root_id, m.id) = $2
		   AND m.kind <> 'system'
		   AND NOT EXISTS (SELECT 1 FROM thread_states ts
		                   WHERE ts.root_id = $2 AND ts.participant_id = p.id
		                     AND ts.left_at IS NOT NULL)
		 ORDER BY m.created_at, m.id`, roomID, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, rows.Err()
}
