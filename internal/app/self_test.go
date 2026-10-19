package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStablePathPrefersHomebrewOpt(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(prefix, "Cellar", "rex-amq-adapter", "0.1.0", "bin", "rex-amq-adapter")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without the opt link the real path is all there is.
	if got := stablePath(real); got != real {
		t.Fatalf("no opt link: %s", got)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "Cellar", "rex-amq-adapter", "0.1.0"), filepath.Join(prefix, "opt", "rex-amq-adapter")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(prefix, "opt", "rex-amq-adapter", "bin", "rex-amq-adapter")
	if got := stablePath(real); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// An opt link to another version is not this binary.
	other := filepath.Join(prefix, "Cellar", "rex-amq-adapter", "0.0.9", "bin", "rex-amq-adapter")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("y"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := stablePath(other); got != other {
		t.Fatalf("opt points elsewhere: %s", got)
	}
	// Outside Homebrew nothing changes.
	if got := stablePath("/Users/x/go/bin/rex-amq-adapter"); got != "/Users/x/go/bin/rex-amq-adapter" {
		t.Fatalf("go install path: %s", got)
	}
}
