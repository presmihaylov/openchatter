package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/presmihaylov/openchatter/web"
)

// The root sends a human to the login page; the account pages serve the SPA.
func TestRootRedirectsToLoginAndAccountPagesServeApp(t *testing.T) {
	srv, _ := newTestServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
		t.Fatalf("GET /: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	if _, err := web.Dist.ReadFile("dist/index.html"); err != nil {
		t.Skip("web/dist not built; run npm run build in web/")
	}
	for _, path := range []string{"/login", "/register", "/settings", "/create", "/r/some-slug", "/w/some-slug", "/w/some-slug/c/general"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

// Every served page carries the link card, with an absolute og:image: X and
// Slack drop a relative one, so the placeholder must be stamped, never shipped.
func TestServedPagesCarryTheSocialCard(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, err := web.Dist.ReadFile("dist/index.html"); err != nil {
		t.Skip("web/dist not built; run npm run build in web/")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/login", "/join/inv-0000-0000-0000-0000", "/register"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(body)
		if strings.Contains(page, "__PUBLIC_URL__") {
			t.Fatalf("GET %s still ships the unstamped placeholder", path)
		}
		want := `<meta property="og:image" content="http://public.test/brand/social-preview.png">`
		if !strings.Contains(page, want) {
			t.Fatalf("GET %s has no absolute og:image (want %s)", path, want)
		}
		if !strings.Contains(page, `name="twitter:card" content="summary_large_image"`) {
			t.Fatalf("GET %s has no large-image twitter card", path)
		}
	}
	// and the card itself is served, at the size GitHub and X expect
	resp, err := client.Get(srv.URL + "/brand/social-preview.png")
	if err != nil {
		t.Fatal(err)
	}
	card, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(card) == 0 {
		t.Fatalf("GET the social card: %d, %d bytes", resp.StatusCode, len(card))
	}
	if len(card) > 1024*1024 {
		t.Fatalf("the social card is over GitHub's 1 MB limit: %d bytes", len(card))
	}
}
