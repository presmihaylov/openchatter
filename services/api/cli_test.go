package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCLIScriptServed(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /cli.sh: got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("content-type = %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	script := string(raw)

	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Fatal("the served script has no shebang")
	}
	// a downloaded copy must already point at this server
	if strings.Contains(script, "{{SERVER}}") {
		t.Fatal("cli.sh still contains an unsubstituted {{SERVER}} placeholder")
	}
	if !strings.Contains(script, "http://public.test") {
		t.Fatal("cli.sh did not substitute the public URL")
	}

	// every verb an agent needs, so a trimmed-down edit cannot ship silently
	for _, want := range []string{
		"cmd_send()", "cmd_reply()", "cmd_read()", "cmd_thread()",
		"cmd_msg()", "cmd_mentions()", "cmd_channels()", "cmd_members()", "cmd_whoami()",
		"cmd_react()", "cmd_reactions()", "cmd_download()", "cmd_join()", "cmd_leave()", "cmd_rejoin()",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cli.sh is missing %q", want)
		}
	}
	for _, removed := range []string{"cmd_broadcast()", "broadcast <channel> <body>", `"broadcast":`} {
		if strings.Contains(script, removed) {
			t.Errorf("the served CLI still exposes hidden broadcasts (%q present)", removed)
		}
	}
	// the token is never printed, so it can never leak through an error path
	if strings.Contains(script, "echo $TOKEN") || strings.Contains(script, "printf '%s' \"$TOKEN\"") {
		t.Error("cli.sh prints the token")
	}

	// it must parse as bash exactly as served, not just in the repo
	path := filepath.Join(t.TempDir(), "cli.sh")
	if err := os.WriteFile(path, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n cli.sh: %v\n%s", err, out)
	}
	// --help works with no config at all, so a first-time agent is never stuck
	cmd := exec.Command("bash", path, "--help")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cli.sh --help: %v\n%s", err, out)
	}
	for _, want := range []string{"reply <message-id>", "reply --latest <channel>", "--new-topic", "EVERYTHING ELSE IS A REPLY"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the help text is missing %q:\n%s", want, out)
		}
	}
	// the top-level caution and the root/reply tags are what make threads the
	// easy path; a script that lost them ships silent agents again
	for _, want := range []string{"caution, top-level post", "--new-topic", "reply in thread", "root, no replies yet", "latest_thread_in"} {
		if !strings.Contains(script, want) {
			t.Errorf("cli.sh is missing %q", want)
		}
	}

	// the skill must point agents at the CLI, or nobody ever downloads it
	sk, err := http.Get(srv.URL + "/skill")
	if err != nil {
		t.Fatal(err)
	}
	defer sk.Body.Close()
	skill, _ := io.ReadAll(sk.Body)
	for _, want := range []string{"/cli.sh", "canonical", "ac reply <message-id>", "ac reply --latest <channel>",
		"A root starts a topic, everything else is a reply", "A timed loop posts ONE root per day", "A root is a headline; the bulk goes in its thread", "reply_to"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("the skill does not reference the CLI (%q missing)", want)
		}
	}
	// the watcher template must surface the thread to answer in on every hit
	cc, err := http.Get(srv.URL + "/skill/claude-code")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Body.Close()
	ccDoc, _ := io.ReadAll(cc.Body)
	for _, want := range []string{"REPLY-TO", ".payload.reply_to", "\"reply_to\":\"...\""} {
		if !strings.Contains(string(ccDoc), want) {
			t.Errorf("the claude-code skill is missing %q", want)
		}
	}

}

// The app must not know or care what sits in front of it, so no served
// surface names a proxy vendor or carries a slot for its credentials.
func TestServedSurfacesNameNoProxy(t *testing.T) {
	srv, _ := newTestServer(t)
	surfaces := []string{"/cli.sh", "/skill", "/skill/claude-code", "/skill/hermes",
		"/skill/watch.sh", "/skill/bridge.sh", "/skill/inject.sh"}
	for _, g := range harnessGuides {
		surfaces = append(surfaces, "/skill/"+g.slug)
	}
	vendor := regexp.MustCompile(`(?i)cloudflare|cf-access|cf_access|\{\{CF_`)
	for _, surface := range surfaces {
		if m := vendor.FindString(getText(t, srv.URL+surface)); m != "" {
			t.Errorf("%s names a proxy vendor: %q", surface, m)
		}
	}
}

// frontProxy passes every request through and records any header beyond what
// the client is meant to send, so a test can prove nothing proxy-specific leaks.
func frontProxy(t *testing.T, upstream string) (*httptest.Server, func() []string) {
	t.Helper()
	target, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var extra []string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "cf-") {
				mu.Lock()
				extra = append(extra, name)
				mu.Unlock()
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), extra...)
	}
}

// redirectGate stands in for an auth proxy that bounces every request to its
// own login page instead of letting it reach the room.
func redirectGate(t *testing.T) *httptest.Server {
	t.Helper()
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.example.com/sign-in", http.StatusFound)
	}))
	t.Cleanup(gate.Close)
	return gate
}

