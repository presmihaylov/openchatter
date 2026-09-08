package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/presmihaylov/openchatter/pkg/slug"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
)

// sessionRoom registers a user and creates a room with the session; the
// returned client already names the room with X-Workspace-Slug.
// roomBody: a create body with a slug unique across test runs, since the
// derived slug of a repeated name would collide on the shared dev db
func roomBody(name string) map[string]any {
	return map[string]any{"name": name, "slug": slug.From(name) + "-" + uniqUser()}
}

func sessionRoom(t *testing.T, base, name string) (creator *testClient, user, room map[string]any) {
	t.Helper()
	creator, reg := register(t, base, uniqUser(), "correct horse")
	out := creator.must("POST", "/api/v1/rooms", roomBody(name), 201)
	room = out["room"].(map[string]any)
	room["invite"] = out["invite"]
	creator.slug = room["slug"].(string)
	return creator, reg["user"].(map[string]any), room
}

// registerAs is register with a chosen display name, so a test can control
// the participant name the human gets on /enter.
func registerAs(t *testing.T, base, displayName string) (*testClient, map[string]any) {
	t.Helper()
	c := &testClient{t: t, base: base}
	out := c.must("POST", "/api/v1/auth/password/register", map[string]any{
		"username": uniqUser(), "password": "correct horse", "display_name": displayName,
	}, 201)
	c.token = out["token"].(string)
	return c, out
}

func TestRoomCreateRequiresSession(t *testing.T) {
	srv, _ := newTestServer(t)
	anon := &testClient{t: t, base: srv.URL}
	if st, out := anon.do("POST", "/api/v1/rooms", roomBody("nope")); st != 401 || out["code"] != "session_required" {
		t.Fatalf("anonymous create: %d %v", st, out)
	}
	_, alice, _ := setupRoom(t, srv.URL)
	if st, out := alice.do("POST", "/api/v1/rooms", roomBody("nope")); st != 401 || out["code"] != "session_required" {
		t.Fatalf("act_ create: %d %v", st, out)
	}

	creator, user, room := sessionRoom(t, srv.URL, "mine")
	if room["created_by_user_id"] != user["id"] {
		t.Fatalf("created_by_user_id: %v", room)
	}
	me := creator.must("GET", "/api/v1/me", nil, 200)
	if me["role"] != "admin" || me["is_human"] != true || me["user_id"] != user["id"] || me["username"] != user["username"] {
		t.Fatalf("creator participant: %v", me)
	}
	if me["name"] != "Test Person" {
		t.Fatalf("creator name should be the display name: %v", me["name"])
	}
	// #general membership and the roster carry the linked human
	overview := creator.must("GET", "/api/v1/room", nil, 200)
	parts := overview["participants"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["user_id"] != user["id"] {
		t.Fatalf("participants: %v", parts)
	}
	if overview["admin"] != true {
		t.Fatalf("admin creator must see the admin flag: %v", overview["admin"])
	}
	if !strings.HasPrefix(room["invite"].(string), "http://public.test/join/inv-") {
		t.Fatalf("create must return an invite link: %v", room["invite"])
	}
	chans := creator.must("GET", "/api/v1/channels", nil, 200)["channels"].([]any)
	if len(chans) != 1 || chans[0].(map[string]any)["name"] != "general" {
		t.Fatalf("channels: %v", chans)
	}
	// the join event names the user; the switcher hint points at the new room
	var payload map[string]any
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT payload FROM events WHERE room_id = $1 AND type = 'participant.joined'`, room["id"]).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["user_id"] != user["id"] {
		t.Fatalf("participant.joined payload: %v", payload)
	}
	acct := creator.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)
	if acct["last_active_workspace_id"] != room["id"] {
		t.Fatalf("last_active_workspace_id: %v", acct)
	}
}

func TestRoomCreateInvalidDisplayNameUsesUsername(t *testing.T) {
	srv, _ := newTestServer(t)
	name := uniqUser()
	c := &testClient{t: t, base: srv.URL}
	out := c.must("POST", "/api/v1/auth/password/register", map[string]any{
		"username": name, "password": "correct horse",
		"display_name": "a display name that is far too long to be a participant name 🎉",
	}, 201)
	c.token = out["token"].(string)
	room := c.must("POST", "/api/v1/rooms", roomBody("long"), 201)["room"].(map[string]any)
	c.slug = room["slug"].(string)
	if me := c.must("GET", "/api/v1/me", nil, 200); me["name"] != name {
		t.Fatalf("want participant name %q, got %v", name, me["name"])
	}
}

func TestRoomQuota(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := register(t, srv.URL, uniqUser(), "correct horse")
	for i := 0; i < models.RoomQuota; i++ {
		c.must("POST", "/api/v1/rooms", roomBody("room"), 201)
	}
	if st, out := c.do("POST", "/api/v1/rooms", roomBody("one too many")); st != 409 || out["code"] != "workspace_quota" {
		t.Fatalf("sixth create: %d %v", st, out)
	}
	// the cap is per creator, not global
	other, _ := register(t, srv.URL, uniqUser(), "correct horse")
	other.must("POST", "/api/v1/rooms", roomBody("room"), 201)
}

// participantsColumns is the participants table before task 03 plus user_id.
var participantsColumns = []string{
	"archive_after_secs", "avatar", "avatar_attachment_id", "created_at", "declared_offline",
	"default_section_collapsed", "description", "id", "is_human",
	"last_seen_at", "name", "notify_enabled", "notify_sound", "offline_since_seq", "owner_id", "presence_online", "revoked",
	"role", "room_id", "token_hash", "user_id",
}

func TestAgentJoinRowUnchanged(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	db := testDB(t)
	out := createRoom(t, srv.URL, "agents")
	code := out["invite"].(string)
	roomID := out["room"].(map[string]any)["id"].(string)

	c := &testClient{t: t, base: srv.URL}
	joined := c.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": code, "name": "worker", "description": "does things",
	}, 201)
	token := joined["token"].(string)
	pid := joined["participant"].(map[string]any)["id"].(string)

	rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'participants'`)
	if err != nil {
		t.Fatal(err)
	}
	cols := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	sort.Strings(cols)
	if strings.Join(cols, ",") != strings.Join(participantsColumns, ",") {
		t.Fatalf("participants columns changed:\n got %v\nwant %v", cols, participantsColumns)
	}

	var row map[string]any
	if err := db.QueryRow(ctx,
		`SELECT to_jsonb(p) - 'id' - 'room_id' - 'created_at' - 'last_seen_at' || jsonb_build_object('token_hex', encode(token_hash, 'hex'))
		 FROM participants p WHERE id = $1`, pid).Scan(&row); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	want := map[string]any{
		"archive_after_secs": float64(3600), "avatar": "🌱", "avatar_attachment_id": nil, "declared_offline": false,
		"default_section_collapsed": false, "description": "does things",
		"is_human": false, "name": "worker", "notify_enabled": true, "notify_sound": true, "offline_since_seq": nil, "owner_id": nil,
		"presence_online": true, "revoked": false, "role": "admin", "user_id": nil,
		"token_hash": row["token_hash"], "token_hex": hex.EncodeToString(sum[:]),
	}
	got, _ := json.Marshal(row)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("agent row drifted:\n got %s\nwant %s", got, exp)
	}

	// the join event carries exactly the keys it always did
	var payload map[string]any
	if err := db.QueryRow(ctx,
		`SELECT payload FROM events WHERE room_id = $1 AND type = 'participant.joined' AND payload->>'participant_id' = $2`,
		roomID, pid).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "description,is_human,name,owner_id,participant_id,role" {
		t.Fatalf("participant.joined keys for an agent: %v", keys)
	}
}

