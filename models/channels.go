package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Store) CreateChannel(ctx context.Context, roomID, name, topic, createdBy string, private bool) (Channel, error) {
	var c Channel
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)

	// advisory-first: the insert takes FK row locks on rooms before appendEventTx
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return c, err
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO channels (room_id, name, topic, created_by, private) VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, room_id, name, topic, created_by, archived, private, created_at`,
		roomID, name, topic, createdBy, private,
	).Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy, &c.Archived, &c.Private, &c.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return c, ErrConflict
		}
		return c, err
	}

	// the creator is the channel's first member
	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_members (channel_id, participant_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, c.ID, createdBy); err != nil {
		return c, err
	}

	payload, _ := json.Marshal(map[string]string{"channel_id": c.ID, "name": c.Name, "created_by": createdBy})
	if err := appendEventTx(ctx, tx, roomID, "channel.created", payload); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}

// IsChannelMember reports whether a participant belongs to a channel.
func (s *Store) IsChannelMember(ctx context.Context, channelID, participantID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = $1 AND participant_id = $2)`,
		channelID, participantID).Scan(&ok)
	return ok, err
}

// ParticipantChannelIDs returns the channel ids a participant is a member of.
// The event filter uses this to gate content delivery by membership.
func (s *Store) ParticipantChannelIDs(ctx context.Context, participantID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT channel_id FROM channel_members WHERE participant_id = $1`, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// JoinChannel adds a participant to a channel and emits channel.member_joined.
// Idempotent: joining a channel you are already in changes nothing and reports
// changed=false. The event carries channel_id, so the membership gate delivers
// it only to that channel's members (the new member included). The actor is
// who performed the action (self-join: the participant themselves); it shapes
// the system timeline entry ("joined #x" vs "was added by Y").
func (s *Store) JoinChannel(ctx context.Context, roomID, channelID, participantID, name, actorID, actorName string) (bool, error) {
	return s.setMembership(ctx, roomID, channelID, participantID, name, actorID, actorName, true)
}

// LeaveChannel removes a participant from a channel and emits channel.member_left.
// Idempotent: leaving a channel you are not in reports changed=false.
func (s *Store) LeaveChannel(ctx context.Context, roomID, channelID, participantID, name, actorID, actorName string) (bool, error) {
	return s.setMembership(ctx, roomID, channelID, participantID, name, actorID, actorName, false)
}

func (s *Store) setMembership(ctx context.Context, roomID, channelID, participantID, name, actorID, actorName string, join bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// advisory-first: keep event seq order == commit order, like every writer
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return false, err
	}

	var tag pgconn.CommandTag
	if join {
		tag, err = tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, participant_id) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`, channelID, participantID)
	} else {
		tag, err = tx.Exec(ctx,
			`DELETE FROM channel_members WHERE channel_id = $1 AND participant_id = $2`,
			channelID, participantID)
	}
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx) // no-op, nothing to announce
	}

	typ := "channel.member_joined"
	if !join {
		typ = "channel.member_left"
	}
	payload, _ := json.Marshal(map[string]string{
		"channel_id": channelID, "participant_id": participantID, "name": name,
	})
	if err := appendEventTx(ctx, tx, roomID, typ, payload); err != nil {
		return false, err
	}

	// Slack-style timeline entry, persisted as a system message so history
	// shows it in place. Authored by the subject; the body carries the rest.
	body := "joined #" + name
	if join && actorID != participantID {
		body = "was added by " + actorName
	}
	if !join {
		body = "left #" + name
		if actorID != participantID {
			body = "was removed by " + actorName
		}
	}
	if err := systemEntryTx(ctx, tx, roomID, channelID, participantID, body); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) ListChannels(ctx context.Context, roomID string) ([]Channel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, room_id, name, topic, created_by, archived, private, created_at
		 FROM channels WHERE room_id = $1 ORDER BY created_at ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy, &c.Archived, &c.Private, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListChannelsUnread is ListChannels plus the viewer's read state. Unread
// counts only top-level messages from others; the baseline for someone who
// never marked a channel read is their own join time, not the room's birth.
func (s *Store) ListChannelsUnread(ctx context.Context, roomID, participantID string) ([]Channel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.room_id, c.name, c.topic, c.created_by, c.archived, c.private, c.created_at,
		        r.last_read_at, cm.muted,
		        (SELECT count(*) FROM messages m
		         WHERE m.channel_id = c.id AND m.thread_root_id IS NULL
		           AND m.author_id <> $2 AND m.kind <> 'system'
		           AND m.created_at > COALESCE(r.last_read_at, p.created_at)) AS unread,
		        (SELECT count(*) FROM messages m
		         WHERE m.channel_id = c.id AND m.thread_root_id IS NULL
		           AND m.author_id <> $2 AND m.kind <> 'system'
		           AND m.created_at > COALESCE(r.last_read_at, p.created_at)
		           AND (m.is_broadcast OR EXISTS (
		                SELECT 1 FROM mentions mn
		                WHERE mn.message_id = m.id AND mn.participant_id = $2))) AS unread_mentions,
		        (SELECT count(*) FROM messages m
		         WHERE m.channel_id = c.id AND m.thread_root_id IS NULL
		           AND m.author_id <> $2 AND m.kind <> 'system'
		           AND m.created_at > COALESCE(r.last_read_at, p.created_at)
		           AND EXISTS (SELECT 1 FROM mentions mn
		                       WHERE mn.message_id = m.id AND mn.participant_id = $2)) AS unread_direct_mentions
		 FROM channels c
		 JOIN participants p ON p.id = $2
		 JOIN channel_members cm ON cm.channel_id = c.id AND cm.participant_id = $2
		 LEFT JOIN channel_reads r ON r.channel_id = c.id AND r.participant_id = $2
		 WHERE c.room_id = $1 ORDER BY c.created_at ASC`, roomID, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Channel{}
	for rows.Next() {
		var c Channel
		if err := rows.Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy, &c.Archived, &c.Private,
			&c.CreatedAt, &c.LastReadAt, &c.Muted, &c.UnreadCount, &c.UnreadMentions, &c.UnreadDirectMentions); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BrowsableChannels lists every non-archived public channel in the room — the
// complete public map — with a live member count and a member flag for the
// channels the caller is already in. Private channels are invite-only and
// never appear here. Sorted by name so the browse list reads alphabetically.
func (s *Store) BrowsableChannels(ctx context.Context, roomID, participantID string) ([]Channel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.room_id, c.name, c.topic, c.created_by, c.archived, c.private, c.created_at,
		        (SELECT count(*) FROM channel_members m WHERE m.channel_id = c.id) AS member_count,
		        EXISTS (SELECT 1 FROM channel_members cm
		                WHERE cm.channel_id = c.id AND cm.participant_id = $2) AS member
		 FROM channels c
		 WHERE c.room_id = $1 AND NOT c.archived AND NOT c.private
		 ORDER BY c.name ASC`, roomID, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Channel{}
	for rows.Next() {
		var c Channel
		var count int64
		var member bool
		if err := rows.Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy,
			&c.Archived, &c.Private, &c.CreatedAt, &count, &member); err != nil {
			return nil, err
		}
		c.MemberCount = &count
		c.Member = &member
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkChannelRead advances the viewer's read marker to now and returns it.
func (s *Store) MarkChannelRead(ctx context.Context, participantID, channelID string) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO channel_reads (participant_id, channel_id, last_read_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (participant_id, channel_id) DO UPDATE SET last_read_at = now()
		 RETURNING last_read_at`, participantID, channelID).Scan(&at)
	return at, err
}

// SetChannelMuted flips the caller's notification mute on a channel they are
// in; a non-member has no row to flip and gets ErrNotFound.
func (s *Store) SetChannelMuted(ctx context.Context, participantID, channelID string, muted bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE channel_members SET muted = $3 WHERE participant_id = $1 AND channel_id = $2`,
		participantID, channelID, muted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ChannelByID(ctx context.Context, roomID, id string) (Channel, error) {
	var c Channel
	err := s.pool.QueryRow(ctx,
		`SELECT id, room_id, name, topic, created_by, archived, private, created_at
		 FROM channels WHERE room_id = $1 AND id = $2`, roomID, id,
	).Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy, &c.Archived, &c.Private, &c.CreatedAt)
	return c, mapRowErr(err)
}

func (s *Store) ChannelByName(ctx context.Context, roomID, name string) (Channel, error) {
	var c Channel
	err := s.pool.QueryRow(ctx,
		`SELECT id, room_id, name, topic, created_by, archived, private, created_at
		 FROM channels WHERE room_id = $1 AND name = $2`, roomID, name,
	).Scan(&c.ID, &c.RoomID, &c.Name, &c.Topic, &c.CreatedBy, &c.Archived, &c.Private, &c.CreatedAt)
	return c, mapRowErr(err)
}

// DeleteChannel removes a channel and (via FK cascade) all its messages.
func (s *Store) DeleteChannel(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	res, err := tx.Exec(ctx,
		`DELETE FROM channels WHERE room_id = $1 AND id = $2`, roomID, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	payload, _ := json.Marshal(map[string]string{"channel_id": id})
	if err := appendEventTx(ctx, tx, roomID, "channel.deleted", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetChannelPrivate converts a public channel to private. One-way by design:
// history shared under an expectation of privacy must never turn public later.
func (s *Store) SetChannelPrivate(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// advisory-first: the UPDATE takes an FK row lock on rooms before appendEventTx
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	res, err := tx.Exec(ctx,
		`UPDATE channels SET private = TRUE WHERE room_id = $1 AND id = $2`, roomID, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	// member-scoped (see gatedChannel): who can see a now-private channel is
	// itself private information
	payload, _ := json.Marshal(map[string]any{"channel_id": id, "private": true})
	if err := appendEventTx(ctx, tx, roomID, "channel.privacy_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SetChannelArchived(ctx context.Context, roomID, id string, archived bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// advisory-first: the UPDATE takes an FK row lock on rooms before appendEventTx
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	res, err := tx.Exec(ctx,
		`UPDATE channels SET archived = $3 WHERE room_id = $1 AND id = $2`, roomID, id, archived)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	typ := "channel.archived"
	if !archived {
		typ = "channel.unarchived"
	}
	// same tx as the UPDATE: a crash between them must not archive silently
	payload, _ := json.Marshal(map[string]string{"channel_id": id})
	if err := appendEventTx(ctx, tx, roomID, typ, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrNotEmpty: a private channel with history or other members stays private.
var ErrNotEmpty = errors.New("a private channel with messages or other members cannot be made public")

// SetChannelPublicIfEmpty is the one exception to one-way privacy: a channel
// nobody wrote in and nobody else joined exposes nothing when it goes public.
// Typically a creator flipped the wrong flag a minute ago.
func (s *Store) SetChannelPublicIfEmpty(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	res, err := tx.Exec(ctx,
		`UPDATE channels c SET private = FALSE
		 WHERE c.room_id = $1 AND c.id = $2 AND c.private
		   AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.channel_id = c.id)
		   AND (SELECT count(*) FROM channel_members cm WHERE cm.channel_id = c.id) <= 1`,
		roomID, id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotEmpty
	}
	payload, _ := json.Marshal(map[string]any{"channel_id": id, "private": false})
	if err := appendEventTx(ctx, tx, roomID, "channel.privacy_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// systemEntryTx persists a Slack-style timeline line as a system message and
// fans it out through message.created, so live feeds and history agree.
// message.created is membership-gated, so only the channel's members render it.
func systemEntryTx(ctx context.Context, tx pgx.Tx, roomID, channelID, authorID, body string) error {
	var msgID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO messages (room_id, channel_id, author_id, body, kind, embed_status)
		 VALUES ($1, $2, $3, $4, 'system', 'skipped') RETURNING id`,
		roomID, channelID, authorID, body).Scan(&msgID); err != nil {
		return err
	}
	msg, err := scanMessage(tx.QueryRow(ctx, messageSelect+` WHERE m.id = $1`, msgID))
	if err != nil {
		return err
	}
	msgPayload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return appendEventTx(ctx, tx, roomID, "message.created", msgPayload)
}

// generalEntryTx writes a workspace-level system line into #general.
func generalEntryTx(ctx context.Context, tx pgx.Tx, roomID, authorID, body string) error {
	var generalID string
	err := tx.QueryRow(ctx,
		`SELECT id FROM channels WHERE room_id = $1 AND name = 'general'`, roomID).Scan(&generalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // a room without #general (legacy fixtures) has nowhere to say it
	}
	if err != nil {
		return err
	}
	return systemEntryTx(ctx, tx, roomID, generalID, authorID, body)
}

// ErrNameTaken: the rename target already exists in the room.
var ErrNameTaken = fmt.Errorf("a channel with that name already exists: %w", ErrConflict)

// RenameChannel changes a channel's name, announces it with channel.renamed
// (old and new name) and a system line in the channel authored by the actor.
func (s *Store) RenameChannel(ctx context.Context, roomID, id, name, actorID, actorName string) (Channel, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Channel{}, err
	}
	defer tx.Rollback(ctx)

	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return Channel{}, err
	}
	var old string
	if err := tx.QueryRow(ctx,
		`SELECT name FROM channels WHERE room_id = $1 AND id = $2 FOR UPDATE`, roomID, id).Scan(&old); err != nil {
		return Channel{}, mapRowErr(err)
	}
	if old == name {
		return s.ChannelByID(ctx, roomID, id)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE channels SET name = $3 WHERE room_id = $1 AND id = $2`, roomID, id, name); err != nil {
		if isUniqueViolation(err) {
			return Channel{}, ErrNameTaken
		}
		return Channel{}, err
	}
	payload, _ := json.Marshal(map[string]string{
		"channel_id": id, "old_name": old, "name": name, "actor_id": actorID, "actor_name": actorName,
	})
	if err := appendEventTx(ctx, tx, roomID, "channel.renamed", payload); err != nil {
		return Channel{}, err
	}
	if err := systemEntryTx(ctx, tx, roomID, id, actorID, "renamed the channel from #"+old+" to #"+name); err != nil {
		return Channel{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Channel{}, err
	}
	return s.ChannelByID(ctx, roomID, id)
}
