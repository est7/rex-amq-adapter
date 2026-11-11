package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	EventStopFailure   = "stop-failure"
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
	// Title, when set, is the agent's pane label for its harness to show as
	// its own conversation title (see syncTitle).
	Title string
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
	if kind == core.Claude {
		var shown bool
		if loc, in, shown = e.jobLocation(ctx, event, loc, in); !shown {
			return Reply{}, nil
		}
	}
	// An Antigravity subagent is a conversation of its own inside its
	// parent's process, with hooks of its own: it is not the pane's agent.
	if kind == core.Agy && e.agySubagent(in.SessionID) {
		return Reply{}, nil
	}
	switch event {
	case EventSessionStart, EventPrompt, EventPreInvocation:
		// A new turn or session: the agent can act again.
		e.clearTurnError(ctx, kind, loc.Block, in)
	}
	switch event {
	case EventSessionStart:
		return e.sessionStart(ctx, kind, loc, in)
	case EventPrompt:
		return e.announce(ctx, kind, loc, in, event, core.PromptSubmitted, onPrompt)
	case EventPreInvocation:
		// Antigravity has no prompt hook; its first model call after a
		// prompt is the nearest point, so window notes ride here too.
		return e.announce(ctx, kind, loc, in, event, core.ToolFinished, onModelCall)
	case EventPostTool:
		return e.announce(ctx, kind, loc, in, event, core.ToolFinished, onToolCall)
	case EventStopFailure:
		detail := joinNotices(in.Error, in.ErrorDetails, in.LastAssistantMessage)
		if err := e.reportTurnError(ctx, kind, loc, in, core.NoticeTurnError, detail); err != nil {
			fmt.Fprintf(Diagnostics, "report turn error: %v\n", err)
		}
		if err := e.SuperviseWaker(ctx, loc); err != nil {
			fmt.Fprintf(Diagnostics, "supervise waker: %v\n", err)
		}
		return Reply{}, nil
	case EventStop, EventSettle:
		if code, detail, abnormal := e.abnormalStop(ctx, kind, loc, in); abnormal {
			if code != "" {
				if err := e.reportTurnError(ctx, kind, loc, in, code, detail); err != nil {
					fmt.Fprintf(Diagnostics, "report turn error: %v\n", err)
				}
			}
			if err := e.SuperviseWaker(ctx, loc); err != nil {
				fmt.Fprintf(Diagnostics, "supervise waker: %v\n", err)
			}
			return Reply{}, nil
		}
		e.clearTurnError(ctx, kind, loc.Block, in)
		r, err := e.announce(ctx, kind, loc, in, event, core.TurnEnding, onTurnEnd)
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
		// and bounces wait for its next run instead.
		return e.announce(ctx, kind, loc, in, event, core.PromptSubmitted, onMail)
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

func (e Env) abnormalStop(ctx context.Context, kind core.Kind, loc Location, in HookInput) (code, detail string, abnormal bool) {
	if !in.NormalStop() {
		switch {
		case in.TerminationReason == "USER_CANCELED" || in.Outcome == "aborted":
			return "", "", true
		case in.TerminationReason == "MAX_INVOCATIONS" || in.TerminationReason == "MAX_TOKEN_BUDGET_EXCEEDED" || in.TerminationReason == "MAX_FORCED_INVOCATIONS":
			return core.NoticeTurnLimit, joinNotices(in.TerminationReason, in.Error), true
		case in.TerminationReason != "":
			return core.NoticeTurnError, joinNotices(in.TerminationReason, in.Error), true
		default:
			d := joinNotices(in.ErrorMessage, in.Error)
			if d == "" {
				d = in.Outcome
			}
			return core.NoticeTurnError, d, true
		}
	}
	if kind == core.Codex {
		if detail, ok := e.codexTurnError(ctx, loc, in); ok {
			return core.NoticeTurnError, detail, true
		}
	}
	return "", "", false
}

func (e Env) sessionStart(ctx context.Context, kind core.Kind, loc Location, in HookInput) (Reply, error) {
	a, err := e.adopt(ctx, loc, kind, in)
	if err != nil {
		return Reply{}, err
	}
	r := Reply{Notice: e.identityNotice(ctx, a, a.Unread), Title: e.syncTitle(ctx, kind, EventSessionStart, a.Record, in.SessionID)}
	if kind.InProcess() {
		// The extension delivers waiting mail itself, with a turn of its own,
		// from the first scan of this inbox; marking it announced here would
		// leave an idle agent never told.
		r.Inbox = amq.Inbox(e.Paths.Root(), string(a.Record.Handle))
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

// announce is the in-band fast path: one record read, two directory reads,
// and the ledger. No subprocess runs unless the agent has just sent mail, so
// it is cheap on every tool call.
//
// An agent without a finished adoption is adopted here first. That happens
// when SessionStart never ran the adapter: Codex runs new hooks only after the
// user approves them, which is usually after the session started, and an
// adapter installed mid-session misses SessionStart too. It also finishes an
// adoption that stopped after writing its record. An agent in that state has a
// mailbox, so a failed retry does not silence its mail.
func (e Env) announce(ctx context.Context, kind core.Kind, loc Location, in HookInput, hook string, event core.HookEvent, may announcing) (Reply, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return Reply{}, err
	}
	identity := ""
	f := own
	if ok {
		f = e.recordFit(kind, rec, in)
	}
	if !ok || !rec.Adopted() || f != own {
		// The record's mailbox is this agent's own unless the record belongs
		// to another agent: then a failed adoption leaves it nothing to read.
		hasMailbox := ok && rec.Live() && f != foreign
		a, adopted, err := e.adoptUnlessBackingOff(ctx, kind, loc, in, may)
		switch {
		case adopted:
			rec = a.Record
			if !a.Known {
				identity = e.identityNotice(ctx, a, 0)
			}
		case !hasMailbox:
			return Reply{}, err
		case err != nil:
			fmt.Fprintf(Diagnostics, "finish adopting %s: %v\n", rec.Handle, err)
		}
	}
	var sent []string
	if may.undelivered {
		sent = e.checkableSent(ctx, rec)
	}
	r, err := e.announceMail(ctx, rec, in, event, identity, may.window, sent)
	if err == nil {
		r.Title = e.syncTitle(ctx, kind, hook, rec, in.SessionID)
		// An identity notice carries the request already: it counts as asked.
		if ask := e.askTask(ctx, kind, hook, rec, in.SessionID); identity == "" {
			r.Notice = joinNotices(r.Notice, ask)
		}
	}
	return r, err
}

// adoptUnlessBackingOff adopts the agent in loc. A failing adoption is not
// retried on every tool call: each attempt can take the hook's whole budget.
func (e Env) adoptUnlessBackingOff(ctx context.Context, kind core.Kind, loc Location, in HookInput, may announcing) (Adoption, bool, error) {
	at, failed, err := e.Store.AdoptFailedAt(loc.Block)
	if err != nil || (failed && !may.retry && e.Now().Sub(at) < AdoptRetryAfter) {
		return Adoption{}, false, err
	}
	a, err := e.adopt(ctx, loc, kind, in)
	return a, err == nil, err
}

func (e Env) announceMail(ctx context.Context, rec store.Record, in HookInput, event core.HookEvent, identity string, window bool, sent []string) (Reply, error) {
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
	mail := ""
	if a.Speak {
		if err := e.Store.SetAnnounced(rec.Handle, core.MergeAnnounced(ledger.IDs, unread, unread), now); err != nil {
			return Reply{}, err
		}
		mail = core.HookNotice(a, rec.Handle, e.EnvCommand(), event)
	}
	mail = joinNotices(e.takeUndelivered(rec, sent), mail)
	// A Stop hook never blocks only to state the identity.
	if mail == "" && event == core.TurnEnding {
		return Reply{}, nil
	}
	return e.withWindowNotes(rec.Handle, window, identity, mail)
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
	notice := joinNotices(core.SessionNotice(a.Record.Handle, e.EnvCommand(), unread), core.MatesNotice(a.Mates)) +
		"\n" + core.TaskNotice(a.Record.Handle, a.Record.Role, e.SelfBin+" title", a.Record.Task)
	if brief := core.BriefNotice(e.briefing(ctx, a)); brief != "" {
		notice += "\n\n" + brief
	}
	// After the briefing, so it reads as the word on whom "the human" means;
	// an orchestrator hears this from its own role prompt.
	if a.Record.Role != core.OrchestratorRole {
		if orch := core.OrchestratorNotice(a.Mates); orch != "" {
			notice += "\n\n" + orch
		}
	}
	return notice
}

// briefing finds the agent's crew (named by its Rex tab) and its role
// prompt. Like session-mates it is advice: what cannot be read is reported
// and left out, and the agent keeps its handle.
func (e Env) briefing(ctx context.Context, a Adoption) core.Briefing {
	name := ""
	if e.Crews != nil && a.Session != "" && a.Window != "" {
		s, err := e.Rex.InspectSession(ctx, a.Session)
		if err != nil {
			fmt.Fprintf(Diagnostics, "read the tab of %s: %v\n", a.Record.Handle, err)
		}
		for _, w := range s.Windows {
			if w.ID == a.Window {
				name = crew.Name(w.Label)
			}
		}
	}
	if name != "" && name != a.Record.Crew {
		e.recordCrew(ctx, a.Record.BlockID, a.Record.Handle, name)
	}
	return e.briefingIn(ctx, a.Record, a.Role, name)
}

func (e Env) recordCrew(ctx context.Context, block string, h core.Handle, crewName string) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return
	}
	defer unlock()
	rec, ok, err := e.Store.Record(block)
	if err != nil || !ok || !rec.Live() || rec.Handle != h || rec.Crew == crewName {
		return
	}
	rec.Crew = crewName
	_ = e.Store.PutRecord(rec)
}

