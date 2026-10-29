package crew

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func lib() Library {
	project := fstest.MapFS{
		"pair/team-conventions.md": {Data: []byte("project pair conventions")},
		"pair/reviewer.md":         {Data: []byte("project pair reviewer")},
		"roles/qa.md":              {Data: []byte("project qa")},
		"pair/goal.md":             {Data: []byte("not a role")},
	}
	user := fstest.MapFS{
		"roles/reviewer.md":        {Data: []byte("user reviewer")},
		"solo/team-conventions.md": {Data: []byte("user solo conventions")},
	}
	return Library{{Name: "project", FS: project, Dir: "/p"}, {Name: "user", FS: user, Dir: "/u"}, Load("")[len(Load(""))-1]}
}

// The first layer holding a file wins it whole; a crew's own role prompt
// beats the shared one in any layer; a tab naming no crew gets shared roles.
func TestLookupOrder(t *testing.T) {
	l := lib()
	for _, tc := range []struct {
		crew, role, want, path string
	}{
		{"pair", "reviewer", "project pair reviewer", "/p/pair/reviewer.md"},
		{"", "reviewer", "user reviewer", "/u/roles/reviewer.md"},
		{"solo", "reviewer", "user reviewer", "/u/roles/reviewer.md"},
		{"pair", "qa", "project qa", "/p/roles/qa.md"},
		{"pair", "implementer", "You are the implementer.", ""},
		{"docs", "architect", "You are the documentation architect.", ""},
		{"", "architect", "You are an architect.", ""},
	} {
		p, ok, err := l.Role(tc.crew, tc.role)
		if err != nil || !ok || !strings.HasPrefix(p.Text, tc.want) || p.Path != filepath.FromSlash(tc.path) {
			t.Errorf("Role(%q, %q) = %+v %v %v, want %q from %q", tc.crew, tc.role, p, ok, err, tc.want, tc.path)
		}
	}
	if p, ok, _ := l.Conventions("pair"); !ok || p.Text != "project pair conventions" {
		t.Errorf("pair conventions: %+v", p)
	}
	if _, ok, _ := l.Conventions("nosuch"); ok {
		t.Error("an unknown crew has no conventions")
	}
	for _, role := range []string{"", "goal", "team-conventions", "nosuch"} {
		if _, ok, _ := l.Role("pair", role); ok {
			t.Errorf("%q must not resolve to a role prompt", role)
		}
	}
}

func TestBuiltinCrewsAreCompleteAndSmall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	l := Load("")
	entries, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.Crew] = e
	}
	for _, crew := range []string{"pair", "ship", "dev", "docs", "design"} {
		if !got[crew].Conventions {
			t.Errorf("built-in crew %s has no conventions: %+v", crew, got[crew])
		}
	}
	if roles := strings.Join(got[""].Roles, ","); roles != "architect,editor,implementer,qa,reviewer,writer" {
		t.Errorf("shared roles: %s", roles)
	}
	for _, e := range entries {
		names := append([]string{}, e.Roles...)
		for _, r := range names {
			p, ok, err := l.Role(e.Crew, r)
			if err != nil || !ok || len(p.Text) > 3000 {
				t.Errorf("role %s/%s: %d bytes, ok %v, %v", e.Crew, r, len(p.Text), ok, err)
			}
		}
		if e.Conventions {
			if p, _, _ := l.Conventions(e.Crew); len(p.Text) > 3000 {
				t.Errorf("conventions of %s: %d bytes", e.Crew, len(p.Text))
			}
		}
	}
}

func TestProjectCrewsAreFoundAboveTheAgentsDirectory(t *testing.T) {
	root := t.TempDir()
	crews := filepath.Join(root, ".agents", "crews", "roles")
	deep := filepath.Join(root, "app", "src")
	for _, d := range []string{crews, deep} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(crews, "reviewer.md"), []byte("project reviewer"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p, ok, err := Load(deep).Role("", "reviewer")
	if err != nil || !ok || p.Text != "project reviewer" {
		t.Fatalf("project role from a subdirectory: %+v %v %v", p, ok, err)
	}
}

func TestNameFoldsTabLabels(t *testing.T) {
	for in, want := range map[string]string{"Pair": "pair", "ship it!": "ship-it", "../etc": "etc", "Window 82b616eb": "window-82b616eb", "": ""} {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}
