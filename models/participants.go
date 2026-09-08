package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"time"
)

var ErrLastAdmin = errors.New("a room must keep at least one admin")

// CreateParticipant adds a member; the first participant in a room becomes admin.
// tokenHash is nil for a human who enters through a login session (userID set).
// inviteID, when set, spends one use of that link in the same transaction.
func (s *Store) CreateParticipant(ctx context.Context, roomID, name, avatar, description string, isHuman bool, tokenHash []byte, ownerID *string, userID *string, inviteID string) (Participant, error) {
	p, err := s.insertParticipant(ctx, roomID, name, avatar, description, isHuman, tokenHash, ownerID, userID, inviteID)
	if err != nil {
		return p, err
	}
	// the joined row with its owner and account names resolved
	return s.ParticipantByID(ctx, roomID, p.ID)
}

// insertParticipant is CreateParticipant without the read-back: the bare row
// the insert returned. Migration tests seed through it at older schemas.
func (s *Store) insertParticipant(ctx context.Context, roomID, name, avatar, description string, isHuman bool, tokenHash []byte, ownerID *string, userID *string, inviteID string) (Participant, error) {
	var p Participant
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)

	// serialize joins per room so exactly one first joiner becomes admin
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomID); err != nil {
		return p, err
	}
	if inviteID != "" {
		if err := consumeInviteTx(ctx, tx, inviteID); err != nil {
			return p, err
		}
	}
	p, err = createParticipantTx(ctx, tx, roomID, name, avatar, description, isHuman, tokenHash, ownerID, userID)
	if err != nil {
		return p, err
	}
	return p, tx.Commit(ctx)
}

