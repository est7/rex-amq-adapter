package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if b, _ := os.ReadFile(pre); !strings.Contains(string(b), "GITX_SCOPE") {
		t.Fatal("a damaged pre-push was not restored")
	}
}

func TestScopeEnvAppendsToInheritedGitConfig(t *testing.T) {
	e := Env{Paths: store.Paths{Home: t.TempDir()}}
	got, err := e.scopeEnv(ScopePR, []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GITX_SCOPE=push+PR", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_2=core.hooksPath", "GIT_CONFIG_VALUE_2=" + e.scopeHooksDir()}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if _, err := e.scopeEnv("local", nil); err == nil {
		t.Fatal("local is no longer a scope")
	}
}
