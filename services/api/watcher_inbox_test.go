package api

import (
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
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "thanks", "thread_root_id": id}, 201)
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

	if want := fmt.Sprintf("showing the newest 5 of %d hits", waiting); !strings.Contains(out, want) {
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
