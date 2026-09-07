package models

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/presmihaylov/openchatter/pkg/secrets"
)

const nullTokensVersion = 27

// TestNullHumanTokens drives 000027 over a schema-26 fixture: only a human
// linked to an account loses its token hash; unlinked humans and agents keep
// theirs, and rolling back is a no-op.
func TestNullHumanTokens(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	if got, err := MigrateTo(ctx, dbURL, nullTokensVersion-1); err != nil || got != nullTokensVersion-1 {
		t.Fatalf("MigrateTo %d: got %d %v", nullTokensVersion-1, got, err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	u, err := s.CreatePasswordUser(ctx, "linked", "Linked", []byte("$2a$04$linkedhashlinkedhashlink"))
	if err != nil {
		t.Fatal(err)
	}
	room := legacyRoom(t, s, "r", "r-slug")
	hashes := map[string][]byte{}
	mk := func(name string, human bool, userID *string) Participant {
		_, hash := secrets.NewToken()
		p := legacyParticipant(t, s, room.ID, name, "🧑", human, hash, nil, userID)
		hashes[p.ID] = hash
		return p
	}
	linked, unlinked, agent := mk("linked", true, &u.ID), mk("unlinked", true, nil), mk("bot", false, nil)

	hashOf := func(id string) []byte {
		var h []byte
		if err := pool.QueryRow(ctx, `SELECT token_hash FROM participants WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	if got, err := MigrateTo(ctx, dbURL, nullTokensVersion); err != nil || got != nullTokensVersion {
		t.Fatalf("MigrateTo %d: got %d %v", nullTokensVersion, got, err)
	}
	if hashOf(linked.ID) != nil {
		t.Fatal("linked human kept its token hash")
	}
	if hashOf(unlinked.ID) == nil {
		t.Fatal("unlinked human lost its token hash")
	}
	if hashOf(agent.ID) == nil {
		t.Fatal("agent lost its token hash")
	}
	// the store's lookup wants later columns; ask the row directly at this version
	var byHash string
	if err := pool.QueryRow(ctx, `SELECT id FROM participants WHERE token_hash = $1 AND NOT revoked`, hashes[agent.ID]).Scan(&byHash); err != nil || byHash != agent.ID {
		t.Fatalf("agent token after 000027: %q %v", byHash, err)
	}
	if got, err := MigrateTo(ctx, dbURL, nullTokensVersion-1); err != nil || got != nullTokensVersion-1 {
		t.Fatalf("down to %d: got %d %v", nullTokensVersion-1, got, err)
	}
	if hashOf(linked.ID) != nil {
		t.Fatal("down restored a token hash; it must be a no-op")
	}
}
