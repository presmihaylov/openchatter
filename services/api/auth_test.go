package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
	"github.com/presmihaylov/openchatter/services/auth"
)

var userSeq atomic.Int64

// uniqUser returns a username no earlier run of the suite can have taken:
// users persist across test runs in the shared dev database.
func uniqUser() string {
	return fmt.Sprintf("u%d-%d", time.Now().UnixNano()%1_000_000_000, userSeq.Add(1))
}

func register(t *testing.T, base, username, password string) (*testClient, map[string]any) {
	t.Helper()
	c := &testClient{t: t, base: base}
	out := c.must("POST", "/api/v1/auth/password/register", map[string]any{
		"username": username, "password": password, "display_name": "Test Person",
	}, 201)
	c.token = out["token"].(string)
	return c, out
}

func login(t *testing.T, base, username, password string) *testClient {
	t.Helper()
	c := &testClient{t: t, base: base}
	out := c.must("POST", "/api/v1/auth/password/login", map[string]any{"username": username, "password": password}, 200)
	c.token = out["token"].(string)
	return c
}

func TestAuthRegisterLoginLogout(t *testing.T) {
	srv, _ := newTestServer(t)
	anon := &testClient{t: t, base: srv.URL}

	provs := anon.must("GET", "/api/v1/auth/providers", nil, 200)
	if names := provs["providers"].([]any); len(names) != 1 || names[0] != "password" {
		t.Fatalf("providers: %v", provs)
	}
	if provs["registration_enabled"] != true {
		t.Fatalf("registration flag: %v", provs)
	}

	name := uniqUser()
	me, out := register(t, srv.URL, name, "correct horse")
	if !strings.HasPrefix(me.token, "ses_") {
		t.Fatalf("token prefix: %q", me.token)
	}
	user := out["user"].(map[string]any)
	if user["username"] != name || user["display_name"] != "Test Person" || user["must_change_password"] != false {
		t.Fatalf("user: %v", user)
	}
	if _, leaked := user["password_hash"]; leaked {
		t.Fatal("password hash in response")
	}
	if got := me.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)["id"]; got != user["id"] {
		t.Fatalf("GET /user: %v", got)
	}

	// registration validation
	if _, out := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": name, "password": "correct horse"}); out["code"] != "username_taken" {
		t.Fatalf("duplicate: %v", out)
	}
	if st, out := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": uniqUser(), "password": "short"}); st != 400 || out["code"] != "weak_password" {
		t.Fatalf("weak: %d %v", st, out)
	}
	if st, out := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": "Bad Name!", "password": "correct horse"}); st != 400 || out["code"] != "bad_username" {
		t.Fatalf("bad username: %d %v", st, out)
	}
	if st, _ := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": uniqUser(), "password": "correct horse", "extra": 1}); st != 400 {
		t.Fatalf("unknown field: %d", st)
	}

	// login
	if st, out := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": name, "password": "wrong horse"}); st != 401 || out["code"] != "invalid_credentials" {
		t.Fatalf("wrong password: %d %v", st, out)
	}
	if st, out := anon.do("POST", "/api/v1/auth/nope/login", map[string]any{}); st != 404 || out["code"] != "unknown_provider" {
		t.Fatalf("unknown provider: %d %v", st, out)
	}
	// usernames are case-insensitive on the way in
	second := login(t, srv.URL, strings.ToUpper(name), "correct horse")
	if second.token == me.token {
		t.Fatal("login reused a token")
	}

	// a session is a person: a room route needs to know which workspace
	if st, out := me.do("GET", "/api/v1/room", nil); st != 400 || out["code"] != "workspace_required" {
		t.Fatalf("session on room route without a slug: %d %v", st, out)
	}
	bogus := &testClient{t: t, base: srv.URL, token: "ses_" + strings.Repeat("x", 32)}
	if st, out := bogus.do("GET", "/api/v1/room", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("bogus session on room route: %d %v", st, out)
	}
	if st, out := bogus.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("bogus session: %d %v", st, out)
	}

	// logout kills exactly that session
	if st, _ := me.do("POST", "/api/v1/auth/logout", nil); st != 204 {
		t.Fatalf("logout: %d", st)
	}
	if st, out := me.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("after logout: %d %v", st, out)
	}
	second.must("GET", "/api/v1/user", nil, 200)
}

