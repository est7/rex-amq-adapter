package app

import (
	"context"
	"fmt"
	"path/filepath"

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

// syncTitle returns rec's pane label when this hook can show it as the
// conversation title of session, the one the hook came from, and that
// session was not already given it, and records that it now is. Giving it
// only on a change keeps a title the user set by hand (Claude's /rename)
// until the task changes; a new session under the same handle starts unnamed
// and is given it. The title is advice: a failure is reported and the hook
// goes on without it. Whether the harness then showed it is not known: a
// hook that dies after this, or an extension that cannot apply it, leaves
// the label unshown until it changes.
func (e Env) syncTitle(ctx context.Context, kind core.Kind, event string, rec store.Record, session string) string {
	if !titleCarried(kind, event) {
		return ""
	}
	unlock, err := e.Store.LockLedger(ctx, rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
		return ""
	}
	defer unlock()
	want := core.PaneLabel(rec.Handle, rec.Task)
	shown, err := e.Store.TitlesShown(rec.Handle)
	if err == nil && shown[session] != want {
		shown[session] = want
		if err = e.Store.SetTitlesShown(rec.Handle, shown); err == nil {
			return want
		}
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "title of %s: %v\n", rec.Handle, err)
	}
	return ""
}

// askTask is the request for a task given with the first prompt of a Claude
// Code session whose pane shows no task. Claude Code runs SessionStart, which
// carries the same request, before the user has asked anything, so the agent
// cannot know its task then; Codex and agy run it with the first prompt. Each
// session is asked once, at its first prompt: a session that stays without a
// task (a quick question), or clears the task it started with, is not asked
// again. An unfinished adoption is not asked: its pane is not relabelled yet.
func (e Env) askTask(ctx context.Context, kind core.Kind, event string, rec store.Record, session string) string {
	if kind != core.Claude || event != EventPrompt || session == "" || !rec.Adopted() {
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
			return core.TaskNotice(rec.Handle, e.SelfBin+" title", "")
		}
	}
	if err != nil {
		fmt.Fprintf(Diagnostics, "ask %s for its task: %v\n", rec.Handle, err)
	}
	return ""
}

// PaneTitle is the window title for a harness that asks for one
// (Antigravity's settings.json "title" command, run on every change of the
// agent's state): the pane label of its agent in loc. A block without a live
// agent of that kind whose process this one descends from (agy has no
// SessionEnd, so a new agy finds the last one's record until its first turn),
// or a record that cannot be read, gives the name of the directory it works
// in, so the title never names another agent.
func (e Env) PaneTitle(loc Location, kind core.Kind, cwd string) string {
	if rec, ok, err := e.Store.Record(loc.Block); err == nil && ok && rec.Live() && rec.Kind == kind && e.descendsFrom(rec) {
		return core.PaneLabel(rec.Handle, rec.Task)
	}
	return DirTitle(cwd)
}

// DirTitle is the window title of a harness the adapter knows nothing about.
func DirTitle(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}