func TestJoinCannotDuplicateLinkedHuman(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "linked")
	me := creator.must("GET", "/api/v1/me", nil, 200)
	c := &testClient{t: t, base: srv.URL}
	st, out := c.do("POST", "/api/v1/rooms/join", map[string]any{
		"invite": room["invite"], "name": me["name"], "is_human": true,
	})
	if st != 409 || !strings.Contains(out["error"].(string), "taken") {
		t.Fatalf("duplicate linked human: %d %v", st, out)
	}
	// the row is untouched and still the user's
	after := creator.must("GET", "/api/v1/me", nil, 200)
	if after["id"] != me["id"] || after["role"] != "admin" || after["user_id"] != me["user_id"] {
		t.Fatalf("linked row changed: %v", after)
	}
}

func TestSessionAuthResolvesParticipant(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, user, room := sessionRoom(t, srv.URL, "first")
	me := creator.must("GET", "/api/v1/me", nil, 200)
	if me["user_id"] != user["id"] || me["username"] != user["username"] || me["room_id"] != room["id"] {
		t.Fatalf("me: %v", me)
	}
	// the session touches presence like an agent request does
	if me["online"] != true {
		t.Fatalf("session participant should be online: %v", me)
	}

	noSlug := &testClient{t: t, base: srv.URL, token: creator.token}
	if st, out := noSlug.do("GET", "/api/v1/me", nil); st != 400 || out["code"] != "workspace_required" {
		t.Fatalf("missing slug: %d %v", st, out)
	}
	noSlug.slug = "no-such-room"
	if st, _ := noSlug.do("GET", "/api/v1/me", nil); st != 404 {
		t.Fatalf("unknown slug: %d", st)
	}
	noSlug.slug = room["slug"].(string)
	noSlug.token = "ses_notarealsession"
	if st, out := noSlug.do("GET", "/api/v1/me", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("bad session: %d %v", st, out)
	}

	// opening a second workspace moves the last-active pointer
	second := creator.must("POST", "/api/v1/rooms", roomBody("second"), 201)["room"].(map[string]any)
	creator.slug = second["slug"].(string)
	creator.must("GET", "/api/v1/me", nil, 200)
	if got := creator.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)["last_active_workspace_id"]; got != second["id"] {
		t.Fatalf("last_active_workspace_id after switch: %v", got)
	}
	creator.slug = room["slug"].(string)
	creator.must("GET", "/api/v1/me", nil, 200)
	if got := creator.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)["last_active_workspace_id"]; got != room["id"] {
		t.Fatalf("last_active_workspace_id after switch back: %v", got)
	}
}

func TestSessionWithoutParticipantIs403(t *testing.T) {
	srv, _ := newTestServer(t)
	_, _, room := sessionRoom(t, srv.URL, "theirs")
	stranger, _ := register(t, srv.URL, uniqUser(), "correct horse")
	stranger.slug = room["slug"].(string)
	st, out := stranger.do("GET", "/api/v1/me", nil)
	if st != 403 || out["code"] != "workspace_forbidden" || out["reason"] != "not_member" {
		t.Fatalf("non-member: %d %v", st, out)
	}
	// a session on a room route never falls through to the act_ path
	if st, _ := stranger.do("POST", "/api/v1/channels/general/messages", map[string]any{"body": "hi"}); st != 403 {
		t.Fatalf("non-member post: %d", st)
	}
}

func TestRevokedHumanIs403FromEnterAndRoom(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "kicked")
	member, _ := register(t, srv.URL, uniqUser(), "correct horse")
	member.slug = room["slug"].(string)
	entered := member.must("POST", "/api/v1/workspaces/"+member.slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
	pid := entered["participant"].(map[string]any)["id"].(string)
	member.must("GET", "/api/v1/me", nil, 200)

	creator.must("DELETE", "/api/v1/participants/"+pid, nil, 200)
	st, out := member.do("GET", "/api/v1/me", nil)
	if st != 403 || out["code"] != "workspace_forbidden" || out["reason"] != "revoked" {
		t.Fatalf("revoked on a room route: %d %v", st, out)
	}
	// a valid code does not reopen the door: the kick is sticky
	st, out = member.do("POST", "/api/v1/workspaces/"+member.slug+"/enter", map[string]any{"invite": room["invite"]})
	if st != 403 || out["code"] != "workspace_forbidden" || out["reason"] != "revoked" {
		t.Fatalf("revoked on enter: %d %v", st, out)
	}
}

func TestEnterWithInviteCodeCreatesLinkedParticipant(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "open")
	slug := room["slug"].(string)

	member, reg := registerAs(t, srv.URL, "Newcomer")
	member.slug = slug
	out := member.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
	p := out["participant"].(map[string]any)
	userID := reg["user"].(map[string]any)["id"]
	if p["user_id"] != userID || p["is_human"] != true || p["role"] != "member" || p["name"] != "Newcomer" {
		t.Fatalf("entered participant: %v", p)
	}
	if _, has := out["room"].(map[string]any)["invite"]; has {
		t.Fatalf("enter must not echo the invite code: %v", out["room"])
	}
	// idempotent for a live member, no code needed
	again := member.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{}, 200)
	if again["participant"].(map[string]any)["id"] != p["id"] {
		t.Fatalf("second enter made a new row: %v", again)
	}
	if me := member.must("GET", "/api/v1/me", nil, 200); me["id"] != p["id"] || me["user_id"] != userID {
		t.Fatalf("me after enter: %v", me)
	}
	var payload map[string]any
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT payload FROM events WHERE room_id = $1 AND type = 'participant.joined' AND payload->>'participant_id' = $2`,
		room["id"], p["id"]).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["user_id"] != userID {
		t.Fatalf("participant.joined for /enter: %v", payload)
	}

	// a bound link opens the room but binds no human to its issuer, as /join
	// does (humans are their own principal); a taken display name gets the
	// -2 suffix. A plain human member can only mint a bound link, and a bound
	// link admits no human: it is an agent's key, never a way to bring in a stranger.
	member.must("POST", "/api/v1/invites", nil, 403)
	inv := creator.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)
	third, _ := registerAs(t, srv.URL, "Newcomer")
	third.slug = slug
	if st, out := third.do("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": inv["join_url"]}); st != 403 || out["code"] != "invite_agents_only" {
		t.Fatalf("human enter on a bound link: %d %v", st, out)
	}
	tp := third.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)["participant"].(map[string]any)
	if _, has := tp["owner_id"]; has || tp["name"] != "Newcomer-2" {
		t.Fatalf("plain enter: %v", tp)
	}
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT payload FROM events WHERE room_id = $1 AND type = 'participant.joined' AND payload->>'participant_id' = $2`,
		room["id"], tp["id"]).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["owner_id"] != nil {
		t.Fatalf("participant.joined for an owner-scoped /enter: %v", payload)
	}
}