// createParticipantTx is the one insert path for joins, /enter and CreateRoomAs.
// The caller holds the room advisory lock.
func createParticipantTx(ctx context.Context, tx pgx.Tx, roomID, name, avatar, description string, isHuman bool, tokenHash []byte, ownerID *string, userID *string) (Participant, error) {
	var p Participant
	err := tx.QueryRow(ctx,
		`INSERT INTO participants (room_id, name, avatar, description, is_human, token_hash, owner_id, user_id, presence_online, role)
		 SELECT $1, $2, $3, $4, $5, $6, $7, $8, TRUE,
		        CASE WHEN EXISTS (SELECT 1 FROM participants WHERE room_id = $1 AND NOT revoked)
		             THEN 'member' ELSE 'admin' END
		 RETURNING id, room_id, name, avatar, description, is_human, role, owner_id, user_id, last_seen_at, created_at`,
		roomID, name, avatar, description, isHuman, tokenHash, ownerID, userID,
	).Scan(&p.ID, &p.RoomID, &p.Name, &p.Avatar, &p.Description, &p.IsHuman, &p.Role, &p.OwnerID, &p.UserID, &p.LastSeenAt, &p.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return p, fmt.Errorf("name %q is taken in this room: %w", name, ErrConflict)
		}
		return p, err
	}
	p.Online = true
	p.Tags = []Tag{}

	// every new participant joins #general, so nobody lands in a room with an
	// empty sidebar. #general is the one channel you cannot leave.
	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_members (channel_id, participant_id)
		 SELECT id, $2 FROM channels WHERE room_id = $1 AND name = 'general'
		 ON CONFLICT DO NOTHING`, roomID, p.ID); err != nil {
		return p, err
	}

	event := map[string]any{
		"participant_id": p.ID, "name": p.Name, "is_human": p.IsHuman,
		"role": p.Role, "description": p.Description, "owner_id": p.OwnerID,
	}
	// additive: agents never carry it, so their payload is byte-identical
	if p.UserID != nil {
		event["user_id"] = *p.UserID
	}
	payload, _ := json.Marshal(event)
	if err := appendEventTx(ctx, tx, roomID, "participant.joined", payload); err != nil {
		return p, err
	}
	return p, nil
}

// ParticipantByTokenHash authenticates a request; revoked participants fail
// auth, and so does an agent whose owner's row is revoked (task 19: an agent
// lives and dies with its human's membership).
func (s *Store) ParticipantByTokenHash(ctx context.Context, hash []byte) (Participant, error) {
	var p Participant
	err := s.pool.QueryRow(ctx,
		`SELECT p.id, p.room_id, p.name, p.avatar, p.avatar_attachment_id, p.description, p.is_human, p.role,
		        p.owner_id, o.name, o.user_id, ou.username, p.user_id, u.username, p.last_seen_at, p.created_at,
		        p.last_seen_at > now() - $2::interval AND NOT p.declared_offline AS online, p.declared_offline
		 FROM participants p LEFT JOIN participants o ON o.id = p.owner_id
		 LEFT JOIN users ou ON ou.id = o.user_id
		 LEFT JOIN users u ON u.id = p.user_id
		 WHERE p.token_hash = $1 AND NOT p.revoked
		   AND (p.owner_id IS NULL OR NOT o.revoked)`,
		hash, OnlineWindow.String(),
	).Scan(&p.ID, &p.RoomID, &p.Name, &p.Avatar, &p.AvatarAttachmentID, &p.Description, &p.IsHuman, &p.Role, &p.OwnerID, &p.OwnerName, &p.OwnerUserID, &p.OwnerUsername, &p.UserID, &p.Username, &p.LastSeenAt, &p.CreatedAt, &p.Online, &p.DeclaredOffline)
	p.Presence = presenceOf(p.Online)
	return p, mapRowErr(err)
}

func (s *Store) ParticipantByID(ctx context.Context, roomID, id string) (Participant, error) {
	list, err := s.listParticipants(ctx, roomID, &id, nil, nil)
	if err != nil {
		return Participant{}, err
	}
	if len(list) == 0 {
		return Participant{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) ParticipantByName(ctx context.Context, roomID, name string) (Participant, error) {
	list, err := s.listParticipants(ctx, roomID, nil, &name, nil)
	if err != nil {
		return Participant{}, err
	}
	if len(list) == 0 {
		return Participant{}, ErrNotFound
	}
	return list[0], nil
}

// ListParticipants returns active (non-revoked) members.
func (s *Store) ListParticipants(ctx context.Context, roomID string) ([]Participant, error) {
	return s.listParticipants(ctx, roomID, nil, nil, nil)
}

// ListChannelMembers lists a channel's members with the full participant shape.
func (s *Store) ListChannelMembers(ctx context.Context, roomID, channelID string) ([]Participant, error) {
	return s.listParticipants(ctx, roomID, nil, nil, &channelID)
}

func (s *Store) listParticipants(ctx context.Context, roomID string, id, name, channelID *string) ([]Participant, error) {
	// a roster listing hides expired agents; a lookup by id or name still finds
	// them, so old messages keep their author and mentions keep resolving
	roster := id == nil && name == nil
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.room_id, p.name, p.avatar, p.avatar_attachment_id, p.description, p.is_human, p.role,
		        p.owner_id, o.name AS owner_name, o.user_id, ou.username, p.user_id, u.username,
		        p.last_seen_at, p.created_at,
		        p.last_seen_at > now() - $2::interval AND NOT p.declared_offline AS online, p.declared_offline,
		        COALESCE(
		            (SELECT json_agg(json_build_object('tag', t.tag, 'tagged_by', tb.name) ORDER BY t.created_at)
		             FROM participant_tags t
		             LEFT JOIN participants tb ON tb.id = t.tagged_by
		             WHERE t.participant_id = p.id),
		            '[]'::json) AS tags
		 FROM participants p
		 LEFT JOIN participants o ON o.id = p.owner_id
		 LEFT JOIN users ou ON ou.id = o.user_id
		 LEFT JOIN users u ON u.id = p.user_id
		 WHERE p.room_id = $1 AND NOT p.revoked
		   AND ($3::uuid IS NULL OR p.id = $3)
		   AND ($4::text IS NULL OR p.name = $4)
		   AND ($5::uuid IS NULL OR p.id IN
		        (SELECT participant_id FROM channel_members WHERE channel_id = $5))
		   AND NOT ($6::boolean AND NOT p.is_human AND p.last_seen_at < now() - $7::interval)
		 ORDER BY p.created_at ASC`,
		roomID, OnlineWindow.String(), id, name, channelID, roster, AgentExpireAfter.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Participant{}
	for rows.Next() {
		var p Participant
		var tagsJSON []byte
		if err := rows.Scan(&p.ID, &p.RoomID, &p.Name, &p.Avatar, &p.AvatarAttachmentID, &p.Description, &p.IsHuman, &p.Role,
			&p.OwnerID, &p.OwnerName, &p.OwnerUserID, &p.OwnerUsername, &p.UserID, &p.Username,
			&p.LastSeenAt, &p.CreatedAt, &p.Online, &p.DeclaredOffline, &tagsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(tagsJSON, &p.Tags); err != nil {
			return nil, err
		}
		p.Presence = presenceOf(p.Online)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProfile updates non-nil fields and returns the fresh participant.
func (s *Store) UpdateProfile(ctx context.Context, roomID, id string, name, avatar, description *string) (Participant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Participant{}, err
	}
	defer tx.Rollback(ctx)

	// advisory-first: the update takes an FK row lock on rooms before appendEventTx
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return Participant{}, err
	}

	tag, err := tx.Exec(ctx,
		`UPDATE participants SET
		    name = COALESCE($3, name),
		    avatar = COALESCE($4, avatar),
		    description = COALESCE($5, description)
		 WHERE room_id = $1 AND id = $2 AND NOT revoked`,
		roomID, id, name, avatar, description)
	if err != nil {
		if isUniqueViolation(err) {
			return Participant{}, ErrConflict
		}
		return Participant{}, err
	}
	if tag.RowsAffected() == 0 {
		return Participant{}, ErrNotFound
	}

	payload, _ := json.Marshal(map[string]any{"participant_id": id})
	if err := appendEventTx(ctx, tx, roomID, "participant.updated", payload); err != nil {
		return Participant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Participant{}, err
	}
	return s.ParticipantByID(ctx, roomID, id)
}

