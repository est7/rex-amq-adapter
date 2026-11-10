package app

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// An agent working under a grant runs under a scope, and the one thing the
// scope decides is whether it may write the shared base branch: a merge, a
// fast-forward after a rebase, or a deletion of refs/heads/<base>. Every other
// git operation, a push of its own branch included, is a two-way door and
// stays open (est9, 2026-11-10). It is checked locally because the server may
// not protect the base.
//
// The scope is REX_AMQ_SCOPE in the agent's environment, else the
// `rex-amq-adapter.scope` worktree config of the checkout it works in. Two
// files enforce it:
//   - a pre-push hook, the door every git write to the remote passes (a raw
//     `git push`, git-command's routes, Worktrunk), attached as core.hooksPath
//     through GIT_CONFIG_* for an agent the adapter launches (summon, scoped)
//     or as worktree config by ScopeWorktree; it runs the repository's own
//     hooks after its check, which core.hooksPath would hide;
//   - a glab wrapper first on a launched agent's PATH, for the one base write
//     that is not a push: a server-side merge.

const (
	// ScopeEnv is the variable that carries an agent's scope.
	ScopeEnv = "REX_AMQ_SCOPE"
	// ScopeConfig is the worktree config key ScopeWorktree writes.
	ScopeConfig = "rex-amq-adapter.scope"
	ScopePR     = "push+PR"
	ScopeMerge  = "merge"
)

//go:embed scopehooks/pre-push scopehooks/chain scopehooks/glab
var scopeFiles embed.FS

// chainedHooks are the client-side hook names git can run; each is a link to
// chain, which runs the repository's own hook of that name.
var chainedHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit",
	"pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "post-rewrite",
	"reference-transaction", "push-to-checkout", "pre-auto-gc",
	"sendemail-validate", "post-index-change",
}

func (e Env) scopeHooksDir() string { return filepath.Join(e.Paths.Home, "git-hooks") }
func (e Env) scopeBinDir() string   { return filepath.Join(e.Paths.Home, "scope-bin") }

func validScope(scope string) error {
	if scope != ScopePR && scope != ScopeMerge {
		return fmt.Errorf("unknown scope %q: expected %s or %s", scope, ScopePR, ScopeMerge)
	}
	return nil
}

// prepareScopeHooks writes the hooks directory and the glab wrapper when they
// differ from the ones this binary carries; it is normally a no-op.
func (e Env) prepareScopeHooks() error {
	for dir, names := range map[string][]string{e.scopeHooksDir(): {"pre-push", "chain"}, e.scopeBinDir(): {"glab"}} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for _, name := range names {
			if err := writeIfChanged(filepath.Join(dir, name), "scopehooks/"+name); err != nil {
				return err
			}
		}
	}
	for _, name := range chainedHooks {
		path := filepath.Join(e.scopeHooksDir(), name)
		if target, err := os.Readlink(path); err == nil && target == "chain" {
			continue
		}
		_ = os.Remove(path)
		if err := os.Symlink("chain", path); err != nil {
			return err
		}
	}
	return nil
}

func writeIfChanged(path, embedded string) error {
	want, err := scopeFiles.ReadFile(embedded)
	if err != nil {
		return err
	}
	if have, err := os.ReadFile(path); err != nil || !bytes.Equal(have, want) {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, want, 0o700); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o700)
}

// scopeEnv is the environment that puts an agent under scope: the scope, the
// hooks directory for git (appended after any GIT_CONFIG_* entries inherited
// holds), and the glab wrapper first on PATH.
func (e Env) scopeEnv(scope string, inherited []string) ([]string, error) {
	if err := validScope(scope); err != nil {
		return nil, err
	}
	if err := e.prepareScopeHooks(); err != nil {
		return nil, fmt.Errorf("write the scope hooks: %w", err)
	}
	n, path := 0, os.Getenv("PATH")
	for _, kv := range inherited {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c > 0 {
				n = c
			}
		}
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	i := strconv.Itoa(n)
	return []string{
		ScopeEnv + "=" + scope,
		"GIT_CONFIG_COUNT=" + strconv.Itoa(n+1),
		"GIT_CONFIG_KEY_" + i + "=core.hooksPath",
		"GIT_CONFIG_VALUE_" + i + "=" + e.scopeHooksDir(),
		"PATH=" + e.scopeBinDir() + string(os.PathListSeparator) + path,
	}, nil
}

// ExecScoped replaces the running process with argv under scope.
func (e Env) ExecScoped(scope string, argv []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	inherited := os.Environ()
	senv, err := e.scopeEnv(scope, inherited)
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, mergeEnv(inherited, senv))
}

// ScopeWorktree caps every git push made in the linked worktree at dir,
// whoever makes it: it attaches the hooks directory and records the scope as
// that worktree's own config. The main worktree is refused (its config is the
// user's checkout), and so is a repository whose common config sets
// core.worktree or a true core.bare, which enabling per-worktree config would
// change for every worktree.
func (e Env) ScopeWorktree(dir, scope string) error {
	if err := validScope(scope); err != nil {
		return err
	}
	gitOut := func(args ...string) (string, error) {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	gitDir, err := gitOut("rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return fmt.Errorf("%s is not a git work tree", dir)
	}
	common, err := gitOut("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if gitDir == common {
		return fmt.Errorf("%s is the main worktree; scope a linked task worktree", dir)
	}
	file := filepath.Join(common, "config")
	if v, _ := gitOut("config", "--file", file, "--get", "core.worktree"); v != "" {
		return fmt.Errorf("the repository sets core.worktree; refusing to enable per-worktree config")
	}
	if v, _ := gitOut("config", "--file", file, "--bool", "--get", "core.bare"); v == "true" {
		return fmt.Errorf("the repository sets core.bare=true; refusing to enable per-worktree config")
	}
	if err := e.prepareScopeHooks(); err != nil {
		return fmt.Errorf("write the scope hooks: %w", err)
	}
	for _, args := range [][]string{
		{"config", "extensions.worktreeConfig", "true"},
		{"config", "--worktree", "core.hooksPath", e.scopeHooksDir()},
		{"config", "--worktree", ScopeConfig, scope},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// mergeEnv returns base with each KEY=VALUE of over replacing its key.
func mergeEnv(base, over []string) []string {
	keys := map[string]bool{}
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		keys[k] = true
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, over...)
}
