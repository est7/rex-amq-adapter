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

// stuckWorld is an orchestrator session: orch-01 in block:g, codex (the peer) in
// block:a at 84x3, owned by the app, with unread mail. The clock stands at
// *now and moves only when a test moves it. With history, 40 s of
// box_not_found lie behind the peer; without, the test runs the real
// injector to record the reason.
func stuckWorld(t *testing.T, box, state string, history bool) (Env, *fakeTerm, store.Record, Session, *time.Time) {
	t.Helper()
	t.Setenv("REX_AMQ_ADAPTER_NO_DESKTOP", "1")
	term := &fakeTerm{state: state, submit: true, box: box,
		windows: map[string]string{block: "window:1", "block:g": "window:1"},
		tabs:    map[string]string{"window:1": "Work"},
		rows:    map[string]int{block: 3}, cols: map[string]int{}, owner: map[string]string{block: "client:app"}}
	e := realAmqEnv(t, term)
	now := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	orch := store.Record{Handle: "orch-01", Kind: core.Claude, Server: "unix:///fake.sock", BlockID: "block:g", SessionID: "session:s", WindowID: "window:1", AgentPID: 1, Role: core.OrchestratorRole, AdoptedAt: now}
	peer := store.Record{Handle: "codex", Kind: core.Codex, Server: "unix:///fake.sock", BlockID: block, SessionID: "session:s", WindowID: "window:1", AgentPID: term.fgPID, AdoptedAt: now}
	for _, r := range []store.Record{orch, peer} {
		if err := e.Store.PutRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"orch-01", "codex"} {
		if err := amq.EnsureMailbox(context.Background(), e.AmqBin, e.Paths.Root(), h); err != nil {
			t.Fatal(err)
		}
	}
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "orch-01", "codex", "brief", "do the thing"); err != nil {
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
	return e, term, orch, s, &now
}

func goalInbox(t *testing.T, e Env) []string {
	t.Helper()
	ids, err := amq.Unread(e.Paths.Root(), "orch-01")
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

func acted(term *fakeTerm) string {
	return strings.Join([]string{"detaches " + strings.Join(term.detaches, ","), "resizes " + strings.Join(term.resizes, ","), "moves " + strings.Join(term.moves, ","), "writes " + strings.Join(term.writes, ",")}, "; ")
}

// (1) A tiny pane with undelivered mail is detached once, resized while
// unowned, verified, delivered to, and placed in the overflow tab; nothing
// the user looks at changes, and the goal hears nothing.
func TestWatchdogRecoversATinyPaneOnce(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	e.watchdog(context.Background(), orch, s)
	if got := acted(term); got != "detaches "+block+"; resizes "+block+":120x30; moves "+block+"->window:overflow; writes 1" && len(term.writes) != 1 {
		t.Fatalf("%s", got)
	}
	if len(term.detaches) != 1 || len(term.resizes) != 1 || len(term.moves) != 1 || len(term.writes) != 1 {
		t.Fatalf("%s", acted(term))
	}
	if !strings.Contains(term.writes[0], "unread message(s) for codex") {
		t.Fatalf("doorbell: %q", term.writes[0])
	}
	if sz, _ := term.Size(context.Background(), block); sz.Rows != 30 || sz.Columns != 120 || sz.Owner != "" {
		t.Fatalf("pane after recovery: %+v", sz)
	}
	if term.windows[block] != "window:overflow" || len(term.closed) != 1 || term.closed[0] != "block:overflow-shell" {
		t.Fatalf("placement: window %q closed %v", term.windows[block], term.closed)
	}
	if hist, _ := e.Store.InjectHistory("codex"); !core.Delivered(hist) {
		t.Fatalf("delivery not recorded: %+v", hist)
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("the episode stayed open after delivery")
	}
	if len(goalInbox(t, e)) != 0 {
		t.Fatal("the orchestrator was reported to for a recovered pane")
	}
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 1 || len(term.moves) != 1 || len(term.writes) != 1 {
		t.Fatalf("the second look acted again: %s", acted(term))
	}
}

// (2) A draft in the box or a permission prompt, as the real injector records
// them, gets zero keystrokes, zero detaches, and the orchestrator one report.
func TestWatchdogOnlyReportsADraftOrADialog(t *testing.T) {
	for name, tc := range map[string]struct{ box, state, code string }{
		"draft":      {"half-typed question", "idle", core.ReportDraft},
		"permission": {"", "blocked", ""},
	} {
		t.Run(name, func(t *testing.T) {
			e, term, orch, s, now := stuckWorld(t, tc.box, tc.state, false)
			term.rows[block] = 24
			if o := inject(e); o.Progress != core.Deferred {
				t.Fatalf("the injector should defer: %+v", o)
			}
			*now = now.Add(time.Minute)
			e.watchdog(context.Background(), orch, s)
			*now = now.Add(core.StuckFor + time.Second) // the blocked look holds a block for StuckFor
			e.watchdog(context.Background(), orch, s)
			e.watchdog(context.Background(), orch, s)
			if len(term.writes) != 0 || len(term.keys) != 0 || len(term.detaches) != 0 || len(term.moves) != 0 {
				t.Fatalf("acted: %s keys %q", acted(term), term.keys)
			}
			if inbox := goalInbox(t, e); len(inbox) != 1 {
				t.Fatalf("orchestrator reports: %v (want exactly one)", inbox)
			}
			if tc.code == core.ReportDraft {
				if r := reported(t, e); len(r) != 1 || r[tc.code].IsZero() {
					t.Fatalf("reported %v, want %s", r, tc.code)
				}
			}
		})
	}
}

// (3) A watcher that restarts mid-episode, with the block detached and
// resized but not yet delivered or placed, does not detach again: the
// detached record is still live after collection, the block is verified,
// delivered to, and placed.
func TestWatchdogRestartContinuesTheEpisodeWithoutASecondDetach(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.windows[block] = ""
	term.detached = map[string]bool{block: true}
	delete(term.owner, block)
	term.rows[block], term.cols[block] = 30, 120
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", Placement: core.Detached, Resized: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	s, err := e.FindSession(context.Background(), "block:g")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.CollectSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if rec, ok, _ := e.Store.Record(block); !ok || !rec.Live() {
		t.Fatalf("a detached block's record was collected as departed: %+v %v", rec, ok)
	}
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 0 || len(term.resizes) != 0 {
		t.Fatalf("acted on the layout again: %s", acted(term))
	}
	if len(term.writes) != 1 || term.windows[block] != "window:overflow" {
		t.Fatalf("not delivered and placed: %s window %q", acted(term), term.windows[block])
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode left open after delivery")
	}
}

