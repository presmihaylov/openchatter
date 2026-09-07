package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/presmihaylov/openchatter/models"
)

func searchBodies(res map[string]any) []string {
	out := []string{}
	for _, raw := range res["results"].([]any) {
		out = append(out, raw.(map[string]any)["body"].(string))
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// TestSearchFilters: author repeats OR (humans and agents alike), kind
// selects roots / thread rows / attachments, fields AND, bad kind is 400.
func TestSearchFilters(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.Close()
	secret, alice, bob := setupRoom(t, srv.URL)
	carol := &testClient{t: t, base: srv.URL}
	out := carol.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "carol", "is_human": true}, 201)
	carol.token = out["token"].(string)

	alice.must("POST", "/api/v1/channels", map[string]any{"name": "ops"}, 201)
	bob.must("POST", "/api/v1/channels/ops/join", nil, 200)

	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alpha budget report"}, 201)
	bob.must("POST", "/api/v1/channels/ops/messages", map[string]any{"body": "alpha budget in ops"}, 201)
	carol.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alpha thanks from carol"}, 201)
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alpha reply here", "thread_root_id": root["id"]}, 201)

	cases := []struct {
		q    string
		want []string
	}{
		{"q=alpha&author=alice", []string{"alpha budget report"}},
		{"q=alpha&author=alice&author=carol", []string{"alpha budget report", "alpha thanks from carol"}},
		{"q=alpha&author=alice,carol", []string{"alpha budget report", "alpha thanks from carol"}},
		{"q=alpha&author=carol", []string{"alpha thanks from carol"}}, // a human author works
		{"q=alpha&author=alice&channel=ops", []string{}},              // fields AND
		{"q=alpha&channel=ops", []string{"alpha budget in ops"}},
		{"q=alpha&kind=thread", []string{"alpha budget report", "alpha reply here"}},
		{"q=alpha&kind=message", []string{"alpha budget report", "alpha budget in ops", "alpha thanks from carol"}},
		{"q=alpha&kind=attachment", []string{}},
		{"q=alpha&kind=thread&author=bob", []string{"alpha reply here"}},
	}
	for _, c := range cases {
		for _, path := range []string{"/api/v1/search?", "/api/v1/search/hybrid?"} {
			got := searchBodies(bob.must("GET", path+c.q, nil, 200))
			if !sameSet(got, c.want...) {
				t.Errorf("%s%s: got %v want %v", path, c.q, got, c.want)
			}
		}
	}
	bob.must("GET", "/api/v1/search?q=alpha&kind=bogus", nil, 400)
	bob.must("GET", "/api/v1/search?q=alpha&author=nobody", nil, 400)
	// no embedder: hybrid still answers, text only, and says so
	out = bob.must("GET", "/api/v1/search/hybrid?q=alpha", nil, 200)
	if out["semantic"] != false {
		t.Fatalf("hybrid without an embedder must report semantic=false: %v", out["semantic"])
	}
	for _, raw := range out["results"].([]any) {
		if raw.(map[string]any)["via"] != "text" {
			t.Fatalf("text-only hybrid row must say via=text: %v", raw)
		}
	}
}

// keywordEmbedder maps a keyword family to one unit axis, so "budget" and
// "finance" land on the same vector and everything else is orthogonal.
type keywordEmbedder struct{}

func (keywordEmbedder) vector(text string) []float32 {
	v := make([]float32, 1536)
	axis := 9
	for word, dim := range map[string]int{"budget": 0, "finance": 0, "financial": 0, "lunch": 1, "webhook": 2} {
		if strings.Contains(strings.ToLower(text), word) {
			axis = dim
		}
	}
	v[axis] = 1
	return v
}

func (e keywordEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vector(t)
	}
	return out, nil
}