// servedCLI writes the served cli.sh to a temp dir and returns its path.
func servedCLI(t *testing.T, base string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli.sh")
	if err := os.WriteFile(path, []byte(getText(t, base+"/cli.sh")), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "room.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An env file written for the old proxy-aware CLI still holds two credential
// lines. They are inert now: the CLI sends the bearer token and nothing else.
func TestCLISendsOnlyTheBearerToken(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	front, extra := frontProxy(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	env := writeEnv(t, "SERVER="+front.URL+"\nTOKEN="+alice.token+
		"\nCF_ACCESS_CLIENT_ID=stale-id\nCF_ACCESS_CLIENT_SECRET=stale-zz9\n")
	out, err := exec.Command("bash", cli, "--env", env, "whoami").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "alice ") {
		t.Fatalf("whoami through a plain proxy: %v\n%s", err, out)
	}
	if got := extra(); len(got) > 0 {
		t.Fatalf("the CLI sent proxy headers: %v", got)
	}
}

// The API never redirects, so a 3xx came from whatever sits in front of it.
// The CLI says that, instead of blaming the token, and never prints the token.
func TestCLINamesARedirectFromTheFront(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	env := writeEnv(t, "SERVER="+redirectGate(t).URL+"\nTOKEN="+alice.token+"\n")
	out, err := exec.Command("bash", cli, "--env", env, "whoami").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "redirect (HTTP 302)") {
		t.Fatalf("a redirect should be named as one: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "rejected the token") || strings.Contains(string(out), alice.token) {
		t.Fatalf("a redirect is not a token problem, and the token stays secret:\n%s", out)
	}
}

// The invite answer is the same whatever sits in front of the server.
func TestInviteCarriesNoProxyBlock(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	if _, has := alice.must("POST", "/api/v1/invites", nil, 201)["access"]; has {
		t.Fatal("the invite must not return an access block")
	}
}

// watcherTemplate pulls the persistent watcher script out of the served
// claude-code reference, so the test runs exactly what an agent would copy.
func watcherTemplate(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/skill/claude-code")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	start := strings.Index(page, "    #!/bin/sh\n")
	if start < 0 {
		t.Fatal("no watcher template in /skill/claude-code")
	}
	// the block ends at the last "done" before the next heading
	end := strings.Index(page[start:], "\n## ")
	if end < 0 {
		t.Fatal("watcher template has no end")
	}
	block := page[start : start+end]
	block = block[:strings.LastIndex(block, "    done\n")+len("    done\n")]
	var lines []string
	for _, l := range strings.Split(block, "\n") {
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	script := strings.Join(lines, "\n")
	for from, to := range map[string]string{
		"<room-slug>.<your-name-with-dashes>": "room.alice",
		"<your-name>":                         "alice",
	} {
		script = strings.ReplaceAll(script, from, to)
	}
	// the served default is WATCH=""; most template tests hear #general in full
	script = strings.Replace(script, `WATCH="" #`, `WATCH="general" #`, 1)
	if strings.Contains(script, "<room-slug>") || strings.Contains(script, "<your-") {
		t.Fatalf("unfilled placeholder in the template:\n%s", script)
	}
	return script
}

// runWatcher runs the template for a few seconds with bob posting once, and
// returns everything it printed.
func runWatcher(t *testing.T, script, home string, bob *testClient) string {
	t.Helper()
	return runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice are you there"}, 201)
	})
}

// runWatcherPosting runs the template for a few seconds, calls post once it is
// up, and returns everything it printed.
func runWatcherPosting(t *testing.T, script, home string, post func()) string {
	t.Helper()
	path := filepath.Join(home, "watch.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(home, ".openchatter", "room.alice.cursor"))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", path)
	cmd.Env = append(os.Environ(), "HOME="+home)
	// kill the whole group, or a long-polling curl child keeps stdout open
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	post()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	_ = cmd.Wait()
	return out.String()
}

func TestWatcherTemplateWorksBehindAnyProxy(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	front, extra := frontProxy(t, srv.URL)
	script := watcherTemplate(t, srv.URL)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	write := func(body string) {
		if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// through a plain reverse proxy, with stale proxy credentials left in the env file
	write("SERVER=" + front.URL + "\nTOKEN=" + alice.token + "\nCF_ACCESS_CLIENT_ID=stale-id\nCF_ACCESS_CLIENT_SECRET=stale-zz9\n")
	out := runWatcher(t, script, home, bob)
	if !strings.Contains(out, "are you there") || strings.Contains(out, "WATCHER-ERROR") {
		t.Fatalf("watcher behind a proxy should hear bob cleanly:\n%s", out)
	}
	for _, beacon := range []string{"WATCHER-UP", "WATCHER-SELFTEST-OK", "WATCHER-SCOPE", "REPLY-TO "} {
		if !strings.Contains(out, beacon) {
			t.Fatalf("watcher output lacks %s:\n%s", beacon, out)
		}
	}
	if got := extra(); len(got) > 0 {
		t.Fatalf("the watcher sent proxy headers: %v", got)
	}
	if strings.Contains(out, "stale-zz9") {
		t.Fatalf("watcher printed an env secret:\n%s", out)
	}

	// straight to the server works the same
	write("SERVER=" + srv.URL + "\nTOKEN=" + alice.token + "\n")
	out = runWatcher(t, script, home, bob)
	if !strings.Contains(out, "are you there") || strings.Contains(out, "WATCHER-ERROR") {
		t.Fatalf("direct watcher should hear bob cleanly:\n%s", out)
	}

	// a front that answers with its own login page is loud, not silent
	write("SERVER=" + redirectGate(t).URL + "\nTOKEN=" + alice.token + "\n")
	out = runWatcher(t, script, home, bob)
	if !strings.Contains(out, "WATCHER-ERROR") || strings.Contains(out, "are you there") {
		t.Fatalf("a redirecting front should stop the watcher loudly:\n%s", out)
	}
}

func TestSkillRawCurlsCarryCredentialsConfig(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, page := range []string{"/skill", "/skill/claude-code"} {
		resp, err := http.Get(srv.URL + page)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		// fold continued lines so a header on the next line counts
		joined := strings.ReplaceAll(string(raw), " \\\n", " ")
		for _, line := range strings.Split(joined, "\n") {
			if !strings.Contains(line, "curl") || !strings.Contains(line, "$SERVER/api/") {
				continue
			}
			// the join runs before there is a token to carry
			if strings.Contains(line, `-K "$CURLRC"`) || strings.Contains(line, "/api/v1/rooms/join") {
				continue
			}
			t.Errorf("%s: raw curl without -K \"$CURLRC\": %s", page, strings.TrimSpace(line))
		}
	}
	resp, err := http.Get(srv.URL + "/skill/hermes")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `req.add_header("Authorization", "Bearer " + TOKEN)`) {
		t.Error("hermes helper does not send the bearer token")
	}
}

// TestWatcherTemplateNagsAboutUnackedAsks: an ask nobody acked must keep coming
// back. The nag is a wake on purpose (task 32), so its format is load-bearing:
// the id, who asked, where, and the command that stops it.
func TestWatcherTemplateNagsAboutUnackedAsks(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := watcherTemplate(t, srv.URL)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	env := "SERVER=" + srv.URL + "\nTOKEN=" + alice.token + "\nOPENCHATTER_ACK_NAG_SECS=1\n"
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	var ask map[string]any
	out := runWatcherPosting(t, script, home, func() {
		ask = bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice please look"}, 201)
	})
	want := "PENDING-ACK: 1 unacked asks: " + ask["id"].(string) + " from bob in #general. ack: ac ack <id>"
	if !strings.Contains(out, want) {
		t.Fatalf("watcher did not nag with %q:\n%s", want, out)
	}

	// acking it stops the nag: the next run is silent
	alice.must("POST", "/api/v1/messages/"+ask["id"].(string)+"/ack", nil, 200)
	out = runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "no ask here"}, 201)
	})
	if strings.Contains(out, "PENDING-ACK") {
		t.Fatalf("watcher nagged with nothing pending:\n%s", out)
	}
}

