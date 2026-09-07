package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

var capSchema = map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}}

func capBody(names ...string) map[string]any {
	caps := []map[string]any{}
	for _, n := range names {
		caps = append(caps, map[string]any{"name": n, "description": "does " + n, "inputSchema": capSchema})
	}
	return map[string]any{"capabilities": caps}
}

// answerCalls polls bob's relevant firehose for capability.call events and
// answers each one; it returns once the first answer is posted.
func answerCalls(t *testing.T, bob *testClient, cursor int64, result map[string]any, errMsg string) chan int64 {
	t.Helper()
	done := make(chan int64, 1)
	go func() {
		c := &testClient{t: t, base: bob.base, token: bob.token}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			out := c.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true&timeout=1", cursor), nil, 200)
			for _, raw := range out["events"].([]any) {
				e := raw.(map[string]any)
				if e["type"] != "capability.call" {
					continue
				}
				id := e["payload"].(map[string]any)["call_id"].(string)
				body := map[string]any{"result": result}
				if errMsg != "" {
					body = map[string]any{"error": errMsg}
				}
				c.must("POST", "/api/v1/capabilities/calls/"+id+"/result", body, 200)
				done <- int64(e["seq"].(float64))
				return
			}
			cursor = int64(out["cursor"].(float64))
		}
		done <- 0
	}()
	return done
}

