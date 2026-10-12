package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"time"
)

// TestWatcherInboxDrain runs the served watch.sh against the test server: an
// agent that was offline for a mention starts its watcher, which replays the
// inbox, prints the hit the way a live one prints, and acks
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
	script = strings.Replace(script, `BASE="$HOME/.openchatter/<room-slug>.<your-name-with-dashes>"`, `BASE="`+base+`"`, 1)
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
	cmd.Env = append(legacyWatcherEnv, os.Environ()...)
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
	// the one beacon comes before the backlog: it counts the replayed inbox
	// and the capabilities file registered once the inbox is drained
	if l := waitFor("WATCHER-UP:"); !strings.Contains(l, "inbox 1,") || !strings.Contains(l, "1 capabilities registered") {
		t.Fatalf("beacon line: %s", l)
	}
	waitFor("REPLY-TO")
	waitFor("you missed this")
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

// timedLine is one watcher stdout line and when it arrived after the start.
type timedLine struct {
	at   time.Duration
	text string
}

// watcherRun is a watcher started with the 3.0.0 defaults (no legacy env).
type watcherRun struct {
	t     *testing.T
	cmd   *exec.Cmd
	start time.Time
	lines chan timedLine
	exit  chan error
}

// startWatcher runs script from home with extra env and args, streaming its
// stdout line by line. The whole process group dies at cleanup.
func startWatcher(t *testing.T, script, home string, env []string, args ...string) *watcherRun {
	t.Helper()
	path := filepath.Join(home, "watch.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{path}, args...)...)
	cmd.Env = append(append(os.Environ(), "HOME="+home), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &watcherRun{t: t, cmd: cmd, start: time.Now(), lines: make(chan timedLine, 200), exit: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			r.lines <- timedLine{time.Since(r.start), sc.Text()}
		}
		close(r.lines)
		r.exit <- cmd.Wait()
	}()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return r
}

// collect returns every line that arrives within d, or until the watcher exits.
func (r *watcherRun) collect(d time.Duration) []timedLine {
	var got []timedLine
	deadline := time.After(d)
	for {
		select {
		case l, ok := <-r.lines:
			if !ok {
				return got
			}
			got = append(got, l)
		case <-deadline:
			return got
		}
	}
}

// exited reports whether the watcher ended on its own within d.
func (r *watcherRun) exited(d time.Duration) bool {
	select {
	case <-r.exit:
		return true
	case <-time.After(d):
		return false
	}
}

func joined(lines []timedLine) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%6.2fs %s\n", l.at.Seconds(), l.text)
	}
	return b.String()
}

func needWatcherTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"jq", "curl", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
}

func addHuman(t *testing.T, base, secret string) *testClient {
	t.Helper()
	human := &testClient{t: t, base: base}
	out := human.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "maya", "is_human": true}, 201)
	human.token = out["token"].(string)
	return human
}

// A healthy start is silent: each stdout line is a wake, and a start used to
// cost four. The opt-in beacon is exactly one line.
func TestWatcherHealthyStartIsSilent(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)

	w := startWatcher(t, script, home, nil)
	if got := w.collect(3 * time.Second); len(got) != 0 {
		t.Fatalf("a healthy start printed:\n%s", joined(got))
	}
	// SIGKILL leaves the pidfile behind a dead process; a stale one must not block
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	w.exited(3 * time.Second)
	got := startWatcher(t, script, home, []string{"OPENCHATTER_BEACON=1"}).collect(3 * time.Second)
	if len(got) != 1 || !strings.HasPrefix(got[0].text, "WATCHER-UP: pid ") ||
		!strings.Contains(got[0].text, "version 3.0.0, self-test ok, mode=mentions+threads") ||
		!strings.Contains(got[0].text, "debounce 90s, auto-ack 1, one-shot 0") {
		t.Fatalf("want one WATCHER-UP line:\n%s", joined(got))
	}
}

// Agent chatter that does not tag me waits for the debounce and goes out as one
// batch; a mention of me flushes it at once, and so does a human's message.
func TestWatcherDebouncesAgentChatter(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	human := addHuman(t, srv.URL, secret)
	script := watcherTemplate(t, srv.URL) // WATCH="general": every agent message there is a hit
	home := watcherHome(t, srv.URL, alice.token)
	w := startWatcher(t, script, home, []string{"OPENCHATTER_DEBOUNCE_SECS=4"})
	if got := w.collect(1500 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a healthy start printed:\n%s", joined(got))
	}

	posted := time.Since(w.start)
	for i := 1; i <= 3; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("progress %d", i)}, 201)
	}
	got := w.collect(7 * time.Second)
	var hits []timedLine
	for _, l := range got {
		if strings.HasPrefix(l.text, "REPLY-TO ") {
			hits = append(hits, l)
		}
	}
	if len(hits) != 3 {
		t.Fatalf("want the 3 agent messages once each:\n%s", joined(got))
	}
	for _, h := range hits {
		if h.at-posted < 3*time.Second {
			t.Fatalf("agent chatter went out before the debounce:\n%s", joined(got))
		}
		if h.at-hits[0].at > time.Second {
			t.Fatalf("the held messages did not go out as one batch:\n%s", joined(got))
		}
	}

	// held chatter rides out with a mention, well inside the debounce
	posted = time.Since(w.start)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "more progress"}, 201)
	time.Sleep(500 * time.Millisecond)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice your call"}, 201)
	got = w.collect(2500 * time.Millisecond)
	if s := joined(got); !strings.Contains(s, "more progress") || !strings.Contains(s, "your call") {
		t.Fatalf("a mention should flush the held chatter at once:\n%s", s)
	}
	if last := got[len(got)-1]; last.at-posted > 3*time.Second {
		t.Fatalf("the mention waited for the debounce:\n%s", joined(got))
	}

	// a human's message never waits either
	posted = time.Since(w.start)
	human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "human here"}, 201)
	got = w.collect(2500 * time.Millisecond)
	if s := joined(got); !strings.Contains(s, "human here") {
		t.Fatalf("a human message should go out at once:\n%s", s)
	}
	if got[0].at-posted > 3*time.Second {
		t.Fatalf("the human message waited for the debounce:\n%s", joined(got))
	}
}