func TestEnterDoesNotAdoptByName(t *testing.T) {
	srv, store := newTestServer(t)
	_, _, room := sessionRoom(t, srv.URL, "names")
	slug := room["slug"].(string)
	// an unlinked human row with the newcomer's display name, offline
	c := &testClient{t: t, base: srv.URL}
	legacy := c.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": room["invite"], "name": "Newcomer", "is_human": true,
	}, 201)["participant"].(map[string]any)
	if err := store.BackdateSeen(context.Background(), legacy["id"].(string)); err != nil {
		t.Fatal(err)
	}

	member, reg := registerAs(t, srv.URL, "Newcomer")
	member.slug = slug
	p := member.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)["participant"].(map[string]any)
	if p["id"] == legacy["id"] || p["name"] != "Newcomer-2" || p["user_id"] != reg["user"].(map[string]any)["id"] {
		t.Fatalf("enter adopted or misnamed: %v", p)
	}
	old, err := store.ParticipantByID(context.Background(), room["id"].(string), legacy["id"].(string))
	if err != nil || old.UserID != nil || old.Name != "Newcomer" {
		t.Fatalf("legacy row changed: %+v %v", old, err)
	}
}

func TestEnterWrongCodeIs400(t *testing.T) {
	srv, _ := newTestServer(t)
	_, _, room := sessionRoom(t, srv.URL, "target")
	_, _, other := sessionRoom(t, srv.URL, "elsewhere")
	slug := room["slug"].(string)
	member, _ := register(t, srv.URL, uniqUser(), "correct horse")
	for _, code := range []any{"inv-not-a-code", other["invite"], ""} {
		st, out := member.do("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": code})
		if st != 400 || out["code"] != "invite_invalid" {
			t.Fatalf("enter with %q: %d %v", code, st, out)
		}
	}
	if st, _ := member.do("POST", "/api/v1/workspaces/no-such-room/enter", map[string]any{"invite": room["invite"]}); st != 404 {
		t.Fatalf("unknown slug: %d", st)
	}
	member.slug = slug
	if st, out := member.do("GET", "/api/v1/me", nil); st != 403 || out["reason"] != "not_member" {
		t.Fatalf("a failed enter must not create a row: %d %v", st, out)
	}
}

// GET /api/v1/user lists live memberships in join order and points at the
// last-active workspace only while the user is still a live member there.
// Names and room creation both run against the join order, so a sort by
// name, slug or room age would fail the order assertion below.
func TestUserRoomsListsLiveParticipations(t *testing.T) {
	srv, _ := newTestServer(t)
	adminOfSecond, _, second := sessionRoom(t, srv.URL, "alpha")
	adminOfThird, _, third := sessionRoom(t, srv.URL, "mike")
	_, _, first := sessionRoom(t, srv.URL, "zulu")
	userID := ""

	user, reg := register(t, srv.URL, uniqUser(), "correct horse")
	userID = reg["user"].(map[string]any)["id"].(string)
	out := user.must("GET", "/api/v1/user", nil, 200)
	if ws := out["workspaces"].([]any); len(ws) != 0 {
		t.Fatalf("fresh user should have no workspaces: %v", ws)
	}
	if _, has := out["last_active_workspace_id"]; has {
		t.Fatalf("fresh user should have no last_active: %v", out)
	}

	for _, room := range []map[string]any{first, third, second} {
		user.must("POST", "/api/v1/workspaces/"+room["slug"].(string)+"/enter", map[string]any{"invite": room["invite"]}, 200)
	}
	// open first, then second: last_active follows the most recent room route
	user.slug = first["slug"].(string)
	user.must("GET", "/api/v1/me", nil, 200)
	user.slug = second["slug"].(string)
	user.must("GET", "/api/v1/me", nil, 200)
	kickUser(t, adminOfThird, userID)

	out = user.must("GET", "/api/v1/user", nil, 200)
	ws := out["workspaces"].([]any)
	if len(ws) != 2 {
		t.Fatalf("expected the two live rooms, got %v", ws)
	}
	for i, want := range []map[string]any{first, second} {
		got := ws[i].(map[string]any)
		if got["id"] != want["id"] || got["slug"] != want["slug"] || got["name"] != want["name"] || got["role"] != "member" {
			t.Fatalf("workspace %d: got %v want %v", i, got, want)
		}
		if _, ok := got["joined_at"].(string); !ok {
			t.Fatalf("workspace %d: joined_at missing: %v", i, got)
		}
	}
	if out["last_active_workspace_id"] != second["id"] {
		t.Fatalf("last_active should be second: %v", out["last_active_workspace_id"])
	}
	if out["user"].(map[string]any)["last_active_workspace_id"] != second["id"] {
		t.Fatalf("user.last_active should be second: %v", out["user"])
	}

	// open third: the pointer moves nowhere (revoked), and the hint stays hidden
	// once the user is kicked from the room it points to
	user.slug = third["slug"].(string)
	if st, _ := user.do("GET", "/api/v1/me", nil); st != 403 {
		t.Fatalf("revoked me: %d", st)
	}
	out = user.must("GET", "/api/v1/user", nil, 200)
	if out["last_active_workspace_id"] != second["id"] {
		t.Fatalf("last_active after a revoked open: %v", out["last_active_workspace_id"])
	}
	kickUser(t, adminOfSecond, userID)
	out = user.must("GET", "/api/v1/user", nil, 200)
	if _, has := out["last_active_workspace_id"]; has {
		t.Fatalf("last_active must vanish with the membership: %v", out)
	}
	if _, has := out["user"].(map[string]any)["last_active_workspace_id"]; has {
		t.Fatalf("user.last_active must vanish with the membership: %v", out)
	}
	if ws := out["workspaces"].([]any); len(ws) != 1 || ws[0].(map[string]any)["id"] != first["id"] {
		t.Fatalf("only first should remain: %v", ws)
	}
}

