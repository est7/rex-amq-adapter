package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	toml "github.com/pelletier/go-toml/v2"
)

// --- native task-list tools ---
//
// Crew roles (such as orchestrator) track Slices in each harness's native task
// list rather than repository files. Two harnesses gate those tools behind a
// setting on current models:
//
//   - Claude Code gates TaskCreate/TaskUpdate/TaskList/TaskGet on a 4.x model
//     allowlist unless CLAUDE_CODE_ENABLE_TODO_TOOLS=1 is set in env (and
//     CLAUDE_CODE_ENABLE_TASKS is not false).
//   - Codex leaves update_plan off on code-mode-only models (such as
//     gpt-6.1-sol) unless tools.update_plan.enabled = true in config.toml.
//
// Setup turns both on unless the user explicitly turned them off, which is
// reported as foreign and left alone unless --force is passed. Remove keeps
// them: setup cannot tell whether the user also relies on task tools outside
// the adapter.

func tasksState(a Agent) (string, error) {
	switch a.Kind {
	case core.Claude:
		return claudeTasksState(a.TasksFile)
	case core.Codex:
		return codexTasksState(a.TasksFile)
	}
	return "", fmt.Errorf("%s has no task-list setting", a.Name)
}

func setupTasks(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "tasks", Path: a.TasksFile}
	st, err := tasksState(a)
	if err != nil {
		return it, err
	}
	it.State = st
	switch st {
	case "current":
		it.Change = "unchanged"
		return it, nil
	case "foreign":
		if !o.Force {
			it.Change = "conflict"
			it.Note = "it is set off; --force turns it on"
			return it, nil
		}
		it.Change = "updated"
	case "missing":
		it.Change = "created"
	}
	if o.DryRun {
		return it, nil
	}
	if a.Kind == core.Claude {
		return it, enableClaudeTasks(a.TasksFile)
	}
	err = enableCodexUpdatePlan(a.TasksFile)
	if errors.Is(err, errNoSafeEdit) {
		it.Change = "conflict"
		it.Note = err.Error()
		return it, nil
	}
	return it, err
}

func removeTasks(a Agent) (Item, error) {
	it := Item{Agent: a.Name, Kind: "tasks", Path: a.TasksFile}
	st, err := tasksState(a)
	if err != nil {
		return it, err
	}
	it.State = st
	if st == "missing" {
		it.Change = "unchanged"
	} else {
		it.Change = "kept"
	}
	return it, nil
}

// --- Claude Code: CLAUDE_CODE_ENABLE_TODO_TOOLS ---

func claudeTasksState(settings string) (string, error) {
	hf, err := loadHooks(settings)
	if err != nil {
		return "", err
	}
	var userEnv *object
	if raw, ok := hf.root.get("env"); ok && raw != nil {
		o, ok := raw.(*object)
		if !ok {
			return "", fmt.Errorf("%s: \"env\" is not an object", settings)
		}
		userEnv = o
	}
	if b, err := os.ReadFile(managedClaudeSettings); err == nil {
		var managed struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(b, &managed) == nil && truthy(managed.Env["CLAUDE_CODE_ENABLE_TODO_TOOLS"]) && !falsy(managed.Env["CLAUDE_CODE_ENABLE_TASKS"]) {
			if _, hasManagedTasks := managed.Env["CLAUDE_CODE_ENABLE_TASKS"]; hasManagedTasks || !envFalsy(userEnv, "CLAUDE_CODE_ENABLE_TASKS") {
				return "current", nil
			}
		}
	}
	if userEnv == nil {
		return "missing", nil
	}
	if envFalsy(userEnv, "CLAUDE_CODE_ENABLE_TASKS") {
		return "foreign", nil
	}
	v, ok := userEnv.get("CLAUDE_CODE_ENABLE_TODO_TOOLS")
	if !ok {
		return "missing", nil
	}
	if s, ok := v.(string); ok && truthy(s) {
		return "current", nil
	}
	return "foreign", nil
}

func falsy(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "0" || v == "false"
}

func envFalsy(env *object, key string) bool {
	if env == nil {
		return false
	}
	v, ok := env.get(key)
	if !ok {
		return false
	}
	if b, ok := v.(bool); ok {
		return !b
	}
	s, ok := v.(string)
	return ok && falsy(s)
}

func enableClaudeTasks(settings string) error {
	hf, err := loadHooks(settings)
	if err != nil {
		return err
	}
	var env *object
	if raw, ok := hf.root.get("env"); ok && raw != nil {
		o, ok := raw.(*object)
		if !ok {
			return fmt.Errorf("%s: \"env\" is not an object", settings)
		}
		env = o
	} else {
		env = newObject()
		hf.root.set("env", env)
	}
	env.set("CLAUDE_CODE_ENABLE_TODO_TOOLS", "1")
	if envFalsy(env, "CLAUDE_CODE_ENABLE_TASKS") {
		env.del("CLAUDE_CODE_ENABLE_TASKS")
	}
	if err := os.MkdirAll(filepath.Dir(hf.path), 0o700); err != nil {
		return err
	}
	return hf.save()
}