// SetAvatarAttachment points the participant's avatar at an uploaded image
// (nil reverts to the emoji avatar).
func (s *Store) SetAvatarAttachment(ctx context.Context, roomID, id string, attachmentID *string) (Participant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Participant{}, err
	}
	defer tx.Rollback(ctx)

	// advisory-first: the update takes an FK row lock on rooms before appendEventTx
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return Participant{}, err
	}

	res, err := tx.Exec(ctx,
		`UPDATE participants SET avatar_attachment_id = $3
		 WHERE room_id = $1 AND id = $2 AND NOT revoked`,
		roomID, id, attachmentID)
	if err != nil {
		return Participant{}, err
	}
	if res.RowsAffected() == 0 {
		return Participant{}, ErrNotFound
	}

	payload, _ := json.Marshal(map[string]string{"participant_id": id})
	if err := appendEventTx(ctx, tx, roomID, "participant.updated", payload); err != nil {
		return Participant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Participant{}, err
	}
	return s.ParticipantByID(ctx, roomID, id)
}

// SetRole promotes or demotes a participant, never leaving the room without an admin.
func (s *Store) SetRole(ctx context.Context, roomID, id, role string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomID); err != nil {
		return err
	}
	if role == "member" {
		var admins int
		err := tx.QueryRow(ctx,
			`SELECT count(*) FROM participants
			 WHERE room_id = $1 AND role = 'admin' AND NOT revoked AND id <> $2`,
			roomID, id).Scan(&admins)
		if err != nil {
			return err
		}
		var isAdmin bool
		if err := tx.QueryRow(ctx,
			`SELECT role = 'admin' FROM participants WHERE room_id = $1 AND id = $2 AND NOT revoked`,
			roomID, id).Scan(&isAdmin); err != nil {
			return mapRowErr(err)
		}
		if isAdmin && admins == 0 {
			return ErrLastAdmin
		}
	}

	res, err := tx.Exec(ctx,
		`UPDATE participants SET role = $3 WHERE room_id = $1 AND id = $2 AND NOT revoked`,
		roomID, id, role)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}

	payload, _ := json.Marshal(map[string]string{"participant_id": id, "role": role})
	if err := appendEventTx(ctx, tx, roomID, "participant.role_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Revoke removes a participant's access but keeps their messages and identity.
// #general gets a system line: "left the workspace" by the participant when
// they go on their own, else "removed <Name> from the workspace" by the actor.
func (s *Store) Revoke(ctx context.Context, roomID, id, actorID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomID); err != nil {
		return err
	}
	var isAdmin bool
	var targetName string
	if err := tx.QueryRow(ctx,
		`SELECT role = 'admin', name FROM participants WHERE room_id = $1 AND id = $2 AND NOT revoked`,
		roomID, id).Scan(&isAdmin, &targetName); err != nil {
		return mapRowErr(err)
	}
	if isAdmin {
		var others int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM participants
			 WHERE room_id = $1 AND role = 'admin' AND NOT revoked AND id <> $2`,
			roomID, id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return ErrLastAdmin
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE participants
		 SET revoked = true, presence_online = false, declared_offline = true
		 WHERE room_id = $1 AND id = $2`,
		roomID, id); err != nil {
		return err
	}
	// a kicked member must not walk back in on a link they minted and saved
	if err := revokeInvitesOfTx(ctx, tx, roomID, id); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"participant_id": id})
	if err := appendEventTx(ctx, tx, roomID, "participant.revoked", payload); err != nil {
		return err
	}
	// a human's agents go with them (task 19): same transaction, own events,
	// so their tokens die the instant the human's does
	agents, err := revokeOwnedAgentsTx(ctx, tx, roomID, id)
	if err != nil {
		return err
	}
	tail := ""
	if n := len(agents); n == 1 {
		tail = " and 1 agent"
	} else if n > 1 {
		tail = fmt.Sprintf(" and %d agents", n)
	}
	author, body := id, "left the workspace"+tail
	if actorID != "" && actorID != id {
		author, body = actorID, "removed "+targetName+tail+" from the workspace"
	}
	if err := generalEntryTx(ctx, tx, roomID, author, body); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// revokeOwnedAgentsTx revokes every live agent owned by the participant and
