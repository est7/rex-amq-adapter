package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/crew"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Hook event names, as installed on the command line.
const (
	EventSessionStart = "session-start"
	EventPrompt       = "user-prompt-submit"
	EventPostTool     = "post-tool-use"
	// EventPreInvocation is Antigravity's hook before each model call.
	EventPreInvocation = "pre-invocation"
	EventStop          = "stop"
	EventSessionEnd    = "session-end"
	// EventMail and EventSettle come from Pi's extension: its inbox watcher
	// saw mail, and a run is about to settle (agent_before_settle).
	EventMail   = "mail"
	EventSettle = "settle"
)

// Reply is what a hook tells the agent. Notice, when set, is added to the
// agent's context; on a Stop hook it keeps the turn going with Notice as the
// reason. Render turns it into the harness's output format.
type Reply struct {
	Notice string
	// Inbox is the directory an in-process integration watches for mail;
	// set on session-start for such agents.
	Inbox string
}

// Hook runs one agent hook.
func (e Env) Hook(ctx context.Context, event string, kind core.Kind, loc Location, in HookInput) (Reply, error) {
	in = normalize(kind, in)
	// A session with no transcript cannot establish ownership of this pane.
	// In particular, Codex memory workers inherit Rex's environment and PID
	// but are ephemeral. Decide from the payload before touching shared state,
	// independent of which thread starts first or whether startup was missed.
	// Workaround for https://github.com/openai/codex/issues/52442.
	// Remove only after supported Codex versions suppress internal-worker hooks
	// or expose a stable worker discriminator; validate interactive switches too.
	if kind == core.Codex && bytes.Equal(bytes.TrimSpace(in.TranscriptPath), []byte("null")) {
		if event == EventSessionStart || event == EventSessionEnd {
			fmt.Fprintf(Diagnostics, "ignored Codex session without transcript %s (%s)\n", in.SessionID, event)
		}
		return Reply{}, nil
	}
	switch event {
	case EventSessionStart:
		return e.sessionStart(ctx, kind, loc, in)
	case EventPrompt:
		return e.announce(ctx, kind, loc, in, core.PromptSubmitted, true)
	case EventPreInvocation:
		// Antigravity has no prompt hook; its first model call after a
		// prompt is the nearest point, so window notes ride here too.
		return e.announce(ctx, kind, loc, in, core.ToolFinished, true)
	case EventPostTool:
		return e.announce(ctx, kind, loc, in, core.ToolFinished, false)
	case EventStop:
		if !in.NormalStop() {
			return Reply{}, nil
		}
		r, err := e.announce(ctx, kind, loc, in, core.TurnEnding, false)
		if err != nil {
			return r, err
		}
		// Supervision is housekeeping: its failure must not change the reply.
		if err := e.SuperviseWaker(ctx, loc); err != nil {
			fmt.Fprintf(Diagnostics, "supervise waker: %v\n", err)
		}
		return r, nil
	case EventSessionEnd:
		return Reply{}, e.Depart(ctx, loc, in)
	case EventMail:
		// Pi's watcher turns a notice into a turn of its own; window notes
		// wait for its next prompt instead.
		return e.announce(ctx, kind, loc, in, core.PromptSubmitted, false)
	case EventSettle:
		return e.announce(ctx, kind, loc, in, core.TurnEnding, false)
	}
	return Reply{}, fmt.Errorf("unknown hook event %q", event)
}

// normalize maps a harness's payload onto the fields the adapter reads.
// Antigravity names the session conversationId and reports no
// stop_hook_active; treating every agy stop as already continued means a
// Stop blocks only for mail that was never announced, so it cannot loop.
func normalize(kind core.Kind, in HookInput) HookInput {
	if kind == core.Agy {
		if in.SessionID == "" {
			in.SessionID = in.ConversationID
		}
		in.StopHookActive = true
	}
	return in
}

func (e Env) sessionStart(ctx context.Context, kind core.Kind, loc Location, in HookInput) (Reply, error) {
	a, err := e.adopt(ctx, loc, kind, in)
	if err != nil {
		return Reply{}, err
	}
	r := Reply{Notice: e.identityNotice(ctx, a, a.Unread)}
	if kind.InProcess() {
		// The extension delivers waiting mail itself, with a turn of its own,
		// from the first scan of this inbox; marking it announced here would
		// leave an idle agent never told.
		r.Inbox = filepath.Join(e.Paths.Root(), "agents", string(a.Record.Handle), "inbox", "new")
		return r, nil
	}
	if a.Unread > 0 {
		unread, err := amq.Unread(e.Paths.Root(), string(a.Record.Handle))
		if err == nil {
			e.markAnnounced(ctx, e.Paths.Root(), a.Record.Handle, unread)
		}
	}
	return r, nil
}

