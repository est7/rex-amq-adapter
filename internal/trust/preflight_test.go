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
// and a missing one pass without a grant.
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
	if err := g.Preflight(ctx, filepath.Join(home, "missing"), core.Claude); err != nil {
		t.Fatalf("a missing directory: %v", err)
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