// The watcher acks each ask it prints, so the asker hears it and no PENDING-ACK
// nag follows. An author on the skip list keeps the printed ack for the agent.
func TestWatcherAutoAcksWhatItPrints(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)
	// the nag would fire every second if auto-ack left it on
	env := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(env, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\nOPENCHATTER_ACK_NAG_SECS=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])

	w := startWatcher(t, script, home, nil)
	time.Sleep(1500 * time.Millisecond)
	ask := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice please look"}, 201)["id"].(string)
	got := joined(w.collect(4 * time.Second))
	if !strings.Contains(got, "please look | acked") || strings.Contains(got, "| ack:") {
		t.Fatalf("the hit should say it was acked:\n%s", got)
	}
	if strings.Contains(got, "PENDING-ACK") {
		t.Fatalf("auto-ack should stop the nag:\n%s", got)
	}
	acks := acksSince(t, bob, before)
	if len(acks) != 1 || acks[0]["message_id"] != ask || acks[0]["participant_name"] != "alice" {
		t.Fatalf("bob should hear alice's ack on his ask, got %v", acks)
	}
	if pending := alice.must("GET", "/api/v1/me/pending-acks", nil, 200)["pending"].([]any); len(pending) != 0 {
		t.Fatalf("nothing should be pending, got %v", pending)
	}
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)

	// bob on the skip list: the line carries the ack for alice to run, nothing is acked
	before = fmt.Sprint(bob.must("GET", "/api/v1/events", nil, 200)["cursor"])
	w = startWatcher(t, script, home, []string{"OPENCHATTER_AUTO_ACK_SKIP=carol bob"})
	time.Sleep(1500 * time.Millisecond)
	skipped := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice by hand"}, 201)["id"].(string)
	got = joined(w.collect(3 * time.Second))
	if !strings.Contains(got, "by hand | ack: ac react "+skipped+" 👀") {
		t.Fatalf("a skipped author should keep the printed ack:\n%s", got)
	}
	if acks := acksSince(t, bob, before); len(acks) != 0 {
		t.Fatalf("a skipped author's ask was acked: %v", acks)
	}
}

// --once exits after the first batch without declaring offline, and the next
// run picks up exactly where it stopped: nothing replayed, nothing lost.
func TestWatcherOneShotHandsOverTheCursor(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)

	w := startWatcher(t, script, home, nil, "--once")
	time.Sleep(1500 * time.Millisecond)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice first"}, 201)
	got := w.collect(5 * time.Second)
	if !w.exited(3*time.Second) || !strings.Contains(joined(got), "first | acked") {
		t.Fatalf("a one-shot run should print its batch and exit:\n%s", joined(got))
	}
	if p := participantByName(t, bob, "alice"); p["declared_offline"] == true {
		t.Fatalf("a one-shot exit must not declare offline: %v", p)
	}
	if _, err := os.Stat(filepath.Join(home, ".openchatter", "room.alice.watch.pid")); !os.IsNotExist(err) {
		t.Fatalf("a one-shot exit should free its pidfile: %v", err)
	}

	// posted between runs: the next run prints it at once, and never "first" again
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice between runs"}, 201)
	w = startWatcher(t, script, home, nil)
	got = w.collect(3 * time.Second)
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	if s := joined(got); !strings.Contains(s, "between runs") || strings.Contains(s, "first") {
		t.Fatalf("the next run should print only what came after the cursor:\n%s", s)
	}

	// ONESHOT=1 is the same switch as --once
	w = startWatcher(t, script, home, []string{"ONESHOT=1"})
	time.Sleep(1500 * time.Millisecond)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice third"}, 201)
	got = w.collect(5 * time.Second)
	if !w.exited(3*time.Second) || !strings.Contains(joined(got), "third") || strings.Contains(joined(got), "between runs") {
		t.Fatalf("ONESHOT=1 should print one batch and exit:\n%s", joined(got))
	}
}

