package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpSession drives the served openchatter-mcp.js over stdio, as a harness would.
type mcpSession struct {
	t      *testing.T
	in     io.WriteCloser
	out    *bufio.Scanner
	nextID int
}

func startMCP(t *testing.T, base string, args ...string) *mcpSession {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	script := filepath.Join(t.TempDir(), "openchatter-mcp.js")
	if err := os.WriteFile(script, []byte(getText(t, base+"/mcp.js")), 0o644); err != nil {
		t.Fatal(err)
	}
	return startMCPCommand(t, "node", append([]string{script}, args...)...)
}

// startMCPCommand runs an MCP server command line as a harness config names it.
func startMCPCommand(t *testing.T, command string, args ...string) *mcpSession {
	t.Helper()
	cmd := exec.Command(command, args...)
	// an empty HOME, so no stray ~/.openchatter/*.env is picked up
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "OPENCHATTER_ENV=")
	in, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); _ = cmd.Wait() })
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	s := &mcpSession{t: t, in: in, out: sc}
	init := s.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "openchatter" {
		t.Fatalf("initialize: %v", init)
	}
	s.notify("notifications/initialized")
	return s
}

func (s *mcpSession) notify(method string) {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, _ = s.in.Write(append(raw, '\n'))
}

func (s *mcpSession) rpc(method string, params any) map[string]any {
	s.t.Helper()
	s.nextID++
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params})
	if _, err := s.in.Write(append(raw, '\n')); err != nil {
		s.t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- s.out.Scan() }()
	select {
	case ok := <-done:
		if !ok {
			s.t.Fatalf("%s: the MCP server closed stdout", method)
		}
	case <-time.After(20 * time.Second):
		s.t.Fatalf("%s: no answer from the MCP server", method)
	}
	var resp map[string]any
	if err := json.Unmarshal(s.out.Bytes(), &resp); err != nil {
		s.t.Fatalf("%s: stdout carried a non-JSON line: %s", method, s.out.Bytes())
	}
	return resp
}

// call runs a tool and returns its text and whether it reported an error.
func (s *mcpSession) call(tool string, args map[string]any) (string, bool) {
	s.t.Helper()
	resp := s.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("%s: no result: %v", tool, resp)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func (s *mcpSession) mustCall(tool string, args map[string]any) string {
	s.t.Helper()
	text, isErr := s.call(tool, args)
	if isErr {
		s.t.Fatalf("%s failed: %s", tool, text)
	}
	return text
}

func TestMCPServesEveryOutboundTool(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))
	list := s.rpc("tools/list", map[string]any{})
	var names []string
	for _, tool := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"reply", "post_message", "read_thread", "read_channel", "react", "unreact", "ack",
		"pending_asks", "list_members", "list_channels", "search", "set_presence", "whoami"} {
		if !strings.Contains(","+got+",", ","+want+",") {
			t.Errorf("tools/list lacks %s: %s", want, got)
		}
	}
	if text := s.mustCall("whoami", nil); !strings.Contains(text, "@alice") {
		t.Fatalf("whoami: %s", text)
	}
}

