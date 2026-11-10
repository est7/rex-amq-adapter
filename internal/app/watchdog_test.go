package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// stuckWorld is a goal session: goal-01 in block:g, codex (the peer) in
// block:a at 84x3 with unread mail and 40 s of box_not_found behind it. The
// clock stands still at now.
func stuckWorld(t *testing.T, box string, state string) (Env, *fakeTerm, store.Record, Session) {
	t.Helper()
	term := &fakeTerm{state: state, submit: true, box: box,
		windows: map[string]string{block: "window:1", "block:g": "window:1"},
		tabs:    map[string]string{"window:1": "Work"},
		rows:    map[string]int{block: 3}, renderedRows: 36, active: "window:1"}
	e := realAmqEnv(t, term)
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	goal := store.Record{Handle: "goal-01", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:g", SessionID: "session:s", WindowID: "window:1", AgentPID: 1, Role: core.GoalRole, AdoptedAt: now}
	peer := store.Record{Handle: "codex", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: block, SessionID: "session:s", WindowID: "window:1", AgentPID: term.fgPID, AdoptedAt: now}
	for _, r := range []store.Record{goal, peer} {
		if err := e.Store.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"goal-01", "codex"} {
		if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "goal-01", "codex", "brief", "do the thing"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-40 * time.Second), now.Add(-10 * time.Second)} {
		if err := e.Store.RecordInject("codex", core.Outcome{Progress: core.Deferred, Code: "box_not_found"}, at); err != nil {
			t.Fatal(err)
		}
	}
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	return e, term, goal, s
}

func goalInbox(t *testing.T, e Env) []string {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), "goal-01")
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// (1) A tiny pane with undelivered mail is moved once to the overflow tab,
// the app is asked to show it and put back on the user's window, the pane
// measures large with an empty box, and the mail is delivered.
func TestWatchdogRecoversATinyPaneOnce(t *testing.T) {
	e, term, goal, s := stuckWorld(t, "", "idle")
	e.watchdog(context.Background(), goal, s)
	if got := strings.Join(term.moves, ","); got != block+"->window:overflow" {
		t.Fatalf("moves: %q", got)
	}
	if got := strings.Join(term.selects, ","); got != "window:overflow,window:1" {
		t.Fatalf("app selections: %q (want the overflow tab, then the user's window back)", got)
	}
	if term.active != "window:1" {
		t.Fatalf("active window left at %s", term.active)
	}
	if len(term.closed) != 1 || term.closed[0] != "block:overflow-shell" {
		t.Fatalf("the overflow tab's shell was not closed: %v", term.closed)
	}
	if sz, _ := term.Size(context.Background(), block); sz.Rows != 36 {
		t.Fatalf("pane not rendered: %+v", sz)
	}
	if len(term.writes) != 1 || !strings.Contains(term.writes[0], "unread message(s) for codex") {
		t.Fatalf("the mail was not handed to the injector: %q", term.writes)
	}
	if hist, _ := e.Store.InjectHistory("codex"); !core.Delivered(hist) {
		t.Fatalf("delivery not recorded: %+v", hist)
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("the episode stayed open after delivery")
	}
	if len(goalInbox(t, e)) != 0 {
		t.Fatal("the goal agent was reported to for a recovered pane")
	}
	// A second look finds the mail delivered and does nothing.
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 1 || len(term.selects) != 2 || len(term.writes) != 1 {
		t.Fatalf("the second look acted again: moves %v selects %v writes %d", term.moves, term.selects, len(term.writes))
	}
}

// (2) A draft in the box, or a permission prompt, gets zero keystrokes and
// zero moves, and the goal agent exactly one report.
func TestWatchdogOnlyReportsADraftOrADialog(t *testing.T) {
	for name, tc := range map[string]struct{ box, state, code string }{
		"draft":      {"half-typed question", "idle", core.ReportDraft},
		"permission": {"", "blocked", core.ReportBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			e, term, goal, s := stuckWorld(t, tc.box, tc.state)
			if tc.state == "blocked" {
				term.rows[block] = 24 // size is not the issue
			}
			e.watchdog(context.Background(), goal, s)
			e.watchdog(context.Background(), goal, s)
			if len(term.writes) != 0 || len(term.keys) != 0 || len(term.moves) != 0 || len(term.selects) != 0 {
				t.Fatalf("acted: writes %q keys %q moves %v selects %v", term.writes, term.keys, term.moves, term.selects)
			}
			inbox := goalInbox(t, e)
			if len(inbox) != 1 {
				t.Fatalf("goal reports: %v (want exactly one)", inbox)
			}
			ep, open, _ := e.Store.Episode(block)
			if !open || len(ep.Reported) != 1 {
				t.Fatalf("episode: %+v %v", ep, open)
			}
			if _, ok := ep.Reported[tc.code]; !ok {
				t.Fatalf("reported %v, want %s", ep.Reported, tc.code)
			}
		})
	}
}

// (3) A watcher that restarts mid-episode (moved, not yet rendered or
// verified) does not move the pane again, and reports no success it did not
// verify: with the pane still tiny it only renders and re-measures.
func TestWatchdogRestartContinuesTheEpisodeWithoutASecondMove(t *testing.T) {
	e, term, goal, s := stuckWorld(t, "", "idle")
	// The earlier watcher moved the block into an existing overflow tab and died.
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	term.windows[block] = "window:overflow"
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", PrevActive: "window:1", Overflow: "window:overflow", Moved: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	term.renderedRows = 3 // rendering does not help this time
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.newTabs) != 0 {
		t.Fatalf("moved again: %v %v", term.moves, term.newTabs)
	}
	if got := strings.Join(term.selects, ","); got != "window:overflow,window:1" {
		t.Fatalf("render step: %q", got)
	}
	if len(term.writes) != 0 {
		t.Fatalf("injected without a verified pane: %q", term.writes)
	}
	cur, open, _ := e.Store.Episode(block)
	if !open || cur.ID != "codex-old" || !cur.Rendered || cur.Verified || cur.Outcome != "" {
		t.Fatalf("episode after restart: %+v %v", cur, open)
	}
	// Still tiny on the next look: the goal hears once that recovery failed.
	e.watchdog(context.Background(), goal, s)
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted after a failed recovery: %v %q", term.moves, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 {
		t.Fatalf("goal reports: %v", inbox)
	}
}

// A normal-sized pane that still finds no box is reported, not moved.
func TestWatchdogDoesNotMoveAPaneThatIsNotTiny(t *testing.T) {
	e, term, goal, s := stuckWorld(t, "", "idle")
	term.rows[block] = 24
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.selects) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %v %v %q", term.moves, term.selects, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 {
		t.Fatalf("goal reports: %v", inbox)
	}
}

// A pane the user moved out of the overflow tab mid-episode is left alone
// and reported once.
func TestWatchdogLeavesAPaneTheUserMoved(t *testing.T) {
	e, term, goal, s := stuckWorld(t, "", "idle")
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", PrevActive: "window:1", Overflow: "window:overflow", Moved: true, Rendered: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s) // block:a sits in window:1, not the overflow tab
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.selects) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %v %v %q", term.moves, term.selects, term.writes)
	}
	cur, _, _ := e.Store.Episode(block)
	if _, ok := cur.Reported[core.ReportMovedByUser]; !ok || len(goalInbox(t, e)) != 1 {
		t.Fatalf("reported %v, goal inbox %v", cur.Reported, goalInbox(t, e))
	}
}
