package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cliHarness downloads /cli.sh from base and returns a runner bound to token.
func cliHarness(t *testing.T, base, token string) func(args ...string) (string, error) {
	t.Helper()
	resp, err := http.Get(base + "/cli.sh")
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
	if err := os.WriteFile(envFile, []byte("SERVER="+base+"\nTOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{path, "--env", envFile}, args...)...)
		cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

func TestCLIScriptNeverEvaluatesCode(t *testing.T) {
	if regexp.MustCompile(`\beval\b`).MatchString(cliScript) {
		t.Fatal("cli.sh must not contain eval: JSON and flags are data, never code")
	}
}

func TestCLIMembersListing(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	run := cliHarness(t, srv.URL, alice.token)

	// no --channel: in_channel is absent (None), so no membership suffix
	out, err := run("members")
	if err != nil {
		t.Fatalf("members: %v\n%s", err, out)
	}
	for _, h := range []string{"alice", "bob"} {
		if !strings.Contains(out, h) {
			t.Fatalf("members lacks %s:\n%s", h, out)
		}
	}
	if !strings.Contains(out, "agent") || strings.Contains(out, "channel") {
		t.Fatalf("members without --channel: want agent rows and no membership column:\n%s", out)
	}

	out, err = run("members", "--channel", "general")
	if err != nil {
		t.Fatalf("members --channel: %v\n%s", err, out)
	}
	if !strings.Contains(out, "in channel") && !strings.Contains(out, "NOT in channel") {
		t.Fatalf("members --channel printed no membership:\n%s", out)
	}
}

func TestCLIReplyLatest(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	run := cliHarness(t, srv.URL, alice.token)

	// no thread yet: the empty list must be a clear refusal, not a crash
	out, err := run("reply", "--latest", "general", "hello")
	if err == nil || !strings.Contains(out, "no thread you are part of") {
		t.Fatalf("reply --latest with no thread: err=%v\n%s", err, out)
	}

	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "start"}, 201)["id"].(string)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "first reply", "thread_root_id": root}, 201)
	if out, err := run("reply", "--latest", "general", "via latest"); err != nil {
		t.Fatalf("reply --latest: %v\n%s", err, out)
	}
	found := false
	for _, raw := range alice.must("GET", "/api/v1/threads/"+root, nil, 200)["messages"].([]any) {
		if raw.(map[string]any)["body"] == "via latest" {
			found = true
		}
	}
	if !found {
		t.Fatal("reply --latest did not land in the thread")
	}
}

func TestCLIReadThreadMsgOutput(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	run := cliHarness(t, srv.URL, alice.token)
	first := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alpha-one"}, 201)["id"].(string)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "beta-two"}, 201)

	out, err := run("read", "general")
	if err != nil || strings.Index(out, "alpha-one") > strings.Index(out, "beta-two") || !strings.Contains(out, "alpha-one") {
		t.Fatalf("read (oldest first): %v\n%s", err, out)
	}
	out, err = run("read", "general", "--newest")
	if err != nil || strings.Index(out, "beta-two") > strings.Index(out, "alpha-one") || !strings.Contains(out, "beta-two") {
		t.Fatalf("read --newest: %v\n%s", err, out)
	}

	out, err = run("read", "general", "--json")
	var parsed struct {
		Messages []struct{ Body string } `json:"messages"`
	}
	if err != nil || json.Unmarshal([]byte(out), &parsed) != nil || len(parsed.Messages) < 2 || parsed.Messages[0].Body != "alpha-one" {
		t.Fatalf("read --json: %v\n%s", err, out)
	}
	out, err = run("read", "general", "--json", "--newest")
	if err != nil || json.Unmarshal([]byte(out), &parsed) != nil || parsed.Messages[0].Body != "beta-two" {
		t.Fatalf("read --json --newest: %v\n%s", err, out)
	}

	if out, err = run("thread", first); err != nil || !strings.Contains(out, "alpha-one") {
		t.Fatalf("thread: %v\n%s", err, out)
	}
	if out, err = run("msg", first); err != nil || !strings.Contains(out, "alpha-one") || strings.Contains(out, "beta-two") {
		t.Fatalf("msg: %v\n%s", err, out)
	}
}

// --since used to be spliced into a string that was then eval'd, so a quote in
// it ran as python on the caller's machine. It is data now.
func TestCLIReadSinceIsData(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	run := cliHarness(t, srv.URL, alice.token)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alpha-one"}, 201)

	sentinel := filepath.Join(t.TempDir(), "pwned")
	payload := `x" or __import__("os").system("touch ` + sentinel + `") or "`
	run("read", "general", "--since", payload)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("--since was executed as code")
	}

	if out, err := run("read", "general", "--since", "9999-01-01T00:00:00Z"); err != nil || strings.Contains(out, "alpha-one") {
		t.Fatalf("a future --since must filter everything out: %v\n%s", err, out)
	}
	if out, err := run("read", "general", "--since", "2000-01-01T00:00:00Z"); err != nil || !strings.Contains(out, "alpha-one") {
		t.Fatalf("a past --since must keep the message: %v\n%s", err, out)
	}
}

func TestCLIOtherListings(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	run := cliHarness(t, srv.URL, alice.token)
	if out, err := run("whoami"); err != nil || !strings.Contains(out, "alice (agent,") {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	if out, err := run("channels"); err != nil || !strings.Contains(out, "general") {
		t.Fatalf("channels: %v\n%s", err, out)
	}
	if out, err := run("pending"); err != nil || !strings.Contains(out, "no unacked asks") {
		t.Fatalf("pending: %v\n%s", err, out)
	}
}

// A body the CLI cannot read must be a loud failure, never a silent exit 0.
func TestCLIMalformedResponsesFail(t *testing.T) {
	var status int
	var body string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cli.sh" {
			_, _ = io.WriteString(w, cliScript)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer fake.Close()
	run := cliHarness(t, fake.URL, "tok")

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"not json", 200, "<html>proxy login</html>"},
		{"empty object", 200, "{}"},
	}
	for _, c := range cases {
		status, body = c.status, c.body
		for _, args := range [][]string{{"whoami"}, {"channels"}, {"members"}, {"pending"}, {"read", "general"}, {"msg", "m1"}, {"send", "general", "hi", "--new-topic"}} {
			out, err := run(args...)
			if err == nil || !strings.Contains(out, "openchatter:") {
				t.Errorf("%s: %v must fail loudly, got err=%v\n%s", c.name, args, err, out)
			}
		}
	}

	// an HTML error page still yields a readable message, not a hang or a crash
	status, body = 502, "<html>bad gateway</html>"
	out, err := run("whoami")
	if err == nil || !strings.Contains(out, "HTTP 502") {
		t.Fatalf("502 page: err=%v\n%s", err, out)
	}
}