// An unknown username must cost the same bcrypt compare as a wrong password,
// or response time tells an attacker which usernames exist.
func TestAuthUnknownUsernameTiming(t *testing.T) {
	srv, _ := newTestServer(t)
	anon := &testClient{t: t, base: srv.URL}
	name := uniqUser()
	register(t, srv.URL, name, "correct horse")

	timeLogins := func(username string) time.Duration {
		start := time.Now()
		for range 3 {
			if st, _ := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": username, "password": "wrong horse"}); st != 401 {
				t.Fatalf("%s: %d", username, st)
			}
		}
		return time.Since(start)
	}
	known := timeLogins(name)
	unknown := timeLogins(uniqUser())
	if unknown < known/2 {
		t.Fatalf("unknown username answered too fast: known %v unknown %v", known, unknown)
	}
}

func TestAuthLockout(t *testing.T) {
	srv, _ := newTestServer(t)
	anon := &testClient{t: t, base: srv.URL}
	name := uniqUser()
	register(t, srv.URL, name, "correct horse")

	for i := range 5 {
		if st, _ := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": name, "password": "wrong horse"}); st != 401 {
			t.Fatalf("attempt %d: %d", i+1, st)
		}
	}
	st, out := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": name, "password": "correct horse"})
	if st != 429 || out["code"] != "locked_out" {
		t.Fatalf("after 5 failures: %d %v", st, out)
	}
	// the lockout is per username
	login(t, srv.URL, register2(t, srv.URL), "correct horse")
}

func register2(t *testing.T, base string) string {
	t.Helper()
	name := uniqUser()
	register(t, base, name, "correct horse")
	return name
}

func TestSessionAbsoluteCapAndTouch(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	me, _ := register(t, srv.URL, uniqUser(), "correct horse")
	hash := secrets.HashToken(me.token)

	// a request refreshes last_used_at only once the throttle window passed
	if err := store.AgeSession(ctx, hash, time.Minute); err != nil {
		t.Fatal(err)
	}
	me.must("GET", "/api/v1/user", nil, 200)
	sess, _, err := store.SessionByTokenHash(ctx, hash, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sess.LastUsedAt) < 50*time.Second {
		t.Fatalf("touched inside the throttle window: %v", sess.LastUsedAt)
	}
	if err := store.AgeSession(ctx, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	me.must("GET", "/api/v1/user", nil, 200)
	sess, _, err = store.SessionByTokenHash(ctx, hash, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sess.LastUsedAt) > 30*time.Second {
		t.Fatalf("not touched after the throttle window: %v", sess.LastUsedAt)
	}
	if sess.ExpiresAt.After(sess.CreatedAt.Add(models.SessionMaxAge + time.Minute)) {
		t.Fatalf("expires_at slid past the cap: %v", sess.ExpiresAt)
	}

	// the sliding window never outlives 90 days from creation
	if err := store.AgeSession(ctx, hash, 91*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if st, out := me.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("capped session: %d %v", st, out)
	}
	n, err := store.SweepSessions(ctx)
	if err != nil || n < 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if _, _, err := store.SessionByTokenHash(ctx, hash, time.Hour); err == nil {
		t.Fatal("swept session still present")
	}
}

// The touching request must see the values it just wrote, not the pre-touch
// row: the outer SELECT of a data-modifying CTE cannot see the UPDATE.
func TestSessionTouchReturnsFreshValues(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	me, _ := register(t, srv.URL, uniqUser(), "correct horse")
	hash := secrets.HashToken(me.token)

	if err := store.AgeSession(ctx, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	sess, _, err := store.SessionByTokenHash(ctx, hash, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sess.LastUsedAt) > 30*time.Second {
		t.Fatalf("touching lookup returned the stale last_used_at: %v", sess.LastUsedAt)
	}
	if d := time.Until(sess.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("touching lookup returned the stale expires_at: %v", sess.ExpiresAt)
	}
}

// An idle-expired session must stop authenticating even while created_at is
// still well inside the 90-day cap.
func TestSessionIdleExpiry(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()
	me, _ := register(t, srv.URL, uniqUser(), "correct horse")
	hash := secrets.HashToken(me.token)

	if err := store.ExpireSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if st, out := me.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_invalid" {
		t.Fatalf("idle-expired session: %d %v", st, out)
	}
	if _, _, err := store.SessionByTokenHash(ctx, hash, time.Hour); err == nil {
		t.Fatal("idle-expired session still resolves")
	}
}

// A TTL longer than the cap must be clamped both at creation and on touch.
func TestSessionTTLClampedToMaxAge(t *testing.T) {
	_, store := newTestServer(t)
	ctx := context.Background()
	ttl := 200 * 24 * time.Hour
	long := httptest.NewServer(New(store, testConfig(store, Config{PublicURL: "http://public.test", SessionTTL: ttl})).Handler())
	defer long.Close()

	me, out := register(t, long.URL, uniqUser(), "correct horse")
	hash := secrets.HashToken(me.token)
	assertClamped := func(step string) models.Session {
		t.Helper()
		sess, _, err := store.SessionByTokenHash(ctx, hash, ttl)
		if err != nil {
			t.Fatal(err)
		}
		if sess.ExpiresAt.After(sess.CreatedAt.Add(models.SessionMaxAge + time.Minute)) {
			t.Fatalf("%s: expires_at past the cap: created %v expires %v", step, sess.CreatedAt, sess.ExpiresAt)
		}
		if sess.ExpiresAt.Before(sess.CreatedAt.Add(models.SessionMaxAge - time.Minute)) {
			t.Fatalf("%s: expires_at is not the clamp: created %v expires %v", step, sess.CreatedAt, sess.ExpiresAt)
		}
		return sess
	}
	sess := assertClamped("create")
	if exp, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string)); err != nil || !exp.Equal(sess.ExpiresAt) {
		t.Fatalf("login response expires_at %v vs stored %v (%v)", out["expires_at"], sess.ExpiresAt, err)
	}

	// a touch with the aged row would land at now+200d if the UPDATE were not clamped
	if err := store.AgeSession(ctx, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	me.must("GET", "/api/v1/user", nil, 200)
	sess = assertClamped("touch")
	if time.Since(sess.LastUsedAt) > 30*time.Second {
		t.Fatalf("not touched: %v", sess.LastUsedAt)
	}
}

