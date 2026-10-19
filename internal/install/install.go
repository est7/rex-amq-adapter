// Package install puts the adapter's skill stub and hooks into Claude Code
// and Codex, reports their state, and removes them. It follows `rex skill`:
// idempotent setup, entries identified by their own content, foreign items
// left alone, and other settings preserved in place.
package install

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
)

// Agent is one harness and where its skill and hooks live.
type Agent struct {
	Kind     core.Kind
	Name     string
	SkillDir string // directory holding SKILL.md
	HookFile string
	Found    bool
}

const skillName = "rex-amq-adapter"

// Agents resolves each harness's paths, honouring CLAUDE_CONFIG_DIR and
// CODEX_HOME. only limits the result when non-empty.
func Agents(only []core.Kind) ([]Agent, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	claudeDir := envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	codexDir := envOr("CODEX_HOME", filepath.Join(home, ".codex"))
	agyDir := filepath.Join(home, ".gemini", "config") // Antigravity's global configuration
	piDir := envOr("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))
	all := []Agent{
		{Kind: core.Claude, Name: "Claude Code", SkillDir: filepath.Join(claudeDir, "skills", skillName), HookFile: filepath.Join(claudeDir, "settings.json"), Found: exists(claudeDir) || onPath("claude")},
		{Kind: core.Codex, Name: "Codex", SkillDir: filepath.Join(home, ".agents", "skills", skillName), HookFile: filepath.Join(codexDir, "hooks.json"), Found: exists(codexDir) || onPath("codex")},
		{Kind: core.Agy, Name: "Antigravity", SkillDir: filepath.Join(agyDir, "skills", skillName), HookFile: filepath.Join(agyDir, "hooks.json"), Found: exists(agyDir) || onPath("agy")},
		// Pi reads the Agent Skills directory it shares with Codex; its "hook"
		// is an extension file.
		{Kind: core.Pi, Name: "Pi", SkillDir: filepath.Join(home, ".agents", "skills", skillName), HookFile: filepath.Join(piDir, "extensions", skillName+".js"), Found: exists(piDir) || onPath("pi")},
	}
	if len(only) == 0 {
		return all, nil
	}
	var out []Agent
	for _, a := range all {
		for _, k := range only {
			if a.Kind == k {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func onPath(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

// Item is one installed thing and what happened to it.
type Item struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // skill | hook
	Path   string `json:"path"`
	State  string `json:"state"`            // current | outdated | missing | foreign
	Change string `json:"change,omitempty"` // created | updated | unchanged | removed | kept | conflict
}

// Options control setup and removal.
type Options struct {
	SelfBin string // absolute path the hooks and skill run
	DryRun  bool
	Force   bool // replace a foreign skill of the same name
}

// hookEvents are installed for both harnesses, with each hook's timeout in
// seconds. The adapter's own budgets (cmd/rex-amq-adapter) stay below these.
// Codex clamps a SessionEnd timeout to 3 s.
var hookEvents = []struct {
	Name, Event string
	Timeout     int
}{
	{"SessionStart", "session-start", 5},
	{"UserPromptSubmit", "user-prompt-submit", 3},
	{"PostToolUse", "post-tool-use", 3},
	{"Stop", "stop", 3},
	{"SessionEnd", "session-end", 3},
}

func hookCommand(self string, event string, kind core.Kind) string {
	return "'" + self + "' hook " + event + " --agent " + string(kind) + " 2>/dev/null || true"
}

var ourCommand = regexp.MustCompile(`^'([^']*/rex-amq-adapter)' hook ([a-z-]+) --agent ([a-z]+)\b`)

// isOurs reports whether command is this adapter's hook for event and kind,
// from any install location.
func isOurs(command, event string, kind core.Kind) bool {
	m := ourCommand.FindStringSubmatch(command)
	return m != nil && m[2] == event && m[3] == string(kind)
}

func checkSelf(self string) error {
	if !filepath.IsAbs(self) || strings.ContainsRune(self, '\'') || filepath.Base(self) != skillName {
		return fmt.Errorf("cannot install hooks for binary %q: need an absolute path to a file named %s without single quotes", self, skillName)
	}
	return nil
}

// Status reports each agent's skill and hooks without changing anything.
func Status(agents []Agent, self string) ([]Item, error) {
	var items []Item
	seen := map[string]bool{}
	for _, a := range agents {
		if !seen[a.SkillDir] {
			seen[a.SkillDir] = true
			st, err := skillState(a, self)
			if err != nil {
				return nil, err
			}
			items = append(items, Item{Agent: a.Name, Kind: "skill", Path: a.SkillDir, State: st})
		}
		hs, err := hookState(a, self)
		if err != nil {
			return nil, err
		}
		items = append(items, Item{Agent: a.Name, Kind: "hook", Path: a.HookFile, State: hs})
	}
	return items, nil
}

// Setup installs or updates the skill stub and hooks of each agent.
func Setup(agents []Agent, o Options) ([]Item, error) {
	if err := checkSelf(o.SelfBin); err != nil {
		return nil, err
	}
	var items []Item
	seen := map[string]bool{} // agents sharing a skill directory get it once
	for _, a := range agents {
		if !seen[a.SkillDir] {
			seen[a.SkillDir] = true
			it, err := setupSkill(a, o)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
		it, err := setupHooks(a, o)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// Remove deletes only what setup installed.
func Remove(agents []Agent, o Options) ([]Item, error) {
	var items []Item
	seen := map[string]bool{}
	for _, a := range agents {
		if !seen[a.SkillDir] {
			seen[a.SkillDir] = true
			it, err := removeSkill(a, o)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
		it, err := removeHooks(a, o)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// --- skill stub ---

const stubMarker = "rex-amq-adapter-stub"

// Description stays under 400 characters and keeps the trigger phrases.
const skillDescription = `Talk to other coding agents in Rex terminals over AMQ. Use when an AMQ notice or doorbell arrives ("AMQ: … unread message(s) for <handle>", "you are <handle> in Rex"); when the user asks to send, ask, or reply to another agent in Rex, or to start a peer agent and hand it work ("开个 codex 审一下", "spin up a reviewer"); or for your AMQ identity. Outside Rex it does not apply.`

func stub(self string) string {
	return "---\nname: " + skillName + "\ndescription: " + yamlQuote(skillDescription) + "\nmetadata:\n  " + stubMarker + ": \"1\"\n---\n" +
		"<!-- Installed by \"rex-amq-adapter setup\". Remove it with \"rex-amq-adapter remove\". -->\n\n" +
		"Run `" + self + " __internal skill` and follow the guide it prints. The guide ships\n" +
		"inside the installed adapter, so it matches the version that delivers your mail.\n\n" +
		"If the command fails, the adapter is not installed at that path: tell the user to\n" +
		"run `rex-amq-adapter setup` again, and stop.\n"
}

func yamlQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func skillState(a Agent, self string) (string, error) {
	b, err := os.ReadFile(filepath.Join(a.SkillDir, "SKILL.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing", nil
	case err != nil:
		return "", err
	case string(b) == stub(self):
		return "current", nil
	case strings.Contains(string(b), stubMarker):
		return "outdated", nil
	}
	return "foreign", nil
}

func setupSkill(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "skill", Path: a.SkillDir}
	st, err := skillState(a, o.SelfBin)
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
	case st == "missing":
		it.Change = "created"
	default:
		it.Change = "updated"
	}
	if o.DryRun {
		return it, nil
	}
	return it, fsutil.WriteAtomic(filepath.Join(a.SkillDir, "SKILL.md"), []byte(stub(o.SelfBin)))
}

func removeSkill(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "skill", Path: a.SkillDir}
	st, err := skillState(a, o.SelfBin)
	if err != nil {
		return it, err
	}
	it.State = st
	switch st {
	case "missing":
		it.Change = "unchanged"
		return it, nil
	case "foreign":
		it.Change = "kept"
		return it, nil
	}
	it.Change = "removed"
	if o.DryRun {
		return it, nil
	}
	if err := os.Remove(filepath.Join(a.SkillDir, "SKILL.md")); err != nil {
		return it, err
	}
	// Remove the directory only if setup's file was all it held.
	if err := os.Remove(a.SkillDir); err != nil && !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(err) {
		return it, err
	}
	return it, nil
}

func isNotEmpty(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "not empty")
}

// --- hooks ---

// hookFile is a parsed hook settings file and where to write it back.
type hookFile struct {
	path string // symlinks resolved, so a linked dotfile stays linked
	root *object
	mode fs.FileMode
}

func loadHooks(path string) (hookFile, error) {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	mode := fs.FileMode(0o600)
	b, err := os.ReadFile(real)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return hookFile{}, err
	default:
		if st, err := os.Stat(real); err == nil {
			mode = st.Mode().Perm()
		}
	}
	root, err := parseObject(b)
	if err != nil {
		return hookFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return hookFile{path: real, root: root, mode: mode}, nil
}

func (h hookFile) save() error {
	b, err := marshal(h.root)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(h.path, b); err != nil {
		return err
	}
	return os.Chmod(h.path, h.mode)
}

// events returns the "hooks" object, or nil when absent or not an object.
func (h hookFile) events() *object {
	v, _ := h.root.get("hooks")
	o, _ := v.(*object)
	return o
}

// ourHook is one of the adapter's entries inside an event's array.
type ourHook struct {
	group, hook int
	entry       *object
}

func findOurs(groups []any, event string, kind core.Kind) []ourHook {
	var out []ourHook
	for gi, g := range groups {
		gobj, ok := g.(*object)
		if !ok {
			continue
		}
		hv, _ := gobj.get("hooks")
		hooks, _ := hv.([]any)
		for hi, hk := range hooks {
			hobj, ok := hk.(*object)
			if !ok {
				continue
			}
			cmd, _ := hobj.get("command")
			if s, ok := cmd.(string); ok && isOurs(s, event, kind) {
				out = append(out, ourHook{group: gi, hook: hi, entry: hobj})
			}
		}
	}
	return out
}

func hookState(a Agent, self string) (string, error) {
	switch a.Kind {
	case core.Agy:
		return agyHookState(a, self)
	case core.Pi:
		return piExtensionState(a, self)
	}
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return "", err
	}
	events := hf.events()
	present, exact := 0, 0
	for _, ev := range hookEvents {
		var groups []any
		if events != nil {
			v, _ := events.get(ev.Name)
			groups, _ = v.([]any)
		}
		ours := findOurs(groups, ev.Event, a.Kind)
		if len(ours) == 0 {
			continue
		}
		present++
		if len(ours) == 1 && entryMatches(ours[0].entry, self, ev.Event, a.Kind, ev.Timeout) {
			exact++
		}
	}
	switch {
	case present == 0:
		return "missing", nil
	case exact == len(hookEvents):
		return "current", nil
	}
	return "outdated", nil
}

func entryMatches(e *object, self, event string, kind core.Kind, timeout int) bool {
	cmd, _ := e.get("command")
	t, _ := e.get("timeout")
	typ, _ := e.get("type")
	return cmd == hookCommand(self, event, kind) && fmt.Sprint(t) == fmt.Sprint(timeout) && typ == "command"
}

func newEntry(self, event string, kind core.Kind, timeout int) *object {
	e := newObject()
	e.set("type", "command")
	e.set("command", hookCommand(self, event, kind))
	e.set("timeout", timeout)
	return e
}

// setupHooks updates the adapter's entries where they are and appends missing
// ones at the end of each event's array. Codex trusts hooks by their position
// in that array plus a content hash, so inserting before existing entries
// would invalidate the trust of every hook after the insertion point.
func setupHooks(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "hook", Path: a.HookFile}
	st, err := hookState(a, o.SelfBin)
	if err != nil {
		return it, err
	}
	it.State = st
	if st == "current" {
		it.Change = "unchanged"
		return it, nil
	}
	switch a.Kind {
	case core.Agy:
		return setupAgyHooks(a, o, it)
	case core.Pi:
		return setupPiExtension(a, o, it)
	}
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return it, err
	}
	events := hf.events()
	if events == nil {
		if v, ok := hf.root.get("hooks"); ok && v != nil {
			return it, fmt.Errorf("%s: \"hooks\" is not an object", a.HookFile)
		}
		events = newObject()
		hf.root.set("hooks", events)
	}
	for _, ev := range hookEvents {
		v, _ := events.get(ev.Name)
		groups, ok := v.([]any)
		if v != nil && !ok {
			return it, fmt.Errorf("%s: hooks.%s is not an array", a.HookFile, ev.Name)
		}
		ours := findOurs(groups, ev.Event, a.Kind)
		if len(ours) == 0 {
			group := newObject()
			group.set("hooks", []any{newEntry(o.SelfBin, ev.Event, a.Kind, ev.Timeout)})
			events.set(ev.Name, append(groups, group))
			continue
		}
		first := ours[0].entry
		first.set("type", "command")
		first.set("command", hookCommand(o.SelfBin, ev.Event, a.Kind))
		first.set("timeout", ev.Timeout)
		events.set(ev.Name, dropHooks(groups, ours[1:]))
	}
	it.Change = "created"
	if st == "outdated" {
		it.Change = "updated"
	}
	if o.DryRun {
		return it, nil
	}
	return it, hf.save()
}

func removeHooks(a Agent, o Options) (Item, error) {
	it := Item{Agent: a.Name, Kind: "hook", Path: a.HookFile}
	st, err := hookState(a, o.SelfBin)
	if err != nil {
		return it, err
	}
	it.State = st
	if st == "missing" {
		it.Change = "unchanged"
		return it, nil
	}
	switch a.Kind {
	case core.Agy:
		return removeAgyHooks(a, o, it)
	case core.Pi:
		return removePiExtension(a, o, it)
	}
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return it, err
	}
	events := hf.events()
	for _, ev := range hookEvents {
		v, _ := events.get(ev.Name)
		groups, _ := v.([]any)
		left := dropHooks(groups, findOurs(groups, ev.Event, a.Kind))
		if len(left) == 0 {
			events.del(ev.Name)
		} else {
			events.set(ev.Name, left)
		}
	}
	it.Change = "removed"
	if o.DryRun {
		return it, nil
	}
	return it, hf.save()
}

// dropHooks removes the given entries, and any group they leave empty.
func dropHooks(groups []any, drop []ourHook) []any {
	if len(drop) == 0 {
		return groups
	}
	gone := map[[2]int]bool{}
	for _, d := range drop {
		gone[[2]int{d.group, d.hook}] = true
	}
	var out []any
	for gi, g := range groups {
		gobj, ok := g.(*object)
		if !ok {
			out = append(out, g)
			continue
		}
		hv, _ := gobj.get("hooks")
		hooks, _ := hv.([]any)
		var kept []any
		for hi, hk := range hooks {
			if !gone[[2]int{gi, hi}] {
				kept = append(kept, hk)
			}
		}
		if len(kept) == 0 && len(hooks) > 0 {
			continue
		}
		if len(kept) != len(hooks) {
			gobj.set("hooks", kept)
		}
		out = append(out, gobj)
	}
	return out
}

// --- Antigravity hooks ---
//
// Antigravity's hooks.json maps a hook name to its event handlers; event
// lists are flat handler arrays for these events. The adapter owns exactly
// the group named after it, so setup and removal never touch another group.

var agyHookEvents = []struct {
	Name, Event string
	Timeout     int
}{
	{"SessionStart", "session-start", 5},
	{"PreInvocation", "pre-invocation", 3},
	{"Stop", "stop", 3},
}

func agyGroup(self string) *object {
	g := newObject()
	for _, ev := range agyHookEvents {
		g.set(ev.Name, []any{newEntry(self, ev.Event, core.Agy, ev.Timeout)})
	}
	return g
}

func agyHookState(a Agent, self string) (string, error) {
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return "", err
	}
	v, ok := hf.root.get(skillName)
	if !ok {
		return "missing", nil
	}
	got, err := marshal(v)
	if err != nil {
		return "", err
	}
	want, err := marshal(agyGroup(self))
	if err != nil {
		return "", err
	}
	if string(got) == string(want) {
		return "current", nil
	}
	return "outdated", nil
}

func setupAgyHooks(a Agent, o Options, it Item) (Item, error) {
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return it, err
	}
	hf.root.set(skillName, agyGroup(o.SelfBin)) // in place when present, else appended
	it.Change = "created"
	if it.State == "outdated" {
		it.Change = "updated"
	}
	if o.DryRun {
		return it, nil
	}
	return it, hf.save()
}

func removeAgyHooks(a Agent, o Options, it Item) (Item, error) {
	hf, err := loadHooks(a.HookFile)
	if err != nil {
		return it, err
	}
	hf.root.del(skillName)
	it.Change = "removed"
	if o.DryRun {
		return it, nil
	}
	return it, hf.save()
}

// --- Pi extension ---
//
// Pi has no command hooks; the adapter installs a small JavaScript extension
// that runs this binary and delivers through Pi's own API. The file carries a
// marker like the skill stub, so a same-named extension the user wrote is
// reported as foreign and never overwritten or removed.

//go:embed pi_extension.js
var piExtensionTemplate string

const extensionMarker = "rex-amq-adapter-extension"

func piExtension(self string) string {
	return strings.Replace(piExtensionTemplate, "__REX_AMQ_ADAPTER__", strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(self), 1)
}

func piExtensionState(a Agent, self string) (string, error) {
	b, err := os.ReadFile(a.HookFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing", nil
	case err != nil:
		return "", err
	case string(b) == piExtension(self):
		return "current", nil
	case strings.Contains(string(b), extensionMarker):
		return "outdated", nil
	}
	return "foreign", nil
}

func setupPiExtension(a Agent, o Options, it Item) (Item, error) {
	switch it.State {
	case "foreign":
		if !o.Force {
			it.Change = "conflict"
			return it, nil
		}
		it.Change = "updated"
	case "missing":
		it.Change = "created"
	default:
		it.Change = "updated"
	}
	if o.DryRun {
		return it, nil
	}
	return it, fsutil.WriteAtomic(a.HookFile, []byte(piExtension(o.SelfBin)))
}

func removePiExtension(a Agent, o Options, it Item) (Item, error) {
	if it.State == "foreign" {
		it.Change = "kept"
		return it, nil
	}
	it.Change = "removed"
	if o.DryRun {
		return it, nil
	}
	return it, os.Remove(a.HookFile)
}