// returns their names.
func revokeOwnedAgentsTx(ctx context.Context, tx pgx.Tx, roomID, ownerID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`UPDATE participants
		 SET revoked = true, presence_online = false, declared_offline = true
		 WHERE room_id = $1 AND owner_id = $2 AND NOT is_human AND NOT revoked
		 RETURNING id, name`, roomID, ownerID)
	if err != nil {
		return nil, err
	}
	type rev struct{ id, name string }
	var revs []rev
	for rows.Next() {
		var r rev
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			return nil, err
		}
		revs = append(revs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(revs))
	for _, r := range revs {
		if err := revokeInvitesOfTx(ctx, tx, roomID, r.id); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]string{"participant_id": r.id, "owner_id": ownerID})
		if err := appendEventTx(ctx, tx, roomID, "participant.revoked", payload); err != nil {
			return nil, err
		}
		names = append(names, r.name)
	}
	return names, nil
}

// OwnedAgents lists the live agents a participant owns, oldest first.
func (s *Store) OwnedAgents(ctx context.Context, roomID, ownerID string) ([]Participant, error) {
	all, err := s.listParticipants(ctx, roomID, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	out := []Participant{}
	for _, p := range all {
		if !p.IsHuman && p.OwnerID != nil && *p.OwnerID == ownerID {
			out = append(out, p)
		}
	}
	return out, nil
}

// ErrBadOwner is an owner that is not a live human member with an account.
var ErrBadOwner = errors.New("the owner must be a human member of this workspace with an account")

// SetOwner rebinds an agent to another human member (admins fix a wrong
// migration guess with it). The target must be a live agent of the room.
func (s *Store) SetOwner(ctx context.Context, roomID, agentID, ownerID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, roomID); err != nil {
		return err
	}
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM participants
		   WHERE room_id = $1 AND id = $2 AND is_human AND NOT revoked AND user_id IS NOT NULL)`,
		roomID, ownerID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrBadOwner
	}
	tag, err := tx.Exec(ctx,
		`UPDATE participants SET owner_id = $3 WHERE room_id = $1 AND id = $2 AND NOT is_human AND NOT revoked`,
		roomID, agentID, ownerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	payload, _ := json.Marshal(map[string]string{"participant_id": agentID, "owner_id": ownerID})
	if err := appendEventTx(ctx, tx, roomID, "participant.owner_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreatorRow is the workspace creator's live member row, the default owner
// of an agent that joins on a plain link; nil when the creator has no row.
func (s *Store) CreatorRow(ctx context.Context, roomID string) (*string, error) {
	var id *string
	err := s.pool.QueryRow(ctx,
		`SELECT p.id FROM rooms r JOIN participants p
		   ON p.room_id = r.id AND p.user_id = r.created_by_user_id AND p.is_human AND NOT p.revoked
		 WHERE r.id = $1`, roomID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return id, err
}

// TouchPresence marks the participant as recently seen. Crossing from an
// announced-offline state emits participant.presence_changed exactly once. A
// participant that declared itself offline (task 21) stays offline: only an
// explicit SetPresence(online) brings it back.
func (s *Store) TouchPresence(ctx context.Context, roomID, id string) error {
	// fast path: already announced online, no event to write
	tag, err := s.pool.Exec(ctx,
		`UPDATE participants SET last_seen_at = now() WHERE id = $1 AND (presence_online OR declared_offline)`, id)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	var name string
	err = tx.QueryRow(ctx,
		`UPDATE participants SET last_seen_at = now(), presence_online = TRUE
		 WHERE id = $1 AND NOT presence_online RETURNING name`, id,
	).Scan(&name)
	if err != nil {
		// a concurrent request won the transition; nothing left to announce
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	payload, _ := json.Marshal(map[string]any{"participant_id": id, "name": name, "online": true})
	if err := appendEventTx(ctx, tx, roomID, "participant.presence_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GoOffline makes the participant immediately count as offline.
func (s *Store) GoOffline(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	var name string
	err = tx.QueryRow(ctx,
		`UPDATE participants SET last_seen_at = now() - $2::interval, presence_online = FALSE
		 WHERE id = $1 RETURNING name`, id, (2 * OnlineWindow).String(),
	).Scan(&name)
	if err != nil {
		return mapRowErr(err)
	}
	payload, _ := json.Marshal(map[string]any{"participant_id": id, "name": name, "online": false})
	if err := appendEventTx(ctx, tx, roomID, "participant.presence_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func presenceOf(online bool) string {
	if online {
		return "online"
	}
	return "offline"
}

// DeclareOffline (task 21) parks the participant: it counts as offline until
// DeclareOnline, whatever it requests meanwhile, and remembers the event seq
// it left at so the catch-up batch starts there. Idempotent; announces the
// transition once.
func (s *Store) DeclareOffline(ctx context.Context, roomID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return err
	}
	var name string
	var wasOnline bool
	err = tx.QueryRow(ctx,
		`UPDATE participants p SET presence_online = FALSE, declared_offline = TRUE,
		        offline_since_seq = COALESCE(p.offline_since_seq, (SELECT COALESCE(MAX(seq), 0) FROM events WHERE room_id = $2))
		 FROM (SELECT id, last_seen_at > now() - $3::interval AND NOT declared_offline AS online FROM participants WHERE id = $1 FOR UPDATE) old
		 WHERE p.id = old.id RETURNING p.name, old.online`,
		id, roomID, OnlineWindow.String(),
	).Scan(&name, &wasOnline)
	if err != nil {
		return mapRowErr(err)
	}
	if !wasOnline {
		return tx.Commit(ctx)
	}
	payload, _ := json.Marshal(map[string]any{"participant_id": id, "name": name, "online": false})
	if err := appendEventTx(ctx, tx, roomID, "participant.presence_changed", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeclareOnline clears a declared-offline state. It returns the seq the
// participant went offline at and whether it was declared offline at all: two
// concurrent onlines hand the catch-up to exactly one caller. Announces the
// transition once.
func (s *Store) DeclareOnline(ctx context.Context, roomID, id string) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return 0, false, err
	}
	var name string
	var since *int64
	var wasOnline bool
	err = tx.QueryRow(ctx,
		`UPDATE participants p SET last_seen_at = now(), presence_online = TRUE, declared_offline = FALSE, offline_since_seq = NULL
		 FROM (SELECT id, declared_offline, offline_since_seq, presence_online FROM participants WHERE id = $1 FOR UPDATE) old
		 WHERE p.id = old.id
		 RETURNING p.name, CASE WHEN old.declared_offline THEN old.offline_since_seq ELSE NULL END, old.presence_online`,
		id,
	).Scan(&name, &since, &wasOnline)
	if err != nil {
		return 0, false, mapRowErr(err)
	}
	if !wasOnline {
		payload, _ := json.Marshal(map[string]any{"participant_id": id, "name": name, "online": true})
		if err := appendEventTx(ctx, tx, roomID, "participant.presence_changed", payload); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	if since == nil {
		return 0, false, nil
	}
	return *since, true, nil
}

// BackdateSeen is a test hook: ages last_seen_at past the online window without
// touching the announced presence flag, so a sweep sees a stale-online row.
func (s *Store) BackdateSeen(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE participants SET last_seen_at = now() - $2::interval WHERE id = $1`, id, (2 * OnlineWindow).String())
	return err
}

