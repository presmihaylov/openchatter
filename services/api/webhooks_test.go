package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/services/webhook"
)

// hookCall is one request the test receiver got.
type hookCall struct {
	header http.Header
	body   []byte
}

// hookReceiver is an HTTPS endpoint that records every call and answers with
// whatever status the test sets.
type hookReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []hookCall
	status int
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	h := &hookReceiver{status: http.StatusNoContent}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls = append(h.calls, hookCall{header: r.Header.Clone(), body: body})
		st := h.status
		h.mu.Unlock()
		if st == http.StatusFound {
			http.Redirect(w, r, "https://example.com/elsewhere", st)
			return
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookReceiver) setStatus(s int) { h.mu.Lock(); h.status = s; h.mu.Unlock() }
func (h *hookReceiver) got() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// testWorker is the production worker that may reach the loopback receiver
// and trusts its certificate. Everything else is the real code path.
func testWorker(store *models.Store, h *hookReceiver) *webhook.Worker {
	w := webhook.NewWorker(store)
	w.Allow = func(netip.Addr) bool { return true }
	w.UseTransport(h.srv.Client().Transport.(*http.Transport).Clone())
	return w
}

func hookDB(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("OPENCHATTER_TEST_DB_URL")
	if url == "" {
		url = "postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable"
	}
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Skipf("db unavailable: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// The secret is write-only, the URL is checked at the door, and a human has
// no push wake to register.
func TestWebhookAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, alice, _ := setupRoom(t, srv.URL)

	alice.must("GET", "/api/v1/me/webhook", nil, 404)
	for _, bad := range []map[string]any{
		{"url": "http://example.com/hook", "secret": "0123456789abcdef"},
		{"url": "https://127.0.0.1/hook", "secret": "0123456789abcdef"},
		{"url": "https://localhost/hook", "secret": "0123456789abcdef"},
		{"url": "https://192.168.1.20/hook", "secret": "0123456789abcdef"},
		{"url": "https://example.com/hook", "secret": "short"},
	} {
		alice.must("PUT", "/api/v1/me/webhook", bad, 400)
	}
	out := alice.must("PUT", "/api/v1/me/webhook", map[string]any{"url": "https://hooks.example.com/wake", "secret": "0123456789abcdef-SECRET"}, 200)
	got := alice.must("GET", "/api/v1/me/webhook", nil, 200)
	for _, body := range []map[string]any{out, got} {
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "SECRET") || body["secret_set"] != true || body["url"] != "https://hooks.example.com/wake" {
			t.Fatalf("the secret must never come back, url and secret_set must: %s", raw)
		}
	}
	alice.must("DELETE", "/api/v1/me/webhook", nil, 204)
	alice.must("DELETE", "/api/v1/me/webhook", nil, 404)

	human := &testClient{t: t, base: srv.URL}
	human.token = human.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "maya", "is_human": true}, 201)["token"].(string)
	human.must("PUT", "/api/v1/me/webhook", map[string]any{"url": "https://hooks.example.com/wake", "secret": "0123456789abcdef"}, 403)
}

