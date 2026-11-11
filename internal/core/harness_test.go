package core

import "testing"

func TestHarnessDirs(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	for name, tc := range map[string]struct {
		home   string
		getenv func(string) string
		want   Dirs
	}{
		"defaults": {"/h", nil, Dirs{Claude: "/h/.claude", Codex: "/h/.codex", Pi: "/h/.pi/agent", AgyCLI: "/h/.gemini/antigravity-cli"}},
		"overrides": {"/h", env(map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x", "PI_CODING_AGENT_DIR": "/p"}),
			Dirs{Claude: "/c", Codex: "/x", Pi: "/p", AgyCLI: "/h/.gemini/antigravity-cli"}},
		"an empty override is unset": {"/h", env(map[string]string{"CODEX_HOME": ""}),
			Dirs{Claude: "/h/.claude", Codex: "/h/.codex", Pi: "/h/.pi/agent", AgyCLI: "/h/.gemini/antigravity-cli"}},
		"no home":               {"", nil, Dirs{}},
		"no home, one override": {"", env(map[string]string{"CLAUDE_CONFIG_DIR": "/c"}), Dirs{Claude: "/c"}},
	} {
		if got := HarnessDirs(tc.home, tc.getenv); got != tc.want {
			t.Errorf("%s: HarnessDirs(%q) = %+v, want %+v", name, tc.home, got, tc.want)
		}
	}
}
