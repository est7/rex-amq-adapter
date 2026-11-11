package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

func tasksAgent(t *testing.T, kind core.Kind, content string) (Agent, string) {
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
	return Agent{Kind: kind, Name: string(kind), TasksFile: path, Found: true}, path
}

// Setup turns on Claude Code's CLAUDE_CODE_ENABLE_TODO_TOOLS and Codex's
// [tools.update_plan] enabled = true so native task-list tools stay exposed on
// current models; an explicit off setting is kept unless --force, and remove
// never turns them back off.
func TestTasksStates(t *testing.T) {
	for _, c := range []struct {
		name, content string
		kind          core.Kind
		want          string
	}{
		{"claude no file", ``, core.Claude, "missing"},
		{"claude default", `{"hooks": {}}`, core.Claude, "missing"},
		{"claude env without key", `{"env": {"A": "1"}}`, core.Claude, "missing"},
		{"claude on 1", `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "1"}}`, core.Claude, "current"},
		{"claude on true", `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "true"}}`, core.Claude, "current"},
		{"claude off 0", `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "0"}}`, core.Claude, "foreign"},
		{"claude off false", `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "false"}}`, core.Claude, "foreign"},
		{"claude tasks disabled", `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "1", "CLAUDE_CODE_ENABLE_TASKS": "0"}}`, core.Claude, "foreign"},
		{"codex no file", ``, core.Codex, "missing"},
		{"codex default", "model = \"x\"\n[features]\nhooks = true\n", core.Codex, "missing"},
		{"codex tools without update_plan", "[tools]\nweb_search = true\n", core.Codex, "missing"},
		{"codex empty update_plan", "[tools.update_plan]\n", core.Codex, "missing"},
		{"codex on", "[tools.update_plan]\nenabled = true # why\n[other]\n", core.Codex, "current"},
		{"codex dotted in tools", "[tools]\nupdate_plan.enabled = true\n", core.Codex, "current"},
		{"codex flat", "tools.update_plan.enabled = true\n[x]\n", core.Codex, "current"},
		{"codex inline on", "[tools]\nupdate_plan = { enabled = true }\n", core.Codex, "current"},
		{"codex other table", "[mcp_servers.x]\nenabled = true\n", core.Codex, "missing"},
		{"codex off", "[tools.update_plan]\nenabled = false\n", core.Codex, "foreign"},
		{"codex scalar update_plan", "[tools]\nupdate_plan = true\n", core.Codex, "foreign"},
		{"codex not a bool", "[tools.update_plan]\nenabled = \"true\"\n", core.Codex, "foreign"},
		{"codex in a string", "instructions = \"\"\"\n[tools.update_plan]\nenabled = true\n\"\"\"\n", core.Codex, "missing"},
	} {
		a, _ := tasksAgent(t, c.kind, c.content)
		if got, err := tasksState(a); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	a, _ := tasksAgent(t, core.Claude, `{}`)
	if err := os.WriteFile(managedClaudeSettings, []byte(`{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := tasksState(a); got != "current" {
		t.Errorf("managed settings enable todo tools: %q", got)
	}
	a, _ = tasksAgent(t, core.Claude, `{"env": "bad"}`)
	if _, err := tasksState(a); err == nil {
		t.Error("non-object env was accepted")
	}
}

func TestSetupEnablesClaudeTasks(t *testing.T) {
	a, path := tasksAgent(t, core.Claude, `{"env": {"FIRST": "1", "SECOND": "2"}, "model": "x"}`)
	it, err := setupTasks(a, Options{})
	if err != nil || it.Change != "created" {
		t.Fatalf("setup: %+v %v", it, err)
	}
	got := read(t, path)
	root, err := parseObject([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(root.keys, ",") != "env,model" {
		t.Errorf("top-level key order changed: %v", root.keys)
	}
	env := root.vals["env"].(*object)
	if strings.Join(env.keys, ",") != "FIRST,SECOND,CLAUDE_CODE_ENABLE_TODO_TOOLS" || env.vals["CLAUDE_CODE_ENABLE_TODO_TOOLS"] != "1" {
		t.Errorf("env after setup: keys=%v vals=%v", env.keys, env.vals)
	}
	if it, _ := setupTasks(a, Options{}); it.Change != "unchanged" {
		t.Errorf("second setup: %+v", it)
	}
	if it, _ := removeTasks(a); it.Change != "kept" || read(t, path) != got {
		t.Errorf("remove turned todo tools off: %+v", it)
	}

	// File without env creates env cleanly.
	a, path = tasksAgent(t, core.Claude, `{"model": "x"}`)
	if it, err := setupTasks(a, Options{}); err != nil || it.Change != "created" || !strings.Contains(read(t, path), `"CLAUDE_CODE_ENABLE_TODO_TOOLS": "1"`) {
		t.Fatalf("create env: %+v %v\n%s", it, err, read(t, path))
	}

	// Explicit off is respected unless --force.
	off := `{"env": {"CLAUDE_CODE_ENABLE_TODO_TOOLS": "0", "CLAUDE_CODE_ENABLE_TASKS": "false"}}`
	a, path = tasksAgent(t, core.Claude, off)
	if it, _ := setupTasks(a, Options{}); it.Change != "conflict" || read(t, path) != off {
		t.Errorf("setup overrode the user's choice: %+v\n%s", it, read(t, path))
	}
	if it, err := setupTasks(a, Options{Force: true}); err != nil || it.Change != "updated" {
		t.Fatalf("--force: %+v %v", it, err)
	}
	if st, err := tasksState(a); err != nil || st != "current" || strings.Contains(read(t, path), "CLAUDE_CODE_ENABLE_TASKS") {
		t.Errorf("after --force: state=%q err=%v\n%s", st, err, read(t, path))
	}
}

func TestSetupEnablesCodexUpdatePlan(t *testing.T) {
	for _, c := range []struct {
		name, content, want string
		force               bool
		change              string
	}{
		{"into update_plan", "model = \"x\"\n\n[tools.update_plan]\n\n[mcp]\na = 1\n",
			"model = \"x\"\n\n[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n\n[mcp]\na = 1\n", false, "created"},
		{"new table", "model = \"x\"\n[mcp]\na = 1",
			"model = \"x\"\n[mcp]\na = 1\n\n[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n", false, "created"},
		{"existing tools table", "[tools]\nweb_search = true\n\n[mcp]\na = 1",
			"[tools]\nweb_search = true\n\n[mcp]\na = 1\n\n[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n", false, "created"},
		{"no file", "", "[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n", false, "created"},
		{"other enabled untouched", "[mcp_servers.x]\nenabled = false\n",
			"[mcp_servers.x]\nenabled = false\n\n[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n", false, "created"},
		{"off, kept", "[tools.update_plan]\nenabled = false # mine\n", "[tools.update_plan]\nenabled = false # mine\n", false, "conflict"},
		{"off, forced", "[tools.update_plan]\nenabled = false # mine\n", "[tools.update_plan]\nenabled = true # mine\n", true, "updated"},
		{"dotted in tools, forced", "[tools]\nupdate_plan.enabled = false\n", "[tools]\nupdate_plan.enabled = true\n", true, "updated"},
		{"flat, forced", "tools.update_plan.enabled = false\n[other]\nhooks = true\n", "tools.update_plan.enabled = true\n[other]\nhooks = true\n", true, "updated"},
		{"not a bool, forced", "[tools.update_plan]\nenabled = \"true\"\n", "[tools.update_plan]\nenabled = true\n", true, "updated"},
		// Dotted sub-table inside [tools] cannot take a [tools.update_plan] header.
		{"dotted subtable in tools", "[tools]\nupdate_plan.other = 1\n[mcp]\na = 1\n",
			"[tools]\n" + codexTasksComment + "\nupdate_plan.enabled = true\nupdate_plan.other = 1\n[mcp]\na = 1\n", false, "created"},
		// Top-level dotted keys cannot take [tools] or [tools.update_plan] headers.
		{"top-level dotted", "tools.update_plan.other = 1\n[mcp]\na = 1\n",
			codexTasksComment + "\ntools.update_plan.enabled = true\ntools.update_plan.other = 1\n[mcp]\na = 1\n", false, "created"},
		{"header in a string", "instructions = \"\"\"\n[tools.update_plan]\n\"\"\"\n\n[tools.update_plan]\n",
			"instructions = \"\"\"\n[tools.update_plan]\n\"\"\"\n\n[tools.update_plan]\n" + codexTasksComment + "\nenabled = true\n", false, "created"},
		{"crlf", "[tools.update_plan]\r\n", "[tools.update_plan]\r\n" + codexTasksComment + "\r\nenabled = true\r\n", false, "created"},
		// Inline tables cannot be edited by lines: left for the user.
		{"inline tools", "tools = { web_search = true }\n", "tools = { web_search = true }\n", false, "conflict"},
		{"inline update_plan off, forced", "[tools]\nupdate_plan = { enabled = false }\n", "[tools]\nupdate_plan = { enabled = false }\n", true, "conflict"},
	} {
		a, path := tasksAgent(t, core.Codex, c.content)
		it, err := setupTasks(a, Options{Force: c.force})
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
		if st, _ := tasksState(a); c.change != "conflict" && st != "current" {
			t.Errorf("%s: state after setup %q", c.name, st)
		}
	}
	a, path := tasksAgent(t, core.Codex, "[tools.update_plan]\n")
	if _, err := setupTasks(a, Options{}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("the file mode was not kept: %v %v", st.Mode(), err)
	}
	a, _ = tasksAgent(t, core.Codex, "[tools.update_plan]\nenabled = \n")
	if _, err := setupTasks(a, Options{}); err == nil {
		t.Error("a config.toml that does not parse was edited")
	}
}
