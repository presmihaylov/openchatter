package models

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/presmihaylov/openchatter/migrations"
	"github.com/presmihaylov/openchatter/pkg/secrets"
)

const backfillVersion = 26

// TestBackfillUsers drives 000026 over a schema-25 fixture: the design's
// section 7 cases plus the pre-linked rules (an operator linked `maya` in one
// room before the file ran).
func TestBackfillUsers(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	if got, err := MigrateTo(ctx, dbURL, backfillVersion-1); err != nil || got != backfillVersion-1 {
		t.Fatalf("MigrateTo %d: got %d %v", backfillVersion-1, got, err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	// seed at 25
	const mayaHash, samHash = "$2a$04$mayahashmayahashmayahash", "$2a$04$samhashsamhashsamhashsam"
	maya, err := s.CreatePasswordUser(ctx, "maya", "Maya", []byte(mayaHash))
	if err != nil {
		t.Fatal(err)
	}
	sam, err := s.CreatePasswordUser(ctx, "sam", "Sam", []byte(samHash))
	if err != nil {
		t.Fatal(err)
	}
	mkRoomSlug := func(name, slug string) Room { return legacyRoom(t, s, name, slug) }
	roomA, roomB, roomC := mkRoomSlug("alpha", "alpha-slug"), mkRoomSlug("beta", "beta-slug"), mkRoomSlug("gamma", "gamma-slug")
	human := func(roomID, name string, userID *string) Participant {
		_, hash := secrets.NewToken()
		return legacyParticipant(t, s, roomID, name, "🧑", true, hash, nil, userID)
	}
	mayaA := human(roomA.ID, "Maya", &maya.ID) // first joiner: admin, pre-linked
	maria := human(roomA.ID, "Maria Chen", nil)
	maria2 := human(roomA.ID, "maria chen", nil)
	mariaLit := human(roomA.ID, "maria-chen-2", nil) // literal '-2': keeps it, maria2 moves to '-3'
	eve := human(roomA.ID, "Eve", nil)
	samRow := human(roomA.ID, "Sam", nil)
	mayaUpper := human(roomA.ID, "MAYA", nil) // same-room clash with the pre-linked maya
	hanaA := human(roomA.ID, "Hana", nil)
	_, botHash := secrets.NewToken()
	bot := legacyParticipant(t, s, roomA.ID, "bot", "🤖", false, botHash, nil, nil)
	mayaB := human(roomB.ID, "Maya", nil) // first joiner: admin, unlinked
	hanaB := human(roomB.ID, "hana", nil)
	olgaB := human(roomB.ID, "Olga", nil)
	// room C creator rule: revoked admin first, member second, two live admins
	olgaC := human(roomC.ID, "Olga", nil) // first joiner: admin, revoked below
	mo := human(roomC.ID, "Mo", nil)
	ann := human(roomC.ID, "Ann", nil)
	ben := human(roomC.ID, "Ben", nil)
	for _, id := range []string{ann.ID, ben.ID} {
		if err := s.SetRole(ctx, roomC.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	// Store.Revoke needs the 000033 invites shape; the fixture only needs the flag
	for _, r := range []Participant{eve, olgaC} {
		if _, err := pool.Exec(ctx, `UPDATE participants SET revoked = true WHERE id = $1`, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	// mayaB and Maria Chen are the most recently seen of their pairs
	if _, err := conn.Exec(ctx, `UPDATE participants SET last_seen_at = now() - interval '1 day' WHERE id = ANY($1)`,
		[]string{mayaA.ID, maria2.ID}); err != nil {
		t.Fatal(err)
	}

	rowMD5 := func(id string) string {
		var sum string
		if err := conn.QueryRow(ctx, `SELECT md5(p::text) FROM participants p WHERE id = $1`, id).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	botBefore := rowMD5(bot.ID)

	if got, err := MigrateTo(ctx, dbURL, backfillVersion); err != nil || got != backfillVersion {
		t.Fatalf("MigrateTo %d: got %d %v", backfillVersion, got, err)
	}

	userOf := func(username string) (id, hash string, must bool, links int, lastRoom *string) {
		err := conn.QueryRow(ctx,
			`SELECT u.id, i.password_hash, u.must_change_password, u.last_active_room_id,
			        (SELECT count(*) FROM participants p WHERE p.user_id = u.id)
			 FROM users u JOIN user_identities i ON i.user_id = u.id AND i.provider = 'password'
			 WHERE u.username = $1`, username).Scan(&id, &hash, &must, &lastRoom, &links)
		if err != nil {
			t.Fatalf("user %s: %v", username, err)
		}
		return id, hash, must, links, lastRoom
	}
	userExists := func(username string) bool {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	linkOf := func(pid string) *string {
		var uid *string
		if err := conn.QueryRow(ctx, `SELECT user_id FROM participants WHERE id = $1`, pid).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	linkedTo := func(pid, userID string) {
		t.Helper()
		if uid := linkOf(pid); uid == nil || *uid != userID {
			t.Fatalf("participant %s linked to %v, want %s", pid, uid, userID)
		}
	}
	deref := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	// maya: pre-linked, keeps its hash, gains the room B link, no maya-2
	mayaID, hash, must, links, last := userOf("maya")
	if mayaID != maya.ID || hash != mayaHash || must || links != 2 {
		t.Fatalf("maya: id=%s hash=%q must=%v links=%d", mayaID, hash, must, links)
	}
	linkedTo(mayaA.ID, maya.ID)
	linkedTo(mayaB.ID, maya.ID)
	if userExists("maya-3") {
		t.Fatal("pre-linked maya must merge mayaB, not spawn another account")
	}
	if deref(last) != roomB.ID {
		t.Fatalf("maya last_active_room_id = %s, want room B (most recently seen)", deref(last))
	}

	// in-room dedupe: the live, most recently seen row keeps the plain name
	mariaID, hash, must, links, last := userOf("maria-chen")
	if !must || links != 1 || deref(last) != roomA.ID {
		t.Fatalf("maria-chen: must=%v links=%d last=%s", must, links, deref(last))
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("developer")); err != nil {
		t.Fatalf("maria-chen default password: %v", err)
	}
	linkedTo(maria.ID, mariaID)
	mariaLitID, _, _, _, _ := userOf("maria-chen-2")
	linkedTo(mariaLit.ID, mariaLitID)
	maria3ID, _, _, _, _ := userOf("maria-chen-3")
	linkedTo(maria2.ID, maria3ID)

	// cross-room merge of two unlinked rows: one user, both rows
	hanaID, _, _, links, _ := userOf("hana")
	if links != 2 {
		t.Fatalf("hana links = %d, want 2", links)
	}
	linkedTo(hanaA.ID, hanaID)
	linkedTo(hanaB.ID, hanaID)

	// same-room clash with the pre-linked maya: a fresh maya-2, not a merge
	maya2ID, _, _, links, _ := userOf("maya-2")
	if links != 1 {
		t.Fatalf("maya-2 links = %d, want 1", links)
	}
	linkedTo(mayaUpper.ID, maya2ID)

	// revoked row linked when its user exists (the kick stays sticky)
	olgaID, _, _, links, _ := userOf("olga")
	if links != 2 {
		t.Fatalf("olga links = %d, want 2", links)
	}
	linkedTo(olgaB.ID, olgaID)
	linkedTo(olgaC.ID, olgaID)

	// revoked-only human: no account, stays unlinked
	if userExists("eve") || linkOf(eve.ID) != nil {
		t.Fatal("eve must get no account and no link")
	}

	// squatter: keeps its hash and zero links; the legacy row gets sam-2
	samID, hash, must, links, _ := userOf("sam")
	if samID != sam.ID || hash != samHash || must || links != 0 {
		t.Fatalf("sam: id=%s hash=%q must=%v links=%d", samID, hash, must, links)
	}
	sam2ID, hash, must, _, _ := userOf("sam-2")
	if !must {
		t.Fatal("sam-2 must change its password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("developer")); err != nil {
		t.Fatalf("sam-2 default password: %v", err)
	}
	linkedTo(samRow.ID, sam2ID)

	// agents untouched, byte for byte
	if rowMD5(bot.ID) != botBefore {
		t.Fatal("agent row changed")
	}

	// creator = earliest live human admin, pre-linked or newly linked: not the
	// revoked olga (created first), not the member mo, not the later admin ben
	annID, _, _, _, _ := userOf("ann")
	for _, want := range []struct {
		room Room
		user string
	}{{roomA, maya.ID}, {roomB, maya.ID}, {roomC, annID}} {
		var creator *string
		if err := conn.QueryRow(ctx, `SELECT created_by_user_id FROM rooms WHERE id = $1`, want.room.ID).Scan(&creator); err != nil {
			t.Fatal(err)
		}
		if deref(creator) != want.user {
			t.Fatalf("room %s created_by_user_id = %s, want %s", want.room.Name, deref(creator), want.user)
		}
	}

	// idempotent: a second run of the file changes nothing
	checksum := func() string {
		var users, idents, parts, rooms string
		q := func(dst *string, sql string) {
			if err := conn.QueryRow(ctx, sql).Scan(dst); err != nil {
				t.Fatal(err)
			}
		}
		q(&users, `SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM (SELECT id, username, display_name, must_change_password, last_active_room_id FROM users) t`)
		q(&idents, `SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM (SELECT user_id, provider, subject, password_hash FROM user_identities) t`)
		q(&parts, `SELECT coalesce(md5(string_agg(p::text, '|' ORDER BY p::text)), '') FROM participants p`)
		q(&rooms, `SELECT coalesce(md5(string_agg(t::text, '|' ORDER BY t::text)), '') FROM (SELECT id, created_by_user_id FROM rooms) t`)
		return strings.Join([]string{users, idents, parts, rooms}, "/")
	}
	before := checksum()
	upSQL, err := migrations.FS.ReadFile("000026_backfill_users.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(upSQL)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if checksum() != before {
		t.Fatal("second run changed rows")
	}
	var backfilled int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM users_backfill_000026`).Scan(&backfilled); err != nil {
		t.Fatal(err)
	}
	if backfilled != 10 {
		t.Fatalf("backfill table holds %d users, want maria-chen, maria-chen-2, maria-chen-3, sam-2, hana, maya-2, olga, mo, ann, ben", backfilled)
	}

	// down: exactly the backfilled users go; maya and sam stay
	if got, err := MigrateTo(ctx, dbURL, backfillVersion-1); err != nil || got != backfillVersion-1 {
		t.Fatalf("MigrateTo %d: got %d %v", backfillVersion-1, got, err)
	}
	var usernames []string
	rows, err := conn.Query(ctx, `SELECT username FROM users ORDER BY username`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		usernames = append(usernames, u)
	}
	if strings.Join(usernames, ",") != "maya,sam" {
		t.Fatalf("users after down: %v", usernames)
	}
	_, hash, _, _, _ = userOf("maya")
	if hash != mayaHash {
		t.Fatal("down touched the pre-linked user's hash")
	}
	_, hash, _, _, _ = userOf("sam")
	if hash != samHash {
		t.Fatal("down touched the registered user's hash")
	}
	linkedTo(mayaA.ID, maya.ID)
	for _, pid := range []string{maria.ID, maria2.ID, mariaLit.ID, samRow.ID, eve.ID, mayaUpper.ID, hanaA.ID, hanaB.ID, olgaB.ID, olgaC.ID, mo.ID, ann.ID, ben.ID} {
		if uid := linkOf(pid); uid != nil {
			t.Fatalf("participant %s still linked to %s after down", pid, *uid)
		}
	}
	var reg *string
	if err := conn.QueryRow(ctx, `SELECT to_regclass('users_backfill_000026')::text`).Scan(&reg); err != nil || reg != nil {
		t.Fatalf("tracking table after down: %v %v", reg, err)
	}
	if rowMD5(bot.ID) != botBefore {
		t.Fatal("agent row changed by the down")
	}
}