// kickUser revokes userID's row through an admin client already scoped to the room.
func kickUser(t *testing.T, admin *testClient, userID string) {
	t.Helper()
	parts := admin.must("GET", "/api/v1/participants", nil, 200)["participants"].([]any)
	for _, p := range parts {
		if p.(map[string]any)["user_id"] == userID {
			admin.must("DELETE", "/api/v1/participants/"+p.(map[string]any)["id"].(string), nil, 200)
			return
		}
	}
	t.Fatalf("user row missing in %s: %v", admin.slug, parts)
}

// Agents and anonymous callers have no account behind GET /api/v1/user.
func TestUserRouteRequiresSession(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	if st, out := alice.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_required" {
		t.Fatalf("act_ on /user: %d %v", st, out)
	}
	anon := &testClient{t: t, base: srv.URL}
	if st, out := anon.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_required" {
		t.Fatalf("anon on /user: %d %v", st, out)
	}
}

// Design section 10: a linked human shows user_id and username on every
// listing an agent reads, and on /me even over a legacy act_ token.
func TestLinkedHumanCarriesUsernameOnLists(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, user, room := sessionRoom(t, srv.URL, "linked")
	me := creator.must("GET", "/api/v1/me", nil, 200)

	agent := &testClient{t: t, base: srv.URL}
	joined := agent.must("POST", "/api/v1/rooms/join", map[string]any{"invite": room["invite"], "name": "worker"}, 201)
	agent.token = joined["token"].(string)

	find := func(list []any, id any) map[string]any {
		for _, it := range list {
			m := it.(map[string]any)
			if m["id"] == id {
				return m
			}
		}
		t.Fatalf("participant %v not listed in %v", id, list)
		return nil
	}
	human := find(agent.must("GET", "/api/v1/participants", nil, 200)["participants"].([]any), me["id"])
	if human["user_id"] != user["id"] || human["username"] != user["username"] {
		t.Fatalf("/participants linked human: %v", human)
	}
	worker := find(agent.must("GET", "/api/v1/participants", nil, 200)["participants"].([]any), joined["participant"].(map[string]any)["id"])
	if _, has := worker["username"]; has {
		t.Fatalf("agent row must not carry username: %v", worker)
	}
	member := find(agent.must("GET", "/api/v1/members", nil, 200)["members"].([]any), me["id"])
	if member["user_id"] != user["id"] || member["username"] != user["username"] {
		t.Fatalf("/members linked human: %v", member)
	}

	// the same human over a legacy act_ token (valid until task 08)
	tok, hash := secrets.NewToken()
	if _, err := testDB(t).Exec(context.Background(), `UPDATE participants SET token_hash = $1 WHERE id = $2`, hash, me["id"]); err != nil {
		t.Fatal(err)
	}
	legacy := &testClient{t: t, base: srv.URL, token: tok}
	if got := legacy.must("GET", "/api/v1/me", nil, 200); got["user_id"] != user["id"] || got["username"] != user["username"] {
		t.Fatalf("/me over act_: %v", got)
	}
}

// Task 11: a workspace has an image (admin-set) or initials on a stable colour.
func TestRoomAvatar(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "Acme Research")
	member, _ := registerAs(t, srv.URL, "Mem")
	member.slug = creator.slug
	member.must("POST", "/api/v1/workspaces/"+creator.slug+"/enter", map[string]any{"invite": room["invite"]}, 200)

	// every room has a colour slot from birth, and the switcher payload carries it
	color, ok := room["color"].(float64)
	if !ok || color < 0 || color >= models.RoomColorSlots {
		t.Fatalf("room color: %v", room["color"])
	}
	wsList := func(c *testClient) map[string]any {
		for _, raw := range c.must("GET", "/api/v1/user", nil, 200)["workspaces"].([]any) {
			ws := raw.(map[string]any)
			if ws["slug"] == creator.slug {
				return ws
			}
		}
		t.Fatalf("workspace %s missing from /user", creator.slug)
		return nil
	}
	if ws := wsList(member); ws["color"] != color || ws["avatar_url"] != nil {
		t.Fatalf("fresh workspace in /user: %v", ws)
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	// members cannot set the image
	if st, out := postAvatarTo(t, srv.URL, "/api/v1/room/avatar", member.token, member.slug, png); st != 403 {
		t.Fatalf("member upload: %d %v, want 403", st, out)
	}
	if st, out := member.do("DELETE", "/api/v1/room/avatar", nil); st != 403 {
		t.Fatalf("member remove: %d %v, want 403", st, out)
	}
	// a non-image is refused even for the admin
	if st, out := postAvatarTo(t, srv.URL, "/api/v1/room/avatar", creator.token, creator.slug, []byte("nope")); st != 400 {
		t.Fatalf("text upload: %d %v, want 400", st, out)
	}

	st, out := postAvatarTo(t, srv.URL, "/api/v1/room/avatar", creator.token, creator.slug, png)
	if st != 200 {
		t.Fatalf("admin upload: %d %v", st, out)
	}
	attID, _ := out["avatar_attachment_id"].(string)
	if attID == "" || out["avatar_url"] != "/api/v1/attachments/"+attID {
		t.Fatalf("upload response: %v", out)
	}
	// members see it on /room and in their workspace list, and can fetch the image
	got := member.must("GET", "/api/v1/room", nil, 200)["room"].(map[string]any)
	if got["avatar_url"] != "/api/v1/attachments/"+attID || got["color"] != color {
		t.Fatalf("member /room after upload: %v", got)
	}
	if ws := wsList(member); ws["avatar_url"] != "/api/v1/attachments/"+attID {
		t.Fatalf("member /user after upload: %v", ws)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/attachments/"+attID, nil)
	req.Header.Set("Authorization", "Bearer "+member.token)
	req.Header.Set("X-Workspace-Slug", member.slug)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("member image fetch: %d", resp.StatusCode)
	}

	// remove brings the initials back; the colour never moves
	out = creator.must("DELETE", "/api/v1/room/avatar", nil, 200)
	if _, has := out["avatar_attachment_id"]; has || out["color"] != color {
		t.Fatalf("after remove: %v", out)
	}
	if ws := wsList(member); ws["avatar_url"] != nil || ws["color"] != color {
		t.Fatalf("member /user after remove: %v", ws)
	}
}

