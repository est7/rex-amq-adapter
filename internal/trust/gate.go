package trust

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
	"github.com/est7/rex-amq-adapter/internal/install"
)

// Stores locates each harness's own trust store. The caller resolves it
// (StoresFor); nothing in this package reads the environment or the user's
// home, so a test's stores stay in its temp directory.
type Stores struct {
	// Home is the user's home: Claude never persists trust in it, and Pi's
	// ~/.agents/skills is the user's own, not a project's.
	Home   string
	Claude string // Claude Code's global config, .claude.json
	Codex  string // config.toml
	Agy    string // Antigravity's settings.json (trustedWorkspaces)
	Pi     string // Pi's trust.json
}

// StoresFor resolves Stores under home the way each harness does: Claude's
// .claude.json sits in CLAUDE_CONFIG_DIR when set, else in home itself;
// Codex and Pi honour CODEX_HOME and PI_CODING_AGENT_DIR (core.HarnessDirs).
func StoresFor(home string, getenv func(string) string) Stores {
	d := core.HarnessDirs(home, getenv)
	claude := ""
	if getenv != nil && getenv("CLAUDE_CONFIG_DIR") != "" {
		claude = filepath.Join(getenv("CLAUDE_CONFIG_DIR"), ".claude.json")
	} else if home != "" {
		claude = filepath.Join(home, ".claude.json")
	}
	s := Stores{Home: home, Claude: claude}
	if d.Codex != "" {
		s.Codex = filepath.Join(d.Codex, "config.toml")
	}
	if d.AgyCLI != "" {
		s.Agy = filepath.Join(d.AgyCLI, "settings.json")
	}
	if d.Pi != "" {
		s.Pi = filepath.Join(d.Pi, "trust.json")
	}
	return s
}

// Gate is folder trust as the adapter keeps it: the harness stores, and its
// own book (trust.json in its state directory) of what the user granted and
// which roots they declared, with every grant appended to logs/trust.log.
type Gate struct {
	Stores Stores
	State  string // the adapter's state directory
	Now    func() time.Time
}

// Grant is one granted project root, by the user (Source "explicit") or by a
// declared root ("root:<root>").
type Grant struct {
	Root   string    `json:"root"`
	Source string    `json:"source"`
	At     time.Time `json:"at"`
}

// Root is a directory the user declared: a project under it is granted the
// first time an Epic or a summon needs it.
type Root struct {
	Path string    `json:"path"`
	At   time.Time `json:"at"`
}

type book struct {
	Grants []Grant `json:"grants"`
	Roots  []Root  `json:"roots"`
}

func (g Gate) bookPath() string { return filepath.Join(g.State, "trust.json") }
func (g Gate) lockPath() string { return filepath.Join(g.State, "trust.lock") }
func (g Gate) logPath() string  { return filepath.Join(g.State, "logs", "trust.log") }

func (g Gate) now() time.Time {
	if g.Now == nil {
		return time.Now().UTC()
	}
	return g.Now().UTC()
}

func (g Gate) readBook() (book, error) {
	var b book
	raw, err := os.ReadFile(g.bookPath())
	if errors.Is(err, fs.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("%s: %w", g.bookPath(), err)
	}
	return b, nil
}

func (g Gate) writeBook(b book) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(g.bookPath(), append(raw, '\n'))
}

