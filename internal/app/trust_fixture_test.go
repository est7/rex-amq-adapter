package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/install"
)

// trustTemp records, in the world's own Claude store under TranscriptHome,
// the user's trust of the system temp directory, so the panes a test opens
// in its temp directories meet no folder dialog. A launch now preflights
// folder trust, and the user of these worlds has given it.
func trustTemp(t *testing.T, e Env) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.TrustClaudeProject(context.Background(), filepath.Join(e.TranscriptHome, ".claude.json"), tmp); err != nil {
		t.Fatal(err)
	}
}

// workDir is a directory under the world's home for an agent to work in: a
// launch is refused in one that does not exist, as Rex would open it in the
// home directory instead.
func workDir(t *testing.T, e Env, name string) string {
	t.Helper()
	dir := filepath.Join(e.TranscriptHome, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// trustAgy lists dirs in the world's own Antigravity store, which trusts
// exact directories only.
func trustAgy(t *testing.T, e Env, dirs ...string) {
	t.Helper()
	path := filepath.Join(e.TranscriptHome, ".gemini", "antigravity-cli", "settings.json")
	b, err := json.Marshal(map[string][]string{"trustedWorkspaces": dirs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// physical is dir with its symlinks resolved, the path a harness keys its
// folder trust by (os.TempDir is under /var, a symlink, on macOS).
func physical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