// announce is the in-band fast path: one record read, one directory read,
// and the ledger. No subprocess runs, so it is cheap on every tool call.
//
// An agent without a live record is adopted here first. That happens when
// SessionStart never ran the adapter: Codex runs new hooks only after the
// user approves them, which is usually after the session started, and an
// adapter installed mid-session misses SessionStart too.
//
// window is set on the hooks that run as the agent takes a prompt: they also
// deliver notes that an agent started beside it.
func (e Env) announce(ctx context.Context, kind core.Kind, loc Location, in HookInput, event core.HookEvent, window bool) (Reply, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return Reply{}, err
	}
	identity := ""
	if !ok || !rec.Live() {
		// A failing adoption is not retried on every tool call: each
		// attempt can take the hook's whole budget.
		if at, failed, err := e.Store.AdoptFailedAt(loc.Block); err != nil || (failed && e.Now().Sub(at) < AdoptRetryAfter) {
			return Reply{}, err
		}
		a, err := e.adopt(ctx, loc, kind, in)
		if err != nil {
			return Reply{}, err
		}
		rec = a.Record
		if !a.Known {
			identity = e.identityNotice(ctx, a, 0)
		}
	}
	return e.announceMail(ctx, rec, in, event, identity, window)
}

func (e Env) announceMail(ctx context.Context, rec store.Record, in HookInput, event core.HookEvent, identity string, window bool) (Reply, error) {
	unlock, err := e.Store.LockLedger(ctx, rec.Handle)
	if err != nil {
		return Reply{}, err
	}
	defer unlock()
	ledger, err := e.Store.Announced(rec.Handle)
	if err != nil {
		return Reply{}, err
	}
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return Reply{}, err
	}
	now := e.Now()
	in2 := core.AnnounceInput{Event: event, Unread: unread, Announced: ledger.IDs, StopHookActive: in.StopHookActive, Idle: in.Idle}
	if !ledger.At.IsZero() {
		in2.Since = now.Sub(ledger.At)
	}
	a := core.DecideAnnounce(in2)
	if !a.Speak {
		// A Stop hook never blocks only to state the identity.
		if event == core.TurnEnding {
			return Reply{}, nil
		}
		return e.withWindowNotes(rec.Handle, window, identity, "")
	}
	if err := e.Store.SetAnnounced(rec.Handle, core.MergeAnnounced(ledger.IDs, unread, unread), now); err != nil {
		return Reply{}, err
	}
	return e.withWindowNotes(rec.Handle, window, identity, core.HookNotice(a, rec.Handle, e.EnvCommand(), event))
}

// withWindowNotes builds the reply, taking h's queued window notes when
// window is set. Taking them is the last step that can fail, so a hook that
// fails earlier leaves them queued for the next prompt. The caller holds h's
// ledger lock.
func (e Env) withWindowNotes(h core.Handle, window bool, identity, mail string) (Reply, error) {
	var notes []string
	if window {
		var err error
		if notes, err = e.Store.TakeWindowNotes(h); err != nil {
			return Reply{}, err
		}
	}
	return Reply{Notice: joinNotices(append(append([]string{identity}, notes...), mail)...)}, nil
}

// identityNotice tells a newly adopted agent who it is, who is beside it,
// and what its crew and role ask of it.
func (e Env) identityNotice(ctx context.Context, a Adoption, unread int) string {
	notice := joinNotices(core.SessionNotice(a.Record.Handle, e.EnvCommand(), unread), core.MatesNotice(a.Mates))
	if brief := core.BriefNotice(e.briefing(ctx, a)); brief != "" {
		notice += "\n\n" + brief
	}
	return notice
}