// briefingIn is rec's briefing as role in the crew named name ("" for none).
func (e Env) briefingIn(ctx context.Context, rec store.Record, role, name string) core.Briefing {
	b := core.Briefing{Role: role, Crew: name}
	if e.Crews == nil {
		return b
	}
	lib := e.Crews(e.agentDir(ctx, rec))
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

// agentDir is the directory rec's agent works in: where its crews are
// looked up. "" when Rex cannot say.
func (e Env) agentDir(ctx context.Context, rec store.Record) string {
	p, ok, err := e.Rex.Foreground(ctx, rec.BlockID)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the directory of %s: %v\n", rec.Handle, err)
		return ""
	}
	if !ok {
		return ""
	}
	return p.Cwd
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

// announcing is what a hook may do besides announcing mail.
type announcing struct {
	// window: deliver notes about agents joining or leaving the window. Set
	// where the agent takes a prompt, so the news never interrupts a turn.
	window bool
	// retry: adopt again at once after a failed adoption. Set on hooks that
	// run at most once a turn; the frequent ones wait out AdoptRetryAfter so
	// a failing adoption cannot slow every tool call. Without it an agent
	// whose start failed (two agents starting at once can run SessionStart
	// out of time) and whose first turn ends quickly would idle unadopted.
	retry bool
	// undelivered: tell of mail the agent sent that no agent will read. Like
	// window notes it is advice, which must not start a turn of its own.
	undelivered bool
}

var (
	onPrompt    = announcing{window: true, retry: true, undelivered: true}
	onModelCall = announcing{window: true, undelivered: true} // Antigravity's PreInvocation: every model call
	onToolCall  = announcing{undelivered: true}               // PostToolUse
	onMail      = announcing{}                                // Pi's mail watcher
	onTurnEnd   = announcing{retry: true, undelivered: true}  // Stop; Pi's settle
)

// AdoptRetryAfter is how long the frequent hooks wait before adopting again
// after a failed adoption.
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
		// Pi's extension reads {"notice", "inbox", "title"}; an empty object
		// means nothing to deliver.
		out := map[string]string{}
		if r.Notice != "" {
			out["notice"] = r.Notice
		}
		if r.Inbox != "" {
			out["inbox"] = r.Inbox
		}
		if r.Title != "" {
			out["title"] = r.Title
		}
		v = out
	case kind == core.Agy && r.Notice == "":
		return "{}\n", nil
	case r.Notice == "" && r.Title == "":
		return "", nil
	case kind == core.Agy && event == EventStop:
		v = map[string]string{"decision": "continue", "reason": r.Notice}
	case kind == core.Agy && event == EventSessionStart:
		v = map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": r.Notice}}}}
	case kind == core.Agy:
		v = map[string]any{"injectSteps": []any{map[string]string{"ephemeralMessage": r.Notice}}}
	case event == EventSessionStart && r.Title == "":
		return r.Notice + "\n", nil
	case event == EventStop:
		v = map[string]string{"decision": "block", "reason": r.Notice}
	default:
		// Claude Code's sessionTitle names the conversation; only
		// SessionStart and UserPromptSubmit carry one (titleCarried).
		out := map[string]string{"hookEventName": hookEventNames[event]}
		if r.Notice != "" {
			out["additionalContext"] = r.Notice
		}
		if r.Title != "" {
			out["sessionTitle"] = r.Title
		}
		v = map[string]any{"hookSpecificOutput": out}
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
	EventSessionStart: "SessionStart",
	EventPrompt:       "UserPromptSubmit",
	EventPostTool:     "PostToolUse",
}
