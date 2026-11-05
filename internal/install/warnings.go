package install

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// Warning is a setting outside the adapter's own files that keeps it from
// placing an agent in its pane: a harness supervisor that runs sessions in a
// process another terminal started, so they all inherit that terminal's
// REX_BLOCK (docs/design.md section 4). Setup reports it and never changes it.
type Warning struct {
	Agent   string `json:"agent"`
	Message string `json:"message"`
}

// managedClaudeSettings is Claude Code's managed settings file on macOS.
var managedClaudeSettings = "/Library/Application Support/ClaudeCode/managed-settings.json"

// Warnings checks each found agent's supervisor settings.
func Warnings(agents []Agent) []Warning {
	var out []Warning
	for _, a := range agents {
		if !a.Found {
			continue
		}
		switch a.Kind {
		case core.Claude:
			if !claudeAgentViewOff(a.HookFile) {
				out = append(out, Warning{Agent: a.Name, Message: `agent view is on: a session moved to the background (←, /bg, claude --bg) runs under Claude Code's supervisor with another terminal's REX_BLOCK. The adapter places it by its pane's title; to keep every conversation in its own pane, set "disableAgentView": true in ` + a.HookFile + ` (or CLAUDE_CODE_DISABLE_AGENT_VIEW=1).`})
			}
		case core.Codex:
			config := filepath.Join(filepath.Dir(a.HookFile), "config.toml")
			if !codexDaemonOff(config) {
				out = append(out, Warning{Agent: a.Name, Message: "daemon_auto_start is on: Codex's shared daemon runs every thread with the first terminal's REX_BLOCK. Set daemon_auto_start = false under [features] in " + config + ", stop a running daemon with `codex app-server daemon stop`, and start Codex with --no-daemon."})
			}
		}
	}
	return out
}

// claudeAgentViewOff reports whether agent view is disabled for the user:
// in the environment, or in user or managed settings (as the key or its
// environment variable). Project settings are not read: they differ per
// folder.
func claudeAgentViewOff(settings string) bool {
	if truthy(os.Getenv("CLAUDE_CODE_DISABLE_AGENT_VIEW")) {
		return true
	}
	for _, path := range []string{managedClaudeSettings, settings} {
		var doc struct {
			Disable *bool             `json:"disableAgentView"`
			Env     map[string]string `json:"env"`
		}
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc.Disable != nil && *doc.Disable || truthy(doc.Env["CLAUDE_CODE_DISABLE_AGENT_VIEW"]) {
			return true
		}
	}
	return false
}

func truthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
}

var (
	tomlTable     = regexp.MustCompile(`^\s*\[([^\]]*)\]`)
	daemonOff     = regexp.MustCompile(`^\s*daemon_auto_start\s*=\s*false\s*(#.*)?$`)
	daemonOffFlat = regexp.MustCompile(`^\s*features\.daemon_auto_start\s*=\s*false\s*(#.*)?$`)
)

// codexDaemonOff reports whether Codex's config turns off its daemon's auto
// start: `daemon_auto_start = false` in [features], or
// `features.daemon_auto_start = false` before any table. It reads only that
// key, so no TOML parser is needed.
func codexDaemonOff(config string) bool {
	f, err := os.Open(config)
	if err != nil {
		return false
	}
	defer f.Close()
	table := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if m := tomlTable.FindStringSubmatch(line); m != nil {
			table = strings.TrimSpace(m[1])
			continue
		}
		if table == "features" && daemonOff.MatchString(line) || table == "" && daemonOffFlat.MatchString(line) {
			return true
		}
	}
	return false
}