// A wake goes out on exactly the watcher's wake set, signed, with ids only:
// a mention, a human reply in a thread the agent is in, a root broadcast. An
// agent's untagged message wakes nobody.
func TestWebhookWakesSignedWithIdsOnly(t *testing.T) {
	srv, store := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	human := &testClient{t: t, base: srv.URL}
	human.token = human.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "maya", "is_human": true}, 201)["token"].(string)
	rcv := newHookReceiver(t)
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	const hookSecret = "0123456789abcdef-hook"
	// the receiver lives on loopback, which the API refuses: register it underneath
	if _, err := store.SetWebhook(context.Background(), aliceID, rcv.srv.URL+"/wake", hookSecret, ""); err != nil {
		t.Fatal(err)
	}
	w := testWorker(store, rcv)

	mention := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice secret plans inside"}, 201)
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alice's topic"}, 201)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "untagged agent chatter", "thread_root_id": root["id"]}, 201)
	humanReply := human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "a human reply", "thread_root_id": root["id"]}, 201)
	broadcast := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel heads up"}, 201)
	w.Tick(context.Background())

	calls := rcv.got()
	if len(calls) != 3 {
		t.Fatalf("want 3 wakes (mention, human thread reply, broadcast), got %d", len(calls))
	}
	want := map[string]string{mention["id"].(string): "mention", humanReply["id"].(string): "thread_reply", broadcast["id"].(string): "broadcast"}
	for _, c := range calls {
		var pl webhook.Payload
		if err := json.Unmarshal(c.body, &pl); err != nil {
			t.Fatal(err)
		}
		if want[pl.MessageID] != pl.Reason || pl.ParticipantID != aliceID || pl.Type != "message.created" || pl.ChannelID == "" || pl.Seq == 0 || pl.ReplyTo == "" {
			t.Fatalf("bad payload: %s", c.body)
		}
		if strings.Contains(string(c.body), "secret plans") || strings.Contains(string(c.body), "heads up") || strings.Contains(string(c.body), "human reply") {
			t.Fatalf("a payload must carry ids only, no message body: %s", c.body)
		}
		ts, err := strconv.ParseInt(c.header.Get("X-OpenChatter-Timestamp"), 10, 64)
		if err != nil || time.Since(time.Unix(ts, 0)) > time.Minute {
			t.Fatalf("bad timestamp header %q", c.header.Get("X-OpenChatter-Timestamp"))
		}
		if c.header.Get("X-OpenChatter-Signature") != webhook.Sign(hookSecret, ts, c.body) {
			t.Fatal("the signature does not verify")
		}
		if c.header.Get("X-OpenChatter-Delivery") != pl.DeliveryID || pl.DeliveryID == "" {
			t.Fatal("the delivery id must be in the header and the body")
		}
	}
	// a sent wake is never sent twice
	w.Tick(context.Background())
	if n := len(rcv.got()); n != 3 {
		t.Fatalf("a delivered wake was sent again: %d calls", n)
	}
}

