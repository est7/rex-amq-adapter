package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// --- shared supervisors ---
//
// Claude Code's agent view and Codex's shared daemon run conversations in a
// process another terminal started, so every conversation they host inherits
// that terminal's REX_BLOCK (docs/design.md section 4). Setup turns each off
// in the harness's user settings: "disableAgentView": true, and
// daemon_auto_start = false under [features]. A value the user set the other
// way is foreign: kept unless --force. Remove keeps the setting, since setup
// cannot tell whether the user wanted it off anyway.
//
// States: current (off), missing (not set: the harness default is on),
// foreign (explicitly on).

// managedClaudeSettings is Claude Code's managed settings file on macOS; a
// managed "disableAgentView": true turns agent view off for every user.
var managedClaudeSettings = "/Library/Application Support/ClaudeCode/managed-settings.json"

func supervisorState(a Agent) (string, error) {
	switch a.Kind {
	case core.Claude:
		return claudeAgentViewState(a.SupervisorFile)
	case core.Codex:
		st, _, err := codexDaemonState(a.SupervisorFile)
		return st, err
	}
	return "", fmt.Errorf("%s has no shared supervisor", a.Name)
}

func setupSupervisor(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "supervisor", Path: a.SupervisorFile}
	st, err := supervisorState(a)
	if err != nil {
		return it, err
	}
	it.State = st
	switch {
	case st == "current":
		it.Change = "unchanged"
		return it, nil
	case st == "foreign" && !o.Force:
		it.Change = "conflict"
		return it, nil
	case st == "foreign":
		it.Change = "updated"
	default:
		it.Change = "created"
	}
	if o.DryRun {
		return it, nil
	}
	if a.Kind == core.Claude {
		return it, disableAgentView(a.SupervisorFile)
	}
	return it, disableCodexDaemon(a.SupervisorFile)
}

func removeSupervisor(a Agent) (Item, error) {
	it := Item{Agent: a.Name, Kind: "supervisor", Path: a.SupervisorFile}
	st, err := supervisorState(a)
	if err != nil {
		return it, err
	}
	it.State, it.Change = st, "kept"
	if st == "missing" {
		it.Change = "unchanged"
	}
	return it, nil
}

// --- Claude Code: agent view ---

func claudeAgentViewState(settings string) (string, error) {
	if b, err := os.ReadFile(managedClaudeSettings); err == nil {
		var managed struct {
			Disable *bool `json:"disableAgentView"`
		}
		if json.Unmarshal(b, &managed) == nil && managed.Disable != nil && *managed.Disable {
			return "current", nil
		}
	}
	hf, err := loadHooks(settings)
	if err != nil {
		return "", err
	}
	if env, ok := hf.root.get("env"); ok {
		if o, ok := env.(*object); ok {
			if v, ok := o.get("CLAUDE_CODE_DISABLE_AGENT_VIEW"); ok {
				if s, ok := v.(string); ok && truthy(s) {
					return "current", nil
				}
			}
		}
	}
	v, ok := hf.root.get("disableAgentView")
	if !ok {
		return "missing", nil
	}
	if b, ok := v.(bool); ok && b {
		return "current", nil
	}
	return "foreign", nil
}

func truthy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "1" || v == "true"
}

func disableAgentView(settings string) error {
	hf, err := loadHooks(settings)
	if err != nil {
		return err
	}
	hf.root.set("disableAgentView", true)
	if err := os.MkdirAll(filepath.Dir(hf.path), 0o700); err != nil {
		return err
	}
	return hf.save()
}

// --- Codex: the shared daemon ---
//
// Only the one key is read and written, line by line, so no TOML parser is
// needed and the rest of the file (comments, order) is left as it was.

var (
	tomlTable      = regexp.MustCompile(`^\s*\[([^\[\]]*)\]\s*(#.*)?$`)
	daemonKey      = regexp.MustCompile(`^(\s*daemon_auto_start\s*=\s*)(true|false)(\s*(#.*)?)$`)
	daemonKeyFlat  = regexp.MustCompile(`^(\s*features\.daemon_auto_start\s*=\s*)(true|false)(\s*(#.*)?)$`)
	inlineFeatures = regexp.MustCompile(`^\s*features\s*=`)
)

// codexLines is config.toml split into lines, with what it says about the
// daemon: the index of the daemon_auto_start line (flat when written as
// features.daemon_auto_start before any table) and of the [features]
// header, -1 when absent, and whether features is an inline table, which
// setup does not edit by lines.
type codexLines struct {
	lines       []string
	key, header int
	flat, keyOn bool
	inline      bool
	path        string
	mode        fs.FileMode
}

func readCodexConfig(config string) (codexLines, error) {
	c := codexLines{key: -1, header: -1, path: config, mode: 0o600}
	if r, err := filepath.EvalSymlinks(config); err == nil {
		c.path = r
	}
	b, err := os.ReadFile(c.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return c, err
	}
	if st, err := os.Stat(c.path); err == nil {
		c.mode = st.Mode().Perm()
	}
	if len(b) > 0 {
		c.lines = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	table := ""
	for i, line := range c.lines {
		if m := tomlTable.FindStringSubmatch(line); m != nil {
			table = strings.TrimSpace(m[1])
			if table == "features" {
				c.header = i
			}
			continue
		}
		var m []string
		switch table {
		case "":
			if inlineFeatures.MatchString(line) {
				c.inline = true
			}
			m = daemonKeyFlat.FindStringSubmatch(line)
		case "features":
			m = daemonKey.FindStringSubmatch(line)
		}
		if m != nil {
			c.key, c.keyOn, c.flat = i, m[2] == "true", table == ""
		}
	}
	return c, nil
}

func codexDaemonState(config string) (string, codexLines, error) {
	c, err := readCodexConfig(config)
	switch {
	case err != nil:
		return "", c, err
	case c.key >= 0 && !c.keyOn:
		return "current", c, nil
	case c.key >= 0 || c.inline:
		return "foreign", c, nil
	}
	return "missing", c, nil
}

const codexDaemonComment = "# rex-amq-adapter: a shared daemon gives every thread the first terminal's Rex environment"

func disableCodexDaemon(config string) error {
	_, c, err := codexDaemonState(config)
	if err != nil {
		return err
	}
	lines := c.lines
	switch {
	case c.key >= 0:
		re := daemonKey
		if c.flat {
			re = daemonKeyFlat
		}
		lines[c.key] = re.ReplaceAllString(lines[c.key], "${1}false${3}")
	case c.inline:
		return fmt.Errorf("%s sets features as an inline table; add daemon_auto_start = false to it by hand", config)
	case c.header >= 0:
		lines = append(lines[:c.header+1], append([]string{codexDaemonComment, "daemon_auto_start = false"}, lines[c.header+1:]...)...)
	default:
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, "[features]", codexDaemonComment, "daemon_auto_start = false")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(c.path, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return err
	}
	return os.Chmod(c.path, c.mode)
}
