package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// stuckWorld is a goal session: goal-01 in block:g, codex (the peer) in
// block:a at 84x3 with unread mail. The clock stands at *now and moves only
// when a test moves it. With history, 40 s of box_not_found lie behind the
// peer; without, the test runs the real injector to record the reason.
func stuckWorld(t *testing.T, box, state string, history bool) (Env, *fakeTerm, store.Record, Session, *time.Time) {
	t.Helper()
	t.Setenv("REX_AMQ_ADAPTER_NO_DESKTOP", "1")
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
	if history {
		for _, at := range []time.Time{now.Add(-40 * time.Second), now.Add(-10 * time.Second)} {
			if err := e.Store.RecordInject("codex", core.Outcome{Progress: core.Deferred, Code: "box_not_found"}, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	return e, term, goal, s, &now
}

func goalInbox(t *testing.T, e Env) []string {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), "goal-01")
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func reported(t *testing.T, e Env) map[string]time.Time {
	t.Helper()
	ep, _, err := e.Store.Episode(block)
	if err != nil {
		t.Fatal(err)
	}
	return ep.Reported
}

// (1) A tiny pane with undelivered mail is moved once to the overflow tab,
// the app is asked to show it and put back on the user's window, the pane
// measures large with an empty box, and the mail is delivered.
func TestWatchdogRecoversATinyPaneOnce(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
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
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 1 || len(term.selects) != 2 || len(term.writes) != 1 {
		t.Fatalf("the second look acted again: moves %v selects %v writes %d", term.moves, term.selects, len(term.writes))
	}
}

// (2) A draft in the box or a permission prompt, as the real injector records
// them, gets zero keystrokes and zero moves, and the goal agent one report.
func TestWatchdogOnlyReportsADraftOrADialog(t *testing.T) {
	for name, tc := range map[string]struct{ box, state, code string }{
		"draft":      {"half-typed question", "idle", core.ReportDraft},
		"permission": {"", "blocked", core.ReportBlocked},
	} {
		t.Run(name, func(t *testing.T) {
			e, term, goal, s, now := stuckWorld(t, tc.box, tc.state, false)
			term.rows[block] = 24
			if o := inject(e); o.Progress != core.Deferred {
				t.Fatalf("the injector should defer: %+v", o)
			}
			*now = now.Add(time.Minute)
			e.watchdog(context.Background(), goal, s)
			e.watchdog(context.Background(), goal, s)
			if len(term.writes) != 0 || len(term.keys) != 0 || len(term.moves) != 0 || len(term.selects) != 0 {
				t.Fatalf("acted: writes %q keys %q moves %v selects %v", term.writes, term.keys, term.moves, term.selects)
			}
			if inbox := goalInbox(t, e); len(inbox) != 1 {
				t.Fatalf("goal reports: %v (want exactly one)", inbox)
			}
			if r := reported(t, e); len(r) != 1 || r[tc.code].IsZero() {
				t.Fatalf("reported %v, want %s", r, tc.code)
			}
		})
	}
}

// (3) A watcher that restarts mid-episode (moved, not yet rendered) does not
// move the pane again, reports no success it did not verify, and when the
// render does not take tells the goal once that recovery failed.
func TestWatchdogRestartContinuesTheEpisodeWithoutASecondMove(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	term.windows[block] = "window:overflow"
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", PrevActive: "window:1", Overflow: "window:overflow", Moved: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	term.renderedRows = 3
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
	if !open || cur.ID != "codex-old" || !cur.Rendered || !cur.Restored || cur.Verified || cur.Outcome != "" {
		t.Fatalf("episode after restart: %+v %v", cur, open)
	}
	e.watchdog(context.Background(), goal, s)
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted after a failed recovery: %v %q", term.moves, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportRecoveryFailed].IsZero() {
		t.Fatalf("goal reports: %v, reported %v", inbox, reported(t, e))
	}
}

// A journal written before the move, with the move taken but not confirmed
// (a crash or a timed-out call), is reconciled from the pane's position:
// no second move, the episode continues to delivery.
func TestWatchdogReconcilesAMoveTheJournalDidNotConfirm(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	term.windows[block] = "window:overflow"
	ep := core.Episode{ID: "interrupted", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", PrevActive: "window:1", Overflow: "window:overflow"}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.newTabs) != 0 {
		t.Fatalf("moved again: %v %v", term.moves, term.newTabs)
	}
	if len(term.writes) != 1 {
		t.Fatalf("delivery: %q", term.writes)
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode left open after delivery")
	}
	// The same journal with the pane still where it was: the move never took,
	// so the episode starts over and moves once.
	e2, term2, goal2, s2, _ := stuckWorld(t, "", "idle", true)
	ep.Block, ep.PID = block, term2.fgPID
	if err := e2.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e2.watchdog(context.Background(), goal2, s2)
	if got := strings.Join(term2.moves, ","); got != block+"->window:overflow" || len(term2.writes) != 1 {
		t.Fatalf("fresh move: %q writes %d", got, len(term2.writes))
	}
}

// A restore that fails does not pass as success: the mail is still handed
// over, but the goal hears once that the app shows the overflow tab.
func TestWatchdogReportsARestoreThatDidNotTake(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.selectErr = map[string]error{"window:1": errors.New("client busy")}
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 1 || len(term.writes) != 1 {
		t.Fatalf("moves %v writes %d", term.moves, len(term.writes))
	}
	if term.active != "window:overflow" {
		t.Fatalf("active %s", term.active)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportRestoreFailed].IsZero() {
		t.Fatalf("goal reports: %v, reported %v", inbox, reported(t, e))
	}
	if cur, open, _ := e.Store.Episode(block); !open || cur.Restored || cur.Outcome != "recovered" {
		t.Fatalf("episode: %+v %v", cur, open)
	}
	// Once the app comes back, the episode closes without a second report.
	delete(term.selectErr, "window:1")
	e.watchdog(context.Background(), goal, s)
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode still open after delivery")
	}
	if len(goalInbox(t, e)) != 1 {
		t.Fatal("reported twice")
	}
}