// Held events are not behind the saved cursor yet, so a watcher killed while
// it holds them prints them on its next start.
func TestWatcherKilledWhileHoldingReplays(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := watcherTemplate(t, srv.URL)
	home := watcherHome(t, srv.URL, alice.token)

	w := startWatcher(t, script, home, []string{"OPENCHATTER_DEBOUNCE_SECS=60"})
	time.Sleep(1500 * time.Millisecond)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "held chatter"}, 201)
	if got := w.collect(2 * time.Second); len(got) != 0 {
		t.Fatalf("the chatter should still be held:\n%s", joined(got))
	}
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	w.exited(3 * time.Second)

	w = startWatcher(t, script, home, []string{"OPENCHATTER_DEBOUNCE_SECS=0"})
	if got := joined(w.collect(3 * time.Second)); strings.Count(got, "bob: held chatter") != 1 {
		t.Fatalf("the held message should print once after the restart:\n%s", got)
	}
}

// Agent chatter that piled up between two runs comes back through the inbox and
// waits out the debounce like a live poll, instead of waking the agent at once.
func TestWatcherBacklogChatterWaitsForTheDebounce(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)
	// a first run sets the cursor; the broadcasts land after it
	w := startWatcher(t, script, home, nil)
	time.Sleep(1500 * time.Millisecond)
	_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
	w.exited(3 * time.Second)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel status one"}, 201)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel status two"}, 201)

	w = startWatcher(t, script, home, []string{"OPENCHATTER_DEBOUNCE_SECS=4"}, "--once")
	got := w.collect(8 * time.Second)
	if !w.exited(3 * time.Second) {
		t.Fatalf("the one-shot run should exit after the batch:\n%s", joined(got))
	}
	var hits []timedLine
	for _, l := range got {
		if strings.HasPrefix(l.text, "REPLY-TO ") {
			hits = append(hits, l)
		}
	}
	if len(hits) != 2 || hits[0].at < 3*time.Second {
		t.Fatalf("the backlog should wait for the debounce and print once:\n%s", joined(got))
	}
}

// expireLeases ends every inbox lease of one recipient, as if the minute passed.
func expireLeases(t *testing.T, recipientID string) {
	t.Helper()
	url := os.Getenv("OPENCHATTER_TEST_DB_URL")
	if url == "" {
		url = "postgres://agentchat:agentchat@localhost:5477/agentchat?sslmode=disable"
	}
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(),
		`UPDATE deliveries SET leased_until = now() - interval '1 second' WHERE recipient_id = $1 AND leased_until IS NOT NULL`, recipientID); err != nil {
		t.Fatal(err)
	}
}

// With auto-ack on, an ask the inbox cap held back was never seen, so nothing
// may ack it: its receipt stays open, a later start replays it, and the nag
// stays on for the run that skipped it.
func TestWatcherInboxCapKeepsUnseenAsksOpen(t *testing.T) {
	needWatcherTools(t)
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	for i := 1; i <= 6; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("@alice ask-%02d", i)}, 201)
	}
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	home := watcherHome(t, srv.URL, alice.token)
	env := []string{"OPENCHATTER_INBOX_SHOW=3"}
	pending := func() int { return len(alice.must("GET", "/api/v1/me/pending-acks", nil, 200)["pending"].([]any)) }

	w := startWatcher(t, script, home, env, "--once")
	got := joined(w.collect(6 * time.Second))
	if !w.exited(3*time.Second) || strings.Count(got, "REPLY-TO ") != 3 || !strings.Contains(got, "ask-06 | acked") || strings.Contains(got, "ask-03") {
		t.Fatalf("the first run should show and ack the newest three:\n%s", got)
	}
	if !strings.Contains(got, "older asks stay unacked") {
		t.Fatalf("the cap line should say the older asks stay open:\n%s", got)
	}
	if n := pending(); n != 3 {
		t.Fatalf("the three unseen asks should stay pending, got %d", n)
	}

	// the drain leased the held-back rows for a minute; let that minute pass
	expireLeases(t, alice.must("GET", "/api/v1/me", nil, 200)["id"].(string))
	w = startWatcher(t, script, home, env, "--once")
	got = joined(w.collect(6 * time.Second))
	if !w.exited(3*time.Second) || strings.Count(got, "REPLY-TO ") != 3 || !strings.Contains(got, "ask-01 | acked") || strings.Contains(got, "ask-06") {
		t.Fatalf("a later start should replay the three it held back:\n%s", got)
	}
	if n := pending(); n != 0 {
		t.Fatalf("nothing should stay pending once every ask was shown, got %d", n)
	}

	// a long-running watcher that held asks back keeps nagging about them
	for i := 7; i <= 10; i++ {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": fmt.Sprintf("@alice ask-%02d", i)}, 201)
	}
	w = startWatcher(t, script, home, append(env, "OPENCHATTER_ACK_NAG_SECS=1"))
	if got := joined(w.collect(5 * time.Second)); !strings.Contains(got, "PENDING-ACK: 1 unacked asks") {
		t.Fatalf("the nag should name the one ask the cap held back:\n%s", got)
	}
}
