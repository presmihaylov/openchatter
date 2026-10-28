package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"testing"
)

// Every range a home-network server must never reach is refused, v4-mapped
// v6 included, and ordinary public addresses pass.
func TestPublicAddr(t *testing.T) {
	for _, s := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.5.5", "172.31.255.255", "192.168.1.10", "169.254.169.254",
		"100.64.0.1", "100.127.255.254", "0.0.0.0", "224.0.0.1", "255.255.255.255", "198.18.0.1",
		"::1", "::", "fe80::1", "fd00::1", "fc00::5", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"64:ff9b::a00:1",
	} {
		if PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s must be refused", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "100.128.0.1", "172.32.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !PublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s must pass", s)
		}
	}
}

func TestCheckURL(t *testing.T) {
	for _, bad := range []string{
		"http://example.com/hook", "ftp://example.com", "example.com/hook", "https://", "https://user:pw@example.com/",
		"https://localhost/hook", "https://api.localhost/x", "https://127.0.0.1/hook", "https://[::1]/hook",
		"https://10.0.0.5:8443/hook", "https://169.254.169.254/latest", "https://100.64.1.1/",
	} {
		if CheckURL(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if !errors.Is(CheckURL("https://192.168.1.2/x"), ErrBlocked) {
		t.Error("a private literal should report ErrBlocked")
	}
	for _, good := range []string{"https://example.com/hook", "https://hooks.example.org:8443/wake?agent=x", "https://1.1.1.1/x"} {
		if err := CheckURL(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
}

// The signature is HMAC-SHA256 over timestamp, a dot and the raw body, so a
// receiver can recompute it with the standard library alone.
func TestSign(t *testing.T) {
	body := []byte(`{"type":"message.created"}`)
	m := hmac.New(sha256.New, []byte("s3cret-s3cret-s3"))
	m.Write([]byte("1700000000." + string(body)))
	want := "v1=" + hex.EncodeToString(m.Sum(nil))
	if got := Sign("s3cret-s3cret-s3", 1700000000, body); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if Sign("s3cret-s3cret-s3", 1700000001, body) == want {
		t.Fatal("the timestamp must be signed")
	}
}
