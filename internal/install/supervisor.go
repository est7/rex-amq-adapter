package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

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
		return codexDaemonState(a.SupervisorFile)
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
		it.Note = "it is set on; --force turns it off"
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
	err = disableCodexDaemon(a.SupervisorFile)
	if errors.Is(err, errNoSafeEdit) {
		it.Change, it.Note = "conflict", err.Error()
		return it, nil
	}
	return it, err
}

// errNoSafeEdit is a config file setup cannot edit without changing what
// else it says; the user edits it by hand.
var errNoSafeEdit = errors.New("no edit by lines keeps the rest of the file as it is")

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
			Disable *bool             `json:"disableAgentView"`
			Env     map[string]string `json:"env"`
		}
		if json.Unmarshal(b, &managed) == nil && (managed.Disable != nil && *managed.Disable || truthy(managed.Env["CLAUDE_CODE_DISABLE_AGENT_VIEW"])) {
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
// The state comes from parsing config.toml, so inline tables, dotted and
// quoted keys, and multi-line strings read as Codex reads them. The parser
// must be as strict as Codex's: go-toml rejects a table defined twice (by
// dotted keys and a header) as Codex does, where BurntSushi/toml accepts it. The edit is
// by lines, so comments and order stay as written, and is written only when
// the edited file parses to the same document with features.daemon_auto_start
// set to false: a line placed wrong (inside a string, in another table, a
// duplicate key) is refused rather than leaving Codex a config it rejects.

var (
	featuresHeader = regexp.MustCompile(`^\s*\[\s*features\s*\]\s*(#.*)?$`)
	daemonLine     = regexp.MustCompile(`^(\s*(?:features\.)?daemon_auto_start\s*=\s*)([^#]*?)(\s*(#.*)?)$`)
)

// codexConfig is config.toml as read: its path (symlinks resolved), mode,
// text, and parsed document.
type codexConfig struct {
	path string
	mode fs.FileMode
	text string
	doc  map[string]any
}

func readCodexConfig(config string) (codexConfig, error) {
	c := codexConfig{path: config, mode: 0o600, doc: map[string]any{}}
	if r, err := filepath.EvalSymlinks(config); err == nil {
		c.path = r
	}
	b, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if st, err := os.Stat(c.path); err == nil {
		c.mode = st.Mode().Perm()
	}
	c.text = string(b)
	if err := toml.Unmarshal(b, &c.doc); err != nil {
		return c, fmt.Errorf("%s: %w", config, err)
	}
	return c, nil
}

// daemonSetting is features.daemon_auto_start in doc.
func daemonSetting(doc map[string]any) (value any, ok bool) {
	features, _ := doc["features"].(map[string]any)
	value, ok = features["daemon_auto_start"]
	return value, ok
}

func codexDaemonState(config string) (string, error) {
	c, err := readCodexConfig(config)
	if err != nil {
		return "", err
	}
	switch v, ok := daemonSetting(c.doc); {
	case !ok:
		return "missing", nil
	case v == false:
		return "current", nil
	}
	return "foreign", nil
}

const codexDaemonComment = "# rex-amq-adapter: a shared daemon gives every thread the first terminal's Rex environment"

func disableCodexDaemon(config string) error {
	c, err := readCodexConfig(config)
	if err != nil {
		return err
	}
	eol := "\n"
	if strings.Contains(c.text, "\r\n") {
		eol = "\r\n"
	}
	var lines []string
	if c.text != "" {
		lines = strings.Split(strings.TrimSuffix(strings.ReplaceAll(c.text, "\r\n", "\n"), "\n"), "\n")
	}
	text, err := "", errors.New("no edit to try")
	for _, edit := range daemonEdits(lines) {
		text = strings.Join(edit, eol) + eol
		if err = checkDaemonEdit(c.doc, text); err == nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("%w (%v): add daemon_auto_start = false to features by hand", errNoSafeEdit, err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(c.path, []byte(text)); err != nil {
		return err
	}
	return os.Chmod(c.path, c.mode)
}

// daemonEdits are the ways to turn the daemon off by lines, most specific
// first: rewrite the value of a daemon_auto_start line, add the key after a
// [features] header, append a [features] table, or add
// features.daemon_auto_start before every table. A line that only looks like
// one (inside a multi-line string, in another table) yields an edit that
// checkDaemonEdit refuses, so the next is tried.
func daemonEdits(lines []string) [][]string {
	var edits [][]string
	for i, line := range lines {
		if daemonLine.MatchString(line) {
			edit := slices.Clone(lines)
			edit[i] = daemonLine.ReplaceAllString(line, "${1}false${3}")
			edits = append(edits, edit)
		}
	}
	for i, line := range lines {
		if featuresHeader.MatchString(line) {
			edits = append(edits, slices.Insert(slices.Clone(lines), i+1, codexDaemonComment, "daemon_auto_start = false"))
		}
	}
	table := slices.Clone(lines)
	if len(table) > 0 && strings.TrimSpace(table[len(table)-1]) != "" {
		table = append(table, "")
	}
	edits = append(edits, append(table, "[features]", codexDaemonComment, "daemon_auto_start = false"))
	return append(edits, slices.Insert(slices.Clone(lines), 0, codexDaemonComment, "features.daemon_auto_start = false"))
}

// checkDaemonEdit reports whether text is before with only
// features.daemon_auto_start set to false.
func checkDaemonEdit(before map[string]any, text string) error {
	after := map[string]any{}
	if err := toml.Unmarshal([]byte(text), &after); err != nil {
		return fmt.Errorf("the edit would not parse (%v)", err)
	}
	if v, ok := daemonSetting(after); !ok || v != false {
		return errors.New("the edit did not set features.daemon_auto_start")
	}
	strip := func(doc map[string]any) map[string]any {
		out := maps.Clone(doc)
		if features, ok := out["features"].(map[string]any); ok {
			features = maps.Clone(features)
			delete(features, "daemon_auto_start")
			if len(features) == 0 {
				delete(out, "features")
			} else {
				out["features"] = features
			}
		}
		return out
	}
	if !reflect.DeepEqual(strip(before), strip(after)) {
		return errors.New("the edit would change other settings")
	}
	return nil
}