// TestCapabilityAPI (task 27): register/replace/delete with validation, the
// human 403, the lists, a call round trip through the REST route with a
// background answerer, the error and timeout answers, relevant=true routing,
// the delivery receipt, and the argument check.
func TestCapabilityAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	bobID := bob.must("GET", "/api/v1/me", nil, 200)["id"].(string)

	human := &testClient{t: t, base: srv.URL}
	out := human.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "maya", "description": "maya", "is_human": true}, 201)
	human.token = out["token"].(string)
	if st, out := human.do("POST", "/api/v1/me/capabilities", capBody("x")); st != 403 || out["code"] != "humans_have_no_capabilities" {
		t.Fatalf("human register: %d %v", st, out)
	}

	// validation: bad name, non-object schema
	if st, out := alice.do("POST", "/api/v1/me/capabilities", capBody("Bad-Name")); st != 400 || out["code"] != "invalid_capability" {
		t.Fatalf("bad name: %d %v", st, out)
	}
	bad := capBody("ok")
	bad["capabilities"].([]map[string]any)[0]["inputSchema"] = map[string]any{"type": "string"}
	if st, out := alice.do("POST", "/api/v1/me/capabilities", bad); st != 400 || out["code"] != "invalid_capability" {
		t.Fatalf("non-object schema: %d %v", st, out)
	}

	aliceCursor := int64(alice.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))
	out = bob.must("POST", "/api/v1/me/capabilities", capBody("echo", "shout"), 200)
	if n := len(out["capabilities"].([]any)); n != 2 {
		t.Fatalf("register: %v", out)
	}
	// with an output schema on echo
	withOut := capBody("echo")
	withOut["capabilities"].([]map[string]any)[0]["outputSchema"] = map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}
	out = bob.must("PUT", "/api/v1/me/capabilities", withOut, 200)
	if n := len(out["capabilities"].([]any)); n != 1 {
		t.Fatalf("replace: %v", out)
	}
	// everyone saw capability.registered with the names
	evs, aliceCursor := eventsAfter(t, alice, aliceCursor)
	seen := 0
	for _, e := range evs {
		if e["type"] == "capability.registered" {
			seen++
			if e["payload"].(map[string]any)["participant_name"] != "bob" {
				t.Fatalf("registered payload: %v", e)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("want 2 capability.registered, got %d", seen)
	}
	// lists
	pl := alice.must("GET", "/api/v1/participants/"+bobID+"/capabilities", nil, 200)
	if pl["online"] != true || len(pl["capabilities"].([]any)) != 1 {
		t.Fatalf("participant list: %v", pl)
	}
	rl := human.must("GET", "/api/v1/capabilities", nil, 200)
	if len(rl["capabilities"].([]any)) != 1 {
		t.Fatalf("room list: %v", rl)
	}

	// call checks
	if st, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "nobody", "name": "echo", "args": map[string]any{"q": "x"}}); st != 404 || out["code"] != "agent_not_found" {
		t.Fatalf("unknown agent: %d %v", st, out)
	}
	if st, out := bob.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "x"}}); st != 400 || out["code"] != "self_call" {
		t.Fatalf("self call: %d %v", st, out)
	}
	if st, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "nope", "args": map[string]any{"q": "x"}}); st != 404 || out["code"] != "capability_not_found" {
		t.Fatalf("unknown capability: %d %v", st, out)
	}
	if st, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": 5}}); st != 400 || out["code"] != "invalid_args" {
		t.Fatalf("wrong arg type: %d %v", st, out)
	}
	if st, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{}}); st != 400 || out["code"] != "invalid_args" {
		t.Fatalf("missing required: %d %v", st, out)
	}
	if st, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "x"}, "timeoutSeconds": 900}); st != 400 || out["code"] != "invalid_timeout" {
		t.Fatalf("timeout too long: %d %v", st, out)
	}

	// round trip: bob answers from the relevant firehose
	bobCursor := int64(bob.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))
	answered := answerCalls(t, bob, bobCursor, map[string]any{"text": "hi back"}, "")
	start := time.Now()
	out = alice.must("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "hi"}, "timeoutSeconds": 10}, 200)
	if out["state"] != "done" || out["result"].(map[string]any)["text"] != "hi back" {
		t.Fatalf("round trip: %v", out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("call took %v", time.Since(start))
	}
	callSeq := <-answered
	if callSeq == 0 {
		t.Fatal("bob never saw the call on relevant=true")
	}
	callID := out["call_id"].(string)
	// the caller sees the result on relevant=true, and the human (neither party) sees nothing
	evs, _ = eventsAfter(t, alice, aliceCursor)
	sawResult := false
	for _, e := range evs {
		if e["type"] == "capability.result" && e["payload"].(map[string]any)["call_id"] == callID {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("caller did not get capability.result: %v", evs)
	}
	hout := human.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", callSeq-1), nil, 200)
	for _, raw := range hout["events"].([]any) {
		e := raw.(map[string]any)
		if e["type"] == "capability.call" || e["type"] == "capability.result" {
			t.Fatalf("third party got a call event on relevant=true: %v", e)
		}
	}
	// the receipt: bob's delivery stats count the call
	st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["delivered"].(float64)+st["acked"].(float64)+st["accepted"].(float64) < 1 {
		t.Fatalf("no receipt for the call: %v", st)
	}
	// GET call: caller, target, admin; not a random member
	alice.must("GET", "/api/v1/capabilities/calls/"+callID, nil, 200)
	bob.must("GET", "/api/v1/capabilities/calls/"+callID, nil, 200)
	if s, _ := human.do("GET", "/api/v1/capabilities/calls/"+callID, nil); s != 403 {
		t.Fatalf("stranger get call: %d", s)
	}
	// a second answer is refused; a non-target answer is refused
	if s, out := bob.do("POST", "/api/v1/capabilities/calls/"+callID+"/result", map[string]any{"result": map[string]any{"text": "again"}}); s != 409 || out["code"] != "call_finished" {
		t.Fatalf("late answer: %d %v", s, out)
	}

	// error answer
	answered = answerCalls(t, bob, callSeq, nil, "cannot do that")
	out = alice.must("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "hi"}, "timeoutSeconds": 10}, 200)
	if out["state"] != "error" || out["error"] != "cannot do that" {
		t.Fatalf("error answer: %v", out)
	}
	<-answered

	// result schema: text is required
	out = alice.must("POST", "/api/v1/capabilities/call?wait=false", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "hi"}, "timeoutSeconds": 10}, 202)
	pendingID := out["call_id"].(string)
	if s, out := bob.do("POST", "/api/v1/capabilities/calls/"+pendingID+"/result", map[string]any{"result": map[string]any{"nope": 1}}); s != 400 || out["code"] != "invalid_result" {
		t.Fatalf("bad result: %d %v", s, out)
	}
	if s, out := alice.do("POST", "/api/v1/capabilities/calls/"+pendingID+"/result", map[string]any{"result": map[string]any{"text": "x"}}); s != 403 || out["code"] != "not_the_target" {
		t.Fatalf("non-target answer: %d %v", s, out)
	}
	bob.must("POST", "/api/v1/capabilities/calls/"+pendingID+"/result", map[string]any{"result": map[string]any{"text": "x"}}, 200)

	// timeout: nobody answers a 1 s call
	if s, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "hi"}, "timeoutSeconds": 1}); s != 504 || out["code"] != "capability_timeout" {
		t.Fatalf("timeout: %d %v", s, out)
	}

	// offline target
	bob.must("POST", "/api/v1/me/offline", nil, 200)
	if s, out := alice.do("POST", "/api/v1/capabilities/call", map[string]any{"agent": "bob", "name": "echo", "args": map[string]any{"q": "hi"}}); s != 409 || out["code"] != "agent_offline" {
		t.Fatalf("offline: %d %v", s, out)
	}
	pl = alice.must("GET", "/api/v1/participants/"+bobID+"/capabilities", nil, 200)
	if pl["online"] != false {
		t.Fatalf("offline flag: %v", pl)
	}
	if n := len(alice.must("GET", "/api/v1/capabilities", nil, 200)["capabilities"].([]any)); n != 0 {
		t.Fatalf("offline agent still listed as callable: %d", n)
	}
	if n := len(alice.must("GET", "/api/v1/capabilities?all=true", nil, 200)["capabilities"].([]any)); n != 1 {
		t.Fatalf("all=true hides the offline agent: %d", n)
	}

	// delete
	if s, _ := bob.do("DELETE", "/api/v1/me/capabilities/echo", nil); s != 204 {
		t.Fatalf("delete: %d", s)
	}
	if s, _ := bob.do("DELETE", "/api/v1/me/capabilities/echo", nil); s != 404 {
		t.Fatalf("second delete: %d", s)
	}

	// an empty PUT clears the set (declarative: what the watcher does on boot); an empty POST is refused
	bob.must("POST", "/api/v1/me/capabilities", capBody("echo", "draw"), 200)
	empty := map[string]any{"capabilities": []any{}}
	if st, out := bob.do("POST", "/api/v1/me/capabilities", empty); st != 400 || out["code"] != "invalid_capability" {
		t.Fatalf("empty POST: %d %v", st, out)
	}
	if st, out := bob.do("PUT", "/api/v1/me/capabilities", empty); st != 200 || len(out["capabilities"].([]any)) != 0 {
		t.Fatalf("empty PUT: %d %v", st, out)
	}
	if _, out := bob.do("GET", "/api/v1/participants/me/capabilities", nil); len(out["capabilities"].([]any)) != 0 {
		t.Fatalf("capabilities survived an empty PUT: %v", out)
	}
}

