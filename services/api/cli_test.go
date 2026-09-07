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
		"cmd_send()", "cmd_reply()", "cmd_broadcast()", "cmd_read()", "cmd_thread()",
		"cmd_msg()", "cmd_mentions()", "cmd_channels()", "cmd_members()", "cmd_whoami()",
		"cmd_react()", "cmd_reactions()", "cmd_download()", "cmd_join()", "cmd_leave()", "cmd_rejoin()",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cli.sh is missing %q", want)
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

// Behind Cloudflare Access every request must carry the service token, or the
// agent gets a login page instead of the API. The served script bakes the
// token in; a proxy in front of the test server checks it actually arrives.
func TestCLICarriesAccessServiceToken(t *testing.T) {
	srv, store := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	// re-serve the script with a token configured; the room itself is unchanged
	withAccess := httptest.NewServer(New(store, testConfig(store, Config{
		PublicURL: "http://public.test", AccessClientID: "cf-id-123", AccessClientSecret: "cf-secret-456",
	})).Handler())
	defer withAccess.Close()
	resp, err := http.Get(withAccess.URL + "/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	script := string(raw)
	if strings.Contains(script, "{{CF_ACCESS") {
		t.Fatal("service token placeholders were not substituted")
	}
	if !strings.Contains(script, `DEFAULT_CF_ACCESS_CLIENT_SECRET="cf-secret-456"`) {
		t.Fatal("service token not baked into the served script")
	}

	// and with no token configured the placeholders are empty, not left literal
	plain, err := http.Get(srv.URL + "/cli.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Body.Close()
	rawPlain, _ := io.ReadAll(plain.Body)
	if !strings.Contains(string(rawPlain), `DEFAULT_CF_ACCESS_CLIENT_SECRET=""`) {
		t.Fatal("plain room should serve an empty service token")
	}

	// a stand-in for Cloudflare Access: reject anything without the headers
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	seen := 0
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != "cf-id-123" || r.Header.Get("CF-Access-Client-Secret") != "cf-secret-456" {
			http.Error(w, "<html>Cloudflare Access login</html>", 403)
			return
		}
		seen++
		proxy.ServeHTTP(w, r)
	}))
	defer gate.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "cli.sh")
	if err := os.WriteFile(path, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "room.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+gate.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", path, "--env", envFile, "whoami").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "alice ") {
		t.Fatalf("whoami through the Access gate: %v\n%s", err, out)
	}
	if seen == 0 {
		t.Fatal("the gate never saw a request with the service token")
	}
	if strings.Contains(string(out), "cf-secret-456") {
		t.Fatal("the CLI printed the service secret")
	}
	// the env file wins over the baked-in token, and a wrong one is a loud 403
	bad := filepath.Join(dir, "bad.env")
	_ = os.WriteFile(bad, []byte("SERVER="+gate.URL+"\nTOKEN="+alice.token+"\nCF_ACCESS_CLIENT_SECRET=leaked-zz9\n"), 0o600)
	out, err = exec.Command("bash", path, "--env", bad, "whoami").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Cloudflare Access") {
		t.Fatalf("a rejected service token should name Cloudflare Access: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "leaked-zz9") || strings.Contains(string(out), "cf-secret") {
		t.Fatalf("the error printed a secret:\n%s", out)
	}
}

func TestInviteCarriesAccessServiceToken(t *testing.T) {
	srv, store := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	if _, has := alice.must("POST", "/api/v1/invites", nil, 201)["access"]; has {
		t.Fatal("plain room must not return an access block")
	}
	withAccess := httptest.NewServer(New(store, testConfig(store, Config{
		PublicURL: "http://public.test", AccessClientID: "cf-id-123", AccessClientSecret: "cf-secret-456",
	})).Handler())
	defer withAccess.Close()
	gated := &testClient{t: t, base: withAccess.URL, token: alice.token}
	access, ok := gated.must("POST", "/api/v1/invites", nil, 201)["access"].(map[string]any)
	if !ok {
		t.Fatal("gated room must return the access block")
	}
	if access["client_id"] != "cf-id-123" || access["client_secret"] != "cf-secret-456" {
		t.Fatalf("access block = %v", access)
	}
	// unauthenticated callers never see it
	if code, _ := (&testClient{t: t, base: withAccess.URL}).do("POST", "/api/v1/invites", nil); code != 401 {
		t.Fatalf("anonymous invite = %d, want 401", code)
	}
}

