package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush keeps a held long poll streaming through the recorder.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ackRecorder proxies to upstream and records the status of every event ack.
func ackRecorder(t *testing.T, upstream string) (*httptest.Server, func() []int) {
	t.Helper()
	target, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var codes []int
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		proxy.ServeHTTP(sw, r)
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v1/events/") && strings.HasSuffix(r.URL.Path, "/ack") {
			mu.Lock()
			codes = append(codes, sw.code)
			mu.Unlock()
		}
	}))
	t.Cleanup(front.Close)
	return front, func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), codes...)
	}
}

func watcherHome(t *testing.T, server, token string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := "SERVER=" + server + "\nTOKEN=" + token + "\n"
	if err := os.WriteFile(filepath.Join(home, ".openchatter", "room.alice.env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// The author never gets a receipt, so the watcher used to POST an ack for its
// own reply and earn a 404 about a second after every reply it sent.
func TestWatcherNeverAcksItsOwnReply(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	front, acks := ackRecorder(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, front.URL, alice.token)
	out := runWatcherPosting(t, script, home, func() {
		root := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice a question"}, 201)
		id := root["id"].(string)
		alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "my answer", "thread_root_id": id}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice thanks", "thread_root_id": id}, 201)
		time.Sleep(time.Second)
	})
	if !strings.Contains(out, "a question") || !strings.Contains(out, "thanks") {
		t.Fatalf("the watcher should hear bob's root and reply:\n%s", out)
	}
	got := acks()
	if len(got) == 0 {
		t.Fatalf("the watcher acked nothing:\n%s", out)
	}
	for _, code := range got {
		if code != http.StatusNoContent {
			t.Fatalf("every ack should land, got statuses %v", got)
		}
	}
}

// An agent away for days can owe thousands of events. The watcher drains and
// acks every page but prints only the newest few, so one start is not a flood.
func TestWatcherInboxReplayShowsOnlyTheNewest(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	for i := 1; i <= 25; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("@alice ping-%02d", i)}, 201)
	}
	waiting := len(alice.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)["events"].([]any))
	if waiting < 25 {
		t.Fatalf("alice should owe 25 receipts, has %d", waiting)
	}
	// pages of 10 prove the drain walks past the first page
	t.Setenv("OPENCHATTER_INBOX_SHOW", "5")
	t.Setenv("OPENCHATTER_INBOX_PAGE", "10")
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	out := runWatcherPosting(t, script, watcherHome(t, srv.URL, alice.token), func() {})

	if want := fmt.Sprintf("showing the newest 5 of %d messages", waiting); !strings.Contains(out, want) {
		t.Fatalf("want %q:\n%s", want, out)
	}
	if n := strings.Count(out, "REPLY-TO "); n != 5 {
		t.Fatalf("printed %d hits, want 5:\n%s", n, out)
	}
	if !strings.Contains(out, "ping-21") || !strings.Contains(out, "ping-25") || strings.Contains(out, "ping-20") {
		t.Fatalf("the five shown should be the newest:\n%s", out)
	}
	if left := alice.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)["events"].([]any); len(left) != 0 {
		t.Fatalf("every drained event should be acked, %d left", len(left))
	}
}

// The cap is for stale messages only. A reminder or a capability call skipped
// there was acked and gone for good: PendingAcks lists messages only.
func TestWatcherInboxReplayKeepsRemindersAndCalls(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, store := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	alice.must("POST", "/api/v1/me/reminders", map[string]any{"text": "check the build", "schedule": "in 30m"}, 201)
	if _, err := store.FireDueReminders(context.Background(), time.Now().Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	alice.must("POST", "/api/v1/me/capabilities", capBody("echo"), 200)
	// the call holds its request open until answered, so it runs on the side
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	raw, _ := json.Marshal(map[string]any{"agent": "alice", "name": "echo", "args": map[string]any{"q": "x"}, "timeoutSeconds": 120})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/v1/capabilities/call", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+bob.token)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(fmt.Sprint(alice.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)["events"]), "capability.call") {
		if time.Now().After(deadline) {
			t.Fatal("the call never reached alice's inbox")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := 1; i <= 8; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("@alice ping-%02d", i)}, 201)
	}
	t.Setenv("OPENCHATTER_INBOX_SHOW", "3")
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	out := runWatcherPosting(t, script, watcherHome(t, srv.URL, alice.token), func() {})

	if !strings.Contains(out, "REMINDER ") || !strings.Contains(out, "check the build") {
		t.Fatalf("the older reminder must still print:\n%s", out)
	}
	if !strings.Contains(out, "CAPABILITY-CALL ") {
		t.Fatalf("the older call must still print:\n%s", out)
	}
	if !strings.Contains(out, "showing the newest 3 of 8 messages") || strings.Count(out, "REPLY-TO ") != 3 {
		t.Fatalf("want 3 of 8 messages shown:\n%s", out)
	}
}

