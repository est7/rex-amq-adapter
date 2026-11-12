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
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// epicWorld is a personal agent (claude-01, in session:me) and a goal
// contract on disk. The fake terminal creates sessions; a test adopts their
// panes by writing the records their first hook would.
func openEpicWorld(t *testing.T) (Env, *fakeTerm, Location, string) {
	t.Helper()
	term := &fakeTerm{state: "idle", windows: map[string]string{"block:me": "window:me"}, tabs: map[string]string{"window:me": "me"}}
	e := fixture(t, term)
	e.TranscriptHome = t.TempDir()
	if err := e.Store.DeleteRecord("codex"); err != nil {
		t.Fatal(err)
	}
	put(t, e, store.Record{Handle: "claude-01", Kind: core.Claude, Role: core.PersonalRole, BlockID: "block:me", SessionID: "session:me", WindowID: "window:me", AdoptedAt: time.Now()})
	goal := filepath.Join(t.TempDir(), "20261112-epic.md")
	if err := os.WriteFile(goal, []byte("GOAL: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e, term, Location{Block: "block:me"}, goal
}

// adoptPane writes the live record a pane's first hook would.
func adoptPane(t *testing.T, e Env, block string, h core.Handle, role string) {
	t.Helper()
	put(t, e, store.Record{Handle: h, Kind: core.Claude, Role: role, BlockID: block, SessionID: "session:epic-1", AdoptedAt: time.Now()})
}

// One command opens the Epic: a session named for it holding the
// orchestrator (with the goal contract as its brief) and the narrator (in
// its own claimed tab, told whom it narrates for), links it to the caller,
// and reports both agents adopted.
func TestOpenEpicStartsOrchestratorAndNarratorAndLinks(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	adoptPane(t, e, "block:epic-1-1", "claude-02", core.NarratorRole)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "login revamp", Dir: dir, Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready() || res.SessionID != "session:epic-1" || res.Orchestrator.Handle != "claude-03" || res.Narrator.Handle != "claude-02" {
		t.Fatalf("result %+v", res)
	}
	if len(term.sessions_) != 1 {
		t.Fatalf("sessions created: %+v", term.sessions_)
	}
	s := term.sessions_[0]
	if s.label != "login revamp" || len(s.panes) != 2 {
		t.Fatalf("session %+v", s)
	}
	narr, orch := s.panes[0], s.panes[1]
	if narr.Tab != core.NarratorTab || narr.Label != "narrator | login revamp" || narr.Dir != dir || !strings.Contains(strings.Join(narr.Command, " "), "claude-01") {
		t.Errorf("narrator pane %+v", narr)
	}
	if orch.Tab != "" || orch.Label != "orchestrator | login revamp" || orch.Command[0] != "claude" || !strings.Contains(orch.Command[len(orch.Command)-1], goal) {
		t.Errorf("orchestrator pane %+v", orch)
	}
	// The brief is make-goal's file-delivery prompt whole: run-goal stops at
	// either exit, so a run whose Slices are all blocked hands back instead
	// of asking for more work (codex-01's s5 review).
	brief := orch.Command[len(orch.Command)-1]
	for _, want := range []string{"/goal Load skill run-goal. Execute the goal contract at " + goal + "; it is the grant.", "Done when every DONE WHEN line holds on fresh evidence", "or when every Slice left is blocked (a thrash fuse, a Boundaries item, or a Slice it builds on) and the hand-back lists each under Blocked on me."} {
		if !strings.Contains(brief, want) {
			t.Errorf("orchestrator brief lacks %q:\n%s", want, brief)
		}
	}
	if tab, ok, err := e.Store.Tab("window:epic-1-1"); err != nil || !ok || tab.Mode != core.WindowNarrator {
		t.Errorf("narrator tab not claimed: %+v %v %v", tab, ok, err)
	}
	links, err := e.Store.Links()
	if err != nil || len(links) != 1 || links[0].Personal.SessionID != "session:me" || links[0].Epic.SessionID != "session:epic-1" {
		t.Errorf("links %+v %v", links, err)
	}
}

// Starting agents needs a grant, as summon does: a goal contract that is a
// regular file, or the user's own words. Nothing is created without one, nor
// for a caller that is not a personal agent.
func TestOpenEpicRefusesWithoutAGrantOrAPersonalCaller(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	ctx := context.Background()
	for name, r := range map[string]EpicRequest{
		"no grant":         {Name: "x", Dir: dir},
		"missing contract": {Name: "x", Dir: dir, Goal: filepath.Join(filepath.Dir(goal), "nope.md")},
		"a directory":      {Name: "x", Dir: dir, Goal: filepath.Dir(goal)},
		"no name":          {Dir: dir, Asked: "start it"},
	} {
		if _, err := e.OpenEpic(ctx, loc, r); err == nil {
			t.Errorf("%s: no refusal", name)
		}
	}
	put(t, e, store.Record{Handle: "claude-07", Kind: core.Claude, Role: "implementer", BlockID: "block:worker", SessionID: "session:me", AdoptedAt: time.Now()})
	if _, err := e.OpenEpic(ctx, Location{Block: "block:worker"}, EpicRequest{Name: "x", Dir: dir, Asked: "start it"}); !errors.Is(err, ErrRefused) {
		t.Errorf("a worker opened an Epic: %v", err)
	}
	if len(term.sessions_) != 0 {
		t.Errorf("a refused Epic created sessions: %+v", term.sessions_)
	}
}

// A failure part way returns what was started, and nothing is torn down:
// here the narrator never adopts, so the result says so and is not ready,
// while the orchestrator and the link stand.
func TestOpenEpicReportsAMidwayFailureWithoutTearingDown(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	dir := filepath.Dir(goal)
	adoptPane(t, e, "block:epic-1-2", "claude-03", core.OrchestratorRole)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	res, err := e.OpenEpic(ctx, loc, EpicRequest{Name: "x", Dir: dir, Asked: "build the login page", Harness: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Ready() || res.Orchestrator.Status != core.StartupAdopted || res.Narrator.Status != core.StartupTimeout || !res.Linked || res.Narrator.Block != "block:epic-1-1" {
		t.Fatalf("result %+v", res)
	}
	if orch := term.sessions_[0].panes[1]; orch.Command[0] != "codex" || orch.Command[len(orch.Command)-1] != "build the login page" {
		t.Errorf("the orchestrator's brief is the user's words: %+v", orch.Command)
	}
	if len(term.closed) != 0 {
		t.Errorf("closed %v", term.closed)
	}
	// Creating the session failing leaves nothing behind and is an error.
	term.newSessionErr = errors.New("conflict: too many sessions")
	if _, err := e.OpenEpic(context.Background(), loc, EpicRequest{Name: "y", Dir: dir, Asked: "go"}); err == nil || slices.ContainsFunc(term.sessions_, func(s fakeSession) bool { return s.label == "y" }) {
		t.Errorf("session failure: %v, sessions %+v", err, term.sessions_)
	}
}

// fakeSession is one NewSession call.
type fakeSession struct {
	label string
	panes []rex.Pane
}

// The Epic's Rex session carries its whole name; only pane labels shorten
// it, to their task width.
func TestOpenEpicKeepsTheWholeNameForTheSession(t *testing.T) {
	e, term, loc, goal := openEpicWorld(t)
	name := "login revamp with SSO, passkeys and account recovery"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := e.OpenEpic(ctx, loc, EpicRequest{Name: name, Dir: filepath.Dir(goal), Goal: goal}); err != nil {
		t.Fatal(err)
	}
	s := term.sessions_[0]
	if s.label != name {
		t.Errorf("session label %q, want %q", s.label, name)
	}
	if l := s.panes[1].Label; l != "orchestrator | "+core.Task(name) || l == "orchestrator | "+name {
		t.Errorf("orchestrator pane label %q", l)
	}
}
