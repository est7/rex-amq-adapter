package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func warningsFor(t *testing.T, kind core.Kind, file, content string) []Warning {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, file)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hook := path
	if kind == core.Codex {
		hook = filepath.Join(dir, "hooks.json")
	}
	return Warnings([]Agent{{Kind: kind, Name: string(kind), HookFile: hook, Found: true}})
}

// Agent view (Claude Code) and the shared daemon (Codex) run sessions with
// another terminal's REX_BLOCK: setup and status say so until they are off.
func TestWarningsNameSupervisorsThatAreOn(t *testing.T) {
	old := managedClaudeSettings
	managedClaudeSettings = filepath.Join(t.TempDir(), "none.json")
	t.Cleanup(func() { managedClaudeSettings = old })
	t.Setenv("CLAUDE_CODE_DISABLE_AGENT_VIEW", "")
	for _, c := range []struct {
		name, content string
		kind          core.Kind
		file          string
		warn          bool
	}{
		{"claude default", `{"hooks": {}}`, core.Claude, "settings.json", true},
		{"claude no file", ``, core.Claude, "settings.json", true},
		{"claude key", `{"disableAgentView": true}`, core.Claude, "settings.json", false},
		{"claude key false", `{"disableAgentView": false}`, core.Claude, "settings.json", true},
		{"claude env in settings", `{"env": {"CLAUDE_CODE_DISABLE_AGENT_VIEW": "1"}}`, core.Claude, "settings.json", false},
		{"codex default", "model = \"x\"\n[features]\nhooks = true\n", core.Codex, "config.toml", true},
		{"codex off", "[features]\napps = false\ndaemon_auto_start = false # why\n[other]\n", core.Codex, "config.toml", false},
		{"codex flat", "features.daemon_auto_start = false\n[features]\n", core.Codex, "config.toml", false},
		{"codex other table", "[agents.x]\ndaemon_auto_start = false\n", core.Codex, "config.toml", true},
		{"codex on", "[features]\ndaemon_auto_start = true\n", core.Codex, "config.toml", true},
	} {
		got := warningsFor(t, c.kind, c.file, c.content)
		if (len(got) > 0) != c.warn {
			t.Errorf("%s: warnings %+v, want warning %v", c.name, got, c.warn)
		}
	}
	t.Setenv("CLAUDE_CODE_DISABLE_AGENT_VIEW", "1")
	if got := warningsFor(t, core.Claude, "settings.json", `{}`); len(got) != 0 {
		t.Errorf("the environment turns agent view off: %+v", got)
	}
	t.Setenv("CLAUDE_CODE_DISABLE_AGENT_VIEW", "")
	if err := os.WriteFile(managedClaudeSettings, []byte(`{"disableAgentView": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := warningsFor(t, core.Claude, "settings.json", `{}`); len(got) != 0 {
		t.Errorf("managed settings turn agent view off: %+v", got)
	}
	// Agents not found on the machine are not checked.
	if got := Warnings([]Agent{{Kind: core.Codex, HookFile: "/nowhere/hooks.json"}}); len(got) != 0 {
		t.Errorf("an agent not found was warned about: %+v", got)
	}
	if got := warningsFor(t, core.Codex, "config.toml", ""); len(got) != 1 || !strings.Contains(got[0].Message, "daemon_auto_start = false") {
		t.Errorf("the warning names the fix: %+v", got)
	}
}