// TestWatcherTemplateHearsAllRepliesInOwnThreads: with WATCH empty (no channel
// heard in full), every human and agent reply in a thread alice wrote in
// surfaces exactly once. Alice's own reply does not.
func TestWatcherTemplateHearsAllRepliesInOwnThreads(t *testing.T) {
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
	if !strings.Contains(script, `WATCH=""`) {
		t.Fatal("could not empty WATCH in the template")
	}
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "my topic"}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "plain top-level, not for alice"}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "agent follow-up", "thread_root_id": root["id"].(string)}, 201)
		alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "own follow-up", "thread_root_id": root["id"].(string)}, 201)
		human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "human follow-up", "thread_root_id": root["id"].(string)}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "WATCHER-SELFTEST-OK") || strings.HasPrefix(line, "WATCHER-SCOPE") {
			t.Log(line)
		}
	}
	if !strings.Contains(out, "REPLY-TO "+root["id"].(string)) ||
		!strings.Contains(out, "human follow-up") || !strings.Contains(out, "agent follow-up") {
		t.Fatalf("watcher missed a participant-thread reply:\n%s", out)
	}
	if n := strings.Count(out, "REPLY-TO "+root["id"].(string)); n != 2 {
		t.Fatalf("thread replies woke the watcher %d times, want twice:\n%s", n, out)
	}
	// the ack nudge names the message that tagged you, never the thread root:
	// an ack on the root would land on the wrong message (task 30, task 32)
	if !strings.Contains(out, "| ack: ac ack ") || strings.Contains(out, "| ack: ac ack "+root["id"].(string)) {
		t.Fatalf("REPLY-TO line lacks the ack nudge, or points it at the root:\n%s", out)
	}
	for _, quiet := range []string{"plain top-level", "own follow-up"} {
		if strings.Contains(out, quiet) {
			t.Fatalf("watcher with WATCH=\"\" leaked %q:\n%s", quiet, out)
		}
	}
}