// TestHybridSearch: exact hits lead, semantic-only hits fill in tagged
// via=semantic, a message in both legs appears once, and filters reach the
// semantic leg too.
func TestHybridSearch(t *testing.T) {
	emb := keywordEmbedder{}
	srv, store := newTestServerCfg(t, Config{PublicURL: "http://public.test", Embedder: emb})
	defer srv.Close()
	_, alice, bob := setupRoom(t, srv.URL)
	alice.must("POST", "/api/v1/channels", map[string]any{"name": "ops"}, 201)
	bob.must("POST", "/api/v1/channels/ops/join", nil, 200)

	post := func(c *testClient, channel, body string) string {
		out := c.must("POST", "/api/v1/channels/"+channel+"/messages", map[string]any{"body": body}, 201)
		id := out["id"].(string)
		if err := store.PutEmbedding(context.Background(), id, emb.vector(body)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	post(alice, "general", "the quarterly budget forecast is due") // text + semantic for "budget"
	post(bob, "general", "finance review at noon")                 // semantic-only for "budget"
	post(bob, "ops", "finance numbers for ops")                    // semantic-only, other channel
	post(alice, "general", "lunch options near the office")        // noise

	out := bob.must("GET", "/api/v1/search/hybrid?q=budget", nil, 200)
	if out["semantic"] != true {
		t.Fatalf("semantic flag: %v", out["semantic"])
	}
	rows := out["results"].([]any)
	got := searchBodies(out)
	if len(got) < 3 || got[0] != "the quarterly budget forecast is due" {
		t.Fatalf("exact hit must lead: %v", got)
	}
	if rows[0].(map[string]any)["via"] != "text" {
		t.Fatalf("exact hit via: %v", rows[0])
	}
	count := 0
	for i, raw := range rows {
		r := raw.(map[string]any)
		if r["body"] == "the quarterly budget forecast is due" {
			count++
		}
		if i > 0 && r["via"] != "semantic" {
			t.Fatalf("semantic-only row %d must say via=semantic: %v", i, r)
		}
		if r["body"] == "lunch options near the office" {
			t.Fatalf("noise leaked into hybrid results: %v", got)
		}
	}
	if count != 1 {
		t.Fatalf("a message found by both legs must appear once, got %d in %v", count, got)
	}

	// filters reach the semantic leg: only the ops finance row survives
	got = searchBodies(bob.must("GET", "/api/v1/search/hybrid?q=budget&channel=ops", nil, 200))
	if !sameSet(got, "finance numbers for ops") {
		t.Fatalf("channel filter on the semantic leg: %v", got)
	}
	got = searchBodies(bob.must("GET", "/api/v1/search/hybrid?q=budget&author=alice", nil, 200))
	if !sameSet(got, "the quarterly budget forecast is due") {
		t.Fatalf("author filter on the semantic leg: %v", got)
	}
}

func TestFuseResults(t *testing.T) {
	mk := func(id string) models.SearchResult {
		var r models.SearchResult
		r.ID = id
		return r
	}
	text := []models.SearchResult{mk("A"), mk("B")}
	sem := []models.SearchResult{mk("C"), mk("B")}
	got := fuseResults(text, sem)
	ids := []string{}
	for _, r := range got {
		ids = append(ids, r.ID+":"+r.Via)
	}
	if strings.Join(ids, " ") != "B:text A:text C:semantic" {
		t.Fatalf("fusion order: %v", ids)
	}
	// the last text hit of a full leg still beats the best semantic-only hit
	text = nil
	for i := 0; i < fuseLegLimit; i++ {
		text = append(text, mk("t"+string(rune('a'+i%26))+string(rune('a'+i/26))))
	}
	got = fuseResults(text, []models.SearchResult{mk("S")})
	if got[len(got)-1].ID != "S" {
		t.Fatalf("semantic-only hit must rank after every text hit, got last %s", got[len(got)-1].ID)
	}
}

type brokenEmbedder struct{}

func (brokenEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("provider down")
}

// TestHybridSearchDegrades: a failing provider must not take the text leg
// down with it; the reply says semantic:false and carries the text hits.
func TestHybridSearchDegrades(t *testing.T) {
	srv, _ := newTestServerCfg(t, Config{PublicURL: "http://public.test", Embedder: brokenEmbedder{}})
	defer srv.Close()
	_, alice, _ := setupRoom(t, srv.URL)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "budget forecast"}, 201)
	out := alice.must("GET", "/api/v1/search/hybrid?q=budget", nil, 200)
	if out["semantic"] != false {
		t.Fatalf("semantic flag: %v", out["semantic"])
	}
	if got := searchBodies(out); !sameSet(got, "budget forecast") {
		t.Fatalf("text hits lost on provider failure: %v", got)
	}
}
