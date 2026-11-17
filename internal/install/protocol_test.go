package install

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// The installed hook command is other programs' contract: Codex approves a
// hook by its exact command string (/hooks), and her's hooks.json repeats
// it. A changed string makes every Codex seat unapproved_hooks, so the
// form is a golden, written here by hand, never derived from hookCommand.
func TestHookCommandFormUnchanged(t *testing.T) {
	const bin = "/opt/homebrew/opt/rex-amq-adapter/bin/rex-amq-adapter"
	for _, kind := range []core.Kind{core.Claude, core.Codex, core.Agy, core.Pi} {
		for _, ev := range hookEventsFor(kind) {
			want := "'" + bin + "' hook " + ev.Event + " --agent " + string(kind) + " 2>/dev/null || true"
			if got := hookCommand(bin, ev.Event, kind); got != want {
				t.Errorf("%s %s:\n got %s\nwant %s", kind, ev.Event, got, want)
			}
		}
	}
	golden := map[string]string{
		"SessionStart":     "'" + bin + "' hook session-start --agent codex 2>/dev/null || true",
		"UserPromptSubmit": "'" + bin + "' hook user-prompt-submit --agent codex 2>/dev/null || true",
		"PostToolUse":      "'" + bin + "' hook post-tool-use --agent codex 2>/dev/null || true",
		"Stop":             "'" + bin + "' hook stop --agent codex 2>/dev/null || true",
		"SessionEnd":       "'" + bin + "' hook session-end --agent codex 2>/dev/null || true",
	}
	dir, agents := setup(t)
	if _, err := Setup(agents, Options{SelfBin: bin}); err != nil {
		t.Fatal(err)
	}
	var file struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, ".codex", "hooks.json"))), &file); err != nil {
		t.Fatal(err)
	}
	for name, want := range golden {
		var got []string
		for _, g := range file.Hooks[name] {
			for _, h := range g.Hooks {
				got = append(got, h.Command)
			}
		}
		if !slices.Contains(got, want) {
			t.Errorf("installed %s hooks %q lack %q", name, got, want)
		}
	}
}