// The switcher list rolls up each workspace's channel badges for the caller:
// a message from someone else in room B marks B unread for A, an @mention
// counts, and marking the channel read clears both.
func TestUserWorkspacesUnreadAndMentions(t *testing.T) {
	srv, _ := newTestServer(t)
	ann, _ := registerAs(t, srv.URL, "annie")
	ann.must("POST", "/api/v1/rooms", roomBody("room a"), 201)
	bob, _, roomB := sessionRoom(t, srv.URL, "room b")
	annB := &testClient{t: t, base: srv.URL, token: ann.token, slug: roomB["slug"].(string)}
	annB.must("POST", "/api/v1/workspaces/"+annB.slug+"/enter", map[string]any{"invite": roomB["invite"]}, 200)

	state := func() (unread bool, mentions float64) {
		t.Helper()
		for _, w := range ann.must("GET", "/api/v1/user", nil, 200)["workspaces"].([]any) {
			ws := w.(map[string]any)
			if ws["slug"] == annB.slug {
				return ws["unread"].(bool), ws["mentions"].(float64)
			}
		}
		t.Fatalf("room b missing from /user")
		return
	}
	if u, m := state(); u || m != 0 {
		t.Fatalf("fresh member: unread=%v mentions=%v", u, m)
	}
	// ann's own post is not unread for her
	annB.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "hello"}, 201)
	if u, m := state(); u || m != 0 {
		t.Fatalf("own post: unread=%v mentions=%v", u, m)
	}
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "plain news"}, 201)
	if u, m := state(); !u || m != 0 {
		t.Fatalf("after a plain post: unread=%v mentions=%v", u, m)
	}
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "hey @annie look"}, 201)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel all hands"}, 201)
	if u, m := state(); !u || m != 2 {
		t.Fatalf("after a mention and a broadcast: unread=%v mentions=%v", u, m)
	}
	annB.must("POST", "/api/v1/channels/general/read", nil, 200)
	if u, m := state(); u || m != 0 {
		t.Fatalf("after read: unread=%v mentions=%v", u, m)
	}
	// a muted channel only counts through its mentions, like the sidebar
	annB.must("POST", "/api/v1/channels/general/mute", map[string]any{"muted": true}, 200)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "muted chatter"}, 201)
	if u, m := state(); u || m != 0 {
		t.Fatalf("muted plain post: unread=%v mentions=%v", u, m)
	}
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@annie even muted"}, 201)
	if u, m := state(); !u || m != 1 {
		t.Fatalf("muted mention: unread=%v mentions=%v", u, m)
	}
}

// Delete workspace: the owner types the name and the room goes with every
// row under it; a non-owner admin and an agent are refused; the agent's
// token dies with its participant row.
func TestDeleteWorkspace(t *testing.T) {
	srv, store := newTestServer(t)
	owner, user, room := sessionRoom(t, srv.URL, "doomed")
	slug := room["slug"].(string)
	other, _ := register(t, srv.URL, uniqUser(), "correct horse")
	other.slug = slug
	entered := other.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
	otherPID := entered["participant"].(map[string]any)["id"].(string)
	owner.must("POST", "/api/v1/participants/"+otherPID+"/role", map[string]any{"role": "admin"}, 200)
	agent := &testClient{t: t, base: srv.URL}
	joined := agent.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": room["invite"], "name": "bot", "description": "a test agent",
	}, 201)
	agent.token = joined["token"].(string)
	ownerPID := owner.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	att, err := store.CreateAttachment(context.Background(), room["id"].(string), ownerPID, "a.txt", "text/plain", []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}

	member, _ := register(t, srv.URL, uniqUser(), "correct horse")
	member.slug = slug
	member.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)

	if st, out := agent.do("DELETE", "/api/v1/room", map[string]any{"name": "doomed"}); st != 403 {
		t.Fatalf("agent delete: %d %v", st, out)
	}
	if st, out := member.do("DELETE", "/api/v1/room", map[string]any{"name": "doomed"}); st != 403 {
		t.Fatalf("plain member delete: %d %v", st, out)
	}
	if st, out := other.do("DELETE", "/api/v1/room", map[string]any{"name": "doomed"}); st != 403 || out["code"] != "owner_required" {
		t.Fatalf("non-owner admin delete: %d %v", st, out)
	}
	if st, out := owner.do("DELETE", "/api/v1/room", map[string]any{"name": "Doomed"}); st != 400 || out["code"] != "name_mismatch" {
		t.Fatalf("wrong name: %d %v", st, out)
	}
	out := owner.must("DELETE", "/api/v1/room", map[string]any{"name": "doomed"}, 200)
	if out["deleted"] != true || out["slug"] != slug {
		t.Fatalf("delete: %v", out)
	}

	if st, out := agent.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("agent after delete: %d %v", st, out)
	}
	// a coded 404 so open tabs of the deleted room route themselves home
	if st, out := owner.do("GET", "/api/v1/room", nil); st != 404 || out["code"] != "workspace_not_found" {
		t.Fatalf("owner after delete: %d %v", st, out)
	}
	if st, out := owner.do("GET", "/api/v1/events?since=0", nil); st != 404 || out["code"] != "workspace_not_found" {
		t.Fatalf("owner events after delete: %d %v", st, out)
	}
	for _, w := range owner.must("GET", "/api/v1/user", nil, 200)["workspaces"].([]any) {
		if w.(map[string]any)["slug"] == slug {
			t.Fatalf("deleted room still listed for %v", user["username"])
		}
	}
	if _, err := store.AttachmentByID(context.Background(), room["id"].(string), att.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("attachment after delete: %v", err)
	}
	if _, err := store.RoomByID(context.Background(), room["id"].(string)); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("room after delete: %v", err)
	}
}

