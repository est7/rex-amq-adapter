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

// An agent the adapter launches runs under a scope, and the one thing the
// scope decides is whether it may write the shared base branch: a merge, a
// fast-forward after a rebase, or a deletion of refs/heads/<base>. Every other
// git operation, a push of its own branch included, is a two-way door and
// stays open (est9, 2026-11-10; git-command docs/spec.md §9.31).
//
// Two layers enforce it, both keyed on GITX_SCOPE. git-command's gitx.py
// refuses `land` and `propose --auto-merge` early with a readable NEXT. The
// pre-push hook below is the one every door passes: a raw `git push`, gitx's
// own pushes, and Worktrunk's. It is attached through GIT_CONFIG_* so it binds
// only the launched process tree, never the user's own git, and it runs the
// repository's own hooks after its check, which core.hooksPath would hide.

const (
	ScopePR    = "push+PR"
	ScopeMerge = "merge"
)

//go:embed scopehooks/pre-push scopehooks/chain
var scopeHookFiles embed.FS

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

// prepareScopeHooks writes the hooks directory when it differs from the one
// this binary carries; it is normally a no-op.
func (e Env) prepareScopeHooks() error {
	dir := e.scopeHooksDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"pre-push", "chain"} {
		want, err := scopeHookFiles.ReadFile("scopehooks/" + name)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, name)
		if have, err := os.ReadFile(path); err != nil || !bytes.Equal(have, want) {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, want, 0o700); err != nil {
				return err
			}
			if err := os.Rename(tmp, path); err != nil {
				return err
			}
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	for _, name := range chainedHooks {
		path := filepath.Join(dir, name)
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

// scopeEnv is the environment that puts an agent under scope: GITX_SCOPE for
// gitx.py and the hooks directory for git, appended after any GIT_CONFIG_*
// entries the caller's environment already holds.
func (e Env) scopeEnv(scope string, inherited []string) ([]string, error) {
	if scope != ScopePR && scope != ScopeMerge {
		return nil, fmt.Errorf("unknown scope %q: expected %s or %s", scope, ScopePR, ScopeMerge)
	}
	if err := e.prepareScopeHooks(); err != nil {
		return nil, fmt.Errorf("write the scope hooks: %w", err)
	}
	n := 0
	for _, kv := range inherited {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			if c, err := strconv.Atoi(v); err == nil && c > 0 {
				n = c
			}
		}
	}
	i := strconv.Itoa(n)
	return []string{
		GitxScopeEnv + "=" + scope,
		"GIT_CONFIG_COUNT=" + strconv.Itoa(n+1),
		"GIT_CONFIG_KEY_" + i + "=core.hooksPath",
		"GIT_CONFIG_VALUE_" + i + "=" + e.scopeHooksDir(),
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