func rpc(t *testing.T, base, slug, token string, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/v1/w/"+slug+"/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("bad rpc json (%d): %s", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func rpcResult(t *testing.T, base, slug, token, body string) map[string]any {
	t.Helper()
	st, out := rpc(t, base, slug, token, body)
	if st != 200 || out["error"] != nil {
		t.Fatalf("rpc %s: %d %v", body, st, out)
	}
	return out["result"].(map[string]any)
}

// TestMCPEndpoint (task 27): initialize, tools/list of online agents only,
// tools/call round trip, isError on an agent error, -32602 on offline and
// unknown tools, a non-member session and another room's act_ token get 404.
func TestMCPEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)
	slug := alice.must("GET", "/api/v1/room", nil, 200)["room"].(map[string]any)["slug"].(string)
	bob.must("PUT", "/api/v1/me/capabilities", capBody("echo"), 200)
	alice.must("PUT", "/api/v1/me/capabilities", capBody("draw"), 200)

	// transport rules
	if resp, err := http.Get(srv.URL + "/api/v1/w/" + slug + "/mcp"); err != nil || resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("GET: %v %v", err, resp.StatusCode)
	}
	if st, _ := rpc(t, srv.URL, slug, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if st, out := rpc(t, srv.URL, slug, alice.token, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); st != 400 || out["code"] != "no_batches" {
		t.Fatalf("batch: %d %v", st, out)
	}
	if st, _ := rpc(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); st != 202 {
		t.Fatalf("notification: %d", st)
	}
	if _, out := rpc(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`); out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", out)
	}
	init := rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if init["protocolVersion"] != "2025-03-26" || init["serverInfo"].(map[string]any)["name"] != "openchatter" {
		t.Fatalf("initialize: %v", init)
	}
	init = rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if init["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("unsupported version fallback: %v", init)
	}
	rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)

	// tools/list: both online agents
	tools := rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)["tools"].([]any)
	names := []string{}
	for _, raw := range tools {
		tl := raw.(map[string]any)
		names = append(names, tl["name"].(string))
		if tl["inputSchema"] == nil || !strings.Contains(tl["description"].(string), "(agent ") {
			t.Fatalf("tool entry: %v", tl)
		}
	}
	if strings.Join(names, ",") != "alice__draw,bob__echo" {
		t.Fatalf("tool names: %v", names)
	}

	// a human session of a member lists and calls too
	creator, _, room := sessionRoom(t, srv.URL, "mcp room")
	if st, out := rpc(t, srv.URL, slug, creator.token, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); st != 404 || out["code"] != "room_not_found" {
		t.Fatalf("non-member session: %d %v", st, out)
	}
	// alice's act_ token against the other room's slug
	if st, out := rpc(t, srv.URL, room["slug"].(string), alice.token, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); st != 404 || out["code"] != "room_not_found" {
		t.Fatalf("other room act_: %d %v", st, out)
	}
	if st, out := rpc(t, srv.URL, "no-such-room", alice.token, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`); st != 404 || out["code"] != "room_not_found" {
		t.Fatalf("bogus slug: %d %v", st, out)
	}

	// tools/call round trip
	bobCursor := int64(bob.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))
	answered := answerCalls(t, bob, bobCursor, map[string]any{"text": "pong"}, "")
	res := rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"bob__echo","arguments":{"q":"ping"},"_meta":{"timeoutSeconds":10}}}`)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["text"] != "pong" {
		t.Fatalf("tools/call: %v", res)
	}
	if txt := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(txt, "pong") {
		t.Fatalf("content text: %q", txt)
	}
	seq := <-answered

	// agent error -> isError
	answered = answerCalls(t, bob, seq, nil, "no can do")
	res = rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"bob__echo","arguments":{"q":"ping"},"_meta":{"timeoutSeconds":10}}}`)
	if res["isError"] != true || res["content"].([]any)[0].(map[string]any)["text"] != "no can do" {
		t.Fatalf("isError: %v", res)
	}
	<-answered

	// timeout -> isError with the message
	res = rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"bob__echo","arguments":{"q":"ping"},"_meta":{"timeoutSeconds":1}}}`)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "capability_timeout") {
		t.Fatalf("timeout: %v", res)
	}

	// invalid args and unknown tool are JSON-RPC errors
	_, out := rpc(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"bob__echo","arguments":{}}}`)
	if e := out["error"].(map[string]any); e["code"].(float64) != -32602 || e["data"].(map[string]any)["code"] != "invalid_args" {
		t.Fatalf("invalid args: %v", out)
	}
	_, out = rpc(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"bob__nope","arguments":{}}}`)
	if e := out["error"].(map[string]any); e["code"].(float64) != -32602 || e["data"].(map[string]any)["code"] != "capability_not_found" {
		t.Fatalf("unknown tool: %v", out)
	}

	// bob offline: gone from the list, a stale call says agent offline
	bob.must("POST", "/api/v1/me/offline", nil, 200)
	tools = rpcResult(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "alice__draw" {
		t.Fatalf("offline agent still listed: %v", tools)
	}
	_, out = rpc(t, srv.URL, slug, alice.token, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"bob__echo","arguments":{"q":"x"}}}`)
	if e := out["error"].(map[string]any); e["code"].(float64) != -32602 || e["data"].(map[string]any)["code"] != "agent_offline" || !strings.Contains(e["message"].(string), "agent offline") {
		t.Fatalf("offline call: %v", out)
	}
}
