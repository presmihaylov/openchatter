package models

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/presmihaylov/openchatter/pkg/secrets"
)

// 000035 gives every agent an owner with an account: an ownerless agent and
// one owned by a cli human (no account) go to the workspace creator; an agent
// owned by a linked human keeps it; a room without a creator row keeps its
// ownerless agents. Nothing is revoked.
func TestAgentOwnersMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	const version = 35
	if got, err := MigrateTo(ctx, dbURL, version-1); err != nil || got != version-1 {
		t.Fatalf("MigrateTo %d: got %d %v", version-1, got, err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	maya := mkPasswordUser(t, s)
	omar := mkPasswordUser(t, s)
	room, mayaRow, err := s.CreateRoomAs(ctx, "fleet", "fleet-slug", "inv-fleet", maya)
	if err != nil {
		t.Fatal(err)
	}
	if mayaRow.Avatar != DefaultAvatar {
		t.Fatalf("a workspace creator starts on the default avatar, got %q", mayaRow.Avatar)
	}
	_, omarHash := secrets.NewToken()
	omarRow := legacyParticipant(t, s, room.ID, "omar", "🧑", true, omarHash, nil, &omar.ID)
	mk := func(roomID, name string, owner *string) Participant {
		_, hash := secrets.NewToken()
		return legacyParticipant(t, s, roomID, name, "🤖", false, hash, owner, nil)
	}
	// a cli human: is_human, no account
	_, theoHash := secrets.NewToken()
	theo := legacyParticipant(t, s, room.ID, "theo", "🧑", true, theoHash, nil, nil)
	orphan := mk(room.ID, "orphan", nil)
	ofTheo := mk(room.ID, "strix", &theo.ID)
	ofOmar := mk(room.ID, "reviewer", &omarRow.ID)
	// a legacy room: no creator, so its agents have nobody to fall back to
	var legacy Room
	if err := pool.QueryRow(ctx, `INSERT INTO rooms (name, slug, color) VALUES ('old', 'old-slug', 0) RETURNING id`).Scan(&legacy.ID); err != nil {
		t.Fatal(err)
	}
	oldBot := mk(legacy.ID, "oldbot", nil)
	// an agent whose linked owner was removed before owners mattered: its
	// token must keep working, so it moves to the creator too
	gone := mkPasswordUser(t, s)
	_, goneHash := secrets.NewToken()
	goneRow := legacyParticipant(t, s, room.ID, "Gone", "🧑", true, goneHash, nil, &gone.ID)
	ofGone := mk(room.ID, "leftover", &goneRow.ID)
	if _, err := pool.Exec(ctx, `UPDATE participants SET revoked = true WHERE id = $1`, goneRow.ID); err != nil {
		t.Fatal(err)
	}

	if got, err := MigrateTo(ctx, dbURL, version); err != nil || got != version {
		t.Fatalf("MigrateTo %d: got %d %v", version, got, err)
	}
	owner := func(id string) (o *string, revoked bool) {
		if err := pool.QueryRow(ctx, `SELECT owner_id, revoked FROM participants WHERE id = $1`, id).Scan(&o, &revoked); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, c := range []struct {
		name string
		id   string
		want *string
	}{
		{"orphan -> creator", orphan.ID, &mayaRow.ID},
		{"cli-human owned -> creator", ofTheo.ID, &mayaRow.ID},
		{"linked owner kept", ofOmar.ID, &omarRow.ID},
		{"legacy room stays ownerless", oldBot.ID, nil},
		{"revoked owner -> creator", ofGone.ID, &mayaRow.ID},
	} {
		got, revoked := owner(c.id)
		if revoked {
			t.Fatalf("%s: revoked by the migration", c.name)
		}
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Fatalf("%s: owner %v want %v", c.name, got, c.want)
		}
	}
	// humans never get an owner
	if got, _ := owner(theo.ID); got != nil {
		t.Fatalf("human got an owner: %v", *got)
	}
}