// A legacy human-thread toggle cannot silence replies. Leaving the thread is
// the only opt-out; an old env file must still wake for both author kinds.
func TestWatcherTemplateThreadRepliesCannotBeDisabled(t *testing.T) {
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
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "my topic"}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	env := "SERVER=" + srv.URL + "\nTOKEN=" + alice.token + "\nOPENCHATTER_HUMAN_THREAD_REPLIES=0\n"
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "human follow-up still wakes", "thread_root_id": root["id"].(string)}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "agent follow-up still wakes", "thread_root_id": root["id"].(string)}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice direct still wakes"}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean with a legacy toggle in the env:\n%s", out)
	}
	for _, want := range []string{"human follow-up still wakes", "agent follow-up still wakes", "direct still wakes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("legacy toggle silenced %q:\n%s", want, out)
		}
	}
}

// TestWatcherTemplateDropsReactions: a reaction never wakes a watcher, not
// even one on its own message (a token measure); the poll excludes them
// server-side and the filter drops any that arrive. A mention still gets through.
func TestWatcherTemplateDropsReactions(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	if !strings.Contains(script, "EXCLUDE=\"message.ack,message.reaction,") {
		t.Fatal("template poll does not ask the server to drop acks and reactions")
	}
	mine := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "my post"}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/messages/"+mine["id"].(string)+"/reactions", map[string]any{"emoji": "👀"}, 200)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice after the reaction"}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if !strings.Contains(out, "mode=mentions+threads") {
		t.Fatalf("scope beacon does not name the default participant-thread mode:\n%s", out)
	}
	if !strings.Contains(out, "after the reaction") {
		t.Fatalf("watcher missed the mention:\n%s", out)
	}
	if strings.Contains(out, `"type":"message.reaction"`) || strings.Contains(out, "REACTION ") {
		t.Fatalf("a reaction woke the watcher:\n%s", out)
	}
}

// TestWatcherTemplateRootBroadcastsOnly: a broadcast wakes a default-scope
// watcher at the root, not when posted inside a thread it never wrote in.
func TestWatcherTemplateRootBroadcastsOnly(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	root := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob's own topic"}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel inside bob's thread", "thread_root_id": root["id"].(string)}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel at the root"}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if !strings.Contains(out, "at the root") {
		t.Fatalf("watcher missed a root broadcast:\n%s", out)
	}
	if strings.Contains(out, "inside bob's thread") {
		t.Fatalf("a broadcast inside a foreign thread woke the watcher:\n%s", out)
	}
}

// TestWatcherTemplateRefusesWrongName: ME is compared byte for byte, so a
// watcher started as "Alice" for the participant "alice" would pass every probe
// and never hear a mention. It must refuse to start instead.
func TestWatcherTemplateRefusesWrongName(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `ME="alice"`, `ME="Alice"`, 1)
	if !strings.Contains(script, `ME="Alice"`) {
		t.Fatalf("could not recase ME in the template:\n%s", script)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcher(t, script, home, bob)
	if !strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, `knows this token as "alice"`) {
		t.Fatalf("a wrong-case ME must refuse to start and name the real name:\n%s", out)
	}
	if strings.Contains(out, "WATCHER-SELFTEST-OK") || strings.Contains(out, "are you there") {
		t.Fatalf("watcher ran on with a wrong ME:\n%s", out)
	}
}

// TestWatcherTemplateDropsSystemEntries: "bob left this thread" is a timeline
// entry in a thread alice wrote in; it must not wake her.
func TestWatcherTemplateDropsSystemEntries(t *testing.T) {
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
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alice's topic"}, 201)
	rootID := root["id"].(string)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob's part", "thread_root_id": rootID}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		bob.must("POST", "/api/v1/threads/"+rootID+"/leave", map[string]any{"left": true}, 200)
		human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "maya is back with a question", "thread_root_id": rootID}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if strings.Contains(out, "left this thread") {
		t.Fatalf("a timeline entry woke the watcher:\n%s", out)
	}
	if !strings.Contains(out, "back with a question") {
		t.Fatalf("watcher missed a real reply in its own thread:\n%s", out)
	}
}

// TestWatcherTemplateWakeHookOptIn: a second prompt per event is a full extra
// turn under Monitor, so the template only runs a wake hook when
// OPENCHATTER_WAKE_CMD is set, and names no harness; the doc says so.
func TestWatcherTemplateWakeHookOptIn(t *testing.T) {
	srv, _ := newTestServer(t)
	script := watcherTemplate(t, srv.URL)
	if !strings.Contains(script, `WAKE_CMD="${OPENCHATTER_WAKE_CMD:-}"`) ||
		!strings.Contains(script, `if [ -n "$WAKE_CMD" ]`) {
		t.Fatalf("wake hook is not gated on OPENCHATTER_WAKE_CMD:\n%s", script)
	}
	if strings.Contains(strings.ToLower(script), "herdr") {
		t.Fatalf("template is harness-specific:\n%s", script)
	}
	resp, err := http.Get(srv.URL + "/skill/claude-code")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "Wake hook, OPT-IN") {
		t.Fatal("skill doc does not say the wake hook is opt-in")
	}
	if strings.Contains(strings.ToLower(string(raw)), "herdr") {
		t.Fatal("skill doc is harness-specific")
	}
}

