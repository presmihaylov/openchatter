package models

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/jackc/pgx/v5"
)

// 000025 must roll back cleanly under a NULL-token human and come back up: the
// deploy's rollback is "-migrate-to 24, start the previous binary".
func TestRoomUsersMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	const latest = 43
	// the version before 000025, where the room-users columns do not exist
	const beforeRoomUsers = 24

	s, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u := mkPasswordUser(t, s)
	room, p, err := s.CreateRoomAs(ctx, "ws", "ws-slug", "inv-secret", u)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	nullable := func() string {
		var n string
		if err := conn.QueryRow(ctx,
			`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'participants' AND column_name = 'token_hash'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	hasColumn := func(table, col string) bool {
		var ok bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`, table, col).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if nullable() != "YES" || !hasColumn("participants", "user_id") || !hasColumn("rooms", "created_by_user_id") {
		t.Fatal("schema at 25 lacks the room-users columns")
	}

	if got, err := MigrateTo(ctx, dbURL, beforeRoomUsers); err != nil || got != beforeRoomUsers {
		t.Fatalf("MigrateTo %d: got %d %v", beforeRoomUsers, got, err)
	}
	if nullable() != "NO" {
		t.Fatal("down must restore token_hash NOT NULL")
	}
	if hasColumn("participants", "user_id") || hasColumn("rooms", "created_by_user_id") {
		t.Fatal("down left a task 03 column behind")
	}
	// the filler is random: the previous binary hashes any bearer string with
	// plain sha256, so a derivable filler (sha256("retired:" || id)) would be
	// a working token for the linked human during the rollback window
	var filler []byte
	if err := conn.QueryRow(ctx, `SELECT token_hash FROM participants WHERE id = $1`, p.ID).Scan(&filler); err != nil {
		t.Fatal(err)
	}
	if len(filler) != sha256.Size {
		t.Fatalf("the NULL-token row did not get a sha256-sized filler: %x", filler)
	}
	if guess := sha256.Sum256([]byte("retired:" + p.ID)); string(filler) == string(guess[:]) {
		t.Fatal("the filler hash is derivable from the public participant id")
	}
	var lastActive *string
	if err := conn.QueryRow(ctx, `SELECT last_active_room_id FROM users WHERE id = $1`, u.ID).Scan(&lastActive); err != nil {
		t.Fatal(err)
	}
	if lastActive == nil || *lastActive != room.ID {
		t.Fatalf("last_active_room_id must survive the down: %v", lastActive)
	}

	s, err = Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open after rollback: %v", err)
	}
	defer s.Close()
	if nullable() != "YES" || !hasColumn("participants", "user_id") {
		t.Fatal("up did not come back")
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT version FROM schema_migrations").Scan(&version); err != nil || version != latest {
		t.Fatalf("version after re-open: %d %v", version, err)
	}
	// the row survives the round trip. Its link was dropped at 24, so the
	// 000026 backfill sees an unlinked human whose derived username belongs
	// to a user with zero links (a squatter, from the row's point of view):
	// the row gets a fresh -2 account, never the original user
	got, err := s.ParticipantByID(ctx, room.ID, p.ID)
	if err != nil || got.UserID == nil || *got.UserID == u.ID {
		t.Fatalf("participant after round trip: %+v %v", got, err)
	}
	if relinked, err := s.UserByID(ctx, *got.UserID); err != nil || relinked.Username != u.Username+"-2" {
		t.Fatalf("relinked user: %+v %v", relinked, err)
	}
	sc, err := s.SessionScope(ctx, mkSession(t, s, u.ID), room.Slug, SessionMaxAge)
	if err != nil || sc.RoomID == nil || sc.Participant != nil {
		t.Fatalf("session after round trip: %+v %v", sc, err)
	}
	guess := sha256.Sum256([]byte("retired:" + p.ID))
	if _, err := s.ParticipantByTokenHash(ctx, guess[:]); err == nil {
		t.Fatal("\"retired:<id>\" authenticates as the linked human after the round trip")
	}
}