// update runs f on the book under the adapter's trust lock and saves it.
// The lock also serializes this adapter's writes to the harness stores.
func (g Gate) update(ctx context.Context, f func(*book) error) error {
	if g.State == "" {
		return errors.New("no adapter state directory, so trust cannot be recorded")
	}
	unlock, err := fsutil.Flock(ctx, g.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	b, err := g.readBook()
	if err != nil {
		return err
	}
	if err := f(&b); err != nil {
		return err
	}
	return g.writeBook(b)
}

// Project is where a directory's folder trust is decided.
type Project struct {
	Dir string // the directory, physical
	// Top is the nearest directory at or above Dir holding .git: the git
	// toplevel, or a linked worktree's own root. "" outside git.
	Top string
	// Root is the project's trust key: the main checkout of a linked
	// worktree, else Top, else Dir itself.
	Root string
}

// Git reports whether the project is a git checkout.
func (p Project) Git() bool { return p.Top != "" }

// Resolve finds dir's project from its physical path, without running git.
func Resolve(dir string) (Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, err
	}
	phys, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Project{}, err
	}
	if info, err := os.Stat(phys); err != nil || !info.IsDir() {
		return Project{}, fmt.Errorf("%s is not a directory", dir)
	}
	p := Project{Dir: phys, Root: phys}
	for cur := phys; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			p.Top, p.Root = cur, cur
			if main, ok, err := MainCheckout(cur); err == nil && ok {
				if real, err := filepath.EvalSymlinks(main); err == nil {
					p.Root = real
				}
			}
			return p, nil
		}
		if filepath.Dir(cur) == cur {
			return p, nil
		}
	}
}

// Verdict is whether one harness would start in a directory without a
// folder-trust dialog, and why.
type Verdict struct {
	Harness core.Kind `json:"harness"`
	Dir     string    `json:"dir"`
	Root    string    `json:"root"`
	Trusted bool      `json:"trusted"`
	Why     string    `json:"why"`
}

// Check decides, reading only, whether kind starts in dir without a
// folder-trust dialog, by that harness's own rule:
//   - claude: an accepted projects[<key>] in .claude.json, for dir or an
//     ancestor up to its git toplevel, or the main checkout of a linked
//     worktree; outside git any ancestor counts. The home directory never
//     does: Claude does not record its trust.
//   - codex: outside git it never asks; in git, projects[<key>].trust_level
//     = "trusted" for dir, its toplevel, or its main checkout.
//   - agy: dir itself in trustedWorkspaces (a parent does not cover it), a
//     linked worktree of a trusted main checkout (Derive), or a grant, which
//     Args carries into the session.
//   - pi: a grant (Args passes --approve), else the nearest entry of
//     trust.json at or above dir, else trusted when dir has no project
//     resources pi gates.
func (g Gate) Check(kind core.Kind, dir string) (Verdict, error) {
	p, err := Resolve(dir)
	if err != nil {
		return Verdict{Harness: kind, Dir: dir}, err
	}
	v := Verdict{Harness: kind, Dir: p.Dir, Root: p.Root}
	if g.Stores.Home == "" {
		return v, errors.New("no home directory, so folder trust cannot be checked")
	}
	switch kind {
	case core.Claude:
		v.Trusted, v.Why, err = g.claude(p)
	case core.Codex:
		v.Trusted, v.Why, err = g.codex(p)
	case core.Agy:
		v.Trusted, v.Why, err = g.agy(p)
	case core.Pi:
		v.Trusted, v.Why, err = g.pi(p)
	default:
		return v, fmt.Errorf("no folder-trust rule for harness %q", kind)
	}
	return v, err
}

// ancestors is dir and its parents up to stop (inclusive), or to / when
// stop is "".
func ancestors(dir, stop string) []string {
	var out []string
	for cur := dir; ; cur = filepath.Dir(cur) {
		out = append(out, cur)
		if cur == stop || filepath.Dir(cur) == cur {
			return out
		}
	}
}

func (g Gate) home() string {
	if real, err := filepath.EvalSymlinks(g.Stores.Home); err == nil {
		return real
	}
	return filepath.Clean(g.Stores.Home)
}

func (g Gate) claude(p Project) (bool, string, error) {
	trusted, err := install.ClaudeTrustedProjects(g.Stores.Claude)
	if err != nil {
		return false, "", err
	}
	keys := ancestors(p.Dir, p.Top)
	if p.Root != p.Top && p.Git() {
		keys = append(keys, p.Root)
	}
	home := g.home()
	for _, k := range keys {
		if k != home && trusted[k] {
			return true, fmt.Sprintf("%s accepts %s", g.Stores.Claude, k), nil
		}
	}
	return false, fmt.Sprintf("%s accepts neither %s nor an ancestor Claude would use", g.Stores.Claude, p.Root), nil
}