// gatedRun drives the watcher template through a proxy whose /events answer
// a test can swap for a fault.
type gatedRun struct {
	setFault func(http.HandlerFunc) // nil lets /events reach the server
	failed   func() int
	post     func(string)
	cursor   func() string
	stop     func() string
}

// badGateway is a proxy 502 page whose request id differs on every hit.
func badGateway(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusBadGateway)
	w.Write([]byte("<html>Bad gateway, request " + strconv.FormatInt(time.Now().UnixNano(), 10) + "</html>"))
}

func gatedWatcher(t *testing.T, extraEnv ...string) gatedRun {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var fault http.HandlerFunc
	fails := 0
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fault
		if f != nil && r.URL.Path == "/api/v1/events" {
			fails++
		}
		mu.Unlock()
		if f != nil && r.URL.Path == "/api/v1/events" {
			f(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	script := watcherTemplate(t, gate.URL)
	for from, to := range map[string]string{
		"wait=25":          "wait=1",
		`sleep "$BACKOFF"`: "sleep 1",
	} {
		if !strings.Contains(script, from) {
			t.Fatalf("template no longer contains %q:\n%s", from, script)
		}
		script = strings.ReplaceAll(script, from, to)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+gate.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "watch.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, "sh", path)
	cmd.Env = append(append(os.Environ(), "HOME="+home), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	return gatedRun{
		setFault: func(f http.HandlerFunc) { mu.Lock(); fault = f; mu.Unlock() },
		failed:   func() int { mu.Lock(); defer mu.Unlock(); return fails },
		post: func(body string) {
			bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": body}, 201)
		},
		cursor: func() string {
			b, _ := os.ReadFile(filepath.Join(home, ".openchatter", "room.alice.cursor"))
			return strings.TrimSpace(string(b))
		},
		stop: func() string {
			cancel()
			_ = cmd.Wait()
			return out.String()
		},
	}
}

// Changing proxy errors must not create repeated outage alerts or lose queued messages.
func TestWatcherTemplateBacksOffOnOutage(t *testing.T) {
	g := gatedWatcher(t, "OPENCHATTER_OUTAGE_QUIET=2")
	g.setFault(badGateway)
	time.Sleep(5 * time.Second)
	g.post("@alice posted while you were down")
	g.setFault(nil)
	n := g.failed()
	time.Sleep(3 * time.Second)
	got := g.stop()
	if n < 3 {
		t.Fatalf("expected at least 3 failed polls, got %d:\n%s", n, got)
	}
	if c := strings.Count(got, "WATCHER-ERROR: HTTP 502"); c != 1 {
		t.Fatalf("want exactly one WATCHER-ERROR for %d failed polls, got %d:\n%s", n, c, got)
	}
	if c := strings.Count(got, "WATCHER-BACK: server back after"); c != 1 {
		t.Fatalf("want exactly one WATCHER-BACK line, got %d:\n%s", c, got)
	}
	if !strings.Contains(got, "posted while you were down") {
		t.Fatalf("a mention posted during the outage was lost:\n%s", got)
	}
}

// Brief outages must stay quiet without losing queued messages.
func TestWatcherTemplateSilentUnderFiveMinutes(t *testing.T) {
	g := gatedWatcher(t)
	g.setFault(badGateway)
	deadline := time.Now().Add(6 * time.Second)
	for g.failed() < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := g.failed(); n < 3 {
		t.Fatalf("wanted several failed polls inside the window, got %d", n)
	}
	g.setFault(nil)
	g.post("@alice posted during the blip")
	time.Sleep(3 * time.Second)
	got := g.stop()
	if strings.Contains(got, "WATCHER-ERROR: HTTP 502") || strings.Contains(got, "WATCHER-BACK") {
		t.Fatalf("an outage inside the quiet window must be silent:\n%s", got)
	}
	if !strings.Contains(got, "posted during the blip") {
		t.Fatalf("a mention posted during the blip was lost:\n%s", got)
	}
}

// A redirect or token error can carry a cursor-shaped body; it must not move the
// cursor, and the error must name which of the two it was.
func TestWatcherTemplateTrustsOnlyA2xxPoll(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusFound, "WATCHER-ERROR: HTTP 302 redirect"},
		{http.StatusUnauthorized, "WATCHER-ERROR: HTTP 401 token rejected"},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			g := gatedWatcher(t, "OPENCHATTER_OUTAGE_QUIET=0")
			before := g.cursor()
			g.setFault(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://login.example.com/sign-in")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				w.Write([]byte(`{"cursor":999999,"events":[]}`))
			})
			deadline := time.Now().Add(6 * time.Second)
			for g.failed() < 2 && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			after := g.cursor()
			got := g.stop()
			if before == "" || after != before {
				t.Fatalf("HTTP %d moved the cursor from %q to %q:\n%s", tc.code, before, after, got)
			}
			if c := strings.Count(got, tc.want); c != 1 {
				t.Fatalf("want one %q line, got %d:\n%s", tc.want, c, got)
			}
		})
	}
}

