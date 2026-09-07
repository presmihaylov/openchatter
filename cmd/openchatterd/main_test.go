package main

import "testing"

func TestAccessConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if id, sec, err := accessConfig(env(map[string]string{})); err != nil || id != "" || sec != "" {
		t.Fatalf("tunnel off: got %q %q %v", id, sec, err)
	}
	// a service token without the tunnel flag is ignored, not baked into the CLI
	if id, _, _ := accessConfig(env(map[string]string{"CF_ACCESS_CLIENT_ID": "a", "CF_ACCESS_CLIENT_SECRET": "b"})); id != "" {
		t.Fatalf("token baked in without CLOUDFLARE_TUNNEL=true")
	}
	if _, _, err := accessConfig(env(map[string]string{"CLOUDFLARE_TUNNEL": "true", "CF_ACCESS_CLIENT_ID": "a"})); err == nil {
		t.Fatal("half a service token must refuse to start")
	}
	id, sec, err := accessConfig(env(map[string]string{"CLOUDFLARE_TUNNEL": "true", "CF_ACCESS_CLIENT_ID": "a", "CF_ACCESS_CLIENT_SECRET": "b"}))
	if err != nil || id != "a" || sec != "b" {
		t.Fatalf("tunnel on: got %q %q %v", id, sec, err)
	}
}

func TestAuthConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	reg, ttl, err := authConfig(env(map[string]string{}))
	if err != nil || !reg || ttl.Hours() != 720 {
		t.Fatalf("defaults: %v %v %v", reg, ttl, err)
	}
	reg, ttl, err = authConfig(env(map[string]string{"OPENCHATTER_REGISTRATION_ENABLED": "false", "OPENCHATTER_SESSION_TTL": "48h"}))
	if err != nil || reg || ttl.Hours() != 48 {
		t.Fatalf("explicit: %v %v %v", reg, ttl, err)
	}
	if _, _, err := authConfig(env(map[string]string{"OPENCHATTER_SESSION_TTL": "soon"})); err == nil {
		t.Fatal("a bad TTL must refuse to start")
	}
	if _, _, err := authConfig(env(map[string]string{"OPENCHATTER_REGISTRATION_ENABLED": "maybe"})); err == nil {
		t.Fatal("a bad registration flag must refuse to start")
	}
}

func TestParseFlagsMigrateTo(t *testing.T) {
	v, err := parseFlags(nil)
	if err != nil || v != nil {
		t.Fatalf("no flags: got %v %v, want the server path", v, err)
	}
	v, err = parseFlags([]string{"-migrate-to", "23"})
	if err != nil || v == nil || *v != 23 {
		t.Fatalf("-migrate-to 23: got %v %v", v, err)
	}
	// 0 is not a version golang-migrate can target; refuse rather than guess
	if _, err := parseFlags([]string{"-migrate-to", "0"}); err == nil {
		t.Fatal("-migrate-to 0 must be refused")
	}
	if _, err := parseFlags([]string{"-migrate-to", "soon"}); err == nil {
		t.Fatal("a non-numeric version must be refused")
	}
	if _, err := parseFlags([]string{"-serve-please"}); err == nil {
		t.Fatal("an unknown flag must be refused")
	}
}

func TestProvidersClerkBehindSecretKey(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if got := authProviders(nil, true, env(map[string]string{})).Names(); len(got) != 1 || got[0] != "password" {
		t.Fatalf("default providers: %v", got)
	}
	got := authProviders(nil, true, env(map[string]string{"CLERK_SECRET_KEY": "sk_test_x"})).Names()
	if len(got) != 2 || got[1] != "clerk" {
		t.Fatalf("with CLERK_SECRET_KEY: %v", got)
	}
}
