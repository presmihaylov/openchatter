package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWatcherInboxDrain runs the served watch.sh against the test server: an
// agent that was offline for a mention starts its watcher, which replays the
// inbox (WATCHER-INBOX), prints the hit the way a live one prints, and acks
// it only after that, so the owner's stats show it handled. A live mention
// during the poll loop is acked too.
func TestWatcherInboxDrain(t *testing.T) {
	for _, bin := range []string{"jq", "curl", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	resp, err := http.Get(srv.URL + "/skill/watch.sh")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	dir := t.TempDir()
	base := filepath.Join(dir, "room.bob")
	script := string(raw)
	script = strings.Replace(script, `ME="<your-name>"`, `ME="bob"`, 1)
	script = strings.Replace(script, `BASE="$HOME/.openflock/<room-slug>.<your-name-with-dashes>"`, `BASE="`+base+`"`, 1)
	if err := os.WriteFile(base+".env", []byte("SERVER="+srv.URL+"\nTOKEN="+bob.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "watch.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// task 27: a capabilities file next to the env file is registered on start
	caps := `[{"name":"echo","description":"echoes","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]`
	if err := os.WriteFile(base+".capabilities.json", []byte(caps), 0o600); err != nil {
		t.Fatal(err)
	}

	// bob was offline when alice tagged him: the receipt is deferred
	bob.must("POST", "/api/v1/me/offline", nil, 200)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob you missed this"}, 201)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(dir, "watch.sh"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	lines := make(chan string, 100)
	go func() {
		buf := make([]byte, 64<<10)
		acc := ""
		for {
			n, err := stdout.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexByte(acc, '\n')
				if i < 0 {
					break
				}
				lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	waitFor := func(prefix string) string {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("watcher exited before %q", prefix)
				}
				if strings.Contains(l, prefix) {
					return l
				}
			case <-ctx.Done():
				t.Fatalf("timeout waiting for %q", prefix)
			}
		}
	}
	waitFor("WATCHER-SELFTEST-OK")
	if l := waitFor("WATCHER-INBOX:"); !strings.Contains(l, "1 unacked") {
		t.Fatalf("inbox line: %s", l)
	}
	waitFor("REPLY-TO")
	waitFor("you missed this")
	// the capabilities file is registered once the inbox is drained
	if l := waitFor("WATCHER-CAPS:"); !strings.Contains(l, "1 registered") {
		t.Fatalf("caps line: %s", l)
	}
	if n := len(bob.must("GET", "/api/v1/participants/me/capabilities", nil, 200)["capabilities"].([]any)); n != 1 {
		t.Fatalf("watcher did not register the capability: %d", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
		if st["acked"].(float64) == 1 && st["pending"].(float64) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inbox replay never acked: %v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// a live mention goes through the poll loop and is acked the same way
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob and this one live"}, 201)
	waitFor("and this one live")
	deadline = time.Now().Add(10 * time.Second)
	for {
		st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
		if st["acked"].(float64) == 2 && st["pending"].(float64) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live hit never acked: %v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// a capability call aimed at bob comes through the same loop and is acked;
	// alice's own registration and a call aimed at alice never wake bob
	alice.must("PUT", "/api/v1/me/capabilities", map[string]any{"capabilities": []map[string]any{{"name": "draw", "description": "draws", "inputSchema": map[string]any{"type": "object"}}}}, 200)
	call := alice.must("POST", "/api/v1/capabilities/call?wait=false", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "ping"}, "timeoutSeconds": 30}, 202)
	l := waitFor("CAPABILITY-CALL")
	if !strings.Contains(l, "call="+call["call_id"].(string)) || !strings.Contains(l, "name=echo") || !strings.Contains(l, `"q":"ping"`) {
		t.Fatalf("call line: %s", l)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
		if st["acked"].(float64) == 3 && st["pending"].(float64) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call receipt never acked: %v", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = fmt.Sprint
}
