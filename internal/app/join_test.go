package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
)

// joinWorld has a Pi agent adopted in block:a of window:1; mates puts more
// agents in the same tab.
func joinWorld(t *testing.T, tab string, mates ...string) (Env, *fakeTerm) {
	t.Helper()
	windows := map[string]string{"block:a": "window:1", "block:z": "window:2"}
	for _, b := range mates {
		windows[b] = "window:1"
	}
	term := &fakeTerm{state: "idle", windows: windows, tabs: map[string]string{"window:1": tab, "window:2": "elsewhere"}}
	e := realAmqEnv(t, term)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	startPi(t, e, "block:a", "s1")
	for i, b := range mates {
		startPi(t, e, b, "m"+string(rune('1'+i)))
	}
	startPi(t, e, "block:z", "z1") // another tab: never in the way
	return e, term
}

func TestJoiningACrewAloneNamesTheTabAndTakesTheLead(t *testing.T) {
	e, term := joinWorld(t, "Window 1a2b")
	j, err := e.Join(context.Background(), Location{Block: "block:a"}, "Pair", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Record.Role != "implementer" || j.Label != "implementer(pi-01)" || term.renamed["block:a"] != j.Label {
		t.Errorf("joined as %q, label %q, pane %q", j.Record.Role, j.Label, term.renamed["block:a"])
	}
	if !slices.Equal(term.renamedTabs, []string{"window:1=pair"}) {
		t.Errorf("tabs renamed: %v", term.renamedTabs)
	}
	for _, want := range []string{`crew "pair"`, "the implementer builds, the reviewer critiques", "Your role here is implementer", "still needs reviewer"} {
		if !strings.Contains(j.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, j.Briefing)
		}
	}
	if strings.Contains(j.Briefing, "lead:") {
		t.Errorf("the crew's header reached the briefing:\n%s", j.Briefing)
	}
	// The role is the record's: the next hooks keep it, whatever the label says.
	if r := recordOf(t, e, j.Record.Handle); r.Role != "implementer" {
		t.Errorf("record role %q", r.Role)
	}
	notice(t, e, EventSessionStart, core.Pi, "block:a", "s1")
	if r := recordOf(t, e, j.Record.Handle); r.Role != "implementer" {
		t.Errorf("a repeated adoption dropped the role: %q", r.Role)
	}
}

func TestJoiningACrewDoesNotRenameATabOthersAreIn(t *testing.T) {
	e, term := joinWorld(t, "Window 1a2b", "block:b")
	_, err := e.Join(context.Background(), Location{Block: "block:a"}, "pair", "")
	if !errors.Is(err, ErrTabTaken) || !strings.Contains(err.Error(), "pi-02") || !strings.Contains(err.Error(), "rex-amq-adapter tab pair") {
		t.Fatalf("err = %v, want ErrTabTaken naming the agent and the way out", err)
	}
	if len(term.renamedTabs) != 0 {
		t.Errorf("the tab was renamed: %v", term.renamedTabs)
	}
	if r, _, _ := e.Store.Record("block:a"); r.Role != "" {
		t.Errorf("a refused join took the role %q", r.Role)
	}
}

// A tab named after the crew already is joined as it stands, others in it or
// not, and a role is taken without a crew in any tab.
func TestJoiningTheTabsOwnCrewOrARole(t *testing.T) {
	e, term := joinWorld(t, "Ship", "block:b")
	j, err := e.Join(context.Background(), Location{Block: "block:b"}, "ship", "qa")
	if err != nil {
		t.Fatal(err)
	}
	if j.Record.Role != "qa" || len(term.renamedTabs) != 0 || !strings.Contains(j.Briefing, "still needs implementer, reviewer") {
		t.Errorf("joined %+v, tabs %v, briefing:\n%s", j.Record, term.renamedTabs, j.Briefing)
	}
	j, err = e.Join(context.Background(), Location{Block: "block:a"}, "", "Reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if j.Record.Role != "reviewer" || j.Crew.Name != "" || !strings.Contains(j.Briefing, `crew "ship"`) || strings.Contains(j.Briefing, "still needs") {
		t.Errorf("a role in a crew's tab: %+v\n%s", j.Record, j.Briefing)
	}
}

func TestJoiningAsTheGoalAgentStartsItsWatcher(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b")
	started := filepath.Join(t.TempDir(), "started")
	self := filepath.Join(t.TempDir(), "self.sh")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho \"$*\" >>'"+started+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = self
	if _, err := e.Join(context.Background(), Location{Block: "block:a"}, "", "goal"); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(started); strings.Contains(string(b), "watch") {
			return
		}
		if time.Now().After(end) {
			t.Fatal("joining as the goal agent did not start its watcher")
		}
	}
}

func TestJoiningNeedsACrewOrARoleThatExists(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b")
	ctx := context.Background()
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "nosuch", ""); err == nil || !strings.Contains(err.Error(), "crews") {
		t.Errorf("unknown crew: %v", err)
	}
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "", ""); err == nil {
		t.Error("a join with neither crew nor role")
	}
	if _, err := e.Join(ctx, Location{Block: "block:none"}, "pair", ""); err == nil {
		t.Error("a block without an agent joined a crew")
	}
}
