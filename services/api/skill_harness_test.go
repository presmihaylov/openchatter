package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSkillHarnessGuides(t *testing.T) {
	srv, _ := newTestServer(t)
	main := getText(t, srv.URL+"/skill")
	for _, slug := range harnessGuideSlugs() {
		if !strings.Contains(main, "/skill/"+slug) {
			t.Fatalf("main skill does not link /skill/%s", slug)
		}
		resp, err := http.Get(srv.URL + "/skill/" + slug)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
			t.Fatalf("/skill/%s: %d %s", slug, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		doc := string(raw)
		if strings.Contains(doc, "{{SERVER}}") || strings.Contains(doc, "§") {
			t.Fatalf("/skill/%s has an unrendered placeholder", slug)
		}
		// every guide covers both modes, the shared scripts, the fleet default
		// scope, the key-in-a-file rule, both process managers and the beacons
		for _, want := range []string{
			"## 5. Foreground mode", "## 6. Background mode",
			"Run only `sh <base>.inject.sh`: it starts the watcher itself", "never pipe `watch.sh | inject.sh`",
			`"ac" means that`, "same command with your env file, nothing else",
			"http://public.test/skill/watch.sh", "http://public.test/skill/bridge.sh", "http://public.test/skill/inject.sh",
			`Keep ` + "`" + `WATCH=""` + "`" + `, the fleet default`, "Reactions never wake you",
			"harness-keys.env", "never typed on a command line",
			"curl -s \"$SERVER/api/v1/me\"", "byte for byte",
			"http://public.test/skill#humans-and-workspaces", "nothing changes for you",
			"launchd", "systemd", "enable-linger",
			"## 7. Self-test beacons", "WATCHER-SELFTEST-OK", "BRIDGE-UP", "INJECT-UP",
			"## 8. Troubleshooting",
			"# You are <your-name> in the OpenFlock room",
			"HARNESS=\"" + slug + "\"",
		} {
			if !strings.Contains(doc, want) {
				t.Fatalf("/skill/%s missing %q", slug, want)
			}
		}
		for _, gone := range skillCreateRecipeGone {
			if strings.Contains(doc, gone) {
				t.Fatalf("/skill/%s still carries the unauthenticated create recipe %q", slug, gone)
			}
		}
	}
}

// TestSkillScriptsParse: the three served scripts are plain POSIX sh and parse.
func TestSkillScriptsParse(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, name := range []string{"watch.sh", "bridge.sh", "inject.sh"} {
		resp, err := http.Get(srv.URL + "/skill/" + name)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/x-shellscript") {
			t.Fatalf("/skill/%s: %d %s", name, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.HasPrefix(string(raw), "#!/bin/sh\n") {
			t.Fatalf("/skill/%s is not a sh script", name)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, raw, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("sh -n %s: %v\n%s", name, err, out)
		}
	}
}

func getText(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// harnessHome lays out what a bridge or injector expects under $HOME: the
// filled-in watcher, the env file and a key file holding a fake secret.
func harnessHome(t *testing.T, srvURL, token string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".agentchat")
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must("room.alice.watch.sh", watcherTemplate(t, srvURL))
	must("room.alice.env", "SERVER="+srvURL+"\nTOKEN="+token+"\n")
	must("secrets/harness-keys.env", "OPENAI_API_KEY=sk-test-do-not-leak\n")
	return home
}

func fillScript(t *testing.T, srvURL, name string, extra map[string]string) string {
	t.Helper()
	script := getText(t, srvURL+"/skill/"+name)
	// longest placeholder first: a map would iterate in random order and leave
	// "<room-slug>.alice" behind
	for _, r := range []struct{ from, to string }{
		{"<room-slug>.<your-name-with-dashes>", "room.alice"},
		{"<your-name-with-dashes>", "alice"},
	} {
		script = strings.ReplaceAll(script, r.from, r.to)
	}
	for from, to := range extra {
		if !strings.Contains(script, from) {
			t.Fatalf("%s has no %q to fill", name, from)
		}
		script = strings.Replace(script, from, to, 1)
	}
	return script
}

// runScriptPosting runs a script for a few seconds with HOME and extra env,
// calls post once it is up, and returns everything it printed.
func runScriptPosting(t *testing.T, script, home string, env []string, post func()) string {
	t.Helper()
	path := filepath.Join(home, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", path)
	cmd.Env = append(append(os.Environ(), "HOME="+home), env...)
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
	time.Sleep(2500 * time.Millisecond)
	cancel()
	_ = cmd.Wait()
	return out.String()
}

// TestBridgeRunsOneTurnPerEvent: a mention becomes exactly one harness turn
// whose prompt carries the REPLY-TO line and the event, the key reaches the
// turn through the environment only, and the spool is empty afterwards.
func TestBridgeRunsOneTurnPerEvent(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	home := harnessHome(t, srv.URL, alice.token)
	script := fillScript(t, srv.URL, "bridge.sh", map[string]string{`HARNESS="<codex|opencode|pi>"`: `HARNESS="codex"`})
	turns := filepath.Join(home, "turns.log")
	out := runScriptPosting(t, script, home, []string{
		`AGENTCHAT_TURN_CMD=printf '%s\n---\n' "$AGENTCHAT_PROMPT" >> ` + turns + `; printf 'key=%s\n' "${OPENAI_API_KEY:+set}" >> ` + turns,
	}, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice are you there"}, 201)
	})
	for _, want := range []string{"BRIDGE-UP: pid", "harness=codex", "WATCHER-SELFTEST-OK"} {
		if !strings.Contains(out, want) {
			t.Fatalf("bridge output lacks %q:\n%s", want, out)
		}
	}
	raw, err := os.ReadFile(turns)
	if err != nil {
		t.Fatalf("no turn ran:\n%s", out)
	}
	got := string(raw)
	if strings.Count(got, "\n---\n") != 1 {
		t.Fatalf("expected exactly one turn:\n%s", got)
	}
	for _, want := range []string{"Handle it as AGENTS.md says. If it asks something of you", "REPLY-TO ", "are you there", `"type":"message.created"`, "key=set"} {
		if !strings.Contains(got, want) {
			t.Fatalf("turn prompt lacks %q:\n%s", want, got)
		}
	}
	log, _ := os.ReadFile(filepath.Join(home, ".agentchat", "room.alice.bridge.log"))
	if !strings.Contains(string(log), "BRIDGE-TURN:") {
		t.Fatalf("bridge log lacks the turn:\n%s", log)
	}
	if strings.Contains(out+string(log)+got, "sk-test-do-not-leak") {
		t.Fatal("the key leaked into output, log or prompt")
	}
	if spool, _ := os.ReadFile(filepath.Join(home, ".agentchat", "room.alice.spool")); len(bytes.TrimSpace(spool)) != 0 {
		t.Fatalf("spool not drained after the turn:\n%s", spool)
	}
	if !strings.Contains(string(log), "BRIDGE-ERROR") == false {
		t.Fatalf("bridge reported an error:\n%s", log)
	}
}

// TestBridgeReplaysSpool: an event left in the spool by a kill mid-turn runs
// again on the next start, before the watcher polls.
func TestBridgeReplaysSpool(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	home := harnessHome(t, srv.URL, alice.token)
	spool := filepath.Join(home, ".agentchat", "room.alice.spool")
	if err := os.WriteFile(spool, []byte("REPLY-TO abc in general: bob: left over\t{\"seq\":1,\"type\":\"message.created\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fillScript(t, srv.URL, "bridge.sh", map[string]string{`HARNESS="<codex|opencode|pi>"`: `HARNESS="pi"`})
	turns := filepath.Join(home, "turns.log")
	out := runScriptPosting(t, script, home, []string{
		`AGENTCHAT_TURN_CMD=printf '%s\n---\n' "$AGENTCHAT_PROMPT" >> ` + turns,
	}, func() {})
	if !strings.Contains(out, "BRIDGE-REPLAY: 1 event(s)") {
		t.Fatalf("no replay announced:\n%s", out)
	}
	raw, _ := os.ReadFile(turns)
	if !strings.Contains(string(raw), "REPLY-TO abc in general: bob: left over") || !strings.Contains(string(raw), `"seq":1`) {
		t.Fatalf("the spooled event did not run:\n%s", raw)
	}
	if _, err := os.Stat(spool + ".replay"); err == nil {
		t.Fatal("replay file left behind")
	}
}

// TestInjectDeliversLine: the foreground injector hands the session one line
// per hit, the REPLY-TO summary plus what to do with it, and the beacons.
func TestInjectDeliversLine(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	home := harnessHome(t, srv.URL, alice.token)
	script := fillScript(t, srv.URL, "inject.sh", map[string]string{`DELIVER="<tmux|herdr|opencode|codex>"`: `DELIVER="tmux"`})
	lines := filepath.Join(home, "lines.log")
	out := runScriptPosting(t, script, home, []string{
		`AGENTCHAT_DELIVER_CMD=printf '%s\n' "$AGENTCHAT_LINE" >> ` + lines,
	}, func() {
		bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice ping"}, 201)
	})
	if !strings.Contains(out, "INJECT-UP: pid") || strings.Contains(out, "INJECT-ERROR") {
		t.Fatalf("injector did not start clean:\n%s", out)
	}
	raw, err := os.ReadFile(lines)
	if err != nil {
		t.Fatalf("nothing delivered:\n%s", out)
	}
	got := string(raw)
	for _, want := range []string{"WATCHER-UP", "REPLY-TO ", "@alice ping",
		// the delivered line must carry the reaction ack, not an "on it" (task 30)
		"| ack: ac react ",
		"run the ack: command on that line, then fetch the thread with ac thread <id>, act, and answer with ac reply <id>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("delivered lines lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{\"seq\"") {
		t.Fatalf("raw JSON was pasted into the session:\n%s", got)
	}
}

// TestBridgeStormGuard: a burst of hits beyond STORM_MAX inside the window
// stops running turns and says so, instead of feeding a reply loop.
func TestBridgeStormGuard(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	home := harnessHome(t, srv.URL, alice.token)
	script := fillScript(t, srv.URL, "bridge.sh", map[string]string{`HARNESS="<codex|opencode|pi>"`: `HARNESS="opencode"`})
	turns := filepath.Join(home, "turns.log")
	out := runScriptPosting(t, script, home, []string{
		`AGENTCHAT_TURN_CMD=printf 'T\n' >> ` + turns,
		"AGENTCHAT_STORM_MAX=2", "AGENTCHAT_STORM_WINDOW=60", "AGENTCHAT_STORM_PAUSE=1",
	}, func() {
		for i := 0; i < 5; i++ {
			bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice again"}, 201)
		}
	})
	if !strings.Contains(out, "BRIDGE-STORM: 3 turns in 60s") {
		t.Fatalf("storm guard did not trip:\n%s", out)
	}
	raw, _ := os.ReadFile(turns)
	if n := strings.Count(string(raw), "T\n"); n >= 5 {
		t.Fatalf("all %d turns ran despite the guard", n)
	}
}
