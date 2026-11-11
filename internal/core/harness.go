package core

import "path/filepath"

// Dirs are where each harness keeps its configuration and its session
// transcripts. A field is "" when it cannot be known (no home directory and
// no override).
type Dirs struct {
	Claude string // CLAUDE_CONFIG_DIR, else <home>/.claude
	Codex  string // CODEX_HOME, else <home>/.codex
	Pi     string // PI_CODING_AGENT_DIR, else <home>/.pi/agent
	// AgyCLI is Antigravity's CLI state, <home>/.gemini/antigravity-cli; it
	// has no override variable.
	AgyCLI string
}

// HarnessDirs resolves Dirs under home. getenv reads the override variables;
// nil reads none. An override set to "" counts as unset.
func HarnessDirs(home string, getenv func(string) string) Dirs {
	under := func(elem ...string) string {
		if home == "" {
			return ""
		}
		return filepath.Join(append([]string{home}, elem...)...)
	}
	or := func(name, def string) string {
		if getenv != nil {
			if v := getenv(name); v != "" {
				return v
			}
		}
		return def
	}
	return Dirs{
		Claude: or("CLAUDE_CONFIG_DIR", under(".claude")),
		Codex:  or("CODEX_HOME", under(".codex")),
		Pi:     or("PI_CODING_AGENT_DIR", under(".pi", "agent")),
		AgyCLI: under(".gemini", "antigravity-cli"),
	}
}