// accessGate stands in for Cloudflare Access: anything without the service
// token gets an HTML login page instead of the room.
func accessGate(t *testing.T, upstream, id, secret string) *httptest.Server {
	t.Helper()
	target, _ := url.Parse(upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("CF-Access-Client-Id") != id || r.Header.Get("CF-Access-Client-Secret") != secret {
			http.Error(w, "<html>Cloudflare Access login</html>", 403)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	return gate
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

func TestWatcherTemplatePassesAccessGate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	gate := accessGate(t, srv.URL, "cf-id-123", "cf-secret-456")
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

	// through the gate with the headers in the env file: hears bob, no errors
	write("SERVER=" + gate.URL + "\nTOKEN=" + alice.token + "\nCF_ACCESS_CLIENT_ID=cf-id-123\nCF_ACCESS_CLIENT_SECRET=cf-secret-456\n")
	out := runWatcher(t, script, home, bob)
	if !strings.Contains(out, "are you there") || strings.Contains(out, "WATCHER-ERROR") {
		t.Fatalf("gated watcher with headers should hear bob cleanly:\n%s", out)
	}
	for _, beacon := range []string{"WATCHER-UP", "WATCHER-SELFTEST-OK", "WATCHER-SCOPE", "REPLY-TO "} {
		if !strings.Contains(out, beacon) {
			t.Fatalf("watcher output lacks %s:\n%s", beacon, out)
		}
	}
	if strings.Contains(out, "cf-secret-456") {
		t.Fatalf("watcher printed the service secret:\n%s", out)
	}

	// a LAN room (no gate, no headers) keeps working unchanged
	write("SERVER=" + srv.URL + "\nTOKEN=" + alice.token + "\n")
	out = runWatcher(t, script, home, bob)
	if !strings.Contains(out, "are you there") || strings.Contains(out, "WATCHER-ERROR") {
		t.Fatalf("plain watcher should hear bob cleanly:\n%s", out)
	}

	// and without the headers the gate is real: the watcher is loud, not silent
	write("SERVER=" + gate.URL + "\nTOKEN=" + alice.token + "\n")
	out = runWatcher(t, script, home, bob)
	if !strings.Contains(out, "WATCHER-ERROR") || strings.Contains(out, "are you there") {
		t.Fatalf("gate should reject a watcher without headers:\n%s", out)
	}
}

func TestSkillRawCurlsCarryAccessHeaders(t *testing.T) {
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
			if strings.Contains(line, "$CFH") {
				continue
			}
			t.Errorf("%s: raw curl without $CFH: %s", page, strings.TrimSpace(line))
		}
	}
	resp, err := http.Get(srv.URL + "/skill/hermes")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `req.add_header("CF-Access-Client-Id"`) {
		t.Error("hermes helper does not send the Access headers")
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

// TestWatcherTemplateHearsOwnThreads: with WATCH empty (no channel heard in
// full), an untagged reply in a thread alice wrote in must still surface.
// Before thread_participants rode on the event, the elsewhere rule ate it.
func TestWatcherTemplateHearsOwnThreads(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
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
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "untagged follow-up", "thread_root_id": root["id"].(string)}, 201)
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if !strings.Contains(out, "REPLY-TO "+root["id"].(string)) || !strings.Contains(out, "untagged follow-up") {
		t.Fatalf("watcher missed an untagged reply in alice's thread:\n%s", out)
	}
	// the ack nudge names the message that tagged you, never the thread root:
	// an ack on the root would land on the wrong message (task 30, task 32)
	if !strings.Contains(out, "| ack: ac ack ") || strings.Contains(out, "| ack: ac ack "+root["id"].(string)) {
		t.Fatalf("REPLY-TO line lacks the ack nudge, or points it at the root:\n%s", out)
	}
	if strings.Contains(out, "plain top-level") {
		t.Fatalf("watcher with WATCH=\"\" leaked a plain top-level message:\n%s", out)
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
	if !strings.Contains(out, "mode=mentions-only") {
		t.Fatalf("scope beacon does not say mentions-only:\n%s", out)
	}
	if !strings.Contains(out, "after the reaction") {
		t.Fatalf("watcher missed the mention:\n%s", out)
	}
	if strings.Contains(out, `"type":"message.reaction"`) || strings.Contains(out, "REACTION ") {
		t.Fatalf("a reaction woke the watcher:\n%s", out)
	}
}

