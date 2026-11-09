package app

import (
	"context"
	"strings"
	"testing"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
)

// startAs starts a Pi agent in b with pane label label.
func startAs(t *testing.T, e Env, term *fakeTerm, b, session, label string) (core.Handle, string) {
	t.Helper()
	term.mu.Lock()
	term.label = label
	term.mu.Unlock()
	return startPi(t, e, b, session)
}

const standsIn = "This session runs unattended"

// In a session a goal agent runs, every other agent is told at start that
// what its texts would take to the human goes to the goal agent; the goal
// agent itself is not told to ask itself.
func TestPeersOfAGoalAgentAreToldItStandsInForTheHuman(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:b": "window:1"}}
	e := realAmqEnv(t, term)
	goal, n := startAs(t, e, term, "block:a", "s1", "goal")
	if strings.Contains(n, standsIn) {
		t.Fatalf("the goal agent was told to defer to a goal agent:\n%s", n)
	}
	_, n = startAs(t, e, term, "block:b", "s2", "reviewer")
	if !strings.Contains(n, standsIn) || !strings.Contains(n, "send it to "+string(goal)+" instead") || !strings.Contains(n, "AGENTS.md") {
		t.Fatalf("a peer of %s was not told it stands in for the human:\n%s", goal, n)
	}
}

// Without a goal agent the user is present, and nobody is told otherwise.
func TestWithoutAGoalAgentTheHumanIsTheHuman(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:b": "window:1"}}
	e := realAmqEnv(t, term)
	startAs(t, e, term, "block:a", "s1", "implementer")
	if _, n := startAs(t, e, term, "block:b", "s2", "reviewer"); strings.Contains(n, standsIn) {
		t.Fatalf("an attended session claimed to run unattended:\n%s", n)
	}
}

// A goal agent that arrives after its peers tells them at their next prompt;
// a goal agent already there hears only that another joined.
func TestALateGoalAgentIsAnnouncedAsTheHumansStandIn(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:b": "window:1", "block:c": "window:2"}}
	e := realAmqEnv(t, term)
	startAs(t, e, term, "block:a", "s1", "reviewer")
	first, _ := startAs(t, e, term, "block:c", "s3", "goal")
	_ = notice(t, e, EventPrompt, core.Pi, "block:a", "s1") // the first goal's arrival
	_ = notice(t, e, EventPrompt, core.Pi, "block:c", "s3") // nothing new for it yet
	late, _ := startAs(t, e, term, "block:b", "s2", "goal")
	if s := notice(t, e, EventPrompt, core.Pi, "block:a", "s1"); !strings.Contains(s, string(late)+" (pi, goal) joined") || !strings.Contains(s, "one of "+string(first)+", "+string(late)) {
		t.Fatalf("the reviewer was not told both %s and %s stand in for the human: %q", first, late, s)
	}
	if s := notice(t, e, EventPrompt, core.Pi, "block:c", "s3"); !strings.Contains(s, string(late)+" (pi, goal) joined") || strings.Contains(s, standsIn) {
		t.Fatalf("the other goal agent was told to defer to %s: %q", late, s)
	}
}

// An agent that takes a role with join hears of a goal agent beside it in the
// briefing join prints; one that becomes the goal agent with join tells its
// mates at their next prompt.
func TestJoinCarriesTheGoalAgentsStandIn(t *testing.T) {
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:a": "window:1", "block:b": "window:1"}, tabs: map[string]string{"window:1": "Window 1"}}
	e := realAmqEnv(t, term)
	e.Crews = func(string) crew.Library { return crew.Load("") }
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	goal, _ := startAs(t, e, term, "block:a", "s1", "goal")
	startAs(t, e, term, "block:b", "s2", "")
	j, err := e.Join(context.Background(), Location{Block: "block:b"}, "", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(j.Briefing, "send it to "+string(goal)+" instead") {
		t.Fatalf("join's briefing lacks the stand-in:\n%s", j.Briefing)
	}

	term2 := &fakeTerm{state: "idle", windows: map[string]string{"block:c": "window:1", "block:d": "window:1"}, tabs: map[string]string{"window:1": "Window 1"}}
	e2 := realAmqEnv(t, term2)
	e2.Crews = e.Crews
	startAs(t, e2, term2, "block:c", "s3", "reviewer")
	h, _ := startAs(t, e2, term2, "block:d", "s4", "")
	_ = notice(t, e2, EventPrompt, core.Pi, "block:c", "s3") // h's arrival
	if _, err := e2.Join(context.Background(), Location{Block: "block:d"}, "", "goal"); err != nil {
		t.Fatal(err)
	}
	if s := notice(t, e2, EventPrompt, core.Pi, "block:c", "s3"); !strings.Contains(s, "send it to "+string(h)+" instead") {
		t.Fatalf("the reviewer did not hear that %s became the goal agent: %q", h, s)
	}
}