// A proxy login page in place of an attachment, or a directory at the target
// name, must fail the download and leave what is already on disk untouched.
func TestCLIDownloadRefusesARedirectOrADirectory(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("the real attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	direct := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n")
	sent, err := exec.Command("bash", cli, "--env", direct, "--json", "send", "general", "@bob see attached", "--attach", src).Output()
	if err != nil {
		t.Fatalf("send with an attachment: %v\n%s", err, sent)
	}
	id := regexp.MustCompile(`"id": "([0-9a-f-]{36})"`).FindStringSubmatch(string(sent))
	if id == nil {
		t.Fatalf("no message id in the send output:\n%s", sent)
	}

	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/attachments/") {
			http.Redirect(w, r, "https://login.example.com/sign-in", http.StatusFound)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	saveDir := t.TempDir()
	kept := filepath.Join(saveDir, "notes.txt")
	if err := os.WriteFile(kept, []byte("an earlier copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	gated := writeEnv(t, "SERVER="+gate.URL+"\nTOKEN="+alice.token+"\n")
	out, err := exec.Command("bash", cli, "--env", gated, "--out", saveDir, "download", id[1]).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "redirect (HTTP 302)") {
		t.Fatalf("a redirected download must fail as a redirect: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(kept); string(b) != "an earlier copy" {
		t.Fatalf("the redirect overwrote the earlier copy with %q", b)
	}
	if entries, _ := os.ReadDir(saveDir); len(entries) != 1 {
		t.Fatalf("the failed download left files behind: %v", entries)
	}

	out, err = exec.Command("bash", cli, "--env", direct, "--out", saveDir, "download", id[1]).CombinedOutput()
	if err != nil {
		t.Fatalf("a clean download: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(kept); string(b) != "the real attachment" {
		t.Fatalf("a clean download saved %q", b)
	}

	// mv into an existing directory (or a symlink to one) succeeds silently.
	for _, link := range []bool{false, true} {
		dirOut := t.TempDir()
		blocker := filepath.Join(dirOut, "notes.txt")
		if link {
			if err := os.Symlink(t.TempDir(), blocker); err != nil {
				t.Fatal(err)
			}
		}
		if !link {
			if err := os.Mkdir(blocker, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		out, err = exec.Command("bash", cli, "--env", direct, "--out", dirOut, "download", id[1]).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "is a directory") {
			t.Fatalf("symlink=%v: a directory target must fail: %v\n%s", link, err, out)
		}
		if entries, _ := os.ReadDir(blocker); len(entries) != 0 {
			t.Fatalf("symlink=%v: the download landed inside the directory: %v", link, entries)
		}
		if entries, _ := os.ReadDir(dirOut); len(entries) != 1 {
			t.Fatalf("symlink=%v: the failed download left files behind: %v", link, entries)
		}
	}
}

// TestWatcherTemplateDropsBenignEvents: a member leaving and rejoining a
// channel, and an edit or delete of someone else's message, are not for alice.
// Before the exclude list grew, each one woke every mentions-only agent as raw
// JSON. A real mention in the same window must still surface.
func TestWatcherTemplateDropsBenignEvents(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	if !strings.Contains(script, `WATCH=""`) {
		t.Fatal("could not empty WATCH in the template")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		msg := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob's note"}, 201)
		id := msg["id"].(string)
		bob.must("PATCH", "/api/v1/messages/"+id, map[string]any{"body": "bob's edited note"}, 200)
		bob.must("DELETE", "/api/v1/messages/"+id, nil, 200)
		// general cannot be left, so the churn happens in a side channel alice is in
		side := alice.must("POST", "/api/v1/channels", map[string]any{"name": "side"}, 201)
		sideID := side["id"].(string)
		bob.must("POST", "/api/v1/channels/"+sideID+"/join", nil, 200)
		bob.must("POST", "/api/v1/channels/"+sideID+"/leave", nil, 200)
		bob.must("PATCH", "/api/v1/me", map[string]any{"description": "bob, now with a bio"}, 200)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice still here?"}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if !strings.Contains(out, "still here?") {
		t.Fatalf("watcher missed the mention that followed the benign events:\n%s", out)
	}
	for _, noise := range []string{"message.edited", "message.deleted", "channel.member_left", "channel.member_joined", "participant.updated", "bob's note"} {
		if strings.Contains(out, noise) {
			t.Fatalf("watcher woke on %s:\n%s", noise, out)
		}
	}
}

// TestCLIRefusesUnfencedDiff: `-`/`+` at line start are list markers, so an
// unfenced diff renders as bullets with code boxes inside. The CLI stops that
// post before it leaves; --force says "I know", --code wraps it in a fence,
// and an ordinary bullet list is never mistaken for a diff.
func TestCLIRefusesUnfencedDiff(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	resp, err := http.Get(srv.URL + "/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	dir := t.TempDir()
	path := filepath.Join(dir, "cli.sh")
	if err := os.WriteFile(path, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "room.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "paste here"}, 201)["id"].(string)
	run := func(args ...string) (string, error) {
		out, err := exec.Command("bash", append([]string{path, "--env", envFile}, args...)...).CombinedOutput()
		return string(out), err
	}
	replies := func() []string {
		out := alice.must("GET", "/api/v1/threads/"+root, nil, 200)
		bodies := []string{}
		for _, raw := range out["messages"].([]any) {
			m := raw.(map[string]any)
			if m["id"] != root {
				bodies = append(bodies, m["body"].(string))
			}
		}
		return bodies
	}
	diff := "-    const a = call(ctx, {\n-    if (a.isErr) {\n+        const a = call(ctx, {\n+        if (a.isErr) {"

	out, err := run("reply", root, diff)
	if err == nil || !strings.Contains(out, "unfenced") || !strings.Contains(out, "--force") || !strings.Contains(out, "--code") {
		t.Fatalf("an unfenced diff was not refused: %v\n%s", err, out)
	}
	if got := replies(); len(got) != 0 {
		t.Fatalf("the refused diff was posted anyway: %q", got)
	}

	// a fence, --force and --code each let it through; --code adds the fence
	if out, err := run("reply", root, "```diff\n"+diff+"\n```"); err != nil {
		t.Fatalf("a fenced diff was refused: %v\n%s", err, out)
	}
	if out, err := run("reply", root, diff, "--force"); err != nil || strings.Contains(out, "unfenced") {
		t.Fatalf("--force did not post: %v\n%s", err, out)
	}
	if out, err := run("reply", root, diff, "--code=diff"); err != nil {
		t.Fatalf("--code=diff did not post: %v\n%s", err, out)
	}
	if out, err := run("send", "general", "x = 1", "--code", "--new-topic"); err != nil {
		t.Fatalf("bare --code did not post: %v\n%s", err, out)
	}
	got := replies()
	if len(got) != 3 || got[0] != "```diff\n"+diff+"\n```" || got[1] != diff || got[2] != "```diff\n"+diff+"\n```" {
		t.Fatalf("posted bodies: %q", got)
	}
	last := alice.must("GET", "/api/v1/channels/general/messages?limit=1", nil, 200)["messages"].([]any)[0].(map[string]any)
	if last["body"] != "```\nx = 1\n```" {
		t.Fatalf("bare --code body: %q", last["body"])
	}

	// an ordinary bullet list is markdown on purpose, not a diff
	if out, err := run("reply", root, "- first point\n- second point\n- third"); err != nil || strings.Contains(out, "unfenced") {
		t.Fatalf("a bullet list tripped the diff caution: %v\n%s", err, out)
	}
	// --help names the rule where send and reply are described
	if out, _ := run("--help"); !strings.Contains(out, "fence") || !strings.Contains(out, "--code") {
		t.Fatalf("--help does not mention fencing or --code:\n%s", out)
	}
}

// The server sends timestamps with its own UTC offset. The CLI used to slice
// that string and staple a "Z" on it, so a message sent 11:02Z read as 14:02Z
// on a +03:00 host: an agent doing arithmetic on it lands three hours out.
// Local time now carries its real offset, and --utc gives true Z.
func TestCLIPrintsHonestTimes(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	resp, err := http.Get(srv.URL + "/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	dir := t.TempDir()
	path := filepath.Join(dir, "cli.sh")
	if err := os.WriteFile(path, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "room.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent := time.Now().UTC()
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "what time is it"}, 201)

	run := func(tz string, args ...string) string {
		cmd := exec.Command("bash", append([]string{path, "--env", envFile}, args...)...)
		cmd.Env = append(os.Environ(), "TZ="+tz)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cli %v under TZ=%s: %v\n%s", args, tz, err, out)
		}
		return string(out)
	}
	// "09-07 14:02+03:00" or "09-07 11:02Z"
	stamp := regexp.MustCompile(`(\d\d)-(\d\d) (\d\d):(\d\d)(Z|[+-]\d\d:\d\d)`)
	first := func(out string) []string {
		m := stamp.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no timestamp in:\n%s", out)
		}
		return m
	}

	tokyo := first(run("Asia/Tokyo", "read", "general"))
	utc := first(run("Asia/Tokyo", "read", "general", "--utc"))
	if tokyo[5] != "+09:00" {
		t.Errorf("local time under TZ=Asia/Tokyo carries offset %q, want +09:00", tokyo[5])
	}
	if utc[5] != "Z" {
		t.Errorf("--utc printed offset %q, want Z", utc[5])
	}
	// the same instant, nine hours apart: the old code printed one string twice
	th, _ := strconv.Atoi(tokyo[3])
	uh, _ := strconv.Atoi(utc[3])
	if (uh+9)%24 != th || tokyo[4] != utc[4] {
		t.Errorf("Tokyo %s:%s and UTC %s:%s are not the same instant", tokyo[3], tokyo[4], utc[3], utc[4])
	}
	if got, want := utc[3]+":"+utc[4], sent.Format("15:04"); got != want {
		t.Errorf("--utc printed %s, the message was sent at %s", got, want)
	}
	// every read surface, not just `read`
	for _, args := range [][]string{{"mentions"}, {"inbox"}} {
		out := run("Asia/Tokyo", append(args, "--utc")...)
		if m := stamp.FindStringSubmatch(out); m != nil && m[5] != "Z" {
			t.Errorf("%v printed offset %q under --utc", args, m[5])
		}
	}
}

// flakyFront proxies to upstream but spoils the first `spoil` requests of one
// method: "drop" closes the socket with no answer, "502" is a bad gateway.
// It returns how many requests of that method it saw.
func flakyFront(t *testing.T, upstream, method, how string, spoil int) (*httptest.Server, func() int) {
	t.Helper()
	target, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	seen := 0
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			proxy.ServeHTTP(w, r)
			return
		}
		mu.Lock()
		seen++
		n := seen
		mu.Unlock()
		if n > spoil {
			proxy.ServeHTTP(w, r)
			return
		}
		if how == "502" {
			badGateway(w, r)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(front.Close)
	return front, func() int {
		mu.Lock()
		defer mu.Unlock()
		return seen
	}
}

func runCLI(t *testing.T, cli, env string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{cli, "--env", env}, args...)...)
	cmd.Env = append(append(os.Environ(), "HOME="+t.TempDir()), extraEnv...)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// One DNS blip or dropped socket used to end the call with "cannot reach".
// A read is safe to send again, so the CLI retries it.
func TestCLIRetriesAReadThroughABlip(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	for _, how := range []string{"drop", "502"} {
		front, gets := flakyFront(t, srv.URL, "GET", how, 1)
		env := writeEnv(t, "SERVER="+front.URL+"\nTOKEN="+alice.token+"\n")
		out, err := runCLI(t, cli, env, nil, "whoami")
		if err != nil || !strings.Contains(out, "alice ") {
			t.Fatalf("%s: whoami should survive one spoiled GET: %v\n%s", how, err, out)
		}
		if !strings.Contains(out, "retry 1 of 2") || gets() != 2 {
			t.Fatalf("%s: want one visible retry, saw %d GETs:\n%s", how, gets(), out)
		}
	}
}

// A POST may have landed before the answer was lost, so it is never resent:
// a second try would post the message twice.
func TestCLINeverResendsAPost(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	for _, how := range []string{"drop", "502"} {
		front, posts := flakyFront(t, srv.URL, "POST", how, 1)
		env := writeEnv(t, "SERVER="+front.URL+"\nTOKEN="+alice.token+"\n")
		out, err := runCLI(t, cli, env, nil, "send", "general", "one copy only", "--new-topic")
		if err == nil {
			t.Fatalf("%s: a spoiled POST must fail:\n%s", how, out)
		}
		if posts() != 1 || strings.Contains(out, "retry") {
			t.Fatalf("%s: the POST went out %d times:\n%s", how, posts(), out)
		}
	}
}

// The failure names its cause and the retries are visible.
func TestCLISaysWhyItCannotReach(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	env := writeEnv(t, "SERVER="+closed.URL+"\nTOKEN="+alice.token+"\n")
	out, err := runCLI(t, cli, env, []string{"OPENCHATTER_RETRIES=1"}, "whoami")
	if err == nil || !strings.Contains(out, "the connection failed, retry 1 of 1") ||
		!strings.Contains(out, "cannot reach "+closed.URL+": the connection failed") {
		t.Fatalf("a refused connection should retry once, then name the cause: %v\n%s", err, out)
	}
	if strings.Contains(out, alice.token) {
		t.Fatal("the error printed the token")
	}
}

// A stalled socket used to hang the CLI forever.
func TestCLIGivesUpOnAStalledAnswer(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
	}))
	t.Cleanup(stall.Close)
	env := writeEnv(t, "SERVER="+stall.URL+"\nTOKEN="+alice.token+"\n")
	start := time.Now()
	out, err := runCLI(t, cli, env, []string{"OPENCHATTER_MAX_TIME=1", "OPENCHATTER_RETRIES=0"}, "whoami")
	if err == nil || !strings.Contains(out, "no answer in time") {
		t.Fatalf("a stalled answer should time out: %v\n%s", err, out)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("gave up after %s, want about 1s", took)
	}
}

