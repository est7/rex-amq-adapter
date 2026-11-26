package app

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// titleCarried reports whether a hook's reply can set its harness's own
// conversation title. Claude Code reads hookSpecificOutput.sessionTitle on
// SessionStart and UserPromptSubmit only; Pi's extension applies the title
// of any reply. Antigravity asks for its window title itself (PaneTitle), and
// Codex offers no way without its shared daemon.
func titleCarried(kind core.Kind, event string) bool {
	switch kind {
	case core.Claude:
		return event == EventSessionStart || event == EventPrompt
	case core.Pi:
		return event != EventSessionEnd
	}
	return false
}

// syncTitle returns rec's conversation title (core.ConversationTitle) when
// this hook can show it as the title of session, the conversation the hook
// came from, and that session was not already given it, and records that it
// now is. Giving it only on a change keeps a title the user set by hand
// (Claude's /rename) until the task, role, or Epic changes; a new session
// under the same handle starts unnamed and is given it. The title is advice: a
// failure, reading the Epic included, is reported and the hook goes on without
// it. Whether the harness then showed it is not known: a hook that dies after
// this, or an extension that cannot apply it, leaves it unshown until it
// changes.
//
// It also tracks the title (store.TitleState) on the hooks that carry one and
// on the turn-start hooks of a harness that takes none, so Codex and agy are
// recorded unsupported. Other hooks run on every tool call and are left
// alone: reading the Epic asks Rex, and SetTask and join track their changes
// themselves.
func (e Env) syncTitle(ctx context.Context, kind core.Kind, event string, rec agent, session string) string {
	carried := titleCarried(kind, event)
	if !carried && event != EventSessionStart && event != EventPrompt {
		return ""
	}
	want, err := e.conversationTitle(ctx, rec)
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
		return ""
	}
	unlock, err := e.Store.LockLedger(ctx, rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
		return ""
	}
	defer unlock()
	given := ""
	shown, err := e.Store.TitlesShown(rec.Handle)
	if err == nil && carried && shown[session] != want {
		shown[session] = want
		if err = e.Store.SetTitlesShown(rec.Handle, shown); err == nil {
			given = want
		}
	}
	if err == nil {
		err = e.Store.SetTitleState(rec.Handle, core.TrackTitle(kind, want, shown[session]))
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
	}
	return given
}

// refreshTitle tracks rec's title after its task or role changed outside a
// hook: a harness that takes titles now wants one its conversation was not
// offered yet, until its next hook that carries a title. Advice: a failure is
// reported.
func (e Env) refreshTitle(ctx context.Context, rec agent) {
	want, err := e.conversationTitle(ctx, rec)
	if err == nil {
		var unlock func()
		if unlock, err = e.Store.LockLedger(ctx, rec.Handle); err == nil {
			defer unlock()
			var shown map[string]string
			if shown, err = e.Store.TitlesShown(rec.Handle); err == nil {
				err = e.Store.SetTitleState(rec.Handle, core.TrackTitle(rec.Kind, want, shown[rec.Session()]))
			}
		}
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
	}
}

// conversationTitle is the title rec's conversation should have: its pane
// label with the Epic its Rex session is named for. A record in no known
// session has no Epic.
func (e Env) conversationTitle(ctx context.Context, rec agent) (string, error) {
	epic := ""
	if rec.SessionID != "" {
		s, err := e.Rex.InspectSession(ctx, rec.SessionID)
		if err != nil {
			return "", fmt.Errorf("read the Epic of %s: %w", rec.SessionID, err)
		}
		epic = s.Label
	}
	return core.ConversationTitle(rec.Handle, rec.Role, epic, rec.Task), nil
}

// askTask is the request for a task given with the first prompt of a Claude
// Code or Pi session whose pane shows no task. Claude Code and Pi run their
// start hook, which carries the same request, before the user has asked
// anything, so the agent cannot know its task then; Codex and agy run it with
// the first prompt. Each
// session is asked once, at its first prompt: a session that stays without a
// task (a quick question), or clears the task it started with, is not asked
// again. An unfinished adoption is not asked: its pane is not relabelled yet.
func (e Env) askTask(ctx context.Context, kind core.Kind, event string, rec agent, session string) string {
	if kind != core.Claude && kind != core.Pi || event != EventPrompt || session == "" || !rec.Adopted() {
		return ""
	}
	unlock, err := e.Store.LockLedger(ctx, rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "ask %s for its task: %v\n", rec.Handle, err)
		return ""
	}
	defer unlock()
	asked, err := e.Store.TaskAsked(rec.Handle)
	if err == nil && asked != session {
		if err = e.Store.SetTaskAsked(rec.Handle, session); err == nil && rec.Task == "" {
			return core.TaskNotice(rec.Handle, rec.Role, e.SelfBin+" agent title", "")
		}
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "ask %s for its task: %v\n", rec.Handle, err)
	}
	return ""
}