// --- Codex: tools.update_plan ---

var (
	updatePlanHeader = regexp.MustCompile(`^\s*\[\s*tools\.update_plan\s*\]\s*(#.*)?$`)
	toolsHeader      = regexp.MustCompile(`^\s*\[\s*tools\s*\]\s*(#.*)?$`)
	updatePlanLine   = regexp.MustCompile(`^(\s*(?:(?:tools\.)?update_plan\.)?enabled\s*=\s*)([^#]*?)(\s*(#.*)?)$`)
)

func codexTasksState(config string) (string, error) {
	c, err := readCodexConfig(config)
	if err != nil {
		return "", err
	}
	rawTools, ok := c.doc["tools"]
	if !ok {
		return "missing", nil
	}
	tools, ok := rawTools.(map[string]any)
	if !ok {
		return "foreign", nil
	}
	rawUP, ok := tools["update_plan"]
	if !ok {
		return "missing", nil
	}
	up, ok := rawUP.(map[string]any)
	if !ok {
		return "foreign", nil
	}
	v, ok := up["enabled"]
	if !ok {
		return "missing", nil
	}
	if v == true {
		return "current", nil
	}
	return "foreign", nil
}

const codexTasksComment = "# rex-amq-adapter: expose update_plan on models that leave it off"

func enableCodexUpdatePlan(config string) error {
	return writeCodexEdit(config, updatePlanEdits, checkUpdatePlanEdit, "add [tools.update_plan] enabled = true by hand")
}

// updatePlanEdits are the ways to enable tools.update_plan by lines, most
// specific first: rewrite an existing enabled line, insert enabled = true into
// [tools.update_plan], append a [tools.update_plan] table, insert
// update_plan.enabled = true into [tools], or prepend
// tools.update_plan.enabled = true before every table. Each candidate is
// checked with go-toml before use.
func updatePlanEdits(lines []string) [][]string {
	var edits [][]string
	for i, line := range lines {
		if updatePlanLine.MatchString(line) {
			edit := slices.Clone(lines)
			edit[i] = updatePlanLine.ReplaceAllString(line, "${1}true${3}")
			edits = append(edits, edit)
		}
	}
	for i, line := range lines {
		if updatePlanHeader.MatchString(line) {
			edits = append(edits, slices.Insert(slices.Clone(lines), i+1, codexTasksComment, "enabled = true"))
		}
	}
	table := slices.Clone(lines)
	if len(table) > 0 && strings.TrimSpace(table[len(table)-1]) != "" {
		table = append(table, "")
	}
	edits = append(edits, append(table, "[tools.update_plan]", codexTasksComment, "enabled = true"))
	for i, line := range lines {
		if toolsHeader.MatchString(line) {
			edits = append(edits, slices.Insert(slices.Clone(lines), i+1, codexTasksComment, "update_plan.enabled = true"))
		}
	}
	return append(edits, slices.Insert(slices.Clone(lines), 0, codexTasksComment, "tools.update_plan.enabled = true"))
}

func updatePlanSetting(doc map[string]any) (value any, ok bool) {
	tools, _ := doc["tools"].(map[string]any)
	up, _ := tools["update_plan"].(map[string]any)
	value, ok = up["enabled"]
	return value, ok
}

// checkUpdatePlanEdit reports whether text is before with only
// tools.update_plan.enabled set to true.
func checkUpdatePlanEdit(before map[string]any, text string) error {
	after := map[string]any{}
	if err := toml.Unmarshal([]byte(text), &after); err != nil {
		return fmt.Errorf("the edit would not parse (%v)", err)
	}
	if v, ok := updatePlanSetting(after); !ok || v != true {
		return errors.New("the edit did not set tools.update_plan.enabled")
	}
	strip := func(doc map[string]any) map[string]any {
		out := maps.Clone(doc)
		if tools, ok := out["tools"].(map[string]any); ok {
			tools = maps.Clone(tools)
			if up, ok := tools["update_plan"].(map[string]any); ok {
				up = maps.Clone(up)
				delete(up, "enabled")
				if len(up) == 0 {
					delete(tools, "update_plan")
				} else {
					tools["update_plan"] = up
				}
			}
			if len(tools) == 0 {
				delete(out, "tools")
			} else {
				out["tools"] = tools
			}
		}
		return out
	}
	if !reflect.DeepEqual(strip(before), strip(after)) {
		return errors.New("the edit would change other settings")
	}
	return nil
}
