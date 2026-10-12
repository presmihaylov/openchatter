package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The laptop rollout regenerates each agent's watcher from the served template:
// it keeps that agent's ME, WATCH and BASE, backs the old file up, swaps it by
// rename, and a --skill refresh keeps the front matter and the hardlink.
func TestRolloutWatchersKeepsEachAgentsLines(t *testing.T) {
	for _, bin := range []string{"sh", "curl", "awk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	srv, _ := newTestServer(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".openchatter")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := func(me, watch, base string) string {
		return "#!/bin/sh\n# Hardened OpenChatter watcher 2.9.0.\nME=\"" + me + "\"   # mine\nWATCH=\"" + watch + "\" # old comment\nBASE=\"" + base + "\"\nWATCHER_VERSION=\"2.9.0\"\necho old\n"
	}
	agents := map[string][3]string{
		"room.lead":      {"Lead", "", `$HOME/.openchatter/room.lead`},
		"room.ops-bot-2": {"ops-bot-2", "ops alerts", `$HOME/.openchatter/room.ops-bot-2`},
	}
	for name, a := range agents {
		if err := os.WriteFile(filepath.Join(dir, name+".watch.sh"), []byte(old(a[0], a[1], a[2])), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "room.lead.env"), []byte("SERVER="+srv.URL+"\nTOKEN=act_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a watcher of another room is not this rollout's business
	other := filepath.Join(dir, "elsewhere.lead.watch.sh")
	if err := os.WriteFile(other, []byte(old("Lead", "", "x")), 0o755); err != nil {
		t.Fatal(err)
	}
	// one SKILL.md hardlinked into two homes, with front matter
	skillA := filepath.Join(home, "a", "SKILL.md")
	skillB := filepath.Join(home, "b", "SKILL.md")
	for _, d := range []string{filepath.Dir(skillA), filepath.Dir(skillB)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(skillA, []byte("---\nname: openchatter\ndescription: chat\n---\n# old body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(skillA, skillB); err != nil {
		t.Fatal(err)
	}

	script, _ := filepath.Abs("../../scripts/rollout-watchers.sh")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{script, "room"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rollout %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	if out := run("--dry-run", "--skill", skillA); !strings.Contains(out, "would regenerate") || !strings.Contains(out, "would refresh") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "room.lead.watch.sh")); !strings.Contains(string(b), "echo old") {
		t.Fatal("a dry run changed a watcher")
	}

	out := run("--skill", skillA)
	if strings.Count(out, "regenerated ") != 2 || !strings.Contains(out, "2.9.0 -> 3.0.1") {
		t.Fatalf("rollout output:\n%s", out)
	}
	for name, a := range agents {
		path := filepath.Join(dir, name+".watch.sh")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := string(b)
		for _, want := range []string{
			"ME=\"" + a[0] + "\"   # mine\n", "WATCH=\"" + a[1] + "\" # old comment\n", "BASE=\"" + a[2] + "\"\n",
			`WATCHER_VERSION="3.0.1"`, "finish_once",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(got, "<your-name>") || strings.Contains(got, "echo old") {
			t.Fatalf("%s still has a placeholder or old code", name)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", name, st.Mode())
		}
		backups, _ := filepath.Glob(path + ".bak-*")
		if len(backups) != 1 {
			t.Fatalf("%s backups: %v", name, backups)
		}
		if b, _ := os.ReadFile(backups[0]); !strings.Contains(string(b), "echo old") {
			t.Fatalf("%s backup is not the old file", name)
		}
		if err := exec.Command("sh", "-n", path).Run(); err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(other); !strings.Contains(string(b), "echo old") {
		t.Fatal("the rollout touched another room's watcher")
	}
	if strings.Contains(out, "act_secret") {
		t.Fatalf("the rollout printed a token:\n%s", out)
	}

	b, _ := os.ReadFile(skillB)
	got := string(b)
	if !strings.HasPrefix(got, "---\nname: openchatter\ndescription: chat\n---\n# OpenChatter") || strings.Contains(got, "old body") {
		t.Fatalf("the hardlinked SKILL.md should keep its front matter and get the served body:\n%.300s", got)
	}
	sa, _ := os.Stat(skillA)
	sb, _ := os.Stat(skillB)
	if !os.SameFile(sa, sb) {
		t.Fatal("the refresh broke the hardlink")
	}
	if backups, _ := filepath.Glob(skillA + ".bak-*"); len(backups) != 1 {
		t.Fatalf("SKILL.md backups: %v", backups)
	}
}
