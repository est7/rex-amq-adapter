package app

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/est7/rex-amq-adapter/internal/crew"
)

// briefed builds an Env whose single agent starts in block:a, in a tab
// labelled tab, with pane label label, and whose crews are built in plus a
// project layer.
func briefed(t *testing.T, tab, label string) (Env, *string) {
	t.Helper()
	term := &fakeTerm{state: "idle", label: label, cwd: "/work/app",
		windows: map[string]string{"block:a": "window:1"}, tabs: map[string]string{"window:1": tab}}
	e := realAmqEnv(t, term)
	var dir string
	project := fstest.MapFS{"pair/reviewer.md": {Data: []byte("Project pair reviewer prompt.")}}
	e.Crews = func(d string) crew.Library {
		dir = d
		builtin := crew.Load("")
		return append(crew.Library{{Name: "project", FS: project, Dir: "/work/.agents/crews"}}, builtin[len(builtin)-1])
	}
	return e, &dir
}

// A start notice carries, after the identity, the conventions of the crew
// the tab is named after and the prompt of the role the pane label names,
// looked up from the agent's own directory.
func TestStartNoticeCarriesCrewAndRole(t *testing.T) {
	e, dir := briefed(t, "Pair", "reviewer")
	_, n := startPi(t, e, "block:a", "s1")
	id, conv, role := strings.Index(n, "you are pi-reviewer-01"), strings.Index(n, `crew "pair"`), strings.Index(n, "Project pair reviewer prompt.")
	if id < 0 || conv < id || role < conv || !strings.Contains(n, "implementer builds, the reviewer critiques") {
		t.Fatalf("start notice:\n%s", n)
	}
	if *dir != "/work/app" {
		t.Fatalf("crews were looked up from %q, want the agent's directory", *dir)
	}
}

// A tab that names no crew still gives a labelled agent its role prompt.
func TestUnnamedTabGivesTheRoleOnly(t *testing.T) {
	e, _ := briefed(t, "Window 82b616eb", "implementer")
	_, n := startPi(t, e, "block:a", "s1")
	if !strings.Contains(n, "Your role here is implementer:\nYou are the implementer.") || strings.Contains(n, "Team conventions") {
		t.Fatalf("start notice:\n%s", n)
	}
}

// An agent without a role in a tab without a crew hears nothing extra.
func TestNoCrewNoRoleNoBriefing(t *testing.T) {
	e, _ := briefed(t, "Window 82b616eb", "")
	_, n := startPi(t, e, "block:a", "s1")
	if strings.Contains(n, "\n\n") || strings.Contains(n, "role here") {
		t.Fatalf("start notice:\n%s", n)
	}
}