// Failed attempts back off; ten in a row switch the hook off, drop what is
// queued and tell the agent and its owner, nobody else. A new PUT turns it on.
func TestWebhookDisablesAfterRepeatedFailures(t *testing.T) {
	srv, store := newTestServer(t)
	db := hookDB(t)
	code := createRoom(t, srv.URL, "hook room")["invite"].(string)
	join := func(code, name string, human bool) *testClient {
		c := &testClient{t: t, base: srv.URL}
		c.token = c.must("POST", "/api/v1/rooms/join", map[string]any{"invite": code, "name": name, "is_human": human}, 201)["token"].(string)
		return c
	}
	maya := join(code, "maya", true)
	inv := maya.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)
	helper := join(inv["join_url"].(string), "helper", false)
	bob := join(code, "bob", false)
	helperID := helper.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	before := fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])

	rcv := newHookReceiver(t)
	rcv.setStatus(http.StatusInternalServerError)
	if _, err := store.SetWebhook(context.Background(), helperID, rcv.srv.URL+"/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	w := testWorker(store, rcv)

	// one failure leaves the job pending, its next try in the future
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@helper one"}, 201)
	w.Tick(context.Background())
	var state string
	var next time.Time
	if err := db.QueryRow(context.Background(), `SELECT state, next_attempt_at FROM webhook_jobs WHERE participant_id = $1`, helperID).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || time.Until(next) < 5*time.Second {
		t.Fatalf("a failed wake should wait to retry: state %s, next in %s", state, time.Until(next))
	}
	hook := helper.must("GET", "/api/v1/me/webhook", nil, 200)
	if hook["last_error"] != "HTTP 500" || hook["consecutive_failures"].(float64) != 1 || hook["disabled_at"] != nil {
		t.Fatalf("hook after one failure: %v", hook)
	}

	// nine more failures in a row, then one more wake that must not go out
	for i := 2; i <= 10; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("@helper %d", i)}, 201)
	}
	if _, err := db.Exec(context.Background(), `UPDATE webhook_jobs SET next_attempt_at = now() WHERE participant_id = $1 AND state = 'pending'`, helperID); err != nil {
		t.Fatal(err)
	}
	w.Tick(context.Background())
	hook = helper.must("GET", "/api/v1/me/webhook", nil, 200)
	if hook["disabled_at"] == nil || hook["consecutive_failures"].(float64) < 10 {
		t.Fatalf("ten straight failures should disable the hook: %v", hook)
	}
	var pending int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM webhook_jobs WHERE participant_id = $1 AND state = 'pending'`, helperID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("a disabled hook keeps %d pending wakes", pending)
	}
	calls := len(rcv.got())
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@helper after"}, 201)
	w.Tick(context.Background())
	if len(rcv.got()) != calls {
		t.Fatal("a disabled hook was still called")
	}

	disabledEvents := func(c *testClient, relevant bool) int {
		path := "/api/v1/events?after=" + before
		if relevant {
			path += "&relevant=true"
		}
		n := 0
		for _, e := range c.must("GET", path, nil, 200)["events"].([]any) {
			if e.(map[string]any)["type"] == models.WebhookDisabledEvent {
				n++
			}
		}
		return n
	}
	if disabledEvents(helper, true) != 1 || disabledEvents(maya, true) != 1 {
		t.Fatal("the agent and its owner should each hear exactly one disable event")
	}
	if disabledEvents(bob, false) != 0 || disabledEvents(bob, true) != 0 {
		t.Fatal("another agent must never see the disable event")
	}

	// a new PUT turns it back on
	rcv.setStatus(http.StatusNoContent)
	helper.must("PUT", "/api/v1/me/webhook", map[string]any{"url": "https://hooks.example.com/wake", "secret": "0123456789abcdef-new"}, 200)
	hook = helper.must("GET", "/api/v1/me/webhook", nil, 200)
	if hook["disabled_at"] != nil || hook["consecutive_failures"].(float64) != 0 {
		t.Fatalf("a new PUT should re-enable the hook: %v", hook)
	}
}

// The guard holds at dial time, whatever the stored URL says: a loopback
// address is refused, and a redirect is never followed.
func TestWebhookGuardAtDialTime(t *testing.T) {
	srv, store := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	rcv := newHookReceiver(t)
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	if _, err := store.SetWebhook(context.Background(), aliceID, rcv.srv.URL+"/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}

	// the production guard, aimed at loopback: refused before a byte is sent
	guarded := webhook.NewWorker(store)
	guarded.UseTransport(rcv.srv.Client().Transport.(*http.Transport).Clone())
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice ping"}, 201)
	guarded.Tick(context.Background())
	if len(rcv.got()) != 0 {
		t.Fatal("the guard let a request through to loopback")
	}
	if hook := alice.must("GET", "/api/v1/me/webhook", nil, 200); !strings.HasPrefix(fmt.Sprint(hook["last_error"]), "blocked") {
		t.Fatalf("want a blocked error, got %v", hook["last_error"])
	}

	// a redirect counts as a failure and is not followed
	db := hookDB(t)
	rcv.setStatus(http.StatusFound)
	if _, err := db.Exec(context.Background(), `UPDATE webhook_jobs SET next_attempt_at = now() WHERE participant_id = $1 AND state = 'pending'`, aliceID); err != nil {
		t.Fatal(err)
	}
	testWorker(store, rcv).Tick(context.Background())
	if hook := alice.must("GET", "/api/v1/me/webhook", nil, 200); hook["last_error"] != "redirect not followed (HTTP 302)" {
		t.Fatalf("want the redirect named, got %v", hook["last_error"])
	}
}

// A slow endpoint costs one attempt of at most webhook.Timeout, then a retry;
// it never holds a worker, or a poster, for longer.
func TestWebhookTimesOut(t *testing.T) {
	srv, store := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(webhook.Timeout + time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	if _, err := store.SetWebhook(context.Background(), aliceID, slow.URL+"/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	w := webhook.NewWorker(store)
	w.Allow = func(netip.Addr) bool { return true }
	w.UseTransport(slow.Client().Transport.(*http.Transport).Clone())

	start := time.Now()
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice slow"}, 201)
	if time.Since(start) > 2*time.Second {
		t.Fatal("posting waited on the webhook")
	}
	start = time.Now()
	w.Tick(context.Background())
	if d := time.Since(start); d > webhook.Timeout+2*time.Second {
		t.Fatalf("an attempt took %s, the cap is %s", d, webhook.Timeout)
	}
	if hook := alice.must("GET", "/api/v1/me/webhook", nil, 200); !strings.HasPrefix(fmt.Sprint(hook["last_error"]), "timeout") {
		t.Fatalf("want a timeout error, got %v", hook["last_error"])
	}
}

// A disabled hook cannot wake its agent to say so, so whoami says it.
func TestWhoamiShowsWebhookState(t *testing.T) {
	srv, store := newTestServer(t)
	db := hookDB(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	env := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n")
	whoami := func() string {
		t.Helper()
		out, err := runCLI(t, cli, env, nil, "whoami")
		if err != nil {
			t.Fatalf("whoami: %v\n%s", err, out)
		}
		return out
	}
	if out := whoami(); strings.Contains(out, "webhook") {
		t.Fatalf("no hook, no line:\n%s", out)
	}
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	if _, err := store.SetWebhook(context.Background(), aliceID, "https://hooks.example.com/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	if out := whoami(); !strings.Contains(out, "webhook: https://hooks.example.com/wake, on") {
		t.Fatalf("a live hook should show as on:\n%s", out)
	}
	if _, err := db.Exec(context.Background(),
		`UPDATE participant_webhooks SET disabled_at = now(), consecutive_failures = 10, last_error = 'HTTP 500' WHERE participant_id = $1`, aliceID); err != nil {
		t.Fatal(err)
	}
	out := whoami()
	if !strings.Contains(out, "DISABLED since") || !strings.Contains(out, "after 10 failed deliveries (last error: HTTP 500)") {
		t.Fatalf("a disabled hook should say when and why:\n%s", out)
	}
	if strings.Contains(out, "0123456789abcdef") {
		t.Fatalf("whoami printed the secret:\n%s", out)
	}
}

// An endpoint behind its own auth gets the Authorization value its agent
// registered, verbatim and write-only; a value that could split the request
// is refused, and a hook without one sends no Authorization header at all.
func TestWebhookAuthorizationHeader(t *testing.T) {
	srv, store := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	base := map[string]any{"url": "https://hooks.example.com/wake", "secret": "0123456789abcdef"}
	with := func(auth string) map[string]any {
		m := map[string]any{"authorization": auth}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	alice.must("PUT", "/api/v1/me/webhook", with("Bearer x\r\nX-Evil: 1"), 400)
	alice.must("PUT", "/api/v1/me/webhook", with("Bearer "+strings.Repeat("k", 1100)), 400)
	out := alice.must("PUT", "/api/v1/me/webhook", with("Bearer KEY-never-shown"), 200)
	got := alice.must("GET", "/api/v1/me/webhook", nil, 200)
	for _, body := range []map[string]any{out, got} {
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "KEY-never-shown") || body["authorization_set"] != true {
			t.Fatalf("the authorization must never come back, authorization_set must: %s", raw)
		}
	}
	if out := alice.must("PUT", "/api/v1/me/webhook", base, 200); out["authorization_set"] != false {
		t.Fatalf("a PUT without authorization replaces the hook and clears it: %v", out)
	}

	rcv := newHookReceiver(t)
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	bobID := bob.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	if _, err := store.SetWebhook(context.Background(), aliceID, rcv.srv.URL+"/alice", "0123456789abcdef-hook", "Bearer test-key-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWebhook(context.Background(), bobID, rcv.srv.URL+"/bob", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice key please"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob no key"}, 201)
	testWorker(store, rcv).Tick(context.Background())
	calls := rcv.got()
	if len(calls) != 2 {
		t.Fatalf("want 2 wakes, got %d", len(calls))
	}
	for _, c := range calls {
		var pl webhook.Payload
		_ = json.Unmarshal(c.body, &pl)
		want := ""
		if pl.ParticipantID == aliceID {
			want = "Bearer test-key-123"
		}
		if got := c.header.Get("Authorization"); got != want {
			t.Fatalf("participant %s: Authorization %q, want %q", pl.ParticipantID, got, want)
		}
	}

	// failures, up to the disable event, never echo the value
	db := hookDB(t)
	rcv.setStatus(http.StatusUnauthorized)
	if _, err := db.Exec(context.Background(), `UPDATE participant_webhooks SET consecutive_failures = 9 WHERE participant_id = $1`, aliceID); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprint(alice.must("GET", "/api/v1/events", nil, 200)["cursor"])
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice again"}, 201)
	testWorker(store, rcv).Tick(context.Background())
	hook := alice.must("GET", "/api/v1/me/webhook", nil, 200)
	evs := alice.must("GET", "/api/v1/events?after="+before, nil, 200)
	raw, _ := json.Marshal([]any{hook, evs})
	if hook["disabled_at"] == nil || hook["last_error"] != "HTTP 401" {
		t.Fatalf("want the hook disabled on HTTP 401: %v", hook)
	}
	if strings.Contains(string(raw), "test-key-123") {
		t.Fatalf("the authorization value leaked into last_error or the disable event: %s", raw)
	}
}

// An agent the room can wake through a live push webhook counts as online
// without a heartbeat: it shows online, the sweep never drops it and raises it
// if it was offline, and it stays on the roster. A disabled hook, or a
// declared offline, takes it offline again.
func TestWebhookKeepsAgentOnline(t *testing.T) {
	srv, store := newTestServer(t)
	db := hookDB(t)
	_, alice, bob := setupRoom(t, srv.URL)
	ctx := context.Background()
	aliceID := alice.must("GET", "/api/v1/me", nil, 200)["id"].(string)
	presenceEvents := func(after string) []bool {
		var out []bool
		for _, e := range bob.must("GET", "/api/v1/events?after="+after, nil, 200)["events"].([]any) {
			ev := e.(map[string]any)
			pl := ev["payload"].(map[string]any)
			if ev["type"] == "participant.presence_changed" && pl["participant_id"] == aliceID {
				out = append(out, pl["online"].(bool))
			}
		}
		return out
	}
	sweep := func() {
		t.Helper()
		if _, err := store.SweepPresence(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// no hook: a stopped heartbeat goes offline, as before
	if err := store.BackdateSeen(ctx, aliceID); err != nil {
		t.Fatal(err)
	}
	sweep()
	if participantByName(t, bob, "alice")["online"] != false {
		t.Fatal("a silent agent without a hook should be offline")
	}

	// a live hook: online at once, and the sweep announces it
	cur := fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])
	if _, err := store.SetWebhook(ctx, aliceID, "https://hooks.example.com/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	if participantByName(t, bob, "alice")["online"] != true {
		t.Fatal("an agent with a live hook should show online")
	}
	sweep()
	if got := presenceEvents(cur); len(got) != 1 || !got[0] {
		t.Fatalf("the sweep should announce alice online once, got %v", got)
	}

	// still silent, still online, never swept off and never expired from the roster
	cur = fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])
	if err := store.ExpireSeen(ctx, aliceID); err != nil {
		t.Fatal(err)
	}
	sweep()
	sweep()
	if p := participantByName(t, bob, "alice"); p["online"] != true {
		t.Fatalf("a live hook keeps a silent agent online and on the roster: %v", p)
	}
	if got := presenceEvents(cur); len(got) != 1 || !got[0] {
		t.Fatalf("ExpireSeen dropped the flag, so one raise and no drop, got %v", got)
	}

	// the hook is switched off: offline again, announced (silent past the online
	// window but not long enough to expire from the roster)
	if err := store.BackdateSeen(ctx, aliceID); err != nil {
		t.Fatal(err)
	}
	cur = fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])
	if _, err := db.Exec(ctx, `UPDATE participant_webhooks SET disabled_at = now() WHERE participant_id = $1`, aliceID); err != nil {
		t.Fatal(err)
	}
	if participantByName(t, bob, "alice")["online"] != false {
		t.Fatal("a disabled hook should not keep the agent online")
	}
	sweep()
	if got := presenceEvents(cur); len(got) != 1 || got[0] {
		t.Fatalf("the sweep should announce alice offline once, got %v", got)
	}

	// re-enabled, but the agent declared itself offline: offline wins
	if _, err := store.SetWebhook(ctx, aliceID, "https://hooks.example.com/wake", "0123456789abcdef-hook", ""); err != nil {
		t.Fatal(err)
	}
	alice.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	sweep()
	if participantByName(t, bob, "alice")["online"] != false {
		t.Fatal("a declared offline should beat a live hook")
	}
}
