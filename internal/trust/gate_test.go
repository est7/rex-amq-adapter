package trust

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/install"
)

// world is a home with every harness store under it and an adapter state
// directory, all in temp directories: nothing reads the user's own.
func world(t *testing.T) (Gate, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	return Gate{Stores: StoresFor(home, nil), State: t.TempDir()}, home
}

func mkdir(t *testing.T, elem ...string) string {
	t.Helper()
	p := filepath.Join(elem...)
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeAccepts(t *testing.T, g Gate, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, err := install.TrustClaudeProject(context.Background(), g.Stores.Claude, k); err != nil {
			t.Fatal(err)
		}
	}
}

func check(t *testing.T, g Gate, kind core.Kind, dir string) Verdict {
	t.Helper()
	v, err := g.Check(kind, dir)
	if err != nil {
		t.Fatalf("Check(%s, %s): %v", kind, dir, err)
	}
	return v
}

// StoresFor finds each store where its harness keeps it: Claude's config in
// CLAUDE_CONFIG_DIR when set, else in home itself (not ~/.claude).
func TestTrustStoresFollowEachHarness(t *testing.T) {
	got := StoresFor("/h", nil)
	want := Stores{Home: "/h", Claude: "/h/.claude.json", Codex: "/h/.codex/config.toml", Agy: "/h/.gemini/antigravity-cli/settings.json", Pi: "/h/.pi/agent/trust.json"}
	if got != want {
		t.Fatalf("%+v", got)
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/c", "CODEX_HOME": "/x", "PI_CODING_AGENT_DIR": "/p"}
	got = StoresFor("/h", func(k string) string { return env[k] })
	if got.Claude != "/c/.claude.json" || got.Codex != "/x/config.toml" || got.Pi != "/p/trust.json" {
		t.Fatalf("%+v", got)
	}
}

// A project's trust key is its physical git toplevel, the main checkout for
// a linked worktree (or a directory inside one), and the directory itself
// outside git.
func TestTrustResolvesTheProjectRoot(t *testing.T) {
	main, wt := repo(t)
	sub := mkdir(t, wt, "pkg", "x")
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}
	plain, _ := filepath.EvalSymlinks(t.TempDir())
	for dir, want := range map[string]Project{
		main:  {Dir: main, Top: main, Root: main},
		wt:    {Dir: wt, Top: wt, Root: main},
		sub:   {Dir: sub, Top: wt, Root: main},
		link:  {Dir: main, Top: main, Root: main},
		plain: {Dir: plain, Root: plain},
	} {
		got, err := Resolve(dir)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", dir, got, err, want)
		}
	}
	if _, err := Resolve(filepath.Join(plain, "missing")); err == nil {
		t.Error("a missing directory resolved")
	}
}

