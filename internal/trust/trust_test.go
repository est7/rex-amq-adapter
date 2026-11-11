package trust

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo makes a main checkout with one commit and a linked worktree beside it.
func repo(t *testing.T) (main, wt string) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	main = filepath.Join(root, "app")
	wt = filepath.Join(root, "app-task")
	for _, args := range [][]string{
		{"init", "-q", main},
		{"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", "--detach", wt},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return main, wt
}

// settings writes Antigravity's settings file as the CLI formats it, with
// the given trusted workspaces.
func settings(t *testing.T, home string, trusted ...string) (path, text string) {
	t.Helper()
	q := make([]string, len(trusted))
	for i, w := range trusted {
		q[i] = "    \"" + w + "\""
	}
	text = "{\n  \"agentMode\": \"auto\",\n  \"notifications\": {\n    \"enabled\": true\n  },\n  \"trustedWorkspaces\": [\n" + strings.Join(q, ",\n") + "\n  ],\n  \"title\": \"x\"\n}\n"
	if len(trusted) == 0 {
		text = "{\n  \"agentMode\": \"auto\",\n  \"trustedWorkspaces\": [],\n  \"title\": \"x\"\n}\n"
	}
	path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, text
}

// trustedList parses a --settings value back into its list.
func trustedList(t *testing.T, settings string) []string {
	t.Helper()
	var s struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal([]byte(settings), &s); err != nil {
		t.Fatalf("settings %q: %v", settings, err)
	}
	return s.Trusted
}

// The worktree is trusted for the session through --settings, which carries
// the whole stored list plus the worktree; the store itself is never written.
func TestTrustedMainExtendsToItsLinkedWorktree(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	p, before := settings(t, home, "/Users/x/other", main)
	r, err := Derive(home, wt)
	if err != nil || r.Main != main || !strings.Contains(r.Note, "for this session") {
		t.Fatalf("Derive: %+v %v", r, err)
	}
	if got := strings.Join(trustedList(t, r.Settings), "|"); got != "/Users/x/other|"+main+"|"+wt {
		t.Fatalf("settings list %s", got)
	}
	if got, _ := os.ReadFile(p); string(got) != before {
		t.Fatalf("the store was written:\n%s", got)
	}
}

// An entry matches by file identity: another spelling of the main checkout
// counts only where the volume makes it the same directory.
func TestEntriesMatchByIdentityNotSpelling(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	other := filepath.Join(filepath.Dir(main), "APP")
	settings(t, home, other)
	a, errA := os.Stat(main)
	b, errB := os.Stat(other)
	same := errA == nil && errB == nil && os.SameFile(a, b)
	r, err := Derive(home, wt)
	if err != nil || (r.Settings != "") != same {
		t.Fatalf("same directory %v: %+v %v", same, r, err)
	}
	if same {
		if l := trustedList(t, r.Settings); l[len(l)-1] != wt {
			t.Fatalf("the worktree is not added with its own spelling: %v", l)
		}
	}
}

// A copied .git pointer does not make an unrelated directory a worktree.
func TestCopiedGitPointerDerivesNothing(t *testing.T) {
	main, wt := repo(t)
	fake := t.TempDir()
	raw, _ := os.ReadFile(filepath.Join(wt, ".git"))
	if err := os.WriteFile(filepath.Join(fake, ".git"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	settings(t, home, main)
	if r, err := Derive(home, fake); err != nil || r.Settings != "" || !strings.Contains(r.Note, "not a linked worktree") {
		t.Fatalf("%+v %v", r, err)
	}
	if _, ok, _ := MainCheckout(fake); ok {
		t.Fatal("a copied pointer resolved to a main checkout")
	}
}

func TestEmptyListGetsNothing(t *testing.T) {
	_, wt := repo(t)
	home := t.TempDir()
	settings(t, home)
	if r, err := Derive(home, wt); err != nil || r.Settings != "" {
		t.Fatalf("an empty list trusts nothing: %+v %v", r, err)
	}
}

// Args adds --settings to an agy session start only.
func TestArgsAddsSettingsToAnAgySessionOnly(t *testing.T) {
	const v = `{"trustedWorkspaces":["/a"]}`
	for name, tc := range map[string]struct {
		in, want []string
		settings string
	}{
		"session start":   {[]string{"agy", "--dangerously-skip-permissions", "-i", "brief"}, []string{"agy", "--settings", v, "--dangerously-skip-permissions", "-i", "brief"}, v},
		"absolute agy":    {[]string{"/opt/homebrew/bin/agy", "-i", "b"}, []string{"/opt/homebrew/bin/agy", "--settings", v, "-i", "b"}, v},
		"nothing derived": {[]string{"agy", "-i", "b"}, []string{"agy", "-i", "b"}, ""},
		"subcommand":      {[]string{"agy", "mcp", "list"}, []string{"agy", "mcp", "list"}, v},
		"own settings":    {[]string{"agy", "--settings", "{}", "-i", "b"}, []string{"agy", "--settings", "{}", "-i", "b"}, v},
		"other harness":   {[]string{"codex", "brief"}, []string{"codex", "brief"}, v},
		"no command":      {nil, nil, v},
	} {
		if got := Args(tc.in, tc.settings); strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestNothingIsDerivedWithoutTrustedMain(t *testing.T) {
	main, wt := repo(t)
	plain := t.TempDir()
	parent := filepath.Dir(main)
	for name, tc := range map[string]struct {
		dir     string
		trusted []string
		note    string
	}{
		"untrusted main":         {wt, []string{"/Users/x/other"}, "is not trusted"},
		"only the parent":        {wt, []string{parent}, "is not trusted"},
		"worktree already there": {wt, []string{main, wt}, "already trusted"},
		"main checkout":          {main, []string{main}, "not a linked worktree"},
		"plain directory":        {plain, []string{plain}, "not a linked worktree"},
	} {
		home := t.TempDir()
		p, text := settings(t, home, tc.trusted...)
		r, err := Derive(home, tc.dir)
		if err != nil || r.Settings != "" || !strings.Contains(r.Note, tc.note) {
			t.Errorf("%s: %+v %v", name, r, err)
		}
		if got, _ := os.ReadFile(p); string(got) != text {
			t.Errorf("%s: settings changed:\n%s", name, got)
		}
	}
	if r, err := Derive(t.TempDir(), wt); err != nil || r.Settings != "" || !strings.Contains(r.Note, "no trust store") {
		t.Errorf("missing store: %+v %v", r, err)
	}
}

func TestMainCheckout(t *testing.T) {
	main, wt := repo(t)
	if got, ok, err := MainCheckout(wt); err != nil || !ok || got != main {
		t.Fatalf("worktree: %q %v %v", got, ok, err)
	}
	if _, ok, err := MainCheckout(main); err != nil || ok {
		t.Fatalf("main: %v %v", ok, err)
	}
	if _, ok, err := MainCheckout(t.TempDir()); err != nil || ok {
		t.Fatalf("plain: %v %v", ok, err)
	}
	fake := t.TempDir()
	os.WriteFile(filepath.Join(fake, ".git"), []byte("gitdir: /nowhere/worktrees/x\n"), 0o600)
	if _, ok, err := MainCheckout(fake); err != nil || ok {
		t.Fatalf("dangling gitdir: %v %v", ok, err)
	}
}