func (g Gate) codex(p Project) (bool, string, error) {
	if !p.Git() {
		return true, "not a git repository: Codex asks only in one", nil
	}
	ok, err := install.CodexProjectTrusted(g.Stores.Codex, p.Dir, p.Top, p.Root)
	if err != nil {
		return false, "", err
	}
	if ok {
		return true, fmt.Sprintf("%s trusts %s", g.Stores.Codex, p.Root), nil
	}
	return false, fmt.Sprintf("%s does not trust %s", g.Stores.Codex, p.Root), nil
}

func (g Gate) granted(root string) (bool, error) {
	b, err := g.readBook()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(b.Grants, func(gr Grant) bool { return gr.Root == root }), nil
}

func (g Gate) agy(p Project) (bool, string, error) {
	list, err := agyTrusted(g.Stores.Agy)
	if err != nil {
		return false, "", err
	}
	if key, ok := lookup(list, p.Dir); ok {
		return true, fmt.Sprintf("%s trusts %s", g.Stores.Agy, key), nil
	}
	if p.Top == p.Dir && p.Root != p.Top {
		if _, ok := lookup(list, p.Root); ok {
			return true, fmt.Sprintf("the linked worktree of trusted %s, for its session", p.Root), nil
		}
	}
	if ok, err := g.granted(p.Root); err != nil || ok {
		return ok, "granted " + p.Root + ", for its session", err
	}
	return false, fmt.Sprintf("%s does not list %s, and the adapter has no grant for %s", g.Stores.Agy, p.Dir, p.Root), nil
}

// agyTrusted reads trustedWorkspaces; a missing store trusts nothing.
func agyTrusted(store string) ([]string, error) {
	raw, err := os.ReadFile(store)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var settings struct {
		Trusted []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("%s: %w", store, err)
	}
	return settings.Trusted, nil
}

// piGated are the entries under <dir>/.pi that make Pi ask (pi-coding-agent
// 0.84.2, core/trust-manager.js).
var piGated = []string{"settings.json", "mcp.json", "extensions", "skills", "prompts", "themes", "SYSTEM.md", "APPEND_SYSTEM.md"}

func (g Gate) pi(p Project) (bool, string, error) {
	// Pi's own order (core/project-trust.js resolveProjectTrusted): no gated
	// resource means no question; then the nearest trust.json entry, true or
	// false, decides without a dialog (false starts without the project's
	// resources); then defaultProjectTrust "always" or "never" does.
	r, gated := piResources(p.Dir, g.home())
	if !gated {
		return true, "no project resources Pi asks about", nil
	}
	if ok, err := g.granted(p.Root); err != nil || ok {
		return ok, "granted " + p.Root + ", approved for its session", err
	}
	if k, d, err := g.piDecision(p.Dir); err != nil {
		return false, "", err
	} else if k != "" {
		if d {
			return true, fmt.Sprintf("%s trusts %s", g.Stores.Pi, k), nil
		}
		return true, fmt.Sprintf("%s distrusts %s: Pi starts without its project resources, asking nothing", g.Stores.Pi, k), nil
	}
	var settings struct {
		Default string `json:"defaultProjectTrust"`
	}
	if raw, err := os.ReadFile(filepath.Join(filepath.Dir(g.Stores.Pi), "settings.json")); err == nil {
		_ = json.Unmarshal(raw, &settings) // Pi reads a broken settings file as its defaults
	}
	if settings.Default == "always" || settings.Default == "never" {
		return true, "defaultProjectTrust " + settings.Default + " answers without a dialog", nil
	}
	return false, fmt.Sprintf("%s holds %s, which Pi asks about, and %s decides nothing for it", p.Dir, r, g.Stores.Pi), nil
}