// ExpireSeen is a test hook: ages last_seen_at past AgentExpireAfter.
func (s *Store) ExpireSeen(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE participants SET last_seen_at = now() - $2::interval, presence_online = FALSE WHERE id = $1`,
		id, (AgentExpireAfter + time.Hour).String())
	return err
}

// SweepPresence announces the online->offline transition for participants whose
// heartbeat stopped: still flagged online but last seen outside the window.
// Meant to run on a ticker. Returns how many transitions it announced.
func (s *Store) SweepPresence(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT room_id FROM participants
		 WHERE presence_online AND last_seen_at < now() - $1::interval`,
		OnlineWindow.String())
	if err != nil {
		return 0, err
	}
	roomIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		roomIDs = append(roomIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, roomID := range roomIDs {
		n, err := s.sweepRoomPresence(ctx, roomID)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) sweepRoomPresence(ctx context.Context, roomID string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockRoomEvents(ctx, tx, roomID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx,
		`UPDATE participants SET presence_online = FALSE
		 WHERE room_id = $1 AND presence_online AND last_seen_at < now() - $2::interval
		 RETURNING id, name`,
		roomID, OnlineWindow.String())
	if err != nil {
		return 0, err
	}
	type gone struct{ id, name string }
	dropped := []gone{}
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.name); err != nil {
			rows.Close()
			return 0, err
		}
		dropped = append(dropped, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, g := range dropped {
		payload, _ := json.Marshal(map[string]any{"participant_id": g.id, "name": g.name, "online": false})
		if err := appendEventTx(ctx, tx, roomID, "participant.presence_changed", payload); err != nil {
			return 0, err
		}
	}
	return len(dropped), tx.Commit(ctx)
}

func (s *Store) AddTag(ctx context.Context, roomID, participantID, tag, taggedBy string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	res, err := tx.Exec(ctx,
		`INSERT INTO participant_tags (participant_id, tag, tagged_by)
		 SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM participants WHERE id = $1 AND room_id = $4)
		 ON CONFLICT DO NOTHING`,
		participantID, tag, taggedBy, roomID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		// either the participant is gone (404) or the tag already exists (no-op,
		// no duplicate event either way)
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM participants WHERE id = $1 AND room_id = $2)`,
			participantID, roomID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return nil
	}

	payload, _ := json.Marshal(map[string]string{
		"participant_id": participantID, "tag": tag, "tagged_by": taggedBy,
	})
	if err := appendEventTx(ctx, tx, roomID, "participant.tagged", payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RemoveTag(ctx context.Context, roomID, participantID, tag string) error {
	res, err := s.pool.Exec(ctx,
		`DELETE FROM participant_tags t USING participants p
		 WHERE t.participant_id = p.id AND p.room_id = $1 AND t.participant_id = $2 AND t.tag = $3`,
		roomID, participantID, tag)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return s.AppendEvent(ctx, roomID, "participant.untagged",
		map[string]string{"participant_id": participantID, "tag": tag})
}

// NotifyPrefs reads the participant's own notification settings.
func (s *Store) NotifyPrefs(ctx context.Context, roomID, id string) (NotifyPrefs, error) {
	var np NotifyPrefs
	err := s.pool.QueryRow(ctx,
		`SELECT notify_enabled, notify_sound, archive_after_secs FROM participants WHERE room_id = $1 AND id = $2 AND NOT revoked`,
		roomID, id).Scan(&np.Enabled, &np.Sound, &np.ArchiveAfterSecs)
	if errors.Is(err, pgx.ErrNoRows) {
		return np, ErrNotFound
	}
	return np, err
}

// SetNotifyPrefs updates whichever of the settings is non-nil. No event:
// a setting is the participant's own business, not the room's.
func (s *Store) SetNotifyPrefs(ctx context.Context, roomID, id string, enabled, sound *bool, archiveAfterSecs *int) (NotifyPrefs, error) {
	var np NotifyPrefs
	err := s.pool.QueryRow(ctx,
		`UPDATE participants SET
		    notify_enabled = COALESCE($3, notify_enabled),
		    notify_sound = COALESCE($4, notify_sound),
		    archive_after_secs = COALESCE($5, archive_after_secs)
		 WHERE room_id = $1 AND id = $2 AND NOT revoked
		 RETURNING notify_enabled, notify_sound, archive_after_secs`,
		roomID, id, enabled, sound, archiveAfterSecs).Scan(&np.Enabled, &np.Sound, &np.ArchiveAfterSecs)
	if errors.Is(err, pgx.ErrNoRows) {
		return np, ErrNotFound
	}
	return np, err
}