// Without an app client nothing can render a pane: it is not moved, and the
// goal hears once.
func TestWatchdogDoesNotMoveWithoutAnAppClient(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.noClients = true
	e.watchdog(context.Background(), goal, s)
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.newTabs) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %v %v %q", term.moves, term.newTabs, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportNoClient].IsZero() {
		t.Fatalf("goal reports: %v, reported %v", inbox, reported(t, e))
	}
}

// A pane the user moved out of the overflow tab mid-episode is left alone
// and reported once, whatever its size now.
func TestWatchdogLeavesAPaneTheUserMoved(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	term.rows[block] = 24 // the user also enlarged it
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", PrevActive: "window:1", Overflow: "window:overflow", Moved: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), goal, s) // block:a sits in window:1, not the overflow tab
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.selects) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %v %v %q", term.moves, term.selects, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportMovedByUser].IsZero() {
		t.Fatalf("goal reports: %v, reported %v", inbox, reported(t, e))
	}
}

// A normal-sized pane that still finds no box is reported, not moved; so is
// a pane whose foreground is not the agent.
func TestWatchdogDoesNotMoveAPaneItShouldNot(t *testing.T) {
	e, term, goal, s, _ := stuckWorld(t, "", "idle", true)
	term.rows[block] = 24
	e.watchdog(context.Background(), goal, s)
	if len(term.moves) != 0 || len(term.selects) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %v %v %q", term.moves, term.selects, term.writes)
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportBoxNotFound].IsZero() {
		t.Fatalf("goal reports: %v, reported %v", inbox, reported(t, e))
	}
	e2, term2, goal2, s2, _ := stuckWorld(t, "", "idle", true)
	term2.fgs = map[string]rex.Process{block: {PID: term2.fgPID + 100000, Name: "vim"}}
	e2.watchdog(context.Background(), goal2, s2)
	if len(term2.moves) != 0 || len(term2.writes) != 0 {
		t.Fatalf("moved a pane that shows vim: %v %q", term2.moves, term2.writes)
	}
}
