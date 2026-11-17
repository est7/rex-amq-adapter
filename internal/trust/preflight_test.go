package trust

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A root covers the projects strictly under it: not itself, not a sibling
// that shares its name as a prefix, not its parent.
func TestTrustRootCoversOnlyWhatIsStrictlyUnderIt(t *testing.T) {
	roots := []Root{{Path: "/u/projects/side"}}
	for path, want := range map[string]bool{
		"/u/projects/side/app":      true,
		"/u/projects/side/a/b":      true,
		"/u/projects/side":          false,
		"/u/projects/sidecar/app":   false,
		"/u/projects":               false,
		"/u/projects/side/../work":  false,
		"/u/projects/side/..hidden": true,
	} {
		if _, got := Under(roots, filepath.Clean(path)); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}

// Outside every root the preflight refuses with untrusted_folder, naming the
// harnesses that would ask and the fix, and writes nothing; under a root it
// grants (source root:<root>, one log line) and passes; a trusted directory
// passes without a grant, and a missing one fails.
func TestTrustPreflightGrantsUnderARootOnly(t *testing.T) {
	g, home := world(t)
	ctx := context.Background()
	side := mkdir(t, home, "projects", "side")
	repoDir := mkdir(t, side, "app")
	gitInit(t, repoDir)
	err := g.Preflight(ctx, repoDir, core.Claude, core.Codex, core.Claude)
	var u Untrusted
	if !errors.As(err, &u) || u.Root != repoDir || len(u.Asking) != 2 || !strings.Contains(err.Error(), "untrusted_folder") || !strings.Contains(err.Error(), "rex-amq-adapter trust grant "+repoDir) {
		t.Fatalf("%v %+v", err, u)
	}
	for _, f := range []string{g.Stores.Claude, g.Stores.Codex, g.logPath()} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refusal wrote %s", f)
		}
	}
	if _, err := g.AddRoots(ctx, side); err != nil {
		t.Fatal(err)
	}
	if err := g.Preflight(ctx, repoDir, core.Claude, core.Codex); err != nil {
		t.Fatal(err)
	}
	if err := g.Preflight(ctx, repoDir, core.Claude, core.Codex, core.Agy, core.Pi); err != nil {
		t.Fatalf("a granted project: %v", err)
	}
	if err := g.Preflight(ctx, filepath.Join(home, "missing"), core.Claude); err == nil {
		t.Fatal("a missing directory passed")
	}
	f, _ := os.Open(g.logPath())
	defer f.Close()
	var lines []string
	for sc := bufio.NewScanner(f); sc.Scan(); {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"source":"root:`+side+`"`) {
		t.Fatalf("log %q", lines)
	}
	// The root itself is not under itself.
	gitInit(t, side)
	if err := g.Preflight(ctx, side, core.Claude); !errors.As(err, &u) {
		t.Fatalf("the root itself: %v", err)
	}
}

// A root never overrides the user's own "no": a project Codex marks
// untrusted, or one Pi's nearest entry distrusts, is refused under a root
// and named; the user's explicit grant still trusts it.
func TestTrustPreflightNeverOverridesTheUsersNo(t *testing.T) {
	g, home := world(t)
	ctx := context.Background()
	side := mkdir(t, home, "projects", "side")
	if _, err := g.AddRoots(ctx, side); err != nil {
		t.Fatal(err)
	}
	codexNo := mkdir(t, side, "suspect")
	gitInit(t, codexNo)
	writeFile(t, g.Stores.Codex, "[projects.\""+codexNo+"\"]\ntrust_level = \"untrusted\"\n")
	piNo := mkdir(t, side, "pi-suspect")
	gitInit(t, piNo)
	writeFile(t, filepath.Join(piNo, ".pi", "extensions", "x.js"), "x")
	writeFile(t, g.Stores.Pi, `{"`+piNo+`": false}`)
	for dir, store := range map[string]string{codexNo: g.Stores.Codex, piNo: g.Stores.Pi} {
		err := g.Preflight(ctx, dir, core.Claude)
		var u Untrusted
		if !errors.As(err, &u) || !strings.HasPrefix(u.Distrusted, store) || !strings.Contains(err.Error(), "do not trust it") || !strings.Contains(err.Error(), "trust grant "+dir) {
			t.Fatalf("%s: %v %+v", dir, err, u)
		}
		if b, _ := g.readBook(); len(b.Grants) != 0 {
			t.Fatalf("%s granted under a root: %+v", dir, b.Grants)
		}
	}
	if _, err := g.Grant(ctx, codexNo, "explicit"); err != nil {
		t.Fatal(err)
	}
	if err := g.Preflight(ctx, codexNo, core.Claude, core.Codex); err != nil {
		t.Fatalf("after the user's own grant: %v", err)
	}
}

// PreviewPreflight decides as Preflight does and writes nothing: a project
// outside every root is untrusted_folder, one under a declared root passes
// without the grant Preflight would record, and the stores stay unwritten.
func TestTrustPreviewPreflightWritesNothing(t *testing.T) {
	g, home := world(t)
	ctx := context.Background()
	side := mkdir(t, home, "projects", "side")
	repoDir := mkdir(t, side, "app")
	gitInit(t, repoDir)
	var u Untrusted
	if err := g.PreviewPreflight(repoDir, core.Claude, core.Codex); !errors.As(err, &u) {
		t.Fatalf("outside every root: %v", err)
	}
	if _, err := g.AddRoots(ctx, side); err != nil {
		t.Fatal(err)
	}
	book, _ := os.ReadFile(g.bookPath())
	if err := g.PreviewPreflight(repoDir, core.Claude, core.Codex); err != nil {
		t.Fatalf("under a declared root: %v", err)
	}
	if after, _ := os.ReadFile(g.bookPath()); string(after) != string(book) {
		t.Errorf("the preview recorded a grant:\n%s", after)
	}
	for _, f := range []string{g.Stores.Claude, g.Stores.Codex} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the preview wrote %s", f)
		}
	}
	if err := g.PreviewPreflight(home, core.Claude); err == nil {
		t.Error("the preview passed the home directory, which a grant refuses")
	}
}
