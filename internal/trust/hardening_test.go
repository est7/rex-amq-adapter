package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrustRejectsAliasedGitPointers(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink"} {
		t.Run(mode, func(t *testing.T) {
			main, wt := repo(t)
			fake := t.TempDir()
			link := os.Symlink
			if mode == "hardlink" {
				link = os.Link
			}
			if err := link(filepath.Join(wt, ".git"), filepath.Join(fake, ".git")); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			settings(t, home, main)
			for _, dir := range []string{fake, wt} {
				if mode == "symlink" && dir == wt {
					continue
				}
				got, err := Derive(home, dir)
				if err != nil || got.Settings != "" {
					t.Errorf("%s pointer derived trust: %+v, %v", mode, got, err)
				}
			}
		})
	}
}

func TestTrustKeepsDirectoryAliases(t *testing.T) {
	main, wt := repo(t)
	home := t.TempDir()
	settings(t, home, main)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(wt, alias); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{alias, filepath.Join(filepath.Dir(wt), "APP-TASK")} {
		a, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		b, berr := os.Stat(wt)
		if err != nil || berr != nil || !os.SameFile(a, b) {
			t.Fatalf("directory alias: %v %v", err, berr)
		}
		got, err := Derive(home, dir)
		if err != nil || got.Settings == "" {
			t.Errorf("directory alias lost trust: %+v %v", got, err)
		}
	}
}
