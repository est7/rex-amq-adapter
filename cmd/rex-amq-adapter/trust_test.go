package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateTrust also points every harness store at a temp home, so a grant
// never reaches the user's own ~/.claude.json, config.toml, or trust.json.
func isolateTrust(t *testing.T) string {
	t.Helper()
	isolate(t)
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR"} {
		t.Setenv(v, "")
	}
	return home
}

// trust grant writes each project's key into Claude's and Codex's stores
// under HOME and prints what it did; the single-directory form still prints agy's --settings or exits 1.
func TestTrustGrantCommand(t *testing.T) {
	home := isolateTrust(t)
	repo := filepath.Join(home, "projects", "app")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	code, out := capture(t, runTrust, "grant", repo)
	if code != 0 {
		t.Fatalf("grant: exit %d %s", code, out)
	}
	var r struct{ Root, Claude, Codex, Source string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil || r.Root != repo || r.Claude != "set" || r.Codex != "set" || r.Source != "explicit" {
		t.Fatalf("grant printed %q (%v)", out, err)
	}
	claude, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	codex, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(string(claude), repo) || !strings.Contains(string(codex), repo) {
		t.Fatalf("stores under HOME:\n%s\n%s", claude, codex)
	}
	if log, _ := os.ReadFile(filepath.Join(os.Getenv("REX_AMQ_ADAPTER_HOME"), "logs", "trust.log")); !strings.Contains(string(log), repo) {
		t.Fatalf("trust.log %q", log)
	}
	if code, _ := capture(t, runTrust, "grant", filepath.Join(home, "missing")); code != 1 {
		t.Errorf("grant of a missing directory: exit %d", code)
	}
	for _, args := range [][]string{{"grant"}, {"grant", "-x"}, {"a", "b"}} {
		if code, _ := capture(t, runTrust, args...); code != 2 {
			t.Errorf("trust %v: exit %d, want 2", args, code)
		}
	}
	if code, out := capture(t, runTrust, repo); code != 1 || out != "" {
		t.Errorf("trust <dir> of a main checkout: %d %q", code, out)
	}
}
