package api

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// participantByName reads one row out of /api/v1/participants as the caller sees it.
func participantByName(t *testing.T, c *testClient, name string) map[string]any {
	t.Helper()
	out := c.must("GET", "/api/v1/participants", nil, 200)
	for _, raw := range out["participants"].([]any) {
		p := raw.(map[string]any)
		if p["name"] == name {
			return p
		}
	}
	t.Fatalf("no participant %q in the roster", name)
	return nil
}

func bodiesOf(evs []any) []string {
	out := []string{}
	for _, raw := range evs {
		e := raw.(map[string]any)
		out = append(out, e["payload"].(map[string]any)["body"].(string))
	}
	return out
}

func sameStrings(a, b []string) bool {
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

// TestAgentPresence (task 21): an agent declares itself offline, hears nothing
// meanwhile, and gets the missed batch exactly once when it declares online.
func TestAgentPresence(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	bobID := bob.must("GET", "/api/v1/me", nil, 200)["id"].(string)

	// bob writes in a thread before leaving, so a later reply there is his
	root := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob's topic"}, 201)
	rootID := root["id"].(string)
	// what bob's watcher already delivered: the cursor stands here
	_, bobCursor := eventsAfter(t, bob, 0)
	_, aliceCursor := eventsAfter(t, alice, 0)

	if p := participantByName(t, alice, "bob"); p["online"] != true || p["presence"] != "online" {
		t.Fatalf("bob should start online: %v", p)
	}

	// humans do not declare presence
	human := &testClient{t: t, base: srv.URL}
	out := human.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": secret, "name": "maya", "is_human": true,
	}, 201)
	human.token = out["token"].(string)
	if _, out := human.do("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}); out["code"] != "agents_only" {
		t.Fatalf("human presence should be 403 agents_only, got %v", out)
	}
	if st, _ := bob.do("POST", "/api/v1/me/presence", map[string]any{"status": "away"}); st != 400 {
		t.Fatalf("bad status should be 400, got %d", st)
	}

	// offline: roster flips, one presence event, sticky across requests
	if out := bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200); out["status"] != "offline" {
		t.Fatalf("offline reply: %v", out)
	}
	bob.must("GET", "/api/v1/me", nil, 200)
	bob.must("POST", "/api/v1/me/heartbeat", nil, 200)
	if p := participantByName(t, alice, "bob"); p["online"] != false || p["presence"] != "offline" || p["declared_offline"] != true {
		t.Fatalf("bob should be offline after requests: %v", p)
	}
	if out := bob.must("POST", "/api/v1/me/heartbeat", nil, 200); out["status"] != "offline" {
		t.Fatalf("heartbeat while declared offline should say offline: %v", out)
	}
	evs, aliceCursor := eventsAfter(t, alice, aliceCursor)
	if got := presenceChangesFor(evs, bobID); len(got) != 1 || got[0] {
		t.Fatalf("after offline want [false], got %v", got)
	}
	// a repeat offline announces nothing more
	bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	evs, aliceCursor = eventsAfter(t, alice, aliceCursor)
	if got := presenceChangesFor(evs, bobID); len(got) != 0 {
		t.Fatalf("second offline should be silent, got %v", got)
	}

	// traffic while bob is away: a mention, a reply in his thread, a root
	// broadcast, and noise he should never see
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob one"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "not for bob"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "two in bob's thread", "thread_root_id": rootID}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel three"}, 201)
	want := []string{"@bob one", "two in bob's thread", "@channel three"}

	// his poll hands out nothing and leaves the cursor where it was
	poll := bob.must("GET", fmt.Sprintf("/api/v1/events?after=%d&wait=1&relevant=true", bobCursor), nil, 200)
	if n := len(poll["events"].([]any)); n != 0 || int64(poll["cursor"].(float64)) != bobCursor || poll["presence"] != "offline" {
		t.Fatalf("offline poll should be empty at the same cursor: %v", poll)
	}
	// the receipts queued as deferred
	inbox := bob.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)
	deferred := 0
	for _, raw := range inbox["receipts"].([]any) {
		if raw.(map[string]any)["state"] == "deferred" {
			deferred++
		}
	}
	if deferred != 3 {
		t.Fatalf("want 3 deferred receipts, got %d: %v", deferred, inbox["receipts"])
	}

	// online: the batch is exactly the missed set, in order, past his cursor
	on := bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": bobCursor}, 200)
	if on["was_offline"] != true {
		t.Fatalf("online should report was_offline: %v", on)
	}
	if got := bodiesOf(on["events"].([]any)); !sameStrings(got, want) {
		t.Fatalf("online batch want %v, got %v", want, got)
	}
	newCursor := int64(on["cursor"].(float64))
	if newCursor <= bobCursor {
		t.Fatalf("cursor should advance: %d -> %d", bobCursor, newCursor)
	}
	if p := participantByName(t, alice, "bob"); p["online"] != true || p["presence"] != "online" || p["declared_offline"] != nil {
		t.Fatalf("bob should be online again: %v", p)
	}
	evs, aliceCursor = eventsAfter(t, alice, aliceCursor)
	if got := presenceChangesFor(evs, bobID); len(got) != 1 || !got[0] {
		t.Fatalf("after online want [true], got %v", got)
	}

	// a second online returns nothing and announces nothing
	again := bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": bobCursor}, 200)
	if n := len(again["events"].([]any)); n != 0 || again["was_offline"] != false {
		t.Fatalf("second online should be empty: %v", again)
	}
	evs, _ = eventsAfter(t, alice, aliceCursor)
	if got := presenceChangesFor(evs, bobID); len(got) != 0 {
		t.Fatalf("second online should be silent, got %v", got)
	}

	// the watcher resumes from the returned cursor and hears nothing twice,
	// and live traffic flows again
	poll = bob.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", newCursor), nil, 200)
	if got := bodiesOf(poll["events"].([]any)); len(got) != 0 {
		t.Fatalf("nothing should replay after the catch-up, got %v", got)
	}
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob four"}, 201)
	poll = bob.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", newCursor), nil, 200)
	if got := bodiesOf(poll["events"].([]any)); !sameStrings(got, []string{"@bob four"}) {
		t.Fatalf("live traffic should flow after online, got %v", got)
	}

	// the cursor dedupes: what a poll delivered before going offline is not
	// in the batch, even though it lands after the offline mark
	bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	// (the offline mark sits before "five"; bob's cursor already covers it)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob five"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob six"}, 201)
	_, seenCursor := eventsAfter(t, alice, 0)
	seenCursor-- // "six" is the last event: pretend the watcher delivered up to "five"
	on = bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": seenCursor}, 200)
	if got := bodiesOf(on["events"].([]any)); !sameStrings(got, []string{"@bob six"}) {
		t.Fatalf("batch should start past the caller's cursor, got %v", got)
	}
	// without a cursor the batch starts at the offline mark
	bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob seven"}, 201)
	on = bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online"}, 200)
	if got := bodiesOf(on["events"].([]any)); !sameStrings(got, []string{"@bob seven"}) {
		t.Fatalf("batch without a cursor should start at the offline mark, got %v", got)
	}

	// a cursor below the offline mark wins: what the agent never heard before
	// it went offline is in the batch too (review finding, task 21)
	_, before := eventsAfter(t, alice, 0)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob eight"}, 201)
	bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob nine"}, 201)
	on = bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": before}, 200)
	if got := bodiesOf(on["events"].([]any)); !sameStrings(got, []string{"@bob eight", "@bob nine"}) {
		t.Fatalf("batch should start at the caller's cursor when it sits below the offline mark, got %v", got)
	}

	// online while not offline echoes the caller's cursor, so a stale cursor
	// file is never pushed past what the poll still owes
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob ten"}, 201)
	on = bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": before}, 200)
	if on["was_offline"] != false || int64(on["cursor"].(float64)) != before {
		t.Fatalf("online while online should echo the caller's cursor %d, got %v", before, on)
	}
	poll = bob.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", before), nil, 200)
	if got := bodiesOf(poll["events"].([]any)); !sameStrings(got, []string{"@bob eight", "@bob nine", "@bob ten"}) {
		t.Fatalf("the poll should still owe everything past the stale cursor, got %v", got)
	}
}

