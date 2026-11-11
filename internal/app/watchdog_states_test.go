package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
)

// A working agent hears of mail through its own hooks, so doorbells that
// defer with agent_working are ordinary: no report, no episode, no move,
// whatever the pane's size.
func TestWatchdogLeavesAWorkingAgentsMailToItsHooks(t *testing.T) {
	e, term, orch, s, now := stuckWorld(t, "", "working", false)
	for _, at := range []time.Time{now.Add(-40 * time.Second), now.Add(-10 * time.Second)} {
		if err := e.Store.RecordInject("codex", core.Outcome{Progress: core.Deferred, Code: "agent_working"}, at); err != nil {
			t.Fatal(err)
		}
	}
	e.watchdog(context.Background(), orch, s)
	if inbox := goalInbox(t, e); len(inbox) != 0 {
		t.Fatalf("orchestrator told about a working agent's mail: %v", inbox)
	}
	if len(term.detaches) != 0 || len(term.writes) != 0 {
		t.Fatalf("acted on a working agent: %s", acted(term))
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("an episode was opened for a working agent")
	}
}

// A resize that fails after the detach leaves the block placed in the
// overflow tab at its old size; the next look resizes it there (nobody
// renders that tab, so nobody owns the terminal) and delivers.
func TestWatchdogResizesAPanePlacedBeforeItsResize(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.resizeErr = errors.New("rex busy")
	e.watchdog(context.Background(), orch, s)
	if term.windows[block] != "window:overflow" || len(term.writes) != 0 {
		t.Fatalf("first look: %s window %q", acted(term), term.windows[block])
	}
	term.resizeErr = nil
	e.watchdog(context.Background(), orch, s)
	if len(term.resizes) != 2 || len(term.writes) != 1 {
		t.Fatalf("the resize was not retried in the overflow tab: %s", acted(term))
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode left open after delivery")
	}
}

// A resize that never takes stops after MaxAttempts and is reported once,
// without claiming the pane was resized.
func TestWatchdogReportsAResizeThatFailsWithoutClaimingIt(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.resizeErr = errors.New("rex busy")
	for i := 0; i < 2*core.MaxAttempts+1; i++ {
		e.watchdog(context.Background(), orch, s)
	}
	if len(term.resizes) > core.MaxAttempts || len(term.writes) != 0 || len(term.detaches) != 1 {
		t.Fatalf("want at most %d resizes and one detach: %s", core.MaxAttempts, acted(term))
	}
	inbox := goalInbox(t, e)
	if len(inbox) != 1 || reported(t, e)[core.ReportRecoveryFailed].IsZero() {
		t.Fatalf("orchestrator reports %v, reported %v", inbox, reported(t, e))
	}
	if body := mailBody(t, e, "orch-01", inbox[0]); strings.Contains(body, "resized it") || strings.Contains(body, "was resized") {
		t.Fatalf("the report claims a resize that never took:\n%s", body)
	}
}

// Mail drained while the block is out of every window ends the episode only
// after the block is placed: a pane is never left detached.
func TestWatchdogPlacesADetachedPaneBeforeEndingTheEpisode(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.windows[block] = ""
	term.detached = map[string]bool{block: true}
	delete(term.owner, block)
	ep := core.Episode{ID: "drained", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: e.Now(), Window: "window:1"}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	drainMail(t, e, "codex")
	e.watchdog(context.Background(), orch, s)
	if term.windows[block] != "window:overflow" {
		t.Fatalf("the detached block was left in no window: %s window %q", acted(term), term.windows[block])
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("episode left open after placement with no mail left")
	}
}

// A placement that fails is tried once per look, counted once, and the
// overflow tab's shell made for it is closed.
func TestWatchdogTriesAPlacementOncePerLook(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.moveErr = errors.New("rex busy")
	e.watchdog(context.Background(), orch, s)
	if term.moveTries != 1 {
		t.Fatalf("placement tried %d times in one look", term.moveTries)
	}
	if r := reported(t, e); r[core.ReportPlaceFailed].IsZero() {
		t.Fatalf("reported %v, want %s", r, core.ReportPlaceFailed)
	}
	shell := "block:" + core.OverflowTab + "-shell"
	if _, open := term.windows[shell]; open {
		t.Fatalf("the overflow tab's shell %s stays open after the failed move: closed %v", shell, term.closed)
	}
}

// An episode left by another agent in the same block is not this agent's:
// its attempts and reports do not stop this agent's recovery.
func TestWatchdogStartsAFreshEpisodeForAnotherAgent(t *testing.T) {
	e, term, orch, s, now := stuckWorld(t, "", "idle", true)
	old := core.Episode{ID: "claude-old", Handle: "claude-07", Block: block, PID: term.fgPID + 1, Session: "session:s", StartedAt: now.Add(-time.Hour), Window: "window:1",
		Attempts: core.MaxAttempts, Reported: map[string]time.Time{core.ReportRecoveryFailed: now.Add(-time.Hour)}}
	if err := e.Store.PutEpisode(old); err != nil {
		t.Fatal(err)
	}
	e.watchdog(context.Background(), orch, s)
	if len(term.detaches) != 1 || len(term.writes) != 1 {
		t.Fatalf("the previous agent's episode blocked this one's recovery: %s", acted(term))
	}
}

