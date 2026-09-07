package models

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/presmihaylov/openchatter/pkg/secrets"
)

const seedlingVersion = 40

// TestSeedlingAvatarMigration drives 000040 over a schema-39 fixture: the two
// old defaults become the seedling, an explicit emoji is untouched, and a new
// row with no avatar picks up the new column default. The down migration is
// lossy on purpose: it restores by is_human, so a member who chose 🌱 itself
// comes back as 🤖 or 🧑.
func TestSeedlingAvatarMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	if got, err := MigrateTo(ctx, dbURL, seedlingVersion-1); err != nil || got != seedlingVersion-1 {
		t.Fatalf("MigrateTo %d: got %d %v", seedlingVersion-1, got, err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	var roomID string
	if err := pool.QueryRow(ctx, `INSERT INTO rooms (name, slug) VALUES ('fleet', 'fleet-slug') RETURNING id`).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	seed := func(name, avatar string, human bool) string {
		_, hash := secrets.NewToken()
		return legacyParticipant(t, s, roomID, name, avatar, human, hash, nil, nil).ID
	}
	bot := seed("bot", "🤖", false)
	person := seed("person", "🧑", true)
	picky := seed("picky", "🦄", false)

	avatarOf := func(id string) string {
		var got string
		if err := pool.QueryRow(ctx, `SELECT avatar FROM participants WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got, err := MigrateTo(ctx, dbURL, seedlingVersion); err != nil || got != seedlingVersion {
		t.Fatalf("MigrateTo %d: got %d %v", seedlingVersion, got, err)
	}
	for _, c := range []struct{ id, want string }{{bot, DefaultAvatar}, {person, DefaultAvatar}, {picky, "🦄"}} {
		if got := avatarOf(c.id); got != c.want {
			t.Fatalf("after up: avatar %q, want %q", got, c.want)
		}
	}
	// the column default moved with the rows
	var fresh string
	if err := pool.QueryRow(ctx,
		`INSERT INTO participants (room_id, name, is_human) VALUES ($1, 'fresh', false) RETURNING avatar`, roomID).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if fresh != DefaultAvatar {
		t.Fatalf("column default after up: %q, want %q", fresh, DefaultAvatar)
	}

	if got, err := MigrateTo(ctx, dbURL, seedlingVersion-1); err != nil || got != seedlingVersion-1 {
		t.Fatalf("MigrateTo down %d: got %d %v", seedlingVersion-1, got, err)
	}
	for _, c := range []struct{ id, want string }{{bot, "🤖"}, {person, "🧑"}, {picky, "🦄"}} {
		if got := avatarOf(c.id); got != c.want {
			t.Fatalf("after down: avatar %q, want %q", got, c.want)
		}
	}
}