// piDecision is the nearest trust.json entry at or above dir: its key and
// decision, or "" when none decides.
func (g Gate) piDecision(dir string) (string, bool, error) {
	entries := map[string]*bool{}
	raw, err := os.ReadFile(g.Stores.Pi)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", false, fmt.Errorf("%s: %w", g.Stores.Pi, err)
	}
	for _, k := range ancestors(dir, "") {
		if d, ok := entries[k]; ok && d != nil {
			return k, *d, nil
		}
	}
	return "", false, nil
}

// piResources names the first project resource Pi gates: an entry of
// <dir>/.pi, or .agents/skills in dir or an ancestor other than home's.
func piResources(dir, home string) (string, bool) {
	for _, e := range piGated {
		if _, err := os.Stat(filepath.Join(dir, ".pi", e)); err == nil {
			return filepath.Join(dir, ".pi", e), true
		}
	}
	user := filepath.Join(home, ".agents", "skills")
	for _, a := range ancestors(dir, "") {
		s := filepath.Join(a, ".agents", "skills")
		if s == user {
			continue
		}
		if _, err := os.Stat(s); err == nil {
			return s, true
		}
	}
	return "", false
}

// GrantResult is what one grant did.
type GrantResult struct {
	Dir    string `json:"dir"`
	Root   string `json:"root"`
	Source string `json:"source"`
	// Claude and Codex say what the grant did to each store: "set",
	// "already", or for Codex outside git "not git" (it never asks there).
	Claude string `json:"claude"`
	Codex  string `json:"codex"`
}

// Grant trusts dir's project for every harness, as the user's "yes, I trust
// this folder" does, once: the root is recorded in the adapter's book,
// Claude's and Codex's stores get its key (Antigravity and Pi get it per
// session from Args), and a line goes to logs/trust.log. It is idempotent
// and safe to run concurrently. source is "explicit" or "root:<root>".
func (g Gate) Grant(ctx context.Context, dir, source string) (GrantResult, error) {
	p, err := Resolve(dir)
	if err != nil {
		return GrantResult{}, err
	}
	r := GrantResult{Dir: p.Dir, Root: p.Root, Source: source}
	if g.Stores.Home == "" {
		return r, errors.New("no home directory, so folder trust cannot be granted")
	}
	if err := g.unbounded(p.Root); err != nil {
		return r, err
	}
	err = g.update(ctx, func(b *book) error {
		if !slices.ContainsFunc(b.Grants, func(gr Grant) bool { return gr.Root == p.Root }) {
			b.Grants = append(b.Grants, Grant{Root: p.Root, Source: source, At: g.now()})
		}
		changed, err := install.TrustClaudeProject(ctx, g.Stores.Claude, p.Root)
		if err != nil {
			return fmt.Errorf("claude: %w", err)
		}
		r.Claude = state(changed)
		r.Codex = "not git"
		if p.Git() {
			changed, err := install.TrustCodexProject(g.Stores.Codex, p.Root)
			if err != nil {
				return fmt.Errorf("codex: %w", err)
			}
			r.Codex = state(changed)
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return r, g.log(map[string]any{"at": g.now().Format(time.RFC3339), "dir": r.Dir, "root": r.Root, "source": r.Source, "claude": r.Claude, "codex": r.Codex})
}

func state(changed bool) string {
	if changed {
		return "set"
	}
	return "already"
}

// unbounded refuses / and the home directory: trusting either would trust
// every project on the machine, and Claude never records the home's.
func (g Gate) unbounded(path string) error {
	if path == string(filepath.Separator) || path == g.home() {
		return fmt.Errorf("%s would trust every project under it; name the project or a narrower root", path)
	}
	return nil
}

// log appends one JSON line to logs/trust.log, the record of every grant.
func (g Gate) log(entry map[string]any) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.logPath()), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(g.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// AddRoots declares each dir a trust root. A root must be an existing
// directory other than / and the home directory; it is kept by its physical
// path. Adding one twice keeps the first.
func (g Gate) AddRoots(ctx context.Context, dirs ...string) ([]Root, error) {
	var add []string
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		phys, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("a trust root must exist: %w", err)
		}
		if info, err := os.Stat(phys); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("a trust root must be a directory: %s", d)
		}
		if err := g.unbounded(phys); err != nil {
			return nil, err
		}
		add = append(add, phys)
	}
	var roots []Root
	err := g.update(ctx, func(b *book) error {
		for _, a := range add {
			if !slices.ContainsFunc(b.Roots, func(r Root) bool { return r.Path == a }) {
				b.Roots = append(b.Roots, Root{Path: a, At: g.now()})
			}
		}
		roots = b.Roots
		return nil
	})
	return roots, err
}