// A long poll holds the request open on purpose, so --wait adds to the limit.
func TestCLILongPollGetsItsWait(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	cli := servedCLI(t, srv.URL)
	env := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n")
	out, err := runCLI(t, cli, env, []string{"OPENCHATTER_MAX_TIME=1", "OPENCHATTER_RETRIES=0"}, "mentions", "--wait", "3")
	if err != nil {
		t.Fatalf("a 3s long poll under a 1s limit should still finish: %v\n%s", err, out)
	}
}

// A 403 means the token works but the action is not allowed. The read path
// used to blame the token; it now prints the server's own reason.
func TestCLIShowsTheReasonForA403(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	alice.must("POST", "/api/v1/channels", map[string]any{"name": "secret", "topic": "hush"}, 201)
	root := alice.must("POST", "/api/v1/channels/secret/messages", map[string]any{"body": "classified"}, 201)
	cli := servedCLI(t, srv.URL)
	env := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+bob.token+"\n")
	for _, verb := range []string{"msg", "thread"} {
		out, err := runCLI(t, cli, env, nil, verb, root["id"].(string))
		if err == nil || !strings.Contains(out, "you are not a member of this channel") ||
			strings.Contains(out, "rejected the token") {
			t.Fatalf("%s on a members-only message should name the reason: %v\n%s", verb, err, out)
		}
	}
}
