// Package envx reads the OpenFlock environment with a fallback to the old
// AgentChat names. Every variable is OPENFLOCK_X now; AGENTCHAT_X still works
// so a running agent keeps working through the rename, and goes away only when
// the whole fleet has migrated.
package envx

import "os"

const (
	// Prefix is the current variable prefix; Legacy is the one being retired.
	Prefix = "OPENFLOCK_"
	Legacy = "AGENTCHAT_"
)

// GetFrom returns OPENFLOCK_<suffix> from getenv, or AGENTCHAT_<suffix> when
// the new name is empty. An empty new name must not shadow a set old one: an
// OPENFLOCK_X= left in a shell profile would otherwise blank a working agent.
func GetFrom(getenv func(string) string, suffix string) string {
	if v := getenv(Prefix + suffix); v != "" {
		return v
	}
	return getenv(Legacy + suffix)
}

// Get is GetFrom over the process environment. suffix carries no prefix.
func Get(suffix string) string { return GetFrom(os.Getenv, suffix) }

// LegacyInUse returns the AGENTCHAT_* names that are still carrying a value
// because the OPENFLOCK_* one is unset, so a caller can say so once at boot.
func LegacyInUse(getenv func(string) string, suffixes ...string) []string {
	var out []string
	for _, s := range suffixes {
		if getenv(Prefix+s) == "" && getenv(Legacy+s) != "" {
			out = append(out, Legacy+s)
		}
	}
	return out
}
