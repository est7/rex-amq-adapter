package trust

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

func TestTrustedMainExtendsToItsLinkedWorktree(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	p, before := settings(t, home, "/Users/x/other", main)
	r, err := Derive(home, wt)
	if err != nil || !r.Written || r.Main != main {
		t.Fatalf("Derive: %+v %v", r, err)
	}
	got, _ := os.ReadFile(p)
	want := strings.Replace(before, "\""+main+"\"\n  ]", "\""+main+"\",\n    \""+wt+"\"\n  ]", 1)
	if string(got) != want {
		t.Fatalf("settings:\n%s\nwant:\n%s", got, want)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// A second run finds the entry and leaves the bytes alone.
	if r, err := Derive(home, wt); err != nil || r.Written || !strings.Contains(r.Note, "already trusted") {
		t.Fatalf("second Derive: %+v %v", r, err)
	}
	if again, _ := os.ReadFile(p); string(again) != want {
		t.Fatal("the settings changed on a no-op run")
	}
}

// An entry matches by file identity: another spelling of the main checkout
// counts only where the volume makes it the same directory.
func TestEntriesMatchByIdentityNotSpelling(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	other := filepath.Join(filepath.Dir(main), "APP")
	p, text := settings(t, home, other)
	a, errA := os.Stat(main)
	b, errB := os.Stat(other)
	same := errA == nil && errB == nil && os.SameFile(a, b)
	r, err := Derive(home, wt)
	if err != nil || r.Written != same {
		t.Fatalf("same directory %v: %+v %v", same, r, err)
	}
	got, _ := os.ReadFile(p)
	if same && !strings.Contains(string(got), "    \""+wt+"\"\n  ]") {
		t.Fatalf("the worktree was not appended with its own spelling:\n%s", got)
	}
	if !same && string(got) != text {
		t.Fatalf("a different directory's trust leaked:\n%s", got)
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
	p, text := settings(t, home, main)
	if r, err := Derive(home, fake); err != nil || r.Written || !strings.Contains(r.Note, "not a linked worktree") {
		t.Fatalf("%+v %v", r, err)
	}
	if got, _ := os.ReadFile(p); string(got) != text {
		t.Fatalf("settings changed:\n%s", got)
	}
	if _, ok, _ := MainCheckout(fake); ok {
		t.Fatal("a copied pointer resolved to a main checkout")
	}
}

// Peers start together; every derivation lands.
func TestConcurrentDerivationsAllLand(t *testing.T) {
	main, first := repo(t)
	const n = 12
	wts := []string{first}
	for i := 1; i < n; i++ {
		wt := filepath.Join(filepath.Dir(main), fmt.Sprintf("peer-%d", i))
		if out, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "--detach", wt).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		wts = append(wts, wt)
	}
	home := t.TempDir()
	p, _ := settings(t, home, main)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, wt := range wts {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			<-start
			if r, err := Derive(home, w); err != nil || !r.Written {
				t.Errorf("%s: %+v %v", w, r, err)
			}
		}(wt)
	}
	close(start)
	wg.Wait()
	raw, _ := os.ReadFile(p)
	var s struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || len(s.Trusted) != n+1 {
		t.Fatalf("%d entries, want %d: %v\n%s", len(s.Trusted), n+1, err, raw)
	}
}

// A managed store behind a symlink is updated through the link.
func TestSymlinkedStoreKeepsItsLink(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	p, _ := settings(t, home, main)
	target := filepath.Join(home, "managed-settings.json")
	if err := os.Rename(p, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if r, err := Derive(home, wt); err != nil || !r.Written {
		t.Fatalf("%+v %v", r, err)
	}
	if info, _ := os.Lstat(p); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), wt) {
		t.Fatalf("the target was not updated:\n%s", got)
	}
}

func TestEmptyListGetsItsFirstEntry(t *testing.T) {
	main, wt := repo(t)
	_ = main
	home := t.TempDir()
	p, _ := settings(t, home)
	if r, err := Derive(home, wt); err != nil || r.Written {
		t.Fatalf("an empty list trusts nothing: %+v %v", r, err)
	}
	if got, _ := os.ReadFile(p); !strings.Contains(string(got), "\"trustedWorkspaces\": []") {
		t.Fatalf("settings changed:\n%s", got)
	}
}

func TestNothingIsWrittenWithoutDerivableTrust(t *testing.T) {
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
		if err != nil || r.Written || !strings.Contains(r.Note, tc.note) {
			t.Errorf("%s: %+v %v", name, r, err)
		}
		if got, _ := os.ReadFile(p); string(got) != text {
			t.Errorf("%s: settings changed:\n%s", name, got)
		}
	}
	if r, err := Derive(t.TempDir(), wt); err != nil || r.Written || !strings.Contains(r.Note, "no trust store") {
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