// A block another agent's run detached before its journal said so is still
// placed: the journal is reconciled with Rex before the episode is judged
// someone else's.
func TestWatchdogPlacesABlockAnotherRunDetached(t *testing.T) {
	e, term, orch, s, now := stuckWorld(t, "", "idle", false)
	term.windows[block] = ""
	term.detached = map[string]bool{block: true}
	delete(term.owner, block)
	old := core.Episode{ID: "claude-old", Handle: "claude-07", Block: block, PID: term.fgPID + 1, Session: "session:s", StartedAt: now.Add(-time.Minute), Window: "window:1"}
	if err := e.Store.PutEpisode(old); err != nil {
		t.Fatal(err)
	}
	drainMail(t, e, "codex")
	e.watchdog(context.Background(), orch, s)
	if term.windows[block] != "window:overflow" || len(term.detaches) != 0 {
		t.Fatalf("the block was left detached or detached again: %s window %q", acted(term), term.windows[block])
	}
	if _, open, _ := e.Store.Episode(block); open {
		t.Fatal("the other run's episode was left open")
	}
}

// A placement that fails is reported under its own code and spends none of
// the recovery's attempts: the resize is still retried on the next look.
func TestWatchdogPlacementFailureLeavesTheRecoveryItsAttempts(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.resizeErr = errors.New("rex busy")
	term.moveErr = errors.New("rex busy")
	e.watchdog(context.Background(), orch, s) // detach; resize and placement both fail
	if r := reported(t, e); r[core.ReportPlaceFailed].IsZero() || !r[core.ReportRecoveryFailed].IsZero() {
		t.Fatalf("reported %v, want only %s", r, core.ReportPlaceFailed)
	}
	term.resizeErr, term.moveErr = nil, nil
	e.watchdog(context.Background(), orch, s)
	if len(term.resizes) != 2 || len(term.writes) != 1 || term.windows[block] != "window:overflow" {
		t.Fatalf("the recovery was not resumed after a failed placement: %s window %q", acted(term), term.windows[block])
	}
}

// A pane the watchdog already moved whose doorbells defer for another
// reason than a missing box is reported, not handed the mail again.
func TestWatchdogReportsAMovedPaneStuckForAnotherReason(t *testing.T) {
	e, term, orch, s, now := stuckWorld(t, "", "idle", false)
	term.tabs["window:overflow"] = core.OverflowTab
	term.windows[block] = "window:overflow"
	term.rows[block], term.cols[block] = 30, 120
	delete(term.owner, block)
	ep := core.Episode{ID: "moved", Handle: "codex", Block: block, PID: term.fgPID, Session: "session:s", StartedAt: now.Add(-time.Minute), Window: "window:1", Overflow: "window:overflow", Placement: core.InOverflow, Resized: true}
	if err := e.Store.PutEpisode(ep); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-40 * time.Second), now.Add(-10 * time.Second)} {
		if err := e.Store.RecordInject("codex", core.Outcome{Progress: core.Deferred, Code: "not_submitted"}, at); err != nil {
			t.Fatal(err)
		}
	}
	e.watchdog(context.Background(), orch, s)
	if len(term.writes) != 0 {
		t.Fatalf("handed the mail over again: %s", acted(term))
	}
	if r := reported(t, e); r[core.ReportUndelivered].IsZero() {
		t.Fatalf("reported %v, want %s", r, core.ReportUndelivered)
	}
}

// A draft the tiny pane hid and the resize reveals is reported once, by the
// next look, saying where the pane is by then: placed, never "not placed
// yet". Nothing is typed over it.
func TestWatchdogReportsADraftTheResizeRevealedOncePlaced(t *testing.T) {
	e, term, orch, s, _ := stuckWorld(t, "", "idle", true)
	term.boxAfterResize = "half-typed question"
	e.watchdog(context.Background(), orch, s)
	e.watchdog(context.Background(), orch, s)
	if len(term.writes) != 0 || term.windows[block] != "window:overflow" {
		t.Fatalf("%s window %q", acted(term), term.windows[block])
	}
	inbox := goalInbox(t, e)
	if len(inbox) != 1 || reported(t, e)[core.ReportDraft].IsZero() {
		t.Fatalf("orchestrator reports %v, reported %v", inbox, reported(t, e))
	}
	if body := mailBody(t, e, "orch-01", inbox[0]); !strings.Contains(body, "placed it in the "+core.OverflowTab+" tab") {
		t.Fatalf("the draft report does not say the pane is placed:\n%s", body)
	}
}