func TestPasswordChange(t *testing.T) {
	srv, _ := newTestServer(t)
	name := uniqUser()
	laptop, _ := register(t, srv.URL, name, "correct horse")
	phone := login(t, srv.URL, name, "correct horse")

	if st, out := laptop.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "wrong horse", "new_password": "battery staple"}); st != 401 || out["code"] != "invalid_credentials" {
		t.Fatalf("wrong current: %d %v", st, out)
	}
	if st, out := laptop.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "correct horse", "new_password": "short"}); st != 400 || out["code"] != "weak_password" {
		t.Fatalf("weak new: %d %v", st, out)
	}
	if st, _ := laptop.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "correct horse", "new_password": "battery staple"}); st != 204 {
		t.Fatalf("change: %d", st)
	}
	// the changing session survives, every other one is gone. The revocation
	// runs inside the password transaction; its atomicity is not observable
	// here, only its outcome.
	laptop.must("GET", "/api/v1/user", nil, 200)
	if st, _ := phone.do("GET", "/api/v1/user", nil); st != 401 {
		t.Fatalf("other session after change: %d", st)
	}
	anon := &testClient{t: t, base: srv.URL}
	if st, _ := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": name, "password": "correct horse"}); st != 401 {
		t.Fatalf("old password: %d", st)
	}
	login(t, srv.URL, name, "battery staple")
}

// A malformed change-password body carries the same code as every other
// auth error, so a client can route on it.
func TestPasswordChangeBadBodyHasCode(t *testing.T) {
	srv, _ := newTestServer(t)
	me, _ := register(t, srv.URL, uniqUser(), "correct horse")
	st, out := me.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "x", "new_password": "y", "extra": 1})
	if st != 400 || out["code"] != "bad_request" {
		t.Fatalf("unknown field: %d %v", st, out)
	}
}

// A too-long password is a client error, never a 500 from bcrypt.
func TestPasswordTooLongIs400(t *testing.T) {
	srv, _ := newTestServer(t)
	anon := &testClient{t: t, base: srv.URL}
	long := strings.Repeat("a", 73)
	if st, out := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": uniqUser(), "password": long}); st != 400 || out["code"] != "password_too_long" {
		t.Fatalf("register: %d %v", st, out)
	}
	me, _ := register(t, srv.URL, uniqUser(), "correct horse")
	if st, out := me.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "correct horse", "new_password": long}); st != 400 || out["code"] != "password_too_long" {
		t.Fatalf("change: %d %v", st, out)
	}
}

// Guessing the current password through a live session hits the same lockout
// as login.
func TestPasswordChangeLockout(t *testing.T) {
	srv, _ := newTestServer(t)
	name := uniqUser()
	me, _ := register(t, srv.URL, name, "correct horse")
	for i := range 5 {
		if st, out := me.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "wrong horse", "new_password": "battery staple"}); st != 401 || out["code"] != "invalid_credentials" {
			t.Fatalf("attempt %d: %d %v", i+1, st, out)
		}
	}
	if st, out := me.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "correct horse", "new_password": "battery staple"}); st != 429 || out["code"] != "locked_out" {
		t.Fatalf("after 5 failures: %d %v", st, out)
	}
	// the password did not change and login shares the lock
	anon := &testClient{t: t, base: srv.URL}
	if st, out := anon.do("POST", "/api/v1/auth/password/login", map[string]any{"username": name, "password": "correct horse"}); st != 429 || out["code"] != "locked_out" {
		t.Fatalf("login during lockout: %d %v", st, out)
	}
}