// A reaction or an ack on alice's ask by its addressee prints one ACKED line, so
// she knows the ask landed. Her own acks and acks on other people's asks stay
// silent, and the ack event never reaches her session as raw JSON.
func TestWatcherHearsAcksOnItsAsks(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	human := &testClient{t: t, base: srv.URL}
	joined := human.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": secret, "name": "maya", "is_human": true,
	}, 201)
	human.token = joined["token"].(string)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	var byReaction, byAck string
	out := runWatcherPosting(t, script, watcherHome(t, srv.URL, alice.token), func() {
		byReaction = alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob please review the plan"}, 201)["id"].(string)
		byAck = alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob a broadcast-style ask"}, 201)["id"].(string)
		bob.must("POST", "/api/v1/messages/"+byReaction+"/reactions", map[string]any{"emoji": "👀"}, 200)
		bob.must("POST", "/api/v1/messages/"+byAck+"/ack", nil, 200)
		// somebody else's ask, acked by its addressee: not alice's news
		foreign := human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob a human ask"}, 201)["id"].(string)
		bob.must("POST", "/api/v1/messages/"+foreign+"/reactions", map[string]any{"emoji": "👀"}, 200)
		// alice's own ack on an ask to her
		mine := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice your turn"}, 201)["id"].(string)
		alice.must("POST", "/api/v1/messages/"+mine+"/reactions", map[string]any{"emoji": "👀"}, 200)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	for _, want := range []string{
		"ACKED " + byReaction + " by bob 👀: @bob please review the plan",
		"ACKED " + byAck + " by bob: @bob a broadcast-style ask",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "ACKED "); n != 2 {
		t.Fatalf("printed %d ACKED lines, want 2:\n%s", n, out)
	}
	if strings.Contains(out, `"type":"message.ack"`) || strings.Contains(out, `"type":"message.reaction"`) {
		t.Fatalf("an ack or reaction reached the session as raw JSON:\n%s", out)
	}
}

// An ack that lands while the asker's watcher is down used to sit behind the
// cursor: the next start drained a later mention, jumped past it and never
// printed ACKED. The ack now holds a receipt, so the inbox replays it once.
func TestWatcherHearsAnAckMadeWhileItWasDown(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)
	var ask string
	runWatcherPosting(t, script, home, func() {
		ask = alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob please review the plan"}, 201)["id"].(string)
	})
	bob.must("POST", "/api/v1/messages/"+ask+"/reactions", map[string]any{"emoji": "👀"}, 200)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice a later question"}, 201)

	out := runWatcherResuming(t, script, home, func() {})
	if want := "ACKED " + ask + " by bob 👀: @bob please review the plan"; !strings.Contains(out, want) {
		t.Fatalf("want %q after the restart:\n%s", want, out)
	}
	if !strings.Contains(out, "a later question") {
		t.Fatalf("the later mention should replay too:\n%s", out)
	}
	if again := runWatcherResuming(t, script, home, func() {}); strings.Contains(again, "ACKED ") {
		t.Fatalf("the replayed ack was not acked, so it printed twice:\n%s", again)
	}
}

// A root broadcast is not an ask a reaction can ack, so the hit must print the
// explicit ack, and running that printed command must reach the asker.
func TestWatcherBroadcastAckCommandAcks(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)
	before := fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])
	var broadcast, mention string
	out := runWatcherPosting(t, script, home, func() {
		broadcast = bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel please check the deploy"}, 201)["id"].(string)
		mention = bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice and you the logs"}, 201)["id"].(string)
	})
	if !strings.Contains(out, "| ack: ac react "+mention+" 👀") {
		t.Fatalf("a mention should print the reaction ack:\n%s", out)
	}
	var cmd []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "REPLY-TO ") && strings.Contains(line, "please check the deploy") {
			_, after, _ := strings.Cut(line, " | ack: ac ")
			cmd = strings.Fields(after)
		}
	}
	if len(cmd) != 2 || cmd[0] != "ack" || cmd[1] != broadcast {
		t.Fatalf("a broadcast should print ac ack %s, got %q:\n%s", broadcast, cmd, out)
	}
	env := filepath.Join(home, ".openchatter", "room.alice.env")
	if got, err := runCLI(t, servedCLI(t, srv.URL), env, nil, cmd...); err != nil {
		t.Fatalf("the printed ack failed: %v\n%s", err, got)
	}
	acks := acksSince(t, bob, before)
	if len(acks) != 1 || acks[0]["message_id"] != broadcast || acks[0]["participant_name"] != "alice" {
		t.Fatalf("bob should hear one ack from alice on the broadcast, got %v", acks)
	}
}
