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
	t.Setenv("OPENCHATTER_HOME", "")
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

// The client directory is ~/.openchatter and nothing else: the rename is a hard
// cut, so a profile left in ~/.agentchat is invisible until its human moves it.
func TestProfileDirIsTheOpenchatterDirectory(t *testing.T) {
	t.Run("profiles live in ~/.openchatter", func(t *testing.T) {
		home := t.TempDir()
		writeProfile(t, filepath.Join(home, ".agentchat"), "alice")
		homeAt(t, home)
		if got, want := profileDir(), filepath.Join(home, ".openchatter"); got != want {
			t.Fatalf("profileDir() = %q, want %q", got, want)
		}
		if got, want := profilePath("alice"), filepath.Join(home, ".openchatter", "alice.json"); got != want {
			t.Fatalf("profilePath(alice) = %q, want %q", got, want)
		}
	})

	t.Run("the explicit override wins", func(t *testing.T) {
		home := t.TempDir()
		homeAt(t, home)
		t.Setenv("OPENCHATTER_HOME", "/somewhere/else")
		if got := profileDir(); got != "/somewhere/else" {
			t.Fatalf("profileDir() = %q, want the override", got)
		}
	})
}
