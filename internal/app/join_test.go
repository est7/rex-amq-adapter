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
	"github.com/est7/rex-amq-adapter/internal/store"
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

// Joining a crew claims it for the Rex session, not the tab: the tab keeps
// following its one agent, and an agent in another tab of the session is
// briefed with the crew's conventions.
func TestJoiningACrewClaimsTheSessionAndTakesTheLead(t *testing.T) {
	e, term := joinWorld(t, "Window 1a2b3c4d")
	j, err := e.Join(context.Background(), Location{Block: "block:a"}, "Pair", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Agent.Role != "implementer" || j.Label != "implementer(pi-01)" || term.renamed["block:a"] != j.Label {
		t.Errorf("joined as %q, label %q, pane %q", j.Agent.Role, j.Label, term.renamed["block:a"])
	}
	if slices.Contains(term.renamedTabs, "window:1=pair") || term.tabs["window:1"] != "implementer(pi-01)" {
		t.Errorf("the tab should follow its agent, not name the crew: renamed %v, now %q", term.renamedTabs, term.tabs["window:1"])
	}
	if c, ok, err := e.Store.SessionCrew("session:s"); err != nil || !ok || c.Crew != "pair" || c.ClaimedBy != "pi-01" {
		t.Errorf("session crew = %+v, %v, %v; want pair claimed by pi-01", c, ok, err)
	}
	for _, want := range []string{`crew "pair"`, "shared by the agents in this Rex session", "the implementer builds, the reviewer critiques", "Your role here is implementer", "still needs reviewer", "in a new tab of this session"} {
		if !strings.Contains(j.Briefing, want) {
			t.Errorf("briefing lacks %q:\n%s", want, j.Briefing)
		}
	}
	if strings.Contains(j.Briefing, "lead:") || strings.Contains(j.Briefing, "rex split") {
		t.Errorf("the briefing carries the crew header or a split:\n%s", j.Briefing)
	}
	if r := recordOf(t, e, j.Agent.Handle); r.Role != "implementer" {
		t.Errorf("record role %q", r.Role)
	}
	notice(t, e, EventSessionStart, core.Pi, "block:a", "s1")
	if r := recordOf(t, e, j.Agent.Handle); r.Role != "implementer" {
		t.Errorf("a repeated adoption dropped the role: %q", r.Role)
	}
	// block:z is in window:2 of the same session.
	if _, n := startPi(t, e, "block:z", "z1"); !strings.Contains(n, `crew "pair"`) {
		t.Errorf("an agent in another tab of the session was not briefed with the crew:\n%s", n)
	}
}

// A session that works as one crew refuses another while other agents are in
// it, whose conventions would change under them.
func TestJoiningAnotherCrewRefusesWhileOthersAreInTheSession(t *testing.T) {
	e, term := joinWorld(t, "Window 1a2b3c4d", "block:b")
	ctx := context.Background()
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "pair", ""); err != nil {
		t.Fatal(err)
	}
	_, err := e.Join(ctx, Location{Block: "block:b"}, "ship", "")
	if !errors.Is(err, ErrCrewTaken) || !strings.Contains(err.Error(), "pair") || !strings.Contains(err.Error(), "pi-01") {
		t.Fatalf("err = %v, want ErrCrewTaken naming the crew and its agents", err)
	}
	if c, _, _ := e.Store.SessionCrew("session:s"); c.Crew != "pair" {
		t.Errorf("a refused join changed the session crew to %q", c.Crew)
	}
	if r, _, _ := e.agentAt("block:b"); r.Role != "" {
		t.Errorf("a refused join took the role %q", r.Role)
	}
	if len(term.renamedTabs) > 0 && slices.Contains(term.renamedTabs, "window:1=ship") {
		t.Errorf("a tab was named after the crew: %v", term.renamedTabs)
	}
}

// The session's own crew is joined as it stands, others in it or not, and a
// role is taken without a crew; it is still briefed with the session's crew.
func TestJoiningTheSessionsOwnCrewOrARole(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b3c4d", "block:b")
	if err := e.Store.PutSessionCrew("session:s", store.SessionCrew{Crew: "ship"}); err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(context.Background(), Location{Block: "block:b"}, "ship", "qa")
	if err != nil {
		t.Fatal(err)
	}
	if j.Agent.Role != "qa" || !strings.Contains(j.Briefing, "still needs implementer, reviewer") {
		t.Errorf("joined %+v, briefing:\n%s", j.Agent, j.Briefing)
	}
	j, err = e.Join(context.Background(), Location{Block: "block:a"}, "", "Reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if j.Agent.Role != "reviewer" || j.Crew.Name != "" || !strings.Contains(j.Briefing, `crew "ship"`) || strings.Contains(j.Briefing, "still needs") {
		t.Errorf("a role in a crew's session: %+v\n%s", j.Agent, j.Briefing)
	}
}

// A narrator or personal agent never joins a crew, and is never briefed with
// its session's crew.
func TestStealthAgentsTakeNoCrew(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b3c4d", "block:b")
	ctx := context.Background()
	for _, role := range []string{core.NarratorRole, core.PersonalRole} {
		if _, err := e.Join(ctx, Location{Block: "block:a"}, "pair", role); !errors.Is(err, ErrRefused) {
			t.Errorf("join pair --role %s: %v, want ErrRefused", role, err)
		}
	}
	if _, ok, _ := e.Store.SessionCrew("session:s"); ok {
		t.Error("a refused stealth join claimed the crew")
	}
	if _, err := e.Join(ctx, Location{Block: "block:b"}, "pair", ""); err != nil {
		t.Fatal(err)
	}
	j, err := e.Join(ctx, Location{Block: "block:a"}, "", core.NarratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(j.Briefing, "Team conventions") || !strings.Contains(j.Briefing, "narrator") {
		t.Errorf("a narrator in a crew's session was briefed with the crew:\n%s", j.Briefing)
	}
	if _, err := e.Join(ctx, Location{Block: "block:a"}, "pair", "implementer"); !errors.Is(err, ErrRefused) {
		t.Errorf("a narrator joining a crew: %v, want ErrRefused", err)
	}
	if _, n := startPi(t, e, "block:a", "s1"); strings.Contains(n, "Team conventions") {
		t.Errorf("a narrator's start notice carried the crew:\n%s", n)
	}
}

func TestJoiningAsTheOrchestratorStartsItsWatcher(t *testing.T) {
	e, _ := joinWorld(t, "Window 1a2b")
	started := filepath.Join(t.TempDir(), "started")
	self := filepath.Join(t.TempDir(), "self.sh")
	if err := os.WriteFile(self, []byte("#!/bin/sh\necho \"$*\" >>'"+started+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.SelfBin = self
	if _, err := e.Join(context.Background(), Location{Block: "block:a"}, "", "orchestrator"); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, _ := os.ReadFile(started); strings.Contains(string(b), "watch") {
			return
		}
		if time.Now().After(end) {
			t.Fatal("joining as the orchestrator did not start its watcher")
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
