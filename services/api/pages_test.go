package api

import (
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