// The flow a woken agent runs: read the thread, reply to a reply, react, ack.
func TestMCPThreadFlow(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))

	text := s.mustCall("post_message", map[string]any{"channel": "#general", "text": "@bob can you check the deploy?"})
	if !strings.Contains(text, "Tagged: @bob") {
		t.Fatalf("post_message should name who it woke: %s", text)
	}
	root := between(t, text, "message_id: ", ",")
	r1 := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "on it", "thread_root_id": root}, 201)

	// replying to a reply lands in the root's thread, with code kept byte for byte
	body := "@bob here it is:\n```go\nfmt.Println(\"it's $HOME `x`\")\n```"
	text = s.mustCall("reply", map[string]any{"message_id": r1["id"], "text": body})
	if !strings.Contains(text, "thread: "+root) {
		t.Fatalf("reply should land in thread %s: %s", root, text)
	}
	thread := alice.must("GET", "/api/v1/threads/"+root, nil, 200)["messages"].([]any)
	last := thread[len(thread)-1].(map[string]any)
	if last["body"] != body || last["thread_root_id"] != root {
		t.Fatalf("reply stored as %v", last)
	}

	text = s.mustCall("read_thread", map[string]any{"message_id": r1["id"]})
	if !strings.Contains(text, "3 message(s)") || !strings.Contains(text, "on it") || !strings.Contains(text, root) {
		t.Fatalf("read_thread: %s", text)
	}
	if text = s.mustCall("read_channel", map[string]any{"channel": "general"}); !strings.Contains(text, "2 replies") {
		t.Fatalf("read_channel should show reply counts: %s", text)
	}

	s.mustCall("react", map[string]any{"message_id": r1["id"], "emoji": "👀"})
	msg := alice.must("GET", "/api/v1/messages/"+r1["id"].(string), nil, 200)
	if rs := msg["reactions"].([]any); len(rs) != 1 || rs[0].(map[string]any)["emoji"] != "👀" {
		t.Fatalf("react: %v", msg["reactions"])
	}
	s.mustCall("unreact", map[string]any{"message_id": r1["id"], "emoji": "👀"})
	if rs := alice.must("GET", "/api/v1/messages/"+r1["id"].(string), nil, 200)["reactions"].([]any); len(rs) != 0 {
		t.Fatalf("unreact left %v", rs)
	}

	// bob tags alice; alice sees it pending, acks it, and it is gone
	ask := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice please review"}, 201)
	if text = s.mustCall("pending_asks", nil); !strings.Contains(text, ask["id"].(string)) {
		t.Fatalf("pending_asks: %s", text)
	}
	s.mustCall("ack", map[string]any{"message_id": ask["id"]})
	if text = s.mustCall("pending_asks", nil); !strings.Contains(text, "No pending asks") {
		t.Fatalf("pending_asks after ack: %s", text)
	}

	// a reaction on an ask acks it too, so 👀 alone is the pick-up
	ask2 := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice one more"}, 201)
	s.mustCall("react", map[string]any{"message_id": ask2["id"], "emoji": "👀"})
	if text = s.mustCall("pending_asks", nil); !strings.Contains(text, "No pending asks") {
		t.Fatalf("pending_asks after 👀: %s", text)
	}

	if text = s.mustCall("list_members", nil); !strings.Contains(text, "@bob · agent") {
		t.Fatalf("list_members: %s", text)
	}
	if text = s.mustCall("list_channels", nil); !strings.Contains(text, "#general") {
		t.Fatalf("list_channels: %s", text)
	}
	if text = s.mustCall("search", map[string]any{"query": "deploy", "from": "alice"}); !strings.Contains(text, root) {
		t.Fatalf("search: %s", text)
	}
	if text = s.mustCall("set_presence", map[string]any{"status": "offline"}); !strings.Contains(text, "offline") {
		t.Fatalf("set_presence: %s", text)
	}
	s.mustCall("set_presence", map[string]any{"status": "online"})
}

// Every refusal says what to do next, and posts nothing.
func TestMCPErrorsNameTheFix(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))
	before := len(alice.must("GET", "/api/v1/channels/general/messages", nil, 200)["messages"].([]any))

	text, isErr := s.call("post_message", map[string]any{"channel": "general", "text": "@bobb ping"})
	if !isErr || !strings.Contains(text, "did you mean @bob") || !strings.Contains(text, "backticks") {
		t.Fatalf("unknown mention: %v %s", isErr, text)
	}
	text, isErr = s.call("post_message", map[string]any{"channel": "general", "text": "-old line\n+new line"})
	if !isErr || !strings.Contains(text, "```diff") {
		t.Fatalf("unfenced diff: %v %s", isErr, text)
	}
	text, isErr = s.call("post_message", map[string]any{"channel": "nope", "text": "hi"})
	if !isErr || !strings.Contains(text, "#general") {
		t.Fatalf("unknown channel should list the real ones: %v %s", isErr, text)
	}
	text, isErr = s.call("reply", map[string]any{"message_id": "abc123", "text": "hi"})
	if !isErr || !strings.Contains(text, "message uuid") {
		t.Fatalf("bad id: %v %s", isErr, text)
	}
	text, isErr = s.call("reply", map[string]any{"message_id": "abc123"})
	if !isErr || !strings.Contains(text, `Missing "text"`) {
		t.Fatalf("missing arg: %v %s", isErr, text)
	}
	if after := len(alice.must("GET", "/api/v1/channels/general/messages", nil, 200)["messages"].([]any)); after != before {
		t.Fatalf("a refused call posted: %d -> %d messages", before, after)
	}

	// a plain bullet list is not a diff
	s.mustCall("post_message", map[string]any{"channel": "general", "text": "todo:\n- one\n- two"})
}

