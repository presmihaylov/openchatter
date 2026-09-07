package api

import (
	"net/http"
	"testing"
)

// the logo and favicons live under /brand in the Vite output; the server has
// to route them like /assets, or every tab icon and heading logo 404s
func TestBrandAssetsServed(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/brand/favicon-32.png", "/brand/openchatter-logo-mark.png", "/favicon.ico"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d, want 200", path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct == "text/html; charset=utf-8" {
			t.Errorf("%s: served as html", path)
		}
	}
}