// RemoveRoots forgets each dir as a trust root, by its physical path when
// it still exists, else as written. What its roots granted stays granted.
func (g Gate) RemoveRoots(ctx context.Context, dirs ...string) ([]Root, error) {
	var roots []Root
	err := g.update(ctx, func(b *book) error {
		for _, d := range dirs {
			key := filepath.Clean(d)
			if abs, err := filepath.Abs(d); err == nil {
				key = abs
				if phys, err := filepath.EvalSymlinks(abs); err == nil {
					key = phys
				}
			}
			n := len(b.Roots)
			b.Roots = slices.DeleteFunc(b.Roots, func(r Root) bool { return r.Path == key })
			if len(b.Roots) == n {
				return fmt.Errorf("%s is not a trust root", d)
			}
		}
		roots = b.Roots
		return nil
	})
	return roots, err
}

// Roots lists the declared trust roots.
func (g Gate) Roots() ([]Root, error) {
	b, err := g.readBook()
	return b.Roots, err
}

// Args carries the adapter's grants into a session that has no persisted
// route for them: an Antigravity session gets `--settings` with dir added to
// trustedWorkspaces (Derive's, for a linked worktree of a trusted main
// checkout, first), and a Pi session gets `--approve`. Any other command, a
// directory without a grant, or a store that cannot be read leaves argv as
// it is, and the session meets the harness's own dialog.
func (g Gate) Args(dir string, argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	switch filepath.Base(argv[0]) {
	case "agy":
		if t, err := derive(g.Stores.Agy, dir); err == nil && t.Settings != "" {
			return Args(argv, t.Settings)
		}
		p, err := Resolve(dir)
		if err != nil {
			return argv
		}
		if ok, err := g.granted(p.Root); err != nil || !ok {
			return argv
		}
		list, err := agyTrusted(g.Stores.Agy)
		if err != nil {
			return argv
		}
		if _, ok := lookup(list, p.Dir); ok {
			return argv
		}
		// The directory as the session is started in it (Derive's spelling),
		// and its physical path when that differs.
		abs, err := filepath.Abs(dir)
		if err != nil {
			return argv
		}
		add := []string{abs}
		if p.Dir != abs {
			add = append(add, p.Dir)
		}
		out, err := json.Marshal(map[string][]string{"trustedWorkspaces": append(slices.Clone(list), add...)})
		if err != nil {
			return argv
		}
		return Args(argv, string(out))
	case "pi":
		p, err := Resolve(dir)
		if err != nil {
			return argv
		}
		if ok, err := g.granted(p.Root); err != nil || !ok {
			return argv
		}
		if slices.ContainsFunc(argv[1:], func(a string) bool { return a == "--approve" || a == "-a" }) {
			return argv
		}
		return append([]string{argv[0], "--approve"}, argv[1:]...)
	}
	return argv
}

