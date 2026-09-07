package main

import (
	"os"
	"path/filepath"
	"testing"
)

// homeAt points both the override and the real HOME at dir, so the tests below
// exercise the directory search itself and never an inherited developer HOME.
func homeAt(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("OPENFLOCK_HOME", "")
	t.Setenv("AGENTCHAT_HOME", "")
	t.Setenv("HOME", dir)
}

func writeProfile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, []byte(`{"name":"`+name+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A profile is a token file. The rename must never make one invisible, so the
// directory search asks which directory HOLDS profiles, not which one exists.
func TestProfileDirPicksTheDirectoryThatHoldsProfiles(t *testing.T) {
	t.Run("fresh install writes to the new directory", func(t *testing.T) {
		home := t.TempDir()
		homeAt(t, home)
		if got, want := profileDir(), filepath.Join(home, ".openflock"); got != want {
			t.Fatalf("profileDir() = %q, want %q", got, want)
		}
	})

	t.Run("an un-migrated install keeps its old directory", func(t *testing.T) {
		home := t.TempDir()
		writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
		homeAt(t, home)
		if got, want := profileDir(), filepath.Join(home, ".agentchat"); got != want {
			t.Fatalf("profileDir() = %q, want %q", got, want)
		}
	})

	// the join instructions mkdir ~/.openflock for cli.sh well before any Go
	// profile moves over; an empty new directory must not hide the old one
	t.Run("an empty new directory does not hide the old one", func(t *testing.T) {
		home := t.TempDir()
		writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
		if err := os.MkdirAll(filepath.Join(home, ".openflock"), 0o700); err != nil {
			t.Fatal(err)
		}
		homeAt(t, home)
		if got, want := profileDir(), filepath.Join(home, ".agentchat"); got != want {
			t.Fatalf("profileDir() = %q, want %q", got, want)
		}
	})

	t.Run("a migrated install wins", func(t *testing.T) {
		home := t.TempDir()
		writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
		writeProfile(t, filepath.Join(home, ".openflock"), "bob")
		homeAt(t, home)
		if got, want := profileDir(), filepath.Join(home, ".openflock"); got != want {
			t.Fatalf("profileDir() = %q, want %q", got, want)
		}
	})

	t.Run("the explicit override still wins over both", func(t *testing.T) {
		home := t.TempDir()
		writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
		homeAt(t, home)
		t.Setenv("OPENFLOCK_HOME", "/somewhere/else")
		if got := profileDir(); got != "/somewhere/else" {
			t.Fatalf("profileDir() = %q, want the override", got)
		}
	})
}

// A half-migrated home is the dangerous one: the new directory holds a profile,
// so it wins the search, but an identity left behind must still load by name.
func TestProfilePathFindsALegacyProfileAfterTheMove(t *testing.T) {
	home := t.TempDir()
	legacy := writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
	writeProfile(t, filepath.Join(home, ".openflock"), "bob")
	homeAt(t, home)

	if got := profilePath("alice"); got != legacy {
		t.Fatalf("profilePath(alice) = %q, want the legacy profile %q", got, legacy)
	}
	if got, want := profilePath("bob"), filepath.Join(home, ".openflock", "bob.json"); got != want {
		t.Fatalf("profilePath(bob) = %q, want %q", got, want)
	}
	// a name that exists nowhere resolves to the new directory, so a fresh
	// join never writes back into the directory being retired
	if got, want := profilePath("carol"), filepath.Join(home, ".openflock", "carol.json"); got != want {
		t.Fatalf("profilePath(carol) = %q, want %q", got, want)
	}
}
