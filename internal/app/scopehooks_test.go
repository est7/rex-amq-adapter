package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/store"
)

// The hooks the adapter writes refuse a push to the base branch under
// push+PR, whatever door it comes through, and leave every other push and the
// repository's own hooks alone. testdata/scopehooks.sh holds the cases: base
// from the remote's HEAD, from .config/git-command.toml, from a superproject
// for a submodule, fail closed when none is found, and the chained hooks.
func TestScopeHooksRefuseOnlyTheBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	e := Env{Paths: store.Paths{Home: t.TempDir()}}
	if err := e.prepareScopeHooks(); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "testdata/scopehooks.sh", e.scopeHooksDir()).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(string(out), "PASS "); n < 17 {
		t.Fatalf("%d cases passed, want 17:\n%s", n, out)
	}
	// A second write is a no-op and a damaged file is restored.
	pre := filepath.Join(e.scopeHooksDir(), "pre-push")
	os.WriteFile(pre, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	if err := e.prepareScopeHooks(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(pre); !strings.Contains(string(b), "REX_AMQ_SCOPE") {
		t.Fatal("a damaged pre-push was not restored")
	}
}

func TestScopeEnvAppendsToInheritedGitConfig(t *testing.T) {
	e := Env{Paths: store.Paths{Home: t.TempDir()}}
	got, err := e.scopeEnv(ScopePR, []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"REX_AMQ_SCOPE=push+PR", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=" + e.scopeHooksDir(), "PATH=" + e.scopeBinDir() + ":/bin"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if _, err := e.scopeEnv("local", nil); err == nil {
		t.Fatal("local is no longer a scope")
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Bounded: a hook that calls itself shows as a failure, not a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REX_AMQ_SCOPE=",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// pushes reports whether `git push origin <spec>` from dir succeeds, with env
// added to a clean environment (no inherited scope, no global config).
func pushes(dir, spec string, env ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "push", "-q", "origin", spec)
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "REX_AMQ_SCOPE="), env...)
	return cmd.Run() == nil
}

// A linked task worktree scoped with ScopeWorktree refuses a write to the base
// from any process working in it, with no environment at all; the main
// worktree and other linked worktrees stay uncapped; REX_AMQ_SCOPE overrides.
func TestScopeWorktreeCapsEveryPushMadeThere(t *testing.T) {
	root := t.TempDir()
	e := Env{Paths: store.Paths{Home: filepath.Join(root, "home")}}
	origin, main := filepath.Join(root, "o.git"), filepath.Join(root, "main")
	gitT(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, root, "init", "-q", "-b", "main", main)
	gitT(t, main, "commit", "-q", "--allow-empty", "-m", "a")
	gitT(t, main, "remote", "add", "origin", origin)
	gitT(t, main, "push", "-q", "origin", "main")
	task, other := filepath.Join(root, "task"), filepath.Join(root, "other")
	gitT(t, main, "worktree", "add", "-q", "-b", "feat/task", task)
	gitT(t, main, "worktree", "add", "-q", "-b", "feat/other", other)
	gitT(t, task, "commit", "-q", "--allow-empty", "-m", "b")
	gitT(t, other, "commit", "-q", "--allow-empty", "-m", "c")

	if err := e.ScopeWorktree(main, ScopePR); err == nil || !strings.Contains(err.Error(), "main worktree") {
		t.Fatalf("the main worktree was scoped: %v", err)
	}
	if err := e.ScopeWorktree(task, "local"); err == nil {
		t.Fatal("local is no longer a scope")
	}
	if err := e.ScopeWorktree(task, ScopePR); err != nil {
		t.Fatal(err)
	}
	base := gitT(t, origin, "rev-parse", "main")
	if pushes(task, "HEAD:main") {
		t.Fatal("a raw push to the base from the scoped worktree went through")
	}
	if got := gitT(t, origin, "rev-parse", "main"); got != base {
		t.Fatalf("base moved to %s", got)
	}
	if !pushes(task, "HEAD:feat/task") {
		t.Fatal("the task branch could not be pushed")
	}
	if !pushes(task, "HEAD:refs/heads/main", ScopeEnv+"="+ScopeMerge) {
		t.Fatal("REX_AMQ_SCOPE=merge did not override the worktree's push+PR")
	}
	gitT(t, other, "fetch", "-q", "origin")
	gitT(t, other, "reset", "-q", "--hard", "origin/main")
	gitT(t, other, "commit", "-q", "--allow-empty", "-m", "c2")
	if !pushes(other, "HEAD:main") {
		t.Fatal("another worktree of the repository was capped too")
	}
	gitT(t, task, "fetch", "-q", "origin")
	gitT(t, task, "reset", "-q", "--hard", "origin/main")
	if err := e.ScopeWorktree(task, ScopeMerge); err != nil {
		t.Fatal(err)
	}
	// The repository's own hook still runs there, once (chain never calls itself).
	marker := filepath.Join(root, "own-pre-commit")
	os.WriteFile(filepath.Join(main, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\necho x >> "+marker+"\n"), 0o700)
	gitT(t, task, "commit", "-q", "--allow-empty", "-m", "d")
	if b, _ := os.ReadFile(marker); string(b) != "x\n" {
		t.Fatalf("own pre-commit ran %q times", string(b))
	}
	if !pushes(task, "HEAD:main") {
		t.Fatal("a worktree scoped merge could not write the base")
	}
}

// The glab wrapper refuses a server-side merge under push+PR and passes every
// other call, and every call under merge, to the real glab.
func TestGlabWrapperRefusesOnlyAServerSideMerge(t *testing.T) {
	root := t.TempDir()
	e := Env{Paths: store.Paths{Home: filepath.Join(root, "home")}}
	if err := e.prepareScopeHooks(); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	os.MkdirAll(real, 0o700)
	logf := filepath.Join(root, "calls")
	os.WriteFile(filepath.Join(real, "glab"), []byte("#!/bin/sh\necho \"$*\" >> "+logf+"\n"), 0o700)
	run := func(scope string, args ...string) bool {
		cmd := exec.Command(filepath.Join(e.scopeBinDir(), "glab"), args...)
		cmd.Env = append(os.Environ(), "PATH="+e.scopeBinDir()+":"+real+":/usr/bin:/bin", ScopeEnv+"="+scope)
		cmd.Dir = root
		return cmd.Run() == nil
	}
	for _, args := range [][]string{{"mr", "merge", "7"}, {"api", "--method", "PUT", "projects/:id/merge_requests/7/merge", "-f", "sha=x"}} {
		if run(ScopePR, args...) {
			t.Fatalf("push+PR ran glab %v", args)
		}
	}
	if !run(ScopePR, "mr", "list") || !run(ScopePR, "mr", "create", "--yes") || !run(ScopeMerge, "mr", "merge", "7") {
		t.Fatal("an allowed call was refused")
	}
	calls, _ := os.ReadFile(logf)
	if got := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(got) != 3 || got[2] != "mr merge 7" {
		t.Fatalf("real glab saw %q", got)
	}
}
