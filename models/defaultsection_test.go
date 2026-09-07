package models

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/presmihaylov/openchatter/pkg/secrets"
)

const defaultSectionVersion = 41

// TestDefaultChannelSectionMigration drives 000041 over a schema-40 fixture.
// Up: group_id becomes nullable, so a channel can be ordered inside the default
// section, and the participant gains a collapsed flag for it. Down is lossy on
// purpose: schema 40 has nowhere to keep that order, so the NULL rows go while
// the named section's rows survive.
func TestDefaultChannelSectionMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	if got, err := MigrateTo(ctx, dbURL, defaultSectionVersion-1); err != nil || got != defaultSectionVersion-1 {
		t.Fatalf("MigrateTo %d: got %d %v", defaultSectionVersion-1, got, err)
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
	_, hash := secrets.NewToken()
	p := legacyParticipant(t, s, roomID, "ann", "🌱", true, hash, nil, nil)

	channelID := func(name string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO channels (room_id, name, created_by) VALUES ($1, $2, $3) RETURNING id`,
			roomID, name, p.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	alpha, beta := channelID("alpha"), channelID("beta")

	var groupID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO channel_groups (participant_id, name) VALUES ($1, 'Work') RETURNING id`, p.ID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO channel_group_items (participant_id, channel_id, group_id, position) VALUES ($1, $2, $3, 0)`,
		p.ID, alpha, groupID); err != nil {
		t.Fatal(err)
	}

	// at schema 40 the default section cannot be ordered at all
	if _, err := pool.Exec(ctx,
		`INSERT INTO channel_group_items (participant_id, channel_id, group_id, position) VALUES ($1, $2, NULL, 0)`,
		p.ID, beta); err == nil {
		t.Fatal("schema 40 accepted a NULL group_id; the migration would be pointless")
	}

	if got, err := MigrateTo(ctx, dbURL, defaultSectionVersion); err != nil || got != defaultSectionVersion {
		t.Fatalf("MigrateTo %d: got %d %v", defaultSectionVersion, got, err)
	}

	// up: the default section is orderable, and it has a collapsed flag
	if err := s.SetChannelGroup(ctx, p.ID, beta, nil, 3); err != nil {
		t.Fatalf("place beta in the default section: %v", err)
	}
	layout, err := s.ListChannelLayout(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Ungrouped) != 1 || layout.Ungrouped[0] != beta {
		t.Fatalf("default section order: %v", layout.Ungrouped)
	}
	if len(layout.Groups) != 1 || len(layout.Groups[0].ChannelIDs) != 1 || layout.Groups[0].ChannelIDs[0] != alpha {
		t.Fatalf("named section lost its channel: %+v", layout.Groups)
	}
	if layout.DefaultCollapsed {
		t.Fatal("the default section starts collapsed")
	}
	if err := s.SetDefaultSectionCollapsed(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if layout, err = s.ListChannelLayout(ctx, p.ID); err != nil || !layout.DefaultCollapsed {
		t.Fatalf("collapsed flag did not stick: %+v %v", layout, err)
	}

	// deleting a section returns its channels to the default one instead of
	// letting the FK cascade throw their placement away
	if err := s.DeleteChannelGroup(ctx, p.ID, groupID); err != nil {
		t.Fatal(err)
	}
	if layout, err = s.ListChannelLayout(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if len(layout.Groups) != 0 || len(layout.Ungrouped) != 2 || layout.Ungrouped[0] != beta || layout.Ungrouped[1] != alpha {
		t.Fatalf("delete did not append the section's channels: %+v", layout)
	}

	// down: the default section's rows go, the named section's would have stayed
	if got, err := MigrateTo(ctx, dbURL, defaultSectionVersion-1); err != nil || got != defaultSectionVersion-1 {
		t.Fatalf("MigrateTo down: got %d %v", got, err)
	}
	var items int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM channel_group_items WHERE participant_id = $1`, p.ID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 0 {
		t.Fatalf("down kept %d placement rows; every one was in the default section", items)
	}
	var col int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'participants' AND column_name = 'default_section_collapsed'`).Scan(&col); err != nil {
		t.Fatal(err)
	}
	if col != 0 {
		t.Fatal("down left default_section_collapsed behind")
	}
}
