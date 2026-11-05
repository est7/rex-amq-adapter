package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func supervisorAgent(t *testing.T, kind core.Kind, content string) (Agent, string) {
	t.Helper()
	old := managedClaudeSettings
	managedClaudeSettings = filepath.Join(t.TempDir(), "none.json")
	t.Cleanup(func() { managedClaudeSettings = old })
	name := "settings.json"
	if kind == core.Codex {
		name = "config.toml"
	}
	path := filepath.Join(t.TempDir(), name)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return Agent{Kind: kind, Name: string(kind), SupervisorFile: path, Found: true}, path
}

// Setup turns off Claude Code's agent view and Codex's shared daemon, which
// run sessions with another terminal's REX_BLOCK; a value the user set the
// other way is kept unless --force, and remove never turns them back on.
func TestSupervisorStates(t *testing.T) {
	for _, c := range []struct {
		name, content string
		kind          core.Kind
		want          string
	}{
		{"claude no file", ``, core.Claude, "missing"},
		{"claude default", `{"hooks": {}}`, core.Claude, "missing"},
		{"claude off", `{"disableAgentView": true}`, core.Claude, "current"},
		{"claude on", `{"disableAgentView": false}`, core.Claude, "foreign"},
		{"claude env", `{"env": {"CLAUDE_CODE_DISABLE_AGENT_VIEW": "1"}}`, core.Claude, "current"},
		{"codex no file", ``, core.Codex, "missing"},
		{"codex default", "model = \"x\"\n[features]\nhooks = true\n", core.Codex, "missing"},
		{"codex off", "[features]\napps = false\ndaemon_auto_start = false # why\n[other]\n", core.Codex, "current"},
		{"codex flat", "features.daemon_auto_start = false\n[x]\n", core.Codex, "current"},
		{"codex other table", "[agents.x]\ndaemon_auto_start = false\n", core.Codex, "missing"},
		{"codex on", "[features]\ndaemon_auto_start = true\n", core.Codex, "foreign"},
		{"codex inline", "features = { hooks = true }\n", core.Codex, "foreign"},
	} {
		a, _ := supervisorAgent(t, c.kind, c.content)
		if got, err := supervisorState(a); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	a, _ := supervisorAgent(t, core.Claude, `{}`)
	if err := os.WriteFile(managedClaudeSettings, []byte(`{"disableAgentView": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := supervisorState(a); got != "current" {
		t.Errorf("managed settings turn agent view off: %q", got)
	}
}

func TestSetupTurnsAgentViewOff(t *testing.T) {
	a, path := supervisorAgent(t, core.Claude, `{"model": "x", "hooks": {}}`)
	it, err := setupSupervisor(a, Options{})
	if err != nil || it.Change != "created" {
		t.Fatalf("setup: %+v %v", it, err)
	}
	got := read(t, path)
	if !strings.Contains(got, `"disableAgentView": true`) || !strings.Contains(got, `"model": "x"`) {
		t.Errorf("settings after setup:\n%s", got)
	}
	if it, _ := setupSupervisor(a, Options{}); it.Change != "unchanged" {
		t.Errorf("second setup: %+v", it)
	}
	if it, _ := removeSupervisor(a); it.Change != "kept" || read(t, path) != got {
		t.Errorf("remove turned agent view back on: %+v", it)
	}

	a, path = supervisorAgent(t, core.Claude, `{"disableAgentView": false}`)
	if it, _ := setupSupervisor(a, Options{}); it.Change != "conflict" || read(t, path) != `{"disableAgentView": false}` {
		t.Errorf("setup overrode the user's choice: %+v\n%s", it, read(t, path))
	}
	if it, _ := setupSupervisor(a, Options{Force: true}); it.Change != "updated" || !strings.Contains(read(t, path), `"disableAgentView": true`) {
		t.Errorf("--force: %+v\n%s", it, read(t, path))
	}
}

func TestSetupTurnsTheCodexDaemonOff(t *testing.T) {
	for _, c := range []struct {
		name, content, want string
		force               bool
		change              string
	}{
		{"into features", "model = \"x\"\n\n[features]\nhooks = true\n\n[mcp]\na = 1\n",
			"model = \"x\"\n\n[features]\n" + codexDaemonComment + "\ndaemon_auto_start = false\nhooks = true\n\n[mcp]\na = 1\n", false, "created"},
		{"new table", "model = \"x\"\n[mcp]\na = 1",
			"model = \"x\"\n[mcp]\na = 1\n\n[features]\n" + codexDaemonComment + "\ndaemon_auto_start = false\n", false, "created"},
		{"no file", "", "[features]\n" + codexDaemonComment + "\ndaemon_auto_start = false\n", false, "created"},
		{"on, kept", "[features]\ndaemon_auto_start = true # mine\n", "[features]\ndaemon_auto_start = true # mine\n", false, "conflict"},
		{"on, forced", "[features]\ndaemon_auto_start = true # mine\n", "[features]\ndaemon_auto_start = false # mine\n", true, "updated"},
		{"flat, forced", "features.daemon_auto_start = true\n[features]\nhooks = true\n", "features.daemon_auto_start = false\n[features]\nhooks = true\n", true, "updated"},
	} {
		a, path := supervisorAgent(t, core.Codex, c.content)
		it, err := setupSupervisor(a, Options{Force: c.force})
		if err != nil || it.Change != c.change {
			t.Errorf("%s: %+v %v", c.name, it, err)
			continue
		}
		got := ""
		if _, err := os.Stat(path); err == nil {
			got = read(t, path)
		}
		if got != c.want {
			t.Errorf("%s: config.toml\n%q\nwant\n%q", c.name, got, c.want)
		}
		if st, _ := supervisorState(a); c.change != "conflict" && st != "current" {
			t.Errorf("%s: state after setup %q", c.name, st)
		}
	}
	a, path := supervisorAgent(t, core.Codex, "[features]\nhooks = true\n")
	if _, err := setupSupervisor(a, Options{}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("the file mode was not kept: %v %v", st.Mode(), err)
	}
	a, _ = supervisorAgent(t, core.Codex, "features = { hooks = true }\n")
	if _, err := setupSupervisor(a, Options{Force: true}); err == nil {
		t.Error("an inline features table was edited by lines")
	}
}