// Claude's rule (probe C1): an accepted key covers the directories below it
// down to a git root, which is a boundary of its own; a linked worktree uses
// its main checkout's key; the home directory's key never counts.
func TestTrustClaudeRule(t *testing.T) {
	g, home := world(t)
	root := mkdir(t, home, "projects")
	plain := mkdir(t, root, "plain", "deep")
	repoDir := mkdir(t, root, "repo")
	gitInit(t, repoDir)
	repoSub := mkdir(t, repoDir, "sub")
	exact := mkdir(t, root, "exact")
	gitInit(t, exact)
	if out, err := exec.Command("git", "-C", exact, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	wt := filepath.Join(root, "exact-wt")
	if out, err := exec.Command("git", "-C", exact, "worktree", "add", "-q", "--detach", wt).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	homeChild := mkdir(t, home, "scratch")
	claudeAccepts(t, g, root, exact, home)
	for dir, want := range map[string]bool{
		plain: true, root: true, repoDir: false, repoSub: false, exact: true, wt: true, homeChild: false,
	} {
		if v := check(t, g, core.Claude, dir); v.Trusted != want {
			t.Errorf("%s: %+v, want trusted=%v", dir, v, want)
		}
	}
	if v := check(t, g, core.Claude, repoDir); v.Root != repoDir || !strings.Contains(v.Why, repoDir) {
		t.Errorf("the verdict names no root: %+v", v)
	}
}

// Codex asks only in a git repository, which its config must trust by
// directory, toplevel, or main checkout.
func TestTrustCodexRule(t *testing.T) {
	g, home := world(t)
	plain := mkdir(t, home, "notes")
	main, wt := repo(t)
	if v := check(t, g, core.Codex, plain); !v.Trusted {
		t.Errorf("non-git: %+v", v)
	}
	if v := check(t, g, core.Codex, wt); v.Trusted || v.Root != main {
		t.Errorf("untrusted repo: %+v", v)
	}
	if _, err := install.TrustCodexProject(g.Stores.Codex, main); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{main, wt, mkdir(t, wt, "src")} {
		if v := check(t, g, core.Codex, d); !v.Trusted {
			t.Errorf("%s: %+v", d, v)
		}
	}
}

// Antigravity trusts exact directories: a listed parent covers no child, a
// linked worktree of a listed main checkout is covered for its session, and
// so is a granted project.
func TestTrustAgyRule(t *testing.T) {
	g, home := world(t)
	main, wt := repo(t)
	settings(t, home, filepath.Dir(main))
	if v := check(t, g, core.Agy, main); v.Trusted {
		t.Errorf("a parent covered its child: %+v", v)
	}
	settings(t, home, main)
	for _, d := range []string{main, wt} {
		if v := check(t, g, core.Agy, d); !v.Trusted {
			t.Errorf("%s: %+v", d, v)
		}
	}
	other, _ := filepath.EvalSymlinks(t.TempDir())
	if v := check(t, g, core.Agy, other); v.Trusted {
		t.Errorf("unlisted: %+v", v)
	}
	if _, err := g.Grant(context.Background(), other, "explicit"); err != nil {
		t.Fatal(err)
	}
	if v := check(t, g, core.Agy, other); !v.Trusted {
		t.Errorf("granted: %+v", v)
	}
}

// Pi asks only when a project holds resources it gates, and its trust.json's
// nearest entry decides; ~/.agents/skills is the user's own.
func TestTrustPiRule(t *testing.T) {
	g, home := world(t)
	writeFile(t, filepath.Join(home, ".agents", "skills", "x", "SKILL.md"), "x")
	quiet := mkdir(t, home, "w", "quiet")
	if v := check(t, g, core.Pi, quiet); !v.Trusted {
		t.Errorf("no resources: %+v", v)
	}
	for _, res := range []string{".pi/settings.json", ".pi/SYSTEM.md", ".pi/skills/s/SKILL.md", ".agents/skills/s/SKILL.md"} {
		d := mkdir(t, home, "w", strings.NewReplacer("/", "-", ".", "_").Replace(res))
		writeFile(t, filepath.Join(d, res), "x")
		if v := check(t, g, core.Pi, d); v.Trusted {
			t.Errorf("%s: %+v", res, v)
		}
	}
	nested := mkdir(t, home, "w", "_agents-skills-s-SKILL_md", "child")
	if v := check(t, g, core.Pi, nested); v.Trusted {
		t.Errorf("an ancestor's .agents/skills: %+v", v)
	}
	gated := filepath.Join(home, "w", "_pi-settings_json")
	writeFile(t, g.Stores.Pi, fmt.Sprintf(`{%q: true, %q: false}`, filepath.Join(home, "w"), gated))
	if v := check(t, g, core.Pi, filepath.Join(home, "w", "_pi-SYSTEM_md")); !v.Trusted {
		t.Errorf("a trusted ancestor: %+v", v)
	}
	if v := check(t, g, core.Pi, gated); v.Trusted {
		t.Errorf("the nearest entry distrusts: %+v", v)
	}
	if _, err := g.Grant(context.Background(), gated, "explicit"); err != nil {
		t.Fatal(err)
	}
	if v := check(t, g, core.Pi, gated); !v.Trusted {
		t.Errorf("granted: %+v", v)
	}
}

// Without a home nothing can be checked or granted: the gate fails closed.
func TestTrustFailsClosedWithoutAHome(t *testing.T) {
	dir := t.TempDir()
	g := Gate{State: t.TempDir()}
	if _, err := g.Check(core.Claude, dir); err == nil {
		t.Error("checked without a home")
	}
	if _, err := g.Grant(context.Background(), dir, "explicit"); err == nil {
		t.Error("granted without a home")
	}
}

func logRows(t *testing.T, g Gate) []map[string]any {
	t.Helper()
	f, err := os.Open(g.logPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		rows = append(rows, m)
	}
	return rows
}

// One grant of a linked worktree trusts its main checkout everywhere:
// Claude's and Codex's stores get the key, Antigravity and Pi sessions get
// it from Args, the book and the log record it, and granting again changes
// nothing but the log.
func TestTrustGrantCoversEveryHarness(t *testing.T) {
	g, home := world(t)
	main, wt := repo(t)
	settings(t, home, "/elsewhere")
	writeFile(t, filepath.Join(wt, ".pi", "settings.json"), "{}")
	for _, k := range []core.Kind{core.Claude, core.Codex, core.Agy, core.Pi} {
		if v := check(t, g, k, wt); v.Trusted {
			t.Fatalf("before the grant %s: %+v", k, v)
		}
	}
	res, err := g.Grant(context.Background(), wt, "explicit")
	if err != nil {
		t.Fatal(err)
	}
	if res.Root != main || res.Claude != "set" || res.Codex != "set" {
		t.Fatalf("%+v", res)
	}
	for _, k := range []core.Kind{core.Claude, core.Codex, core.Agy, core.Pi} {
		if v := check(t, g, k, wt); !v.Trusted {
			t.Errorf("after the grant %s: %+v", k, v)
		}
	}
	agy := g.Args(wt, []string{"agy", "--dangerously-skip-permissions", "-i", "brief"})
	if len(agy) < 3 || agy[1] != "--settings" || !slices.Equal(trustedList(t, agy[2]), []string{"/elsewhere", wt}) {
		t.Errorf("agy argv %q", agy)
	}
	if pi := g.Args(wt, []string{"pi", "--", "brief"}); !slices.Equal(pi, []string{"pi", "--approve", "--", "brief"}) {
		t.Errorf("pi argv %q", pi)
	}
	if pi := g.Args(wt, []string{"pi", "-a", "--", "b"}); !slices.Equal(pi, []string{"pi", "-a", "--", "b"}) {
		t.Errorf("pi argv approved twice %q", pi)
	}
	if c := g.Args(wt, []string{"claude", "--", "b"}); !slices.Equal(c, []string{"claude", "--", "b"}) {
		t.Errorf("claude argv %q", c)
	}
	again, err := g.Grant(context.Background(), main, "explicit")
	if err != nil || again.Claude != "already" || again.Codex != "already" {
		t.Fatalf("again %+v %v", again, err)
	}
	b, _ := g.readBook()
	if len(b.Grants) != 1 || b.Grants[0].Root != main || b.Grants[0].Source != "explicit" {
		t.Errorf("book %+v", b)
	}
	rows := logRows(t, g)
	if len(rows) != 2 || rows[0]["root"] != main || rows[0]["dir"] != wt || rows[0]["source"] != "explicit" || rows[0]["at"] == nil {
		t.Errorf("log %v", rows)
	}
}

// An ungranted project gets nothing from Args.
func TestTrustArgsWithoutAGrantChangeNothing(t *testing.T) {
	g, _ := world(t)
	dir := t.TempDir()
	for _, argv := range [][]string{{"agy", "-i", "b"}, {"pi", "--", "b"}, {"codex", "--", "b"}, nil} {
		if got := g.Args(dir, argv); !slices.Equal(got, argv) {
			t.Errorf("%q became %q", argv, got)
		}
	}
}

// A grant outside git trusts the directory itself in Claude and leaves
// Codex alone; / and the home directory are refused.
func TestTrustGrantBounds(t *testing.T) {
	g, home := world(t)
	plain := mkdir(t, home, "notes")
	res, err := g.Grant(context.Background(), plain, "explicit")
	if err != nil || res.Root != plain || res.Claude != "set" || res.Codex != "not git" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(g.Stores.Codex); !os.IsNotExist(err) {
		t.Errorf("codex config written for a non-git directory: %v", err)
	}
	for _, d := range []string{home, "/"} {
		if _, err := g.Grant(context.Background(), d, "explicit"); err == nil || !strings.Contains(err.Error(), "every project") {
			t.Errorf("%s: %v", d, err)
		}
	}
}

// Concurrent grants of eight repositories keep every key in both stores.
func TestTrustConcurrentGrantsKeepEveryKey(t *testing.T) {
	g, home := world(t)
	var dirs []string
	for i := range 8 {
		d := mkdir(t, home, "r", fmt.Sprint(i))
		gitInit(t, d)
		dirs = append(dirs, d)
	}
	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Grant(context.Background(), d, "explicit"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, d := range dirs {
		for _, k := range []core.Kind{core.Claude, core.Codex} {
			if v := check(t, g, k, d); !v.Trusted {
				t.Errorf("%s %s lost: %+v", k, d, v)
			}
		}
	}
	if rows := logRows(t, g); len(rows) != 8 {
		t.Errorf("log rows %d", len(rows))
	}
}

// Trust roots are existing directories, kept by physical path, never / or
// the home directory; removing one forgets it, and an unknown one is an error.
func TestTrustRootsAddListRemove(t *testing.T) {
	g, home := world(t)
	ctx := context.Background()
	side := mkdir(t, home, "projects", "side")
	alias := filepath.Join(home, "side-link")
	if err := os.Symlink(side, alias); err != nil {
		t.Fatal(err)
	}
	roots, err := g.AddRoots(ctx, alias, side)
	if err != nil || len(roots) != 1 || roots[0].Path != side {
		t.Fatalf("%+v %v", roots, err)
	}
	for _, bad := range []string{"/", home, filepath.Join(home, "missing")} {
		if _, err := g.AddRoots(ctx, bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if got, _ := g.Roots(); len(got) != 1 {
		t.Fatalf("a refused root was kept: %+v", got)
	}
	if _, err := g.RemoveRoots(ctx, filepath.Join(home, "nope")); err == nil {
		t.Error("an unknown root was removed")
	}
	if roots, err := g.RemoveRoots(ctx, alias); err != nil || len(roots) != 0 {
		t.Fatalf("%+v %v", roots, err)
	}
}