// Under reports the declared root that strictly contains path (both
// physical), if any.
func Under(roots []Root, path string) (string, bool) {
	for _, r := range roots {
		rel, err := filepath.Rel(r.Path, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return r.Path, true
	}
	return "", false
}

// Untrusted is the preflight's refusal: a harness would stop on a
// folder-trust dialog in Dir, and no declared root covers its project, or
// the user said no to it in a harness's own store (Distrusted), which only
// their explicit `trust grant` overrides.
type Untrusted struct {
	Dir, Root  string
	Asking     []Verdict // the harnesses that would ask
	Distrusted string    // the store and key holding the user's "no", if any
}

// ReasonUntrusted names the refusal in a result or an error.
const ReasonUntrusted = "untrusted_folder"

func (u Untrusted) Error() string {
	var who []string
	for _, v := range u.Asking {
		who = append(who, fmt.Sprintf("%s (%s)", v.Harness, v.Why))
	}
	if u.Distrusted != "" {
		return fmt.Sprintf("%s: %s would stop on a folder-trust dialog in %s, and %s records that you do not trust it, so no trust root grants it; if you trust it now, run `rex-amq-adapter trust grant %s`",
			ReasonUntrusted, strings.Join(who, ", "), u.Dir, u.Distrusted, u.Root)
	}
	return fmt.Sprintf("%s: %s would stop on a folder-trust dialog in %s, and no trust root covers %s; give the trust with `rex-amq-adapter trust grant %s` (or declare a parent with `rex-amq-adapter trust root add <dir>`)",
		ReasonUntrusted, strings.Join(who, ", "), u.Dir, u.Root, u.Root)
}

// distrusted names the store holding the user's explicit "no" to p: Codex's
// trust_level = "untrusted", or Pi's nearest false entry. Claude and
// Antigravity record no refusal.
func (g Gate) distrusted(p Project) (string, error) {
	if no, err := install.CodexProjectDistrusted(g.Stores.Codex, p.Dir, p.Top, p.Root); err != nil {
		return "", err
	} else if no && p.Git() {
		return g.Stores.Codex, nil
	}
	k, d, err := g.piDecision(p.Dir)
	if err != nil {
		return "", err
	}
	if k != "" && !d {
		return g.Stores.Pi + " (" + k + ")", nil
	}
	return "", nil
}

// Preflight makes sure none of kinds stops on a folder-trust dialog in dir,
// before any pane opens there. When one would, and dir's project lies
// strictly under a declared root, it grants the project as `trust grant`
// does (source "root:<root>", logged) and checks again; otherwise it
// returns Untrusted and changes nothing.
// A root never overrides the user's explicit "no" in a harness's store.
func (g Gate) Preflight(ctx context.Context, dir string, kinds ...core.Kind) error {
	asking, err := g.asking(dir, kinds)
	if err != nil || len(asking) == 0 {
		return err
	}
	p, err := Resolve(dir)
	if err != nil {
		return err
	}
	roots, err := g.Roots()
	if err != nil {
		return err
	}
	root, ok := Under(roots, p.Root)
	if !ok {
		return Untrusted{Dir: p.Dir, Root: p.Root, Asking: asking}
	}
	if no, err := g.distrusted(p); err != nil {
		return err
	} else if no != "" {
		return Untrusted{Dir: p.Dir, Root: p.Root, Asking: asking, Distrusted: no}
	}
	if _, err := g.Grant(ctx, dir, "root:"+root); err != nil {
		return err
	}
	if asking, err = g.asking(dir, kinds); err != nil || len(asking) == 0 {
		return err
	}
	return Untrusted{Dir: p.Dir, Root: p.Root, Asking: asking}
}

func (g Gate) asking(dir string, kinds []core.Kind) ([]Verdict, error) {
	var asking []Verdict
	for _, k := range slices.Compact(slices.Sorted(slices.Values(kinds))) {
		v, err := g.Check(k, dir)
		if err != nil {
			return nil, err
		}
		if !v.Trusted {
			asking = append(asking, v)
		}
	}
	return asking, nil
}
