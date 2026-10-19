package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A settings file with someone else's hooks and keys in a deliberate order.
const userSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {"hooks": [{"type": "command", "command": "echo mine-stop", "timeout": 10}]},
      {"matcher": ".*", "hooks": [{"type": "command", "command": "echo second-stop"}]}
    ],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "rtk hook"}]}]
  },
  "env": {"A": "<&>"},
  "n": 1.50
}
`

func setup(t *testing.T) (dir string, agents []Agent) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
	for _, d := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "hooks.json"), []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	agents, err := Agents([]core.Kind{core.Claude, core.Codex})
	if err != nil {
		t.Fatal(err)
	}
	return dir, agents
}

func changes(items []Item) string {
	var s []string
	for _, it := range items {
		s = append(s, it.Kind+"="+it.Change)
	}
	return strings.Join(s, ",")
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetupIsIdempotentAndAppendOnly(t *testing.T) {
	dir, agents := setup(t)
	self := "/opt/x/rex-amq-adapter"
	items, err := Setup(agents, Options{SelfBin: self})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(items); got != "skill=created,hook=created,skill=created,hook=created" {
		t.Fatalf("first setup: %s", got)
	}
	codexPath := filepath.Join(dir, ".codex", "hooks.json")
	after := read(t, codexPath)

	root, err := parseObject([]byte(after))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(root.keys, ",") != "model,hooks,env,n" {
		t.Fatalf("top-level key order changed: %v", root.keys)
	}
	if !strings.Contains(after, `"A": "<&>"`) || !strings.Contains(after, `"n": 1.50`) {
		t.Fatalf("values were rewritten:\n%s", after)
	}
	events := root.vals["hooks"].(*object)
	stop := events.vals["Stop"].([]any)
	if len(stop) != 3 {
		t.Fatalf("Stop groups: %d", len(stop))
	}
	// Existing entries keep their positions; ours is appended.
	firstCmd := stop[0].(*object).vals["hooks"].([]any)[0].(*object).vals["command"]
	lastCmd := stop[2].(*object).vals["hooks"].([]any)[0].(*object).vals["command"]
	if firstCmd != "echo mine-stop" || lastCmd != hookCommand(self, "stop", core.Codex) {
		t.Fatalf("positions: first=%v last=%v", firstCmd, lastCmd)
	}
	for _, ev := range hookEvents {
		if _, ok := events.vals[ev.Name]; !ok {
			t.Errorf("event %s not installed", ev.Name)
		}
	}

	items, err = Setup(agents, Options{SelfBin: self})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(items); got != "skill=unchanged,hook=unchanged,skill=unchanged,hook=unchanged" {
		t.Fatalf("second setup: %s", got)
	}
	if read(t, codexPath) != after {
		t.Fatal("an unchanged setup rewrote the file")
	}
	st, _ := Status(agents, self)
	for _, it := range st {
		if it.State != "current" {
			t.Errorf("status %s %s: %s", it.Agent, it.Kind, it.State)
		}
	}
}

func TestMovedBinaryUpdatesInPlace(t *testing.T) {
	dir, agents := setup(t)
	if _, err := Setup(agents, Options{SelfBin: "/old/rex-amq-adapter"}); err != nil {
		t.Fatal(err)
	}
	st, _ := Status(agents, "/new/rex-amq-adapter")
	if st[0].State != "outdated" || st[1].State != "outdated" {
		t.Fatalf("moved binary: %+v", st)
	}
	items, err := Setup(agents, Options{SelfBin: "/new/rex-amq-adapter"})
	if err != nil || changes(items) != "skill=updated,hook=updated,skill=updated,hook=updated" {
		t.Fatalf("update: %s %v", changes(items), err)
	}
	after := read(t, filepath.Join(dir, ".codex", "hooks.json"))
	if strings.Contains(after, "/old/") {
		t.Fatal("old command left behind")
	}
	root, _ := parseObject([]byte(after))
	stop := root.vals["hooks"].(*object).vals["Stop"].([]any)
	if len(stop) != 3 {
		t.Fatalf("update must replace in place, not append: %d Stop groups", len(stop))
	}
}

func TestRemoveRestoresOnlyWhatSetupAdded(t *testing.T) {
	dir, agents := setup(t)
	codexPath := filepath.Join(dir, ".codex", "hooks.json")
	before, _ := parseObject([]byte(userSettings))
	want, _ := marshal(before)
	if _, err := Setup(agents, Options{SelfBin: "/opt/x/rex-amq-adapter"}); err != nil {
		t.Fatal(err)
	}
	items, err := Remove(agents, Options{SelfBin: "/opt/x/rex-amq-adapter"})
	if err != nil || changes(items) != "skill=removed,hook=removed,skill=removed,hook=removed" {
		t.Fatalf("remove: %s %v", changes(items), err)
	}
	if got := read(t, codexPath); got != string(want) {
		t.Fatalf("after remove:\n%s\nwant:\n%s", got, want)
	}
	if exists(filepath.Join(dir, ".claude", "skills", skillName)) {
		t.Fatal("skill directory left behind")
	}
	items, _ = Remove(agents, Options{SelfBin: "/opt/x/rex-amq-adapter"})
	if changes(items) != "skill=unchanged,hook=unchanged,skill=unchanged,hook=unchanged" {
		t.Fatalf("second remove: %s", changes(items))
	}
}

func TestForeignSkillIsKept(t *testing.T) {
	dir, agents := setup(t)
	foreign := filepath.Join(dir, ".claude", "skills", skillName, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("---\nname: rex-amq-adapter\n---\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := agents[:1]
	items, err := Setup(claude, Options{SelfBin: "/opt/x/rex-amq-adapter"})
	if err != nil || items[0].Change != "conflict" || read(t, foreign) != "---\nname: rex-amq-adapter\n---\nmine\n" {
		t.Fatalf("setup over foreign: %+v %v", items, err)
	}
	items, _ = Remove(claude, Options{SelfBin: "/opt/x/rex-amq-adapter"})
	if items[0].Change != "kept" || !exists(foreign) {
		t.Fatalf("remove must keep a foreign skill: %+v", items)
	}
	items, err = Setup(claude, Options{SelfBin: "/opt/x/rex-amq-adapter", Force: true})
	if err != nil || items[0].Change != "updated" || !strings.Contains(read(t, foreign), stubMarker) {
		t.Fatalf("forced setup: %+v %v", items, err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir, agents := setup(t)
	items, err := Setup(agents, Options{SelfBin: "/opt/x/rex-amq-adapter", DryRun: true})
	if err != nil || changes(items) != "skill=created,hook=created,skill=created,hook=created" {
		t.Fatalf("dry run: %s %v", changes(items), err)
	}
	if read(t, filepath.Join(dir, ".codex", "hooks.json")) != userSettings || exists(filepath.Join(dir, ".claude", "settings.json")) {
		t.Fatal("dry run changed files")
	}
}

func TestSymlinkedSettingsStayLinked(t *testing.T) {
	dir, agents := setup(t)
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".claude", "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(agents[:1], Options{SelfBin: "/opt/x/rex-amq-adapter"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("settings symlink was replaced by a file")
	}
	if !strings.Contains(read(t, target), "session-start") {
		t.Fatal("hooks not written to the link target")
	}
}

func TestSelfPathIsValidated(t *testing.T) {
	_, agents := setup(t)
	for _, bad := range []string{"rex-amq-adapter", "/opt/it's/rex-amq-adapter", "/opt/x/other"} {
		if _, err := Setup(agents, Options{SelfBin: bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSkillDescriptionFitsTheValidator(t *testing.T) {
	if n := len([]rune(skillDescription)); n > 400 {
		t.Fatalf("description is %d characters", n)
	}
}

// Antigravity's hooks.json maps hook names to event handlers; someone else's
// group must survive every operation untouched.
const agyHooks = `{
  "orca-status": {
    "PreInvocation": [{"type": "command", "command": "echo orca", "timeout": 10}],
    "Stop": [{"type": "command", "command": "echo orca-stop", "timeout": 10}]
  }
}
`

func TestAgyHookGroup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := filepath.Join(dir, ".gemini", "config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "hooks.json")
	if err := os.WriteFile(path, []byte(agyHooks), 0o644); err != nil {
		t.Fatal(err)
	}
	agents, err := Agents([]core.Kind{core.Agy})
	if err != nil || len(agents) != 1 || agents[0].HookFile != path || agents[0].SkillDir != filepath.Join(cfg, "skills", skillName) {
		t.Fatalf("agy paths: %+v %v", agents, err)
	}
	items, err := Setup(agents, Options{SelfBin: "/old/rex-amq-adapter"})
	if err != nil || changes(items) != "skill=created,hook=created" {
		t.Fatalf("setup: %s %v", changes(items), err)
	}
	root, _ := parseObject([]byte(read(t, path)))
	if strings.Join(root.keys, ",") != "orca-status,rex-amq-adapter" {
		t.Fatalf("groups: %v", root.keys)
	}
	ours := root.vals[skillName].(*object)
	if strings.Join(ours.keys, ",") != "SessionStart,PreInvocation,Stop" {
		t.Fatalf("events: %v", ours.keys)
	}
	cmd := ours.vals["PreInvocation"].([]any)[0].(*object).vals["command"]
	if cmd != hookCommand("/old/rex-amq-adapter", "pre-invocation", core.Agy) {
		t.Fatalf("command: %v", cmd)
	}
	if items, _ := Setup(agents, Options{SelfBin: "/old/rex-amq-adapter"}); changes(items) != "skill=unchanged,hook=unchanged" {
		t.Fatalf("second setup: %s", changes(items))
	}
	if st, _ := Status(agents, "/new/rex-amq-adapter"); st[1].State != "outdated" {
		t.Fatalf("moved binary: %+v", st)
	}
	if items, err := Setup(agents, Options{SelfBin: "/new/rex-amq-adapter"}); err != nil || changes(items) != "skill=updated,hook=updated" {
		t.Fatalf("update: %s %v", changes(items), err)
	}
	root, _ = parseObject([]byte(read(t, path)))
	if strings.Join(root.keys, ",") != "orca-status,rex-amq-adapter" || strings.Contains(read(t, path), "/old/") {
		t.Fatalf("update must replace the group in place: %v", root.keys)
	}
	if items, err := Remove(agents, Options{SelfBin: "/new/rex-amq-adapter"}); err != nil || changes(items) != "skill=removed,hook=removed" {
		t.Fatalf("remove: %s %v", changes(items), err)
	}
	before, _ := parseObject([]byte(agyHooks))
	want, _ := marshal(before)
	if got := read(t, path); got != string(want) {
		t.Fatalf("after remove:\n%s\nwant:\n%s", got, want)
	}
}

func TestPiExtension(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(dir, "piagent"))
	agents, err := Agents([]core.Kind{core.Codex, core.Pi})
	if err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(dir, "piagent", "extensions", "rex-amq-adapter.js")
	if agents[1].HookFile != ext || agents[1].SkillDir != agents[0].SkillDir {
		t.Fatalf("pi paths: %+v", agents[1])
	}
	items, err := Setup(agents, Options{SelfBin: "/old/rex-amq-adapter"})
	// The skill Codex and Pi share is installed once.
	if err != nil || changes(items) != "skill=created,hook=created,hook=created" {
		t.Fatalf("setup: %s %v", changes(items), err)
	}
	body := read(t, ext)
	if !strings.Contains(body, `const BIN = "/old/rex-amq-adapter";`) || strings.Contains(body, "__REX_AMQ_ADAPTER__") {
		t.Fatalf("binary path not substituted:\n%s", body[:400])
	}
	if items, _ := Setup(agents, Options{SelfBin: "/old/rex-amq-adapter"}); changes(items) != "skill=unchanged,hook=unchanged,hook=unchanged" {
		t.Fatalf("second setup: %s", changes(items))
	}
	if st, _ := Status(agents[1:], "/new/rex-amq-adapter"); st[1].State != "outdated" {
		t.Fatalf("moved binary: %+v", st)
	}
	if items, _ := Remove(agents, Options{SelfBin: "/new/rex-amq-adapter"}); changes(items) != "skill=removed,hook=removed,hook=removed" || exists(ext) {
		t.Fatalf("remove: %s", changes(items))
	}

	// Someone's own extension of the same name is left alone.
	if err := os.WriteFile(ext, []byte("export default function () {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if items, _ := Setup(agents[1:], Options{SelfBin: "/x/rex-amq-adapter"}); items[1].Change != "conflict" {
		t.Fatalf("setup over a foreign extension: %+v", items)
	}
	if items, _ := Remove(agents[1:], Options{SelfBin: "/x/rex-amq-adapter"}); items[1].Change != "kept" || !exists(ext) {
		t.Fatalf("remove of a foreign extension: %+v", items)
	}
}