// TestWatcherTemplateDeclaresPresence (task 21): the served watcher declares
// online when it starts, prints what was missed while declared offline exactly
// once, and declares offline on SIGTERM.
func TestWatcherTemplateDeclaresPresence(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := watcherTemplate(t, srv.URL)
	// mentions-only, as served: the online batch is the poll it would have made
	script = strings.Replace(script, `WATCH="general" #`, `WATCH="" #`, 1)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".openchatter", "room.alice.env"), []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// alice's last watcher stood here, then she went offline and missed one
	_, cursor := eventsAfter(t, alice, 0)
	if err := os.WriteFile(filepath.Join(home, ".openchatter", "room.alice.cursor"), []byte(fmt.Sprint(cursor)), 0o600); err != nil {
		t.Fatal(err)
	}
	alice.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice while you were out"}, 201)
	// the deferred receipt is drained by the inbox first; the batch must not repeat it

	path := filepath.Join(home, "watch.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if p := participantByName(t, bob, "alice"); p["online"] != true {
		t.Fatalf("watcher start should declare alice online: %v", p)
	}
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice now live"}, 201)
	time.Sleep(1500 * time.Millisecond)
	// SIGTERM to the script only: the trap must declare offline and exit on its own
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("watcher did not exit on SIGTERM:\n%s", out.String())
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // a stray curl child
	got := out.String()
	for _, want := range []string{"WATCHER-ONLINE", "while you were out", "now live", "WATCHER-OFFLINE"} {
		if !strings.Contains(got, want) {
			t.Fatalf("watcher output lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "REPLY-TO "); n != 2 {
		t.Fatalf("want exactly 2 hits (missed one once, live one once), got %d:\n%s", n, got)
	}
	if strings.Contains(got, "WATCHER-ERROR") {
		t.Fatalf("watcher errored:\n%s", got)
	}
	if p := participantByName(t, bob, "alice"); p["online"] != false || p["declared_offline"] != true {
		t.Fatalf("SIGTERM should declare alice offline: %v", p)
	}
}