// briefing finds the agent's crew (named by its Rex tab) and its role
// prompt. Like window-mates it is advice: what cannot be read is reported
// and left out, and the agent keeps its handle.
func (e Env) briefing(ctx context.Context, a Adoption) core.Briefing {
	b := core.Briefing{Role: a.Role}
	if e.Crews == nil {
		return b
	}
	if a.Session != "" && a.Window != "" {
		s, err := e.Rex.InspectSession(ctx, a.Session)
		if err != nil {
			fmt.Fprintf(Diagnostics, "read the tab of %s: %v\n", a.Record.Handle, err)
		}
		for _, w := range s.Windows {
			if w.ID == a.Window {
				b.Crew = crew.Name(w.Label)
			}
		}
	}
	dir := ""
	if p, ok, err := e.Rex.Foreground(ctx, a.Record.BlockID); err != nil {
		fmt.Fprintf(Diagnostics, "read the directory of %s: %v\n", a.Record.Handle, err)
	} else if ok {
		dir = p.Cwd
	}
	lib := e.Crews(dir)
	if p, ok, err := lib.Conventions(b.Crew); err != nil {
		fmt.Fprintf(Diagnostics, "read the conventions of crew %s: %v\n", b.Crew, err)
	} else if ok {
		b.Conventions, b.ConventionsPath = p.Text, p.Path
	}
	if p, ok, err := lib.Role(b.Crew, b.Role); err != nil {
		fmt.Fprintf(Diagnostics, "read the prompt of role %s: %v\n", b.Role, err)
	} else if ok {
		b.RolePrompt, b.RolePath = p.Text, p.Path
	}
	return b
}

// joinNotices joins the non-empty notices into one line.
func joinNotices(notices ...string) string {
	var out []string
	for _, n := range notices {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// AdoptRetryAfter is how long hooks wait before adopting again after a
// failed adoption.
const AdoptRetryAfter = time.Minute

// adopt runs Adopt for a hook: it collects first, so a resumed session finds
// the record of the pane it left parked and takes its handle back; it records
// a failure so the fast-path hooks back off, clears it on success, and
// reports a label that could not be set.
func (e Env) adopt(ctx context.Context, loc Location, kind core.Kind, in HookInput) (Adoption, error) {
	if err := e.Collect(ctx); err != nil {
		// Collection is housekeeping; adoption must still happen.
		fmt.Fprintf(Diagnostics, "collect: %v\n", err)
	}
	a, err := e.Adopt(ctx, loc, kind, in)
	if err != nil {
		_ = e.Store.SetAdoptFailed(loc.Block, e.Now())
		return Adoption{}, err
	}
	_ = e.Store.ClearAdoptFailed(loc.Block)
	if a.LabelErr != nil {
		fmt.Fprintf(Diagnostics, "show handle %s as the pane label: %v\n", a.Record.Handle, a.LabelErr)
	}
	return a, nil
}

// Render writes r in the output format of kind's hook for event.
//
// Claude Code and Codex: SessionStart prints plain text, which becomes
// context; other hooks add context through hookSpecificOutput; Stop blocks
// with a reason; nothing to say prints nothing.
//
// Pi's extension reads {"notice", "inbox"} and delivers through its API.
//
// Antigravity always reads a JSON object: SessionStart and PreInvocation
// inject steps (a persistent system message for the identity, an ephemeral
// one for mail); Stop continues with a reason; nothing to say is {}.
func Render(kind core.Kind, event string, r Reply) (string, error) {
	var v any
	switch {
	case kind == core.Pi:
		// Pi's extension reads {"notice", "inbox"}; an empty object means
		// nothing to deliver.
		out := map[string]string{}
		if r.Notice != "" {
			out["notice"] = r.Notice
		}
		if r.Inbox != "" {
			out["inbox"] = r.Inbox
		}
		v = out
	case kind == core.Agy && r.Notice == "":
		return "{}\n", nil
	case r.Notice == "":
		return "", nil
	case kind == core.Agy && event == EventStop:
		v = map[string]string{"decision": "continue", "reason": r.Notice}
	case kind == core.Agy && event == EventSessionStart:
		v = map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": r.Notice}}}}
	case kind == core.Agy:
		v = map[string]any{"injectSteps": []any{map[string]string{"ephemeralMessage": r.Notice}}}
	case event == EventSessionStart:
		return r.Notice + "\n", nil
	case event == EventStop:
		v = map[string]string{"decision": "block", "reason": r.Notice}
	default:
		v = map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": hookEventNames[event], "additionalContext": r.Notice}}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// hookEventNames are the Claude Code and Codex names hookSpecificOutput
// must carry.
var hookEventNames = map[string]string{
	EventPrompt:   "UserPromptSubmit",
	EventPostTool: "PostToolUse",
}