func TestKickMembers(t *testing.T) {
	srv, _ := newTestServer(t)
	owner, _, room := sessionRoom(t, srv.URL, "crew")
	slug := room["slug"].(string)
	enter := func(name string) (*testClient, string) {
		c, _ := register(t, srv.URL, uniqUser(), "correct horse")
		c.slug = slug
		out := c.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
		return c, out["participant"].(map[string]any)["id"].(string)
	}
	admin, adminPID := enter("admin")
	owner.must("POST", "/api/v1/participants/"+adminPID+"/role", map[string]any{"role": "admin"}, 200)
	human, humanPID := enter("human")
	agent := &testClient{t: t, base: srv.URL}
	joined := agent.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": room["invite"], "name": "bot", "description": "a test agent",
	}, 201)
	agent.token = joined["token"].(string)
	agentPID := joined["participant"].(map[string]any)["id"].(string)
	ownerPID := owner.must("GET", "/api/v1/me", nil, 200)["id"].(string)

	// the owner is protected from admins and from themself
	if st, out := admin.do("DELETE", "/api/v1/participants/"+ownerPID, nil); st != 409 || out["code"] != "owner_protected" {
		t.Fatalf("admin removes owner: %d %v", st, out)
	}
	if st, out := owner.do("DELETE", "/api/v1/participants/me", nil); st != 409 || out["code"] != "owner_cannot_leave" {
		t.Fatalf("owner self-remove: %d %v", st, out)
	}
	if st, out := admin.do("POST", "/api/v1/participants/"+ownerPID+"/role", map[string]any{"role": "member"}); st != 403 || out["code"] != "owner_protected" {
		t.Fatalf("admin demotes owner: %d %v", st, out)
	}
	// a plain member cannot remove others, but may leave
	if st, out := human.do("DELETE", "/api/v1/participants/"+agentPID, nil); st != 403 {
		t.Fatalf("member removes agent: %d %v", st, out)
	}
	leaver, _ := enter("leaver")
	leaver.must("DELETE", "/api/v1/participants/me", nil, 200)
	if st, out := leaver.do("GET", "/api/v1/me", nil); st != 403 || out["reason"] != "revoked" {
		t.Fatalf("member after leave: %d %v", st, out)
	}

	// an admin removes the human: the login survives, the workspace is gone for them
	admin.must("DELETE", "/api/v1/participants/"+humanPID, nil, 200)
	if st, out := human.do("GET", "/api/v1/me", nil); st != 403 || out["code"] != "workspace_forbidden" || out["reason"] != "revoked" {
		t.Fatalf("kicked human on the room: %d %v", st, out)
	}
	for _, w := range human.must("GET", "/api/v1/user", nil, 200)["workspaces"].([]any) {
		if w.(map[string]any)["slug"] == slug {
			t.Fatalf("kicked human still lists the workspace")
		}
	}
	// and the agent: its token dies
	admin.must("DELETE", "/api/v1/participants/"+agentPID, nil, 200)
	if st, out := agent.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("kicked agent: %d %v", st, out)
	}
	for _, p := range owner.must("GET", "/api/v1/participants", nil, 200)["participants"].([]any) {
		if id := p.(map[string]any)["id"]; id == humanPID || id == agentPID {
			t.Fatalf("kicked row still listed: %v", id)
		}
	}
}