// A journal written before the detach, with the block still placed, means
// nothing happened: the episode runs once from the start.
func TestWatchdogJournalBeforeDetachIsNotADetach(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	ep := core.Episode{ID: "interrupted", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1"}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 1 || len(term.writes) != 1 || term.windows[block] != "window:overflow" {
		t.Fatalf("%s window %q", acted(term), term.windows[block])
	}
}

// A placement that fails leaves the block detached, reports once, and is
// retried on the next look without a second detach; delivery is not undone.
func TestWatchdogReportsAPlacementThatFailedAndRetries(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.moveErr = errors.New("rex busy")
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 1 || len(term.writes) != 1 || term.windows[block] != "" {
		t.Fatalf("%s window %q", acted(term), term.windows[block])
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportPlaceFailed].IsZero() {
		t.Fatalf("orchestrator reports: %v, reported %v", inbox, reported(t, e))
	}
	e.watchdog(context.Background(), orch, s) // still failing: no second report, no second detach
	if len(term.detaches) != 1 || len(goalInbox(t, e)) != 1 {
		t.Fatalf("second look: %s reports %v", acted(term), goalInbox(t, e))
	}
	term.moveErr = nil
	e.watchdog(context.Background(), orch, s)
	if term.windows[block] != "window:overflow" || len(term.detaches) != 1 {
		t.Fatalf("not placed on retry: %s window %q", acted(term), term.windows[block])
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode left open after delivery and placement")
	}
}

// A resize the terminal ignores (the box never draws) is not a success: no
// doorbell, the block is still placed in the overflow tab on the next look
// so the user can find it, and the goal hears once that recovery failed.
func TestWatchdogReportsAResizeThatDidNotTake(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.resizeIgnored = true
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 1 || len(term.writes) != 0 {
		t.Fatalf("%s", acted(term))
	}
	e.watchdog(context.Background(), orch, s)
	e.watchdog(context.Background(), orch, s)
	if term.windows[block] != "window:overflow" || len(term.writes) != 0 || len(term.detaches) != 1 {
		t.Fatalf("%s window %q", acted(term), term.windows[block])
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportRecoveryFailed].IsZero() {
		t.Fatalf("orchestrator reports: %v, reported %v", inbox, reported(t, e))
	}
}