// TestWatcherTemplateRootBroadcastsOnly: a broadcast wakes a mentions-only
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
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel inside bob's thread", "thread_root_id": root["id"].(string), "broadcast": true}, 201)
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel at the root", "broadcast": true}, 201)
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
	_, alice, bob := setupRoom(t, srv.URL)
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
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob is back with a question", "thread_root_id": rootID}, 201)
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

// gatedWatcher runs the watcher template through a proxy that can be switched
// to answer /events with a Cloudflare-style 502 page. It returns the switch,
// the failed-poll count, and a stop that yields the watcher's output.
func gatedWatcher(t *testing.T) (setDown func(bool), failed func() int, post func(string), stop func() string) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	down, fails := false, 0
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isDown := down
		if isDown && r.URL.Path == "/api/v1/events" {
			fails++
		}
		n := fails
		mu.Unlock()
		if isDown && r.URL.Path == "/api/v1/events" {
			// a Cloudflare 502 page: HTML, and a ray id that differs on every hit
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>Bad gateway, ray " + strconv.Itoa(n) + "</html>"))
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
	cmd.Env = append(os.Environ(), "HOME="+home)
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
	setDown = func(v bool) { mu.Lock(); down = v; mu.Unlock() }
	failed = func() int { mu.Lock(); defer mu.Unlock(); return fails }
	post = func(body string) {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": body}, 201)
	}
	stop = func() string {
		cancel()
		_ = cmd.Wait()
		return out.String()
	}
	return setDown, failed, post, stop
}

// TestWatcherTemplateBacksOffOnOutage: a dead server used to be a WATCHER-ERROR
// wake every 5s for every agent. Now a failed retry prints once, repeats of
// the same code stay silent, recovery prints one WATCHER-BACK line, and the
// cursor is untouched so a message posted during the outage still arrives.
func TestWatcherTemplateBacksOffOnOutage(t *testing.T) {
	setDown, failed, post, stop := gatedWatcher(t)
	setDown(true)
	time.Sleep(4 * time.Second)
	post("@alice posted while you were down")
	setDown(false)
	n := failed()
	time.Sleep(3 * time.Second)
	got := stop()
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

// TestWatcherTemplateSilentOnBlip: a deploy restart cuts one long-poll and the
// server is back before the 5s retry. That used to cost every agent two wakes
// (ERROR + BACK) per deploy; now a single failed poll prints nothing at all,
// and the mention posted during the blip still arrives.
func TestWatcherTemplateSilentOnBlip(t *testing.T) {
	setDown, failed, post, stop := gatedWatcher(t)
	setDown(true)
	deadline := time.Now().Add(5 * time.Second)
	for failed() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	setDown(false)
	post("@alice posted during the blip")
	time.Sleep(3 * time.Second)
	got := stop()
	if n := failed(); n != 1 {
		t.Fatalf("wanted exactly one failed poll, got %d:\n%s", n, got)
	}
	if strings.Contains(got, "WATCHER-ERROR: HTTP 502") || strings.Contains(got, "WATCHER-BACK") {
		t.Fatalf("a one-poll blip must be silent:\n%s", got)
	}
	if !strings.Contains(got, "posted during the blip") {
		t.Fatalf("a mention posted during the blip was lost:\n%s", got)
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