// An agent's token is its identity. A name alone can never adopt an existing
// row; only deleting the old agent frees that name for a brand-new identity.
func TestAgentTokenIdentityDeleteAndRecreate(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "token identity")
	slug := room["slug"].(string)

	enter := func(name string) (*testClient, string) {
		t.Helper()
		c, _ := registerAs(t, srv.URL, name)
		c.slug = slug
		out := c.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
		return c, out["participant"].(map[string]any)["id"].(string)
	}
	owner, ownerID := enter("Olive Owner")
	other, _ := enter("Nora Neighbor")
	bound := owner.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)["join_url"].(string)

	agent := &testClient{t: t, base: srv.URL}
	joined := agent.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": bound, "name": "worker", "description": "keeps one identity",
	}, 201)
	agent.token = joined["token"].(string)
	oldID := joined["participant"].(map[string]any)["id"].(string)
	if joined["participant"].(map[string]any)["owner_id"] != ownerID {
		t.Fatalf("bound owner: %v", joined["participant"])
	}
	oldMessage := agent.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "old identity spoke"}, 201)

	// Active or offline makes no difference: possession of an invite and the
	// name never replaces the token that owns the existing identity.
	agent.must("POST", "/api/v1/me/offline", nil, 200)
	if st, _ := (&testClient{t: t, base: srv.URL}).do("POST", "/api/v1/rooms/join", map[string]any{"invite": bound, "name": "worker"}); st != 409 {
		t.Fatalf("offline duplicate name joined: %d, want 409", st)
	}

	// A human manages exactly their own agents; admins keep their wider scope.
	if st, _ := other.do("DELETE", "/api/v1/participants/"+oldID, nil); st != 403 {
		t.Fatalf("non-owner deleted agent: %d, want 403", st)
	}
	owner.must("DELETE", "/api/v1/participants/"+oldID, nil, 200)
	if st, _ := agent.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("deleted agent token: %d, want 401", st)
	}
	var revoked, online bool
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT revoked, presence_online FROM participants WHERE id = $1`, oldID).Scan(&revoked, &online); err != nil {
		t.Fatal(err)
	}
	if !revoked || online {
		t.Fatalf("deleted row state: revoked=%v online=%v", revoked, online)
	}

	// The released name creates a new row and token; old messages still point
	// at the tombstoned row and retain its author name.
	fresh := (&testClient{t: t, base: srv.URL}).must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": bound, "name": "worker", "description": "new identity",
	}, 201)
	newID := fresh["participant"].(map[string]any)["id"].(string)
	if newID == oldID || fresh["token"] == joined["token"] {
		t.Fatalf("delete/recreate reused identity: old=%s new=%s", oldID, newID)
	}
	history := creator.must("GET", "/api/v1/messages/"+oldMessage["id"].(string), nil, 200)
	if history["author_id"] != oldID || history["author_name"] != "worker" || history["body"] != "old identity spoke" {
		t.Fatalf("old message lost attribution: %v", history)
	}

	creator.must("DELETE", "/api/v1/participants/"+newID, nil, 200)
	freshClient := &testClient{t: t, base: srv.URL, token: fresh["token"].(string)}
	if st, _ := freshClient.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("admin-deleted fresh token: %d, want 401", st)
	}
}

func TestCreateRoomSlug(t *testing.T) {
	srv, _ := newTestServer(t)
	c, _ := register(t, srv.URL, uniqUser(), "correct horse")
	tag := uniqUser()
	// derived from the name: folded, lowercased, hyphenated
	out := c.must("POST", "/api/v1/rooms", map[string]any{"name": "Café Crème " + tag}, 201)
	if got := out["room"].(map[string]any)["slug"]; got != "cafe-creme-"+tag {
		t.Fatalf("derived slug = %v", got)
	}
	// a taken slug is a 409 with its own code, no suffixing
	if st, out := c.do("POST", "/api/v1/rooms", map[string]any{"name": "cafe creme " + tag}); st != 409 || out["code"] != "slug_taken" {
		t.Fatalf("taken slug: %d %v", st, out)
	}
	// an explicit slug wins over the name
	out = c.must("POST", "/api/v1/rooms", map[string]any{"name": "Whatever", "slug": "custom-" + tag}, 201)
	if got := out["room"].(map[string]any)["slug"]; got != "custom-"+tag {
		t.Fatalf("custom slug = %v", got)
	}
	for _, bad := range []string{"Bad Slug", "-x", "a--b", "日本"} {
		if st, out := c.do("POST", "/api/v1/rooms", map[string]any{"name": "n", "slug": bad}); st != 400 || out["code"] != "slug_invalid" {
			t.Fatalf("slug %q: %d %v", bad, st, out)
		}
	}
	// a name with nothing to derive from is invalid too
	if st, out := c.do("POST", "/api/v1/rooms", map[string]any{"name": "日本"}); st != 400 || out["code"] != "slug_invalid" {
		t.Fatalf("underivable name: %d %v", st, out)
	}
}

// Task 18: the rail order and the workspace mute are per account, validated
// against live membership; the list carries unread_count and muted.
func TestWorkspaceOrderAndMute(t *testing.T) {
	srv, _ := newTestServer(t)
	adminA, _, roomA := sessionRoom(t, srv.URL, "order a")
	adminB, _, roomB := sessionRoom(t, srv.URL, "order b")
	_, _, roomC := sessionRoom(t, srv.URL, "order c")
	_, _, roomX := sessionRoom(t, srv.URL, "order x")

	user, _ := registerAs(t, srv.URL, "Ora Rail")
	other, _ := registerAs(t, srv.URL, "Otto Rail")
	for _, room := range []map[string]any{roomA, roomB, roomC} {
		user.must("POST", "/api/v1/workspaces/"+room["slug"].(string)+"/enter", map[string]any{"invite": room["invite"]}, 200)
		other.must("POST", "/api/v1/workspaces/"+room["slug"].(string)+"/enter", map[string]any{"invite": room["invite"]}, 200)
	}
	ids := func(out map[string]any) []string {
		var got []string
		for _, w := range out["workspaces"].([]any) {
			got = append(got, w.(map[string]any)["id"].(string))
		}
		return got
	}
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	a, b, c, x := roomA["id"].(string), roomB["id"].(string), roomC["id"].(string), roomX["id"].(string)
	if got := ids(user.must("GET", "/api/v1/user", nil, 200)); !same(got, []string{a, b, c}) {
		t.Fatalf("default order should be join order: %v", got)
	}

	// a room the user is not in, a bad id, a duplicate: all refused, order untouched
	if st, out := user.do("PATCH", "/api/v1/user/workspace-order", map[string]any{"order": []string{c, x}}); st != 403 || out["code"] != "not_a_member" {
		t.Fatalf("order with a foreign room: %d %v", st, out)
	}
	if st, _ := user.do("PATCH", "/api/v1/user/workspace-order", map[string]any{"order": []string{c, "nope"}}); st != 400 {
		t.Fatalf("order with a bad id: %d", st)
	}
	if st, _ := user.do("PATCH", "/api/v1/user/workspace-order", map[string]any{"order": []string{c, c}}); st != 400 {
		t.Fatalf("order with a duplicate: %d", st)
	}
	if got := ids(user.must("GET", "/api/v1/user", nil, 200)); !same(got, []string{a, b, c}) {
		t.Fatalf("a refused order must not change anything: %v", got)
	}

	// a full order sticks; a partial one places the listed rooms first
	out := user.must("PATCH", "/api/v1/user/workspace-order", map[string]any{"order": []string{c, a, b}}, 200)
	if got := ids(out); !same(got, []string{c, a, b}) {
		t.Fatalf("order response: %v", got)
	}
	if got := ids(user.must("GET", "/api/v1/user", nil, 200)); !same(got, []string{c, a, b}) {
		t.Fatalf("order did not persist: %v", got)
	}
	user.must("PATCH", "/api/v1/user/workspace-order", map[string]any{"order": []string{b}}, 200)
	if got := ids(user.must("GET", "/api/v1/user", nil, 200)); !same(got, []string{b, a, c}) {
		t.Fatalf("partial order: %v", got)
	}
	// the other member keeps the join order: the order is per account
	if got := ids(other.must("GET", "/api/v1/user", nil, 200)); !same(got, []string{a, b, c}) {
		t.Fatalf("another user's order moved: %v", got)
	}

	// unread_count is the plain unread total, mentions the @mention subset
	for i := 0; i < 3; i++ {
		adminA.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "news"}, 201)
	}
	adminA.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "hey @Ora Rail"}, 201)
	adminB.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "b news"}, 201)
	find := func(out map[string]any, id string) map[string]any {
		for _, w := range out["workspaces"].([]any) {
			if m := w.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("room %s missing from %v", id, out)
		return nil
	}
	out = user.must("GET", "/api/v1/user", nil, 200)
	if wa := find(out, a); wa["unread_count"] != 4.0 || wa["mentions"] != 1.0 || wa["unread"] != true || wa["muted"] != false {
		t.Fatalf("room a badge: %v", wa)
	}
	if wb := find(out, b); wb["unread_count"] != 1.0 || wb["mentions"] != 0.0 {
		t.Fatalf("room b badge: %v", wb)
	}
	if wc := find(out, c); wc["unread_count"] != 0.0 || wc["unread"] != false {
		t.Fatalf("room c badge: %v", wc)
	}

	// the mute is per account and per room; the count stays
	if st, out := user.do("PATCH", "/api/v1/user/workspaces/"+x, map[string]any{"muted": true}); st != 403 || out["code"] != "not_a_member" {
		t.Fatalf("mute a foreign room: %d %v", st, out)
	}
	if st, _ := user.do("PATCH", "/api/v1/user/workspaces/"+a, map[string]any{}); st != 400 {
		t.Fatalf("mute without a value: %d", st)
	}
	wa := user.must("PATCH", "/api/v1/user/workspaces/"+a, map[string]any{"muted": true}, 200)
	if wa["muted"] != true || wa["unread_count"] != 4.0 || wa["id"] != a {
		t.Fatalf("mute response: %v", wa)
	}
	out = user.must("GET", "/api/v1/user", nil, 200)
	if find(out, a)["muted"] != true || find(out, b)["muted"] != false {
		t.Fatalf("mute did not persist: %v", out)
	}
	if find(other.must("GET", "/api/v1/user", nil, 200), a)["muted"] != false {
		t.Fatal("another user's mute moved")
	}
	wa = user.must("PATCH", "/api/v1/user/workspaces/"+a, map[string]any{"muted": false}, 200)
	if wa["muted"] != false {
		t.Fatalf("unmute response: %v", wa)
	}
}

// TestAgentOwners: an agent belongs to a human (task 19). A plain-link join
// hands it to the creator, a bound link to the link's owner; removing the
// human revokes their agents at once (tokens 401, roster clean, one counted
// #general line); duplicate names do not adopt them; admins rebind an owner; the
// creator and the last admin can neither be removed nor leave.
func TestAgentOwners(t *testing.T) {
	srv, _ := newTestServer(t)
	creator, _, room := sessionRoom(t, srv.URL, "owned crew")
	slug := room["slug"].(string)
	creatorPID := creator.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	enter := func() (*testClient, string) {
		c, _ := register(t, srv.URL, uniqUser(), "correct horse")
		c.slug = slug
		out := c.must("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}, 200)
		return c, out["participant"].(map[string]any)["id"].(string)
	}
	joinAgent := func(link, name string) (*testClient, map[string]any) {
		c := &testClient{t: t, base: srv.URL}
		out := c.must("POST", "/api/v1/rooms/join", map[string]any{"invite": link, "name": name, "description": "t"}, 201)
		c.token = out["token"].(string)
		return c, out["participant"].(map[string]any)
	}

	// plain link: the creator owns the agent
	plainBot, plainP := joinAgent(room["invite"].(string), "plainbot")
	if plainP["owner_id"] != creatorPID {
		t.Fatalf("plain-link agent owner: %v want creator %s", plainP["owner_id"], creatorPID)
	}
	// bound link: the human who minted it
	omar, omarPID := enter()
	link := omar.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)["join_url"].(string)
	bot1, b1 := joinAgent(link, "reviewer")
	bot2, b2 := joinAgent(link, "opus")
	if b1["owner_id"] != omarPID || b2["owner_id"] != omarPID || b1["owner_user_id"] == nil {
		t.Fatalf("bound-link owners: %v %v", b1, b2)
	}

	// admin rebinds an owner: only to a live human with an account, only on an agent
	nina, ninaPID := enter()
	if st, out := omar.do("PATCH", "/api/v1/participants/"+b2["id"].(string)+"/owner", map[string]any{"owner_id": ninaPID}); st != 403 {
		t.Fatalf("member rebinds: %d %v", st, out)
	}
	if st, out := creator.do("PATCH", "/api/v1/participants/"+b2["id"].(string)+"/owner", map[string]any{"owner_id": plainP["id"]}); st != 400 || out["code"] != "bad_owner" {
		t.Fatalf("agent as owner: %d %v", st, out)
	}
	if st, out := creator.do("PATCH", "/api/v1/participants/"+ninaPID+"/owner", map[string]any{"owner_id": omarPID}); st != 400 {
		t.Fatalf("human target: %d %v", st, out)
	}
	moved := creator.must("PATCH", "/api/v1/participants/"+b2["id"].(string)+"/owner", map[string]any{"owner_id": ninaPID}, 200)["participant"].(map[string]any)
	if moved["owner_id"] != ninaPID {
		t.Fatalf("rebind: %v", moved)
	}
	_ = nina

	// removing omar takes reviewer with them, not opus (now nina's)
	creator.must("DELETE", "/api/v1/participants/"+omarPID, nil, 200)
	if st, _ := bot1.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("owned agent after owner removal: %d want 401", st)
	}
	if st, _ := bot2.do("GET", "/api/v1/me", nil); st != 200 {
		t.Fatalf("rebound agent after old owner removal: %d want 200", st)
	}
	if st, _ := plainBot.do("GET", "/api/v1/me", nil); st != 200 {
		t.Fatalf("creator's agent: %d want 200", st)
	}
	names := map[string]bool{}
	for _, p := range creator.must("GET", "/api/v1/participants", nil, 200)["participants"].([]any) {
		names[p.(map[string]any)["name"].(string)] = true
	}
	if names["reviewer"] || !names["opus"] || !names["plainbot"] {
		t.Fatalf("roster after cascade: %v", names)
	}
	general := creator.must("GET", "/api/v1/channels/general/messages?limit=5", nil, 200)["messages"].([]any)
	found := false
	for _, m := range general {
		if strings.Contains(m.(map[string]any)["body"].(string), "and 1 agent from the workspace") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no counted #general line: %v", general)
	}
	// A duplicate name on a plain link cannot transfer an existing agent to the
	// creator; the original token and owner remain the identity.
	keeperOwner, keeperOwnerPID := enter()
	keeperLink := keeperOwner.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)["join_url"].(string)
	_, kp := joinAgent(keeperLink, "keeper")
	rc := &testClient{t: t, base: srv.URL}
	if st, _ := rc.do("POST", "/api/v1/rooms/join", map[string]any{"invite": room["invite"], "name": "keeper", "description": "t"}); st != 409 {
		t.Fatalf("plain-link duplicate name: %d, want 409", st)
	}
	kept := creator.must("GET", "/api/v1/participants/"+kp["id"].(string), nil, 200)
	if kept["owner_id"] != keeperOwnerPID {
		t.Fatalf("duplicate join changed owner: %v want %s", kept["owner_id"], keeperOwnerPID)
	}
	// a removed human stays out, and so does their agent
	if st, out := omar.do("POST", "/api/v1/workspaces/"+slug+"/enter", map[string]any{"invite": room["invite"]}); st != 403 || out["reason"] != "revoked" {
		t.Fatalf("removed human re-enters: %d %v", st, out)
	}
	if st, _ := bot1.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("agent revived by rejoin attempt: %d", st)
	}
	// a second human leaves on their own: same cascade, "left ... and 1 agent"
	nina2, _ := enter()
	link2 := nina2.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)["join_url"].(string)
	bot3, _ := joinAgent(link2, "reviewer2")
	nina2.must("DELETE", "/api/v1/participants/me", nil, 200)
	if st, _ := bot3.do("GET", "/api/v1/me", nil); st != 401 {
		t.Fatalf("agent after owner left: %d", st)
	}

	// the creator (also the last admin here) can neither be removed nor leave,
	// and no agent token changes when it is tried
	if st, out := creator.do("DELETE", "/api/v1/participants/me", nil); st != 409 || out["code"] != "owner_cannot_leave" {
		t.Fatalf("creator leaves: %d %v", st, out)
	}
	admin, adminPID := enter()
	creator.must("POST", "/api/v1/participants/"+adminPID+"/role", map[string]any{"role": "admin"}, 200)
	if st, out := admin.do("DELETE", "/api/v1/participants/"+creatorPID, nil); st != 409 || out["code"] != "owner_protected" {
		t.Fatalf("admin removes creator: %d %v", st, out)
	}
	if st, _ := plainBot.do("GET", "/api/v1/me", nil); st != 200 {
		t.Fatalf("creator's agent after refused removals: %d", st)
	}
	// last admin: demote the creator is refused (owner_protected), so make the
	// only other admin leave and check the last-admin guard on a plain room
	admin.must("DELETE", "/api/v1/participants/me", nil, 200)
	if st, out := creator.do("DELETE", "/api/v1/participants/me", nil); st != 409 {
		t.Fatalf("last admin leaves: %d %v", st, out)
	}
}