// A pane the user moved out of the overflow tab mid-episode is left alone
// and reported once, whatever its size now.
func TestWatchdogLeavesAPaneTheUserMoved(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows["block:overflow-anchor"] = "window:overflow"
	term.rows[block] = 24
	ep := core.Episode{ID: "codex-old", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now().Add(-time.Minute), Window: "window:1", Overflow: "window:overflow", Placement: core.InOverflow, Resized: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), orch, s) // block:a sits in window:1, not the overflow tab
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 0 || len(term.moves) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %s", acted(term))
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportMovedByUser].IsZero() {
		t.Fatalf("orchestrator reports: %v, reported %v", inbox, reported(t, e))
	}
}

// A normal-sized pane that still finds no box is reported, not touched; so
// is a pane whose foreground is not the agent.
func TestWatchdogDoesNotTouchAPaneItShouldNot(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.rows[block] = 24
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 0 || len(term.moves) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted: %s", acted(term))
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportBoxNotFound].IsZero() {
		t.Fatalf("orchestrator reports: %v, reported %v", inbox, reported(t, e))
	}
	e2, term2, goal2, s2, _ := stuckWorld(t, "", "idle", true)
	term2.fgs = map[string]rex.Process{block: {PID: term2.fgPID + 100000, Name: "vim"}}
	e2.watchdog(context.Background(), goal2, s2)
	if len(term2.detaches) != 0 || len(term2.writes) != 0 {
		t.Fatalf("touched a pane that shows vim: %s", acted(term2))
	}
}

// A restart after the detach but before the resize continues with the
// resize, delivers and places; a pre-detach journal with the block already
// detached does not detach again.
func TestWatchdogRestartContinuesBeforeTheResize(t *testing.T) {
	for name, journaled := range map[string]bool{"detach journaled": true, "detach not journaled": false} {
		t.Run(name, func(t *testing.T) {
			e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
			term.windows[block] = ""
			term.detached = map[string]bool{block: true}
			delete(term.owner, block)
			ep := core.Episode{ID: "before-resize", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now(), Window: "window:1", Placement: map[bool]core.Placement{true: core.Detached}[journaled]}
			if err := e.Store.PutEpisode(ep); err != nil {
				t.Fatal(err)
			}
			e.watchdog(context.Background(), orch, s)
			if len(term.detaches) != 0 || len(term.resizes) != 1 || len(term.writes) != 1 || term.windows[block] != "window:overflow" {
				t.Fatalf("%s window %q", acted(term), term.windows[block])
			}
		})
	}
}

// A detached block the user placed in a window of their own is theirs: no
// doorbell from the watchdog, one moved_by_user report, nothing else.
func TestWatchdogLeavesAManualPlacementAlone(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.windows[block] = "window:user-chosen"
	term.tabs["window:user-chosen"] = "User"
	term.rows[block] = 24
	ep := core.Episode{ID: "manual", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now(), Window: "window:1", Overflow: "window:overflow", Placement: core.Detached, Resized: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), orch, s)
	e.watchdog(context.Background(), orch, s)
	if len(term.writes) != 0 || len(term.moves) != 0 || len(term.detaches) != 0 || term.windows[block] != "window:user-chosen" {
		t.Fatalf("%s window %q", acted(term), term.windows[block])
	}
	if inbox := goalInbox(t, e); len(inbox) != 1 || reported(t, e)[core.ReportMovedByUser].IsZero() {
		t.Fatalf("orchestrator reports: %v, reported %v", inbox, reported(t, e))
	}
	if cur, _, _ := e.Store.Episode(block); cur.Placement != core.Theirs {
		t.Fatalf("episode: %+v", cur)
	}
}