// PaneTitleInput is the subset of a harness's title-command JSON payload the
// adapter consumes.
type PaneTitleInput struct {
	Cwd            string
	AgentState     string
	ConversationID string
	// ObservedAt is when the callback entered the adapter (before identity and
	// ancestry I/O). When zero, PaneTitle captures it at entry before reading
	// any store or process state.
	ObservedAt time.Time
}

// PaneTitle is the window title for a harness that asks for one
// (Antigravity's settings.json "title" command, run on every change of the
// agent's state): the pane label of its agent in loc. It names the terminal
// window, not the conversation, so it carries no Epic and is never tracked as
// a conversation title offered. A block without a live
// agent of that kind whose process this one descends from (agy has no
// SessionEnd, so a new agy finds the last one's record until its first turn),
// or a record that cannot be read, gives the name of the directory it works
// in, so the title never names another agent.
//
// When agy reports agent_state="idle" for rec's active conversation, PaneTitle
// also heals a stale TurnWorking ledger entry to TurnIdle: in agy v1.5.18,
// cancelling a turn with Esc while a background task or schedule timer is
// active fires no Stop hook and leaves OSC 7501 stuck at working, while the
// title command still transitions agent_state to "idle".
func (e Env) PaneTitle(loc Location, kind core.Kind, in PaneTitleInput) string {
	observedAt := in.ObservedAt
	if observedAt.IsZero() {
		if e.Now != nil {
			observedAt = e.Now()
		} else {
			observedAt = time.Now()
		}
	}
	if rec, ok, err := e.agentIn(loc); err == nil && ok && rec.Live() && rec.Kind == kind && e.descendsFrom(rec) {
		if kind == core.Agy && in.AgentState == "idle" && in.ConversationID != "" && in.ConversationID == rec.Session() {
			e.healTurnIdle(rec, in.ConversationID, observedAt)
		}
		return core.PaneLabel(rec.Handle, rec.Role, rec.Task)
	}
	return DirTitle(in.Cwd)
}

// healTurnIdle settles an existing live-incarnation TurnWorking record for rec
// to TurnIdle without incrementing Seq. It takes a lock-free fast path when
// nothing needs healing, then serializes the write through Store.UpdateTurn
// with a CAS guard on the observed Seq and ingress observedAt so a callback
// delayed in identity/ancestry lookup or before commit can never overwrite a
// newer PreInvocation turn or roll back Seq. I/O and lock errors are ignored
// because PaneTitle must always answer.
func (e Env) healTurnIdle(rec agent, sessionID string, observedAt time.Time) {
	if rec.lease == nil {
		return
	}
	initial, ok, err := e.Store.Turn(rec.Handle)
	if err != nil || !ok || !initial.Incarnation.Same(rec.lease.Incarnation) || initial.Phase != core.TurnWorking {
		return
	}
	if initial.SessionID != "" && initial.SessionID != sessionID {
		return
	}
	if initial.At.After(observedAt) {
		return
	}
	e.healTurnIdleCAS(rec, sessionID, observedAt, initial.Seq)
}

func (e Env) healTurnIdleCAS(rec agent, sessionID string, observedAt time.Time, expectedSeq uint64) {
	if rec.lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = e.Store.UpdateTurn(ctx, rec.Handle, func(prev store.TurnRecord, ok bool) (store.TurnRecord, bool) {
		if !ok || !prev.Incarnation.Same(rec.lease.Incarnation) || prev.Phase != core.TurnWorking || prev.Seq != expectedSeq {
			return store.TurnRecord{}, false
		}
		if prev.SessionID != "" && prev.SessionID != sessionID {
			return store.TurnRecord{}, false
		}
		if prev.At.After(observedAt) {
			return store.TurnRecord{}, false
		}
		prev.Phase = core.TurnIdle
		prev.At = observedAt.UTC()
		prev.SessionID = sessionID
		return prev, true
	})
}

// DirTitle is the window title of a harness the adapter knows nothing about.
func DirTitle(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}
