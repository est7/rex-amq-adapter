package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// The Epic's narrator starts on Claude's cheap tier; its orchestrator keeps
// the CLI's default model.
func TestOpenEpicStartsTheNarratorOnHaiku(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "cheap", Dir: filepath.Dir(goal), Goal: goal}); err != nil {
		t.Fatal(err)
	}
	narr, orch := term.sessions_[0].panes[0].Command, term.sessions_[0].panes[1].Command
	if len(narr) < 3 || narr[0] != "claude" || narr[1] != "--model" || narr[2] != "haiku" {
		t.Errorf("narrator %q", narr)
	}
	if slices.Contains(orch, "--model") {
		t.Errorf("orchestrator %q", orch)
	}
}

// A narrator resumed by summon keeps its cheap model; a resumed worker does not get one.
func TestResumedNarratorKeepsItsCheapModel(t *testing.T) {
	if got := withModel([]string{"agy", "--conversation", "c", "-i", "p"}, core.RoleModel(core.NarratorRole, core.Agy)); !slices.Equal(got, []string{"agy", "--model", "gemini-4-hydrogen-low", "--conversation", "c", "-i", "p"}) {
		t.Errorf("agy narrator %q", got)
	}
	if got := withModel([]string{"claude", "--resume", "s", "p"}, core.RoleModel("implementer", core.Claude)); !slices.Equal(got, []string{"claude", "--resume", "s", "p"}) {
		t.Errorf("implementer %q", got)
	}
	if got := withModel(nil, "haiku"); got != nil {
		t.Errorf("empty argv %q", got)
	}
}
