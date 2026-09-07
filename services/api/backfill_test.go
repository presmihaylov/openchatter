package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
)

// scratchDBURL creates a throwaway database for a schema-level fixture; the
// shared dev database cannot be moved to 25 while other packages run.
func scratchDBURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("OPENCHATTER_TEST_DB_URL")
	if base == "" {
		base = "postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	name := fmt.Sprintf("agentchat_api_scratch_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop scratch db %s: %v", name, err)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// A legacy human seeded at schema 25 logs in with the default password after
// 000026 and lands on the participant row it always had.
func TestBackfillLoginResolvesParticipant(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDBURL(t)
	if got, err := models.MigrateTo(ctx, dbURL, 25); err != nil || got != 25 {
		t.Fatalf("MigrateTo 25: got %d %v", got, err)
	}
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var roomID, pid string
	if err := conn.QueryRow(ctx,
		`INSERT INTO rooms (name, slug, secret) VALUES ('legacy', 'legacy-slug', $1) RETURNING id`,
		secrets.InviteCode()).Scan(&roomID); err != nil {
		t.Fatal(err)
	}
	_, hash := secrets.NewToken()
	if err := conn.QueryRow(ctx,
		`INSERT INTO participants (room_id, name, avatar, is_human, token_hash, role)
		 VALUES ($1, 'Backfill Person', '🧑', true, $2, 'admin') RETURNING id`, roomID, hash).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	// Open runs 000026
	store, err := models.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	srv := httptest.NewServer(New(store, testConfig(store, Config{PublicURL: "http://public.test"})).Handler())
	defer srv.Close()

	c := &testClient{t: t, base: srv.URL}
	out := c.must("POST", "/api/v1/auth/password/login", map[string]any{"username": "backfill-person", "password": "developer"}, 200)
	user := out["user"].(map[string]any)
	if user["must_change_password"] != true || user["display_name"] != "Backfill Person" {
		t.Fatalf("login user: %v", user)
	}
	c.token = out["token"].(string)
	c.slug = "legacy-slug"
	me := c.must("GET", "/api/v1/me", nil, 200)
	if me["id"] != pid || me["user_id"] != user["id"] || me["username"] != "backfill-person" || me["role"] != "admin" {
		t.Fatalf("me: %v", me)
	}
}