func TestMustChangePasswordRoundTrip(t *testing.T) {
	srv, store := newTestServer(t)
	me, out := register(t, srv.URL, uniqUser(), "correct horse")
	userID := out["user"].(map[string]any)["id"].(string)

	if err := store.SetMustChangePassword(context.Background(), userID, true); err != nil {
		t.Fatal(err)
	}
	if me.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)["must_change_password"] != true {
		t.Fatal("flag not visible")
	}
	if st, _ := me.do("POST", "/api/v1/auth/password/change", map[string]any{"current_password": "correct horse", "new_password": "battery staple"}); st != 204 {
		t.Fatalf("change: %d", st)
	}
	if me.must("GET", "/api/v1/user", nil, 200)["user"].(map[string]any)["must_change_password"] != false {
		t.Fatal("flag not cleared by a password change")
	}
}

func TestRegistrationDisabled(t *testing.T) {
	_, store := newTestServer(t)
	closed := httptest.NewServer(New(store, Config{
		PublicURL: "http://public.test",
		Providers: auth.NewRegistry(auth.NewPasswordProvider(store, false)),
	}).Handler())
	defer closed.Close()
	anon := &testClient{t: t, base: closed.URL}

	if anon.must("GET", "/api/v1/auth/providers", nil, 200)["registration_enabled"] != false {
		t.Fatal("providers must advertise registration off")
	}
	st, out := anon.do("POST", "/api/v1/auth/password/register", map[string]any{"username": uniqUser(), "password": "correct horse"})
	if st != 403 || out["code"] != "registration_disabled" {
		t.Fatalf("register: %d %v", st, out)
	}
}

// The act_ path is today's code: a participant token ignores every header a
// session client might send, and a session never reaches a participant handler.
func TestActTokenIgnoresRoomHeader(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)

	req, err := http.NewRequest("GET", srv.URL+"/api/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+alice.token)
	req.Header.Set("X-Workspace-Slug", "bogus-room-that-does-not-exist")
	req.Header.Set("X-Session", "ses_"+strings.Repeat("x", 32))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var me map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || me["name"] != "alice" {
		t.Fatalf("act_ with stray session headers: %d %v", resp.StatusCode, me)
	}
	// the act_ path never reads the header and an agent carries no account fields
	if _, has := me["user_id"]; has {
		t.Fatalf("agent must not carry user_id: %v", me)
	}
	if _, has := me["username"]; has {
		t.Fatalf("agent must not carry username: %v", me)
	}

	// an agent token has no user behind it
	if st, out := alice.do("GET", "/api/v1/user", nil); st != 401 || out["code"] != "session_required" {
		t.Fatalf("act_ on a session route: %d %v", st, out)
	}
	if st, _ := alice.do("POST", "/api/v1/auth/logout", nil); st != 401 {
		t.Fatalf("act_ logout: %d", st)
	}
}

// A Clerk install is a separate deployment (design section 11): the provider
// is listed only when configured, and until the verifier lands it refuses
// every login with 501 rather than letting a token through unchecked.
func TestClerkProviderStub(t *testing.T) {
	_, store := newTestServer(t)
	srv := httptest.NewServer(New(store, Config{
		PublicURL: "http://public.test",
		Providers: auth.NewRegistry(auth.NewPasswordProvider(store, true), auth.NewClerkProvider("sk_test_x")),
	}).Handler())
	defer srv.Close()
	anon := &testClient{t: t, base: srv.URL}

	names := anon.must("GET", "/api/v1/auth/providers", nil, 200)["providers"].([]any)
	if len(names) != 2 || names[0] != "password" || names[1] != "clerk" {
		t.Fatalf("providers: %v", names)
	}
	st, out := anon.do("POST", "/api/v1/auth/clerk/login", map[string]any{"token": "eyJ.fake.jwt"})
	if st != 501 || out["code"] != "provider_not_implemented" {
		t.Fatalf("clerk login: %d %v", st, out)
	}
	st, out = anon.do("POST", "/api/v1/auth/clerk/login", map[string]any{"token": ""})
	if st != 401 || out["code"] != "invalid_credentials" {
		t.Fatalf("clerk empty token: %d %v", st, out)
	}
	if _, ok := auth.Provider(auth.NewClerkProvider("k")).(auth.Registrar); ok {
		t.Fatal("clerk must not register accounts locally")
	}
}
