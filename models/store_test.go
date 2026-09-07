package models

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/presmihaylov/agentchat/pkg/secrets"
)

// Integration tests; they need the docker compose db (make db-up) and skip otherwise.

func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("AGENTCHAT_TEST_DB_URL")
	if url == "" {
		url = "postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func mkRoom(t *testing.T, s *Store) Room {
	t.Helper()
	r, _ := mkRoomLink(t, s)
	return r
}

// mkRoomLink also hands back the token of the room's first invite link.
func mkRoomLink(t *testing.T, s *Store) (Room, string) {
	t.Helper()
	token := secrets.InviteCode()
	r, err := s.CreateRoom(context.Background(), "test room", secrets.RoomSlug(), token)
	if err != nil {
		t.Fatal(err)
	}
	return r, token
}

func mkParticipant(t *testing.T, s *Store, roomID, name string) (Participant, string) {
	t.Helper()
	token, hash := secrets.NewToken()
	p, err := s.CreateParticipant(context.Background(), roomID, name, "🤖", "test agent", false, hash, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return p, token
}

func generalChannel(t *testing.T, s *Store, roomID string) Channel {
	t.Helper()
	c, err := s.ChannelByName(context.Background(), roomID, "general")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoomLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r, token := mkRoomLink(t, s)

	inv, got, err := s.InviteByToken(ctx, token)
	if err != nil || got.ID != r.ID || inv.Status != "active" || inv.CreatedBy != nil || inv.OwnerID != nil {
		t.Fatalf("InviteByToken: %v %+v %+v", err, inv, got)
	}
	if _, _, err := s.InviteByToken(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	chans, err := s.ListChannels(ctx, r.ID)
	if err != nil || len(chans) != 1 || chans[0].Name != "general" {
		t.Fatalf("expected default general channel: %v %+v", err, chans)
	}
}

func TestParticipantsAuthPresenceTags(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mkRoom(t, s)

	p1, token := mkParticipant(t, s, r.ID, "alice")
	p2, _ := mkParticipant(t, s, r.ID, "bob")

	if _, _, err := func() (Participant, string, error) {
		_, hash := secrets.NewToken()
		p, err := s.CreateParticipant(ctx, r.ID, "alice", "x", "", false, hash, nil, nil, "")
		return p, "", err
	}(); err == nil {
		t.Fatal("expected duplicate-name conflict")
	}

	auth, err := s.ParticipantByTokenHash(ctx, secrets.HashToken(token))
	if err != nil || auth.ID != p1.ID {
		t.Fatalf("auth failed: %v", err)
	}
	if !auth.Online {
		t.Fatal("expected online after create")
	}

	if err := s.GoOffline(ctx, r.ID, p1.ID); err != nil {
		t.Fatal(err)
	}
	auth, _ = s.ParticipantByTokenHash(ctx, secrets.HashToken(token))
	if auth.Online {
		t.Fatal("expected offline after GoOffline")
	}
	if err := s.TouchPresence(ctx, r.ID, p1.ID); err != nil {
		t.Fatal(err)
	}
	auth, _ = s.ParticipantByTokenHash(ctx, secrets.HashToken(token))
	if !auth.Online {
		t.Fatal("expected online after touch")
	}

	if err := s.AddTag(ctx, r.ID, p2.ID, "researcher", p1.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ParticipantByID(ctx, r.ID, p2.ID)
	if err != nil || len(got.Tags) != 1 || got.Tags[0].Tag != "researcher" || *got.Tags[0].TaggedBy != "alice" {
		t.Fatalf("tags: %v %+v", err, got.Tags)
	}
	if err := s.RemoveTag(ctx, r.ID, p2.ID, "researcher"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveTag(ctx, r.ID, p2.ID, "researcher"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	profDesc := "updated desc"
	upd, err := s.UpdateProfile(ctx, r.ID, p2.ID, nil, nil, &profDesc)
	if err != nil || upd.Description != profDesc {
		t.Fatalf("update profile: %v %+v", err, upd)
	}
}

func TestMessagesThreadsAttachmentsEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mkRoom(t, s)
	ch := generalChannel(t, s, r.ID)
	alice, _ := mkParticipant(t, s, r.ID, "alice")
	bob, _ := mkParticipant(t, s, r.ID, "bob")

	att, err := s.CreateAttachment(ctx, r.ID, alice.ID, "notes.txt", "text/plain", []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}

	root, err := s.CreateMessage(ctx, CreateMessageParams{
		RoomID: r.ID, ChannelID: ch.ID, AuthorID: alice.ID,
		Body: "hey @bob check this **markdown**", IsBroadcast: false,
		AttachmentIDs: []string{att.ID}, MentionIDs: []string{bob.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if root.AuthorName != "alice" || len(root.Attachments) != 1 || len(root.Mentions) != 1 || root.Mentions[0] != "bob" {
		t.Fatalf("hydration wrong: %+v", root)
	}

	reply, err := s.CreateMessage(ctx, CreateMessageParams{
		RoomID: r.ID, ChannelID: ch.ID, ThreadRootID: &root.ID, AuthorID: bob.ID, Body: "nice!",
	})
	if err != nil {
		t.Fatal(err)
	}

	top, err := s.ListChannelMessages(ctx, r.ID, ch.ID, nil, nil, nil, 50)
	if err != nil || len(top) != 1 || top[0].ID != root.ID || top[0].ReplyCount != 1 {
		t.Fatalf("top-level listing wrong: %v %+v", err, top)
	}

	thread, err := s.ListThread(ctx, r.ID, root.ID, 0)
	if err != nil || len(thread) != 2 || thread[0].ID != root.ID || thread[1].ID != reply.ID {
		t.Fatalf("thread listing wrong: %v", err)
	}

	gotAtt, err := s.AttachmentByID(ctx, r.ID, att.ID)
	if err != nil || string(gotAtt.Data) != "hello world" {
		t.Fatalf("attachment fetch: %v", err)
	}

	events, err := s.ListEvents(ctx, r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, e := range events {
		types[e.Type]++
	}
	if types["room.created"] != 1 || types["participant.joined"] != 2 || types["message.created"] != 2 {
		t.Fatalf("event log wrong: %+v", types)
	}

	// cross-room attachment reference must fail
	r2 := mkRoom(t, s)
	eve, _ := mkParticipant(t, s, r2.ID, "eve")
	ch2 := generalChannel(t, s, r2.ID)
	_, err = s.CreateMessage(ctx, CreateMessageParams{
		RoomID: r2.ID, ChannelID: ch2.ID, AuthorID: eve.ID, Body: "steal",
		AttachmentIDs: []string{att.ID},
	})
	if err != ErrNotFound {
		t.Fatalf("expected cross-room attachment rejection, got %v", err)
	}
}

func TestSearchText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mkRoom(t, s)
	ch := generalChannel(t, s, r.ID)
	alice, _ := mkParticipant(t, s, r.ID, "alice")
	bob, _ := mkParticipant(t, s, r.ID, "bob")

	for i, body := range []string{
		"deploying the payment service tonight",
		"the payment gateway is flaky",
		"lunch plans anyone?",
	} {
		author := alice.ID
		if i == 1 {
			author = bob.ID
		}
		if _, err := s.CreateMessage(ctx, CreateMessageParams{
			RoomID: r.ID, ChannelID: ch.ID, AuthorID: author, Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := s.SearchText(ctx, r.ID, "payment", SearchFilters{})
	if err != nil || len(res) != 2 {
		t.Fatalf("fts: %v, got %d results", err, len(res))
	}
	res, err = s.SearchText(ctx, r.ID, "payment", SearchFilters{AuthorIDs: []string{bob.ID}})
	if err != nil || len(res) != 1 || res[0].AuthorName != "bob" {
		t.Fatalf("fts author filter: %v %d", err, len(res))
	}

	fmt.Println("search ok")
}

func TestEmbeddingsQueue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mkRoom(t, s)
	ch := generalChannel(t, s, r.ID)
	alice, _ := mkParticipant(t, s, r.ID, "alice")

	msg, err := s.CreateMessage(ctx, CreateMessageParams{
		RoomID: r.ID, ChannelID: ch.ID, AuthorID: alice.ID, Body: "embed me",
	})
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := s.ClaimPendingEmbeddings(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var mine *PendingMessage
	for i := range claimed {
		if claimed[i].ID == msg.ID {
			mine = &claimed[i]
		}
	}
	if mine == nil {
		t.Fatal("message not claimed")
	}

	vec := make([]float32, 1536)
	vec[0] = 1
	if err := s.SaveEmbedding(ctx, msg.ID, vec); err != nil {
		t.Fatal(err)
	}

	res, err := s.SearchSemantic(ctx, r.ID, vec, SearchFilters{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range res {
		if m.ID == msg.ID && m.Score > 0.99 {
			found = true
		}
	}
	if !found {
		t.Fatalf("semantic search did not return embedded message: %+v", res)
	}

	// release the rest of the claimed batch back to the queue
	ids := []string{}
	for _, c := range claimed {
		if c.ID != msg.ID {
			ids = append(ids, c.ID)
		}
	}
	if err := s.ReleaseEmbeddings(ctx, ids); err != nil {
		t.Fatal(err)
	}
}

func mkPasswordUser(t *testing.T, s *Store) User {
	t.Helper()
	name := fmt.Sprintf("pw%d", time.Now().UnixNano()%1_000_000_000_000)
	u, err := s.CreatePasswordUser(context.Background(), name, name, []byte("$2a$04$notarealhash"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mkSession(t *testing.T, s *Store, userID string) []byte {
	t.Helper()
	_, hash := secrets.NewSessionToken()
	if _, err := s.CreateSession(context.Background(), userID, "password", hash, time.Hour); err != nil {
		t.Fatal(err)
	}
	return hash
}

// nil keepHash must revoke every session; a keepHash spares exactly that one.
func TestDeleteUserSessionsNilKeepsNone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mkPasswordUser(t, s)
	a, b := mkSession(t, s, u.ID), mkSession(t, s, u.ID)

	n, err := s.DeleteUserSessions(ctx, u.ID, nil)
	if err != nil || n != 2 {
		t.Fatalf("nil keep: %d %v", n, err)
	}
	for _, h := range [][]byte{a, b} {
		if _, _, err := s.SessionByTokenHash(ctx, h, time.Hour); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session survived a nil-keep delete: %v", err)
		}
	}

	kept := mkSession(t, s, u.ID)
	n, err = s.DeleteUserSessions(ctx, u.ID, kept)
	if err != nil || n != 0 {
		t.Fatalf("keep own: %d %v", n, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, kept, time.Hour); err != nil {
		t.Fatalf("kept session gone: %v", err)
	}
}

// SetPasswordHash revokes sessions in the same transaction as the new hash.
func TestSetPasswordHashRevokesSessions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mkPasswordUser(t, s)
	kept, other := mkSession(t, s, u.ID), mkSession(t, s, u.ID)

	n, err := s.SetPasswordHash(ctx, u.ID, []byte("$2a$04$anotherfakehash"), kept)
	if err != nil || n != 1 {
		t.Fatalf("keep one: %d %v", n, err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, other, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session survived: %v", err)
	}
	if _, _, err := s.SessionByTokenHash(ctx, kept, time.Hour); err != nil {
		t.Fatalf("kept session gone: %v", err)
	}
	if _, hash, err := s.PasswordIdentity(ctx, u.Username); err != nil || string(hash) != "$2a$04$anotherfakehash" {
		t.Fatalf("hash not stored: %q %v", hash, err)
	}
	if n, err := s.SetPasswordHash(ctx, u.ID, []byte("$2a$04$third"), nil); err != nil || n != 1 {
		t.Fatalf("nil keep: %d %v", n, err)
	}
	if _, err := s.SetPasswordHash(ctx, "00000000-0000-0000-0000-000000000000", []byte("x"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

// scratchDB creates a throwaway database next to the test one and returns its
// URL. Schema-level tests need it: the suite runs packages in parallel against
// one dev database, so down-migrating that one would break services/api tests
// mid-run.
func scratchDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AGENTCHAT_TEST_DB_URL")
	if base == "" {
		base = "postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable"
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	name := fmt.Sprintf("agentchat_scratch_%d", time.Now().UnixNano())
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

func TestMigrateTo(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	const latest = 42
	// 000024 created users; rolling to the version before it drops the table
	const beforeUsers = 23

	usersTable := func() *string {
		conn, err := pgx.Connect(ctx, dbURL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var reg *string
		if err := conn.QueryRow(ctx, "SELECT to_regclass('users')::text").Scan(&reg); err != nil {
			t.Fatal(err)
		}
		return reg
	}

	s, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if usersTable() == nil {
		t.Fatal("fresh Open must migrate up to users")
	}

	got, err := MigrateTo(ctx, dbURL, beforeUsers)
	if err != nil || got != beforeUsers {
		t.Fatalf("MigrateTo %d: got %d %v", beforeUsers, got, err)
	}
	if reg := usersTable(); reg != nil {
		t.Fatalf("users must be gone at version %d, to_regclass = %q", beforeUsers, *reg)
	}
	// already there: ErrNoChange is success, not a failed rollback
	if got, err := MigrateTo(ctx, dbURL, beforeUsers); err != nil || got != beforeUsers {
		t.Fatalf("MigrateTo repeat: got %d %v", got, err)
	}

	s, err = Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open after rollback: %v", err)
	}
	defer s.Close()
	if usersTable() == nil {
		t.Fatal("Open must bring users back")
	}
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM schema_migrations").Scan(&version); err != nil || version != latest {
		t.Fatalf("version after re-open: %d %v", version, err)
	}
}

// legacyRoom inserts a room on a schema older than 000028 (no colour column),
// for the tests that drive a single migration over an old fixture.
// legacyParticipant seeds a member through the real insert path but skips the
// read-back, which selects columns a not-yet-migrated schema lacks.
func legacyParticipant(t *testing.T, s *Store, roomID, name, avatar string, human bool, hash []byte, ownerID, userID *string) Participant {
	t.Helper()
	p, err := s.insertParticipant(context.Background(), roomID, name, avatar, "", human, hash, ownerID, userID, "")
	if err != nil {
		t.Fatalf("legacy participant %s: %v", name, err)
	}
	return p
}

func legacyRoom(t *testing.T, s *Store, name, slug string) Room {
	t.Helper()
	var r Room
	var secret string
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO rooms (name, slug, secret) VALUES ($1, $2, $3) RETURNING id, slug, secret, name, created_by_user_id, created_at`,
		name, slug, secrets.InviteCode(),
	).Scan(&r.ID, &r.Slug, &secret, &r.Name, &r.CreatedByUserID, &r.CreatedAt)
	if err != nil {
		t.Fatalf("legacy room %s: %v", name, err)
	}
	return r
}
