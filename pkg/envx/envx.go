// Package envx reads the OpenChatter environment. Every variable is OPENCHATTER_X;
// the old AGENTCHAT_X names are gone, so a stale one is simply unset.
package envx

import "os"

// Prefix is the variable prefix for every OpenChatter setting.
const Prefix = "OPENCHATTER_"

// GetFrom returns OPENCHATTER_<suffix> from getenv. suffix carries no prefix.
func GetFrom(getenv func(string) string, suffix string) string {
	return getenv(Prefix + suffix)
}

// Get is GetFrom over the process environment.
func Get(suffix string) string { return GetFrom(os.Getenv, suffix) }