// The env file is the identity, Access headers included; a missing one is a
// tool error with the launch line, never a crash the harness hides.
func TestMCPIdentityComesFromTheEnvFile(t *testing.T) {
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	gate := accessGate(t, srv.URL, "svc-id.access", "svc-zz9")

	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+gate.URL+"\nTOKEN="+bob.token+"\nCF_ACCESS_CLIENT_ID=svc-id.access\nCF_ACCESS_CLIENT_SECRET=svc-zz9\n"))
	if text := s.mustCall("whoami", nil); !strings.Contains(text, "@bob") {
		t.Fatalf("whoami through Access: %s", text)
	}

	s = startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+gate.URL+"\nTOKEN="+alice.token+"\n"))
	text, isErr := s.call("whoami", nil)
	if !isErr || !strings.Contains(text, "CF_ACCESS_CLIENT_ID") || strings.Contains(text, alice.token) {
		t.Fatalf("Access without the service token: %v %s", isErr, text)
	}

	s = startMCP(t, srv.URL)
	text, isErr = s.call("whoami", nil)
	if !isErr || !strings.Contains(text, "--env") {
		t.Fatalf("no env file: %v %s", isErr, text)
	}
}

func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%q not in %q", start, s)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("%q not after %q in %q", end, start, s)
	}
	return rest[:j]
}

// The guide names every tool the server lists, and the main skill points at it.
func TestMCPGuideNamesEveryTool(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))
	guide := getText(t, srv.URL+"/skill/mcp")
	for _, tool := range s.rpc("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any) {
		name := tool.(map[string]any)["name"].(string)
		if !strings.Contains(guide, "`"+name+"`") {
			t.Errorf("/skill/mcp does not document %s", name)
		}
	}
	if strings.Contains(guide, "§") || strings.Contains(guide, "{{SERVER}}") {
		t.Fatal("/skill/mcp has unrendered placeholders")
	}
	if !strings.Contains(getText(t, srv.URL+"/skill"), "http://public.test/skill/mcp") {
		t.Fatal("the main skill does not link /skill/mcp")
	}
}

// mcp-setup.sh registers the server with Cursor and Codex, keeps what else the
// config holds, backs it up, and proves the identity.
func TestMCPSetupScriptRegistersHarnesses(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	t.Setenv("CF_ACCESS_CLIENT_ID", "")
	t.Setenv("CF_ACCESS_CLIENT_SECRET", "")
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	home := t.TempDir()
	script := filepath.Join(t.TempDir(), "mcp-setup.sh")
	body := strings.Replace(getText(t, srv.URL+"/skill/mcp-setup.sh"), `SERVER="http://public.test"`, `SERVER="`+srv.URL+`"`, 1)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	env := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a commented header and a quoted subtable are still ours to replace
	old := "model = \"x\"\n\n[mcp_servers.openchatter] # room tools\ncommand = \"stale\"\n[mcp_servers.\"openchatter\".tools.reply]\napproval_mode = \"prompt\"\n\n[profiles.a]\nmodel = \"y\"\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "OPENCHATTER_ENV=", "CODEX_HOME=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mcp-setup.sh %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := run("codex", "--env", env); !strings.Contains(out, "ok: @alice") {
		t.Fatalf("codex setup did not prove the identity:\n%s", out)
	}
	run("codex", "--env", env) // a re-run replaces, never duplicates
	toml, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(string(toml), "stale") || strings.Contains(string(toml), "approval_mode = \"prompt\"") ||
		strings.Count(string(toml), "[mcp_servers.openchatter]") != 1 ||
		!strings.Contains(string(toml), `default_tools_approval_mode = "approve"`) || !strings.Contains(string(toml), "[profiles.a]") {
		t.Fatalf("codex config:\n%s", toml)
	}
	if baks, _ := filepath.Glob(filepath.Join(home, ".codex", "config.toml.bak-*")); len(baks) == 0 {
		t.Fatal("codex config was edited without a backup")
	}
	if out, err := exec.Command("python3", "-c", "import sys, tomllib; tomllib.load(open(sys.argv[1], 'rb'))",
		filepath.Join(home, ".codex", "config.toml")).CombinedOutput(); err != nil && !strings.Contains(string(out), "No module named") {
		t.Fatalf("codex config does not parse: %s", out)
	}

	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run("cursor", "--env", env)
	var cur struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err := json.Unmarshal(raw, &cur); err != nil {
		t.Fatal(err)
	}
	oc := cur.MCPServers["openchatter"]
	if _, kept := cur.MCPServers["other"]; !kept || oc.Command != "node" || len(oc.Args) != 3 || oc.Args[2] != env {
		t.Fatalf("cursor config: %s", raw)
	}
	// the registered command really serves the tools
	s := startMCPCommand(t, oc.Command, oc.Args...)
	if text := s.mustCall("whoami", nil); !strings.Contains(text, "@alice") {
		t.Fatalf("whoami via the cursor config: %s", text)
	}
}

