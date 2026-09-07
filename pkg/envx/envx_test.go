package envx

import "testing"

func TestGetReadsTheOpenchatterName(t *testing.T) {
	t.Setenv("OPENCHATTER_DB_URL", "new")
	if got := Get("DB_URL"); got != "new" {
		t.Fatalf("Get: %q, want the OPENCHATTER_ value", got)
	}
}

// The rename is a hard cut: an AGENTCHAT_ name left in a shell profile must not
// keep a service alive, or the break nobody sees is the one that bites later.
func TestGetIgnoresTheOldName(t *testing.T) {
	t.Setenv("OPENCHATTER_PORT", "")
	t.Setenv("AGENTCHAT_PORT", "8090")
	if got := Get("PORT"); got != "" {
		t.Fatalf("Get: %q, want empty: the AGENTCHAT_ name is gone", got)
	}
}
