package envx

import (
	"reflect"
	"testing"
)

func TestGetPrefersTheNewName(t *testing.T) {
	t.Setenv("OPENFLOCK_DB_URL", "new")
	t.Setenv("AGENTCHAT_DB_URL", "old")
	if got := Get("DB_URL"); got != "new" {
		t.Fatalf("Get: %q, want the OPENFLOCK_ value", got)
	}
}

func TestGetFallsBackToTheOldName(t *testing.T) {
	t.Setenv("AGENTCHAT_PORT", "8090")
	if got := Get("PORT"); got != "8090" {
		t.Fatalf("Get: %q, want the AGENTCHAT_ value", got)
	}
}

// an empty new name must not shadow a set old one, or an OPENFLOCK_X= left in
// a shell profile silently blanks a working agent's config
func TestEmptyNewNameFallsBack(t *testing.T) {
	t.Setenv("OPENFLOCK_PORT", "")
	t.Setenv("AGENTCHAT_PORT", "8090")
	if got := Get("PORT"); got != "8090" {
		t.Fatalf("Get: %q, want the AGENTCHAT_ value", got)
	}
}

func TestUnsetIsEmpty(t *testing.T) {
	if got := Get("NOTHING_SET_ANYWHERE"); got != "" {
		t.Fatalf("Get: %q", got)
	}
}

func TestLegacyInUseNamesOnlyTheUnmigrated(t *testing.T) {
	env := map[string]string{
		"OPENFLOCK_DB_URL": "new", "AGENTCHAT_DB_URL": "old",
		"AGENTCHAT_PORT": "8090",
	}
	got := LegacyInUse(func(k string) string { return env[k] }, "DB_URL", "PORT", "PUBLIC_URL")
	if !reflect.DeepEqual(got, []string{"AGENTCHAT_PORT"}) {
		t.Fatalf("LegacyInUse: %v", got)
	}
}
