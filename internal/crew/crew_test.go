package crew

import (
	"os"
	"path/filepath"
	"slices"
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
	for _, role := range []string{"", "team-conventions", "nosuch"} {
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
	if roles := strings.Join(got[""].Roles, ","); roles != "architect,editor,implementer,narrator,orchestrator,personal,qa,reviewer,writer" {
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

// The orchestrator role is the shared prompt; a crew's goal.md is Runner's mission
// description and stays out of the role lookup.
func TestOrchestratorRoleComesFromTheSharedPromptOnly(t *testing.T) {
	builtin := Load("")
	p, ok, err := builtin.Role("pair", "orchestrator")
	if err != nil || !ok || !strings.Contains(p.Text, "coordinate") {
		t.Fatalf("built-in orchestrator role: %v %v %q", ok, err, p.Text)
	}
	project := fstest.MapFS{"pair/goal.md": {Data: []byte("a mission description")}}
	l := append(Library{{Name: "project", FS: project, Dir: "/p"}}, builtin...)
	if p, _, _ := l.Role("pair", "orchestrator"); strings.Contains(p.Text, "mission description") {
		t.Fatal("a crew's goal.md was read as the role prompt")
	}
	entries, err := builtin.List()
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, e := range entries {
		if e.Crew == "" {
			listed = slices.Contains(e.Roles, "orchestrator")
		}
	}
	if !listed {
		t.Fatal("crews does not list the orchestrator role")
	}
}

// A crew's header declares who leads and who it is made of; the briefing an
// agent reads never shows it.
func TestCrewHeaderDeclaresItsShape(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	l := Load("")
	for name, want := range map[string]struct {
		lead    string
		members []string
	}{
		"pair":   {"implementer", []string{"implementer", "reviewer"}},
		"ship":   {"implementer", []string{"implementer", "reviewer", "qa"}},
		"dev":    {"architect", []string{"architect", "implementer", "reviewer"}},
		"design": {"architect", []string{"architect", "architect"}},
		"docs":   {"architect", []string{"architect", "writer", "editor"}},
	} {
		c, ok, err := l.Crew(name)
		if err != nil || !ok || c.Lead != want.lead || !slices.Equal(c.Members, want.members) {
			t.Errorf("%s: %+v %v %v, want %v", name, c, ok, err, want)
		}
		if strings.Contains(c.Conventions.Text, "lead:") || strings.HasPrefix(c.Conventions.Text, "---") || !strings.HasPrefix(c.Conventions.Text, "This tab") {
			t.Errorf("%s: the header reached the conventions: %q", name, c.Conventions.Text[:40])
		}
		if p, _, _ := l.Conventions(name); p.Text != c.Conventions.Text {
			t.Errorf("%s: Conventions and Crew disagree", name)
		}
	}
	entries, err := l.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Crew == "pair" && (e.Lead != "implementer" || !slices.Equal(e.Members, []string{"implementer", "reviewer"})) {
			t.Errorf("listed pair: %+v", e)
		}
	}
}

func TestCrewHeaders(t *testing.T) {
	for text, want := range map[string]struct {
		lead, body string
		members    []string
		bad        bool
	}{
		"no header": {body: "no header"},
		"---\nlead: Lead\nmembers: a, B ,\n---\nbody": {lead: "lead", members: []string{"a", "b"}, body: "body"},
		"---\n\nlead: x\n---\n":                       {lead: "x"},
		"---\nlead: x\nbody":                          {bad: true},
		"---\nleader: x\n---\nbody":                   {bad: true},
		"---\njust words\n---\nbody":                  {bad: true},
	} {
		lead, members, body, err := parseHeader(text)
		if (err != nil) != want.bad {
			t.Errorf("%q: err %v", text, err)
			continue
		}
		if !want.bad && (lead != want.lead || !slices.Equal(members, want.members) || body != want.body) {
			t.Errorf("%q: %q %v %q", text, lead, members, body)
		}
	}
	bad := Library{{Name: "project", FS: fstest.MapFS{"x/team-conventions.md": {Data: []byte("---\nleader: y\n---\n")}}, Dir: "/p"}}
	if _, _, err := bad.Crew("x"); err == nil || !strings.Contains(err.Error(), "x/team-conventions.md") {
		t.Errorf("a broken header is not reported with its file: %v", err)
	}
}

// A crew tab's label may carry a topic after "|"; the crew is the part
// before it, and a hyphenated crew name is read whole, as Name reads it.
func TestCrewTabName(t *testing.T) {
	if Name("ship-topic") != "ship-topic" || TabName("ship-topic") != "ship-topic" {
		t.Errorf("hyphenated crew: Name %q, TabName %q", Name("ship-topic"), TabName("ship-topic"))
	}
	for label, want := range map[string]string{
		"ship | login":        "ship",
		"Ship|login":          "ship",
		"ship-topic | fix it": "ship-topic",
		"Pair":                "pair",
		"narrator":            "narrator",
		" | topic":            "",
		"":                    "",
	} {
		if got := TabName(label); got != want {
			t.Errorf("TabName(%q) = %q, want %q", label, got, want)
		}
	}
	if Name("ship | login") != "ship-login" {
		t.Errorf("Name changed: %q", Name("ship | login"))
	}
}

func TestPurposeSaysMoreThanWhoItIs(t *testing.T) {
	for text, want := range map[string]string{
		"You are QA. You run the candidate the way a user would. Then report.": "You are QA. You run the candidate the way a user would.",
		"This tab is a two-agent loop: the implementer builds. More.\n\n1. x":  "This tab is a two-agent loop: the implementer builds.",
		"One line\nwrapped here.\n\nSecond paragraph.":                         "One line wrapped here.",
		"": "",
	} {
		if got := Purpose(text); got != want {
			t.Errorf("Purpose(%q) = %q, want %q", text, got, want)
		}
	}
}