// Every id lands in a URL path, so anything but a bare uuid is refused before
// a request: "<uuid>?" would turn DELETE .../reactions/x into DELETE the message.
func TestMCPRefusesIDsThatRewriteThePath(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))
	own := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "keep me"}, 201)["id"].(string)
	for _, bad := range []string{own + "?", own + "#", own + "/../../channels/general", "../" + own, own + "%3F"} {
		for _, tool := range []string{"unreact", "react", "ack", "reply", "read_thread"} {
			text, isErr := s.call(tool, map[string]any{"message_id": bad, "emoji": "👀", "text": "x"})
			if !isErr || !strings.Contains(text, "message uuid") {
				t.Errorf("%s(%q) was not refused: %s", tool, bad, text)
			}
		}
	}
	if msg := alice.must("GET", "/api/v1/messages/"+own, nil, 200); msg["body"] != "keep me" {
		t.Fatalf("the message changed: %v", msg)
	}
}

// Once the POST succeeds the call reports success, even if the channel-name
// lookup after it fails: an error there would make the agent post again.
func TestMCPReplySucceedsWhenTheLookupAfterItFails(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	root := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@alice status?"}, 201)["id"].(string)
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/channels" {
			http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+front.URL+"\nTOKEN="+alice.token+"\n"))
	text := s.mustCall("reply", map[string]any{"message_id": root, "text": "all green"})
	if !strings.Contains(text, "message_id: ") || !strings.Contains(text, "thread: "+root) {
		t.Fatalf("reply after a failed lookup: %s", text)
	}
	if n := len(alice.must("GET", "/api/v1/threads/"+root, nil, 200)["messages"].([]any)); n != 2 {
		t.Fatalf("want the root and one reply, got %d messages", n)
	}
}

// A Codex config the script cannot safely edit is left byte for byte as it was:
// an inline mcp_servers table, or a Python with no TOML parser to check the result.
func TestMCPSetupScriptLeavesUneditableCodexConfigs(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	script := filepath.Join(t.TempDir(), "mcp-setup.sh")
	body := strings.Replace(getText(t, srv.URL+"/skill/mcp-setup.sh"), `SERVER="http://public.test"`, `SERVER="`+srv.URL+`"`, 1)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	env := writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n")
	// a module that fails to import stands in for a Python without tomllib or tomli
	noParser := t.TempDir()
	for _, mod := range []string{"tomllib.py", "tomli.py"} {
		if err := os.WriteFile(filepath.Join(noParser, mod), []byte("raise ImportError('hidden by the test')\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, config, pythonPath, want string
	}{
		{"inline parent", "mcp_servers = { openchatter = { command = \"old\" } }\n", "", "would not parse"},
		{"no parser", "model = \"x\"\n", noParser, "Add this block by hand"},
	}
	for _, c := range cases {
		home := t.TempDir()
		cfg := filepath.Join(home, ".codex", "config.toml")
		if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg, []byte(c.config), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", script, "codex", "--env", env)
		cmd.Env = append(os.Environ(), "HOME="+home, "OPENCHATTER_ENV=", "CODEX_HOME=", "PYTHONPATH="+c.pythonPath)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: want a refusal naming %q: %v\n%s", c.name, c.want, err, out)
		}
		if got, _ := os.ReadFile(cfg); string(got) != c.config {
			t.Errorf("%s: the config changed:\n%s", c.name, got)
		}
	}
}

// Tagging a room member outside a public channel adds them, and the result says so.
func TestMCPTagAddsMemberToPublicChannel(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, _ := setupRoom(t, srv.URL)
	alice.must("POST", "/api/v1/channels", map[string]any{"name": "scratch"}, 201)
	s := startMCP(t, srv.URL, "--env", writeEnv(t, "SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"))
	text := s.mustCall("post_message", map[string]any{"channel": "scratch", "text": "@bob please look"})
	if !strings.Contains(text, "Tagged: @bob") || !strings.Contains(text, "Added @bob to #scratch") || strings.Contains(text, "Note:") {
		t.Fatalf("post_message: %s", text)
	}
	// a private channel never opens by a tag, and nobody is claimed as woken
	alice.must("POST", "/api/v1/channels", map[string]any{"name": "secret", "private": true}, 201)
	text = s.mustCall("post_message", map[string]any{"channel": "secret", "text": "@bob psst"})
	if strings.Contains(text, "Tagged: @bob") || !strings.Contains(text, "Note: @bob is not a member of private #secret") {
		t.Fatalf("private post_message: %s", text)
	}
}