// The orchestrator's own pane can be recovered too: after a detach the
// watcher's next look still finds its session, and the block is placed
// before the look ends even when the resize did not take.
func TestWatchdogRecoversTheOrchestratorsOwnPane(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", false)
	orch.AgentPID = term.fgPID
	if err := e.Store.PutRecord(orch); err != nil {
		t.Fatal(err)
	}
	term.rows[orch.BlockID] = 3
	term.owner[orch.BlockID] = "client:app"
	term.resizeIgnored = true
	if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "codex", "orch-01", "brief", "do this"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{e.Now().Add(-40 * time.Second), e.Now().Add(-10 * time.Second)} {
		if err := e.Store.RecordInject(orch.Handle, core.Outcome{Progress: core.Deferred, Code: "box_not_found"}, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.watchPeer(context.Background(), orch, s, orch); err != nil {
		t.Fatal(err)
	}
	if term.windows[orch.BlockID] != "window:overflow" {
		t.Fatalf("the orchestrator's block was left detached: window %q, %s", term.windows[orch.BlockID], acted(term))
	}
	if err := e.watchOnce(context.Background(), core.SessionKey{Server: s.server, SessionID: s.id}, []store.Record{orch}); err != nil {
		t.Fatalf("the watcher cannot look again: %v", err)
	}
}

// A report about the orchestrator's own pane never goes to its own stuck
// inbox: a live agent of another model family hears it, or the desktop when
// none can (none is there, or the only one's last turn ended in an error).
func TestWatchdogReportsTheOrchestratorsOwnPaneElsewhere(t *testing.T) {
	setup := func(t *testing.T) (Env, store.Record, Session, *[]string) {
		e, term, orch, s, _ := stuckWorld(t, "", "idle", false)
		orch.AgentPID = term.fgPID
		if err := e.Store.PutRecord(orch); err != nil {
			t.Fatal(err)
		}
		term.rows[orch.BlockID] = 40
		if err := amq.Send(context.Background(), e.AmqBin, e.Paths.Root(), "codex", "orch-01", "brief", "do this"); err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{e.Now().Add(-40 * time.Second), e.Now().Add(-10 * time.Second)} {
			if err := e.Store.RecordInject(orch.Handle, core.Outcome{Progress: core.Deferred, Code: "box_not_found"}, at); err != nil {
				t.Fatal(err)
			}
		}
		var desk []string
		e.Notify = func(_, text string) { desk = append(desk, text) }
		return e, orch, s, &desk
	}
	codexUnread := func(t *testing.T, e Env) int {
		ids, err := amq.Unread(e.Paths.Root(), "codex")
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	}
	t.Run("another family hears it", func(t *testing.T) {
		e, orch, s, desk := setup(t)
		before := codexUnread(t, e)
		if err := e.watchPeer(context.Background(), orch, s, orch); err != nil {
			t.Fatal(err)
		}
		if n := len(goalInbox(t, e)); n != 1 {
			t.Fatalf("the orchestrator's own inbox holds %d messages, want only the one stuck there", n)
		}
		if got := codexUnread(t, e); got != before+1 {
			t.Fatalf("codex unread %d, want %d", got, before+1)
		}
		if len(*desk) != 0 {
			t.Fatalf("desktop told: %v", *desk)
		}
	})
	t.Run("the desktop hears it when no other family is there", func(t *testing.T) {
		e, orch, s, desk := setup(t)
		peer, _, err := e.Store.Record(block)
		if err != nil {
			t.Fatal(err)
		}
		peer.Kind = core.Claude
		if err := e.Store.PutRecord(peer); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := e.watchPeer(context.Background(), orch, s, orch); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(goalInbox(t, e)); n != 1 {
			t.Fatalf("the orchestrator's own inbox holds %d messages, want 1", n)
		}
		if len(*desk) != 1 || !strings.Contains((*desk)[0], "box_not_found") {
			t.Fatalf("desktop: %v, want one box_not_found notice", *desk)
		}
	})
	t.Run("a flagged other family is skipped", func(t *testing.T) {
		e, orch, s, desk := setup(t)
		peer, _, err := e.Store.Record(block)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.markTurnError(context.Background(), peer); err != nil {
			t.Fatal(err)
		}
		before := codexUnread(t, e)
		if err := e.watchPeer(context.Background(), orch, s, orch); err != nil {
			t.Fatal(err)
		}
		if got := codexUnread(t, e); got != before {
			t.Fatalf("a flagged agent was told: unread %d, want %d", got, before)
		}
		if len(*desk) != 1 {
			t.Fatalf("desktop: %v, want one notice", *desk)
		}
	})
}
