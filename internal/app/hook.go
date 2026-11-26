package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
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
	// A harness that a command started under the pane's live agent (its tool
	// shell's `codex exec`, `claude -p`, `agy -p`, `pi -p`) inherited the
	// pane's REX_BLOCK: none of its hooks speaks for the pane (F14).
	if owner, nested := e.startedUnderOwner(kind, loc); nested {
		if event == EventSessionStart || event == EventSessionEnd {
			fmt.Fprintf(Diagnostics, "ignored %s %s started under %s's agent (pid %d) in its pane (%s)\n", kind, in.SessionID, owner.Handle, owner.pid(), event)
		}
		return Reply{}, nil
	}
	switch event {
	case EventSessionStart, EventPrompt, EventPreInvocation:
		// A new turn or session: the agent can act again.
		e.clearTurnError(kind, loc, in)
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
		e.recordTurnIdleAt(kind, loc, in)
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
			e.recordTurnIdleAt(kind, loc, in)
			if err := e.SuperviseWaker(ctx, loc); err != nil {
				fmt.Fprintf(Diagnostics, "supervise waker: %v\n", err)
			}
			return Reply{}, nil
		}
		e.clearTurnError(kind, loc, in)
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
// Antigravity names the session conversationId. Live probing on agy v1.5.18
// showed that Stop's continuationActive stays false even on executionNum=1
// after a {"decision":"continue"} reply (both -p and interactive -i), so
// treating every agy Stop as already continued remains the safe loop guard:
// Stop blocks only for mail never yet announced and cannot loop.
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
	detail = joinNotices(in.TerminationReason, in.ErrorMessage, in.Error)
	switch core.ClassifyStop(in.TerminationReason, in.Outcome) {
	case core.StopCancelled:
		return "", "", true
	case core.StopLimit:
		return core.NoticeTurnLimit, detail, true
	case core.StopError:
		if detail == "" {
			detail = in.Outcome
		}
		return core.NoticeTurnError, detail, true
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
	if errors.Is(err, ErrStartedInPane) {
		// Not the pane's agent, and nothing is wrong: it gets no identity.
		fmt.Fprintf(Diagnostics, "%v\n", err)
		return Reply{}, nil
	}
	if dup, ok := refusedDuplicate(err); ok {
		// Another pane runs this conversation: this one is told it has no
		// identity, and why.
		return Reply{Notice: dup.Error()}, nil
	}
	var hr *HubRefusal
	if errors.As(err, &hr) {
		// Refused admission (C25): no identity, and the agent hears why.
		return Reply{Notice: hr.Notice}, nil
	}
	if err != nil {
		return Reply{}, err
	}
	r := Reply{Notice: core.StartNotice(e.identityParts(ctx, a, a.Unread)), Title: e.syncTitle(ctx, kind, EventSessionStart, a.Agent, in.SessionID)}
	if kind.InProcess() {
		// The extension delivers waiting mail itself, with a turn of its own,
		// from the first scan of this inbox; marking it announced here would
		// leave an idle agent never told.
		r.Inbox = amq.Inbox(e.Paths.Root(), string(a.Agent.Handle))
		return r, nil
	}
	if a.Unread > 0 {
		unread, err := amq.Unread(e.Paths.Root(), string(a.Agent.Handle))
		if err == nil {
			e.markAnnounced(ctx, e.Paths.Root(), a.Agent.Handle, unread)
		}
	}
	return r, nil
}

// announce is the in-band fast path: one record read, two directory reads,
// the ledger, and for each unread message its header and provenance (the
// arrival gate, admit). No subprocess runs unless the agent has just sent
// mail, so it is cheap on every tool call.
//
// An agent without a finished adoption is adopted here first. That happens
// when SessionStart never ran the adapter: Codex runs new hooks only after the
// user approves them, which is usually after the session started, and an
// adapter installed mid-session misses SessionStart too. It also finishes an
// adoption that stopped after writing its record. An agent in that state has a
// mailbox, so a failed retry does not silence its mail.
func (e Env) announce(ctx context.Context, kind core.Kind, loc Location, in HookInput, hook string, event core.HookEvent, may announcing) (Reply, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return Reply{}, err
	}
	var identity *core.StartParts
	f := own
	if ok {
		f = e.recordFit(kind, rec, in)
	}
	if !ok || !rec.Adopted() || f != own {
		// The record's mailbox is this agent's own unless the record belongs
		// to another agent: then a failed adoption leaves it nothing to read.
		hasMailbox := ok && rec.Live() && f != foreign
		a, adopted, err := e.adoptOnce(ctx, kind, loc, in)
		switch {
		case adopted:
			rec = a.Agent
			if !a.Known {
				p := e.identityParts(ctx, a, 0)
				identity = &p
			}
		case !hasMailbox && errors.Is(err, ErrStartedInPane):
			return Reply{}, nil
		case !hasMailbox && errors.Is(err, ErrHubRefused):
			// Refused admission (C25): told why at each prompt it submits.
			var hr *HubRefusal
			if hook == EventPrompt && errors.As(err, &hr) {
				return Reply{Notice: hr.Notice}, nil
			}
			return Reply{}, nil
		case errors.As(err, new(*DuplicateSessionError)):
			// A refused process is not the record's agent, whatever the
			// record's fit: it reads none of its mail. It hears why at each
			// prompt it submits; any other hook says nothing (a Stop that
			// spoke would keep the turn going).
			dup, _ := refusedDuplicate(err)
			if hook == EventPrompt {
				return Reply{Notice: dup.Error()}, nil
			}
			return Reply{}, nil
		case !hasMailbox:
			return Reply{}, err
		case err != nil:
			fmt.Fprintf(Diagnostics, "finish adopting %s: %v\n", rec.Handle, err)
		}
	}
	if hook == EventPrompt {
		e.touchActivity(rec)
	}
	switch hook {
	case EventPrompt:
		e.recordTurn(rec, core.TurnWorking, in, true)
	case EventPreInvocation:
		forceOpen := in.InvocationNum != nil && *in.InvocationNum == 0
		e.recordTurn(rec, core.TurnWorking, in, forceOpen)
	case EventPostTool:
		e.recordTurn(rec, core.TurnWorking, in, false)
	}
	var sent []string
	if may.undelivered {
		sent = e.checkableSent(ctx, rec)
	}
	// The pane-label request joins the reply after it is composed, so the
	// reply keeps room for it (core.HookReply).
	reserve := 0
	if identity == nil && hook == EventPrompt {
		reserve = core.ContextLen(core.TaskNotice(rec.Handle, rec.Role, e.SelfBin+" agent title", "")) + 1
	}
	r, err := e.announceMail(ctx, rec, in, event, identity, may.window, sent, core.ContextCap-reserve)
	if err == nil {
		r.Title = e.syncTitle(ctx, kind, hook, rec, in.SessionID)
		// An identity notice carries the request already: it counts as asked.
		if ask := e.askTask(ctx, kind, hook, rec, in.SessionID); identity == nil {
			r.Notice = joinNotices(r.Notice, ask)
		}
	}
	// On Stop/Settle, record idle whenever no continuation will be returned,
	// including when mail announcement errors (runHook replies {} on error).
	if (hook == EventStop || hook == EventSettle) && (err != nil || r.Notice == "") {
		e.recordTurn(rec, core.TurnIdle, in, false)
	}
	return r, err
}

// recordTurnIdleAt writes Phase=idle for the pane's live agent when the hook
// passes recordFit != foreign (used on StopFailure and abnormal Stop/settle).
func (e Env) recordTurnIdleAt(kind core.Kind, loc Location, in HookInput) {
	rec, ok, err := e.agentIn(loc)
	if err != nil || !ok || !rec.Live() || e.recordFit(kind, rec, in) == foreign {
		return
	}
	e.recordTurn(rec, core.TurnIdle, in, false)
}

// recordTurn updates rec's hook turn ledger atomically. Seq increments on turn
// open (transitioning from absent/idle/other-incarnation to working, or when
// forceNewTurn signals an explicit new prompt/turn start).
func (e Env) recordTurn(rec agent, phase core.TurnPhase, in HookInput, forceNewTurn bool) {
	if rec.lease == nil {
		return
	}
	inc := rec.lease.Incarnation
	prev, ok, err := e.Store.Turn(rec.Handle)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read turn ledger of %s: %v\n", rec.Handle, err)
		ok = false
	}
	seq := uint64(0)
	if ok {
		seq = prev.Seq
	}
	if phase == core.TurnWorking {
		if !ok || !prev.Incarnation.Same(inc) || prev.Phase != core.TurnWorking || forceNewTurn {
			seq++
		}
	}
	sid := in.SessionID
	if sid == "" {
		sid = rec.Session()
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	if err := e.Store.PutTurn(rec.Handle, store.TurnRecord{
		Seq:         seq,
		Phase:       phase,
		At:          now.UTC(),
		Incarnation: inc,
		SessionID:   sid,
	}); err != nil {
		fmt.Fprintf(Diagnostics, "write turn ledger of %s: %v\n", rec.Handle, err)
	}
}

// adoptOnce adopts the agent in loc, reporting whether it was. Adoption
// holds the lifecycle lock for local file I/O only and asks Rex a fixed
// number of questions, so a failing one is tried again at the next hook.
func (e Env) adoptOnce(ctx context.Context, kind core.Kind, loc Location, in HookInput) (Adoption, bool, error) {
	a, err := e.adopt(ctx, loc, kind, in)
	return a, err == nil, err
}

// announceMail is rec's reply to event: identity's start notice (nil for
// none), its queued window notes when window is set, and its mail notice,
// within budget (withWindowNotes).
func (e Env) announceMail(ctx context.Context, rec agent, in HookInput, event core.HookEvent, identity *core.StartParts, window bool, sent []string, budget int) (Reply, error) {
	// The gate before the ledger lock: its quarantines take the lifecycle
	// lock, which `agent join` holds while it takes a ledger lock.
	unread, err := e.admit(ctx, rec)
	if err != nil {
		return Reply{}, err
	}
	unlock, err := e.Store.LockLedger(ctx, rec.Handle)
	if err != nil {
		return Reply{}, err
	}
	defer unlock()
	ledger, err := e.Store.Announced(rec.Handle)
	if err != nil {
		return Reply{}, err
	}
	unread = e.holdBack(rec, unread, ledger.IDs)
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
		mail = core.HookNotice(a, rec.Handle, e.SelfBin, event)
	}
	mail = joinNotices(e.takeUndelivered(rec, sent), mail)
	// A Stop hook never blocks only to state the identity.
	if mail == "" && event == core.TurnEnding {
		return Reply{}, nil
	}
	return e.withWindowNotes(rec.Handle, window, identity, mail, budget)
}

// holdBack leaves out of unread the messages not yet announced while rec's
// doorbell hold (its .wake.settings, which its waker applies too) still
// holds them, so a hook or Pi's mail watcher tells of routine mail no sooner
// than the waker would ring for it; urgent mail is never held. A hold that
// cannot be read holds nothing: it batches, it guards nothing.
func (e Env) holdBack(rec agent, unread []string, announced map[string]bool) []string {
	root, h := e.Paths.Root(), string(rec.Handle)
	normal, low, err := amq.ReadHolds(root, h)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the doorbell hold of %s: %v\n", h, err)
		return unread
	}
	if normal <= 0 && low <= 0 {
		return unread
	}
	var fresh []core.Waiting
	for _, id := range unread {
		if announced[id] {
			continue
		}
		w := core.Waiting{Priority: core.PriorityUrgent}
		hdr, herr := amq.ReadHeader(root, h, amq.BoxUnread, id)
		info, serr := os.Stat(filepath.Join(amq.Inbox(root, h), id+".md"))
		if herr == nil && serr == nil {
			w = core.Waiting{Priority: hdr.Priority, Arrived: info.ModTime()}
		}
		fresh = append(fresh, w)
	}
	if _, held := core.HoldUntil(fresh, normal, low, e.Now()); !held {
		return unread
	}
	var out []string
	for _, id := range unread {
		if announced[id] {
			out = append(out, id)
		}
	}
	return out
}

// withWindowNotes builds the reply, taking h's queued window notes when
// window is set, the whole of it within budget (core.HookReply).
// Taking them is the last step that can fail, so a hook that fails earlier
// leaves them queued for the next prompt. The caller holds h's ledger lock.
func (e Env) withWindowNotes(h core.Handle, window bool, identity *core.StartParts, mail string, budget int) (Reply, error) {
	var notes []string
	if window {
		var err error
		if notes, err = e.Store.TakeWindowNotes(h); err != nil {
			return Reply{}, err
		}
	}
	return Reply{Notice: core.HookReply(budget, identity, notes, mail)}, nil
}

// identityNotice tells a newly adopted agent who it is, who is beside it,
// and what its crew and role ask of it, within core.StartBudget.
func (e Env) identityNotice(ctx context.Context, a Adoption, unread int) string {
	return core.StartNotice(e.identityParts(ctx, a, unread))
}

// identityParts are the parts of a's start notice, for the allocator to
// compose (core.StartNotice, core.HookReply).
func (e Env) identityParts(ctx context.Context, a Adoption, unread int) core.StartParts {
	mates := core.MatesNotice(a.Mates)
	if core.Stealth(a.Agent.Role) {
		// A narrator never mails a worker, and a personal agent only a
		// linked narrator: neither is told to reach its session's agents.
		mates = ""
	}
	// After the briefing, so it reads as the word on whom "the human" means;
	// an orchestrator hears this from its own role prompt. The Goal and
	// Grant lines end it, where a lead's brief tells it to look.
	var tail []string
	if a.Agent.Role != core.OrchestratorRole && !core.Stealth(a.Agent.Role) {
		tail = append(tail, core.OrchestratorNotice(a.Mates))
	}
	tail = append(tail, e.obligationNotice(a.Agent))
	g, lead := e.leadGrant(a.Agent)
	var receipt *core.GrantReceipt
	if lead {
		r := e.grantReceipt(a.Agent.SessionID, g)
		receipt = &r
		tail = append(tail, core.GrantNotice(g.Contract, e.Paths.GrantPath(a.Agent.SessionID), g.RelayedBy, g.RelayedRole))
	}
	var kept []string
	for _, t := range tail {
		if t != "" {
			kept = append(kept, t)
		}
	}
	return core.StartParts{
		Head:     core.SessionNotice(a.Agent.Handle, a.Agent.Role, e.SelfBin, unread),
		Mates:    mates,
		Task:     core.TaskNotice(a.Agent.Handle, a.Agent.Role, e.SelfBin+" agent title", a.Agent.Task),
		Briefing: e.briefing(ctx, a),
		Receipt:  receipt,
		Tail:     strings.Join(kept, "\n\n"),
	}
}

// obligationNotice is the line a personal agent's start notice adds when
// Epics linked to its Hub hold a hand-back it has not closed them on (their
// lifecycles, whatever mail was read); "" otherwise. Like the briefing it is
// advice: what cannot be read is reported and left out.
func (e Env) obligationNotice(rec agent) string {
	if rec.Role != core.PersonalRole {
		return ""
	}
	me, err := e.gatewayEndpoint(rec)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the Epics of %s: %v\n", rec.Handle, err)
		return ""
	}
	links, err := e.linkedEpics(me)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the Epics of %s: %v\n", rec.Handle, err)
		return ""
	}
	n := 0
	for _, l := range links {
		if lc, ok, err := e.Store.EpicLifecycle(l.Epic.SessionID); err == nil && ok && lc.State == store.EpicHandBack {
			n++
		}
	}
	return core.ObligationNotice(n)
}

// grantNotice is the Goal and Grant lines of rec's start notice when rec
// leads an Epic that has a notarized grant (sessionLead); "" for every other
// agent.
func (e Env) grantNotice(rec agent) string {
	g, ok := e.leadGrant(rec)
	if !ok {
		return ""
	}
	return core.GrantNotice(g.Contract, e.Paths.GrantPath(rec.SessionID), g.RelayedBy, g.RelayedRole)
}

// leadGrant is the stored grant of rec's session when rec leads it
// (sessionLead); ok is false for every other agent and a session with no
// grant. It reads the store only, never mail. Like the briefing it is
// advice: what cannot be read is reported and left out.
func (e Env) leadGrant(rec agent) (store.Grant, bool) {
	lead, err := e.sessionLead(rec)
	if err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	if !lead {
		return store.Grant{}, false
	}
	g, ok, err := e.Store.Grant(rec.SessionID)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the grant of %s: %v\n", rec.SessionID, err)
	}
	return g, ok
}

// grantReceipt is the receipt of grant g of session, its contract checked
// against the bytes it has now. A grant that names no contract has nothing
// to check.
func (e Env) grantReceipt(session string, g store.Grant) core.GrantReceipt {
	ok := g.Contract == ""
	if !ok {
		ok, _ = contractCheck(g)
	}
	return core.GrantReceipt{
		SessionID: session, RelayedBy: g.RelayedBy, RelayedRole: g.RelayedRole, RelayedAt: g.RelayedAt,
		GrantPath: e.Paths.GrantPath(session), ContractPath: g.Contract, ContractSHA256: g.ContractSHA256,
		ContractOK: ok, Asked: g.Asked,
	}
}

// briefing finds the agent's crew (its Rex session's, sessionCrew) and its
// role prompt. Like session-mates it is advice: what cannot be read is
// reported and left out, and the agent keeps its handle.
func (e Env) briefing(ctx context.Context, a Adoption) core.Briefing {
	return e.briefingIn(ctx, a.Agent, a.Role, e.sessionCrew(a.Session, a.Role))
}

// briefingIn is rec's briefing as role in the crew named name ("" for none).
func (e Env) briefingIn(ctx context.Context, rec agent, role, name string) core.Briefing {
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
func (e Env) agentDir(ctx context.Context, rec agent) string {
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
	// undelivered: tell of mail the agent sent that no agent will read. Like
	// window notes it is advice, which must not start a turn of its own.
	undelivered bool
}

var (
	onPrompt    = announcing{window: true, undelivered: true}
	onModelCall = announcing{window: true, undelivered: true} // Antigravity's PreInvocation: every model call
	onToolCall  = announcing{undelivered: true}               // PostToolUse
	onMail      = announcing{}                                // Pi's mail watcher
	onTurnEnd   = announcing{undelivered: true}               // Stop; Pi's settle
)

// adopt runs Adopt for a hook and reports a label that could not be set.
// Collection is not its business: the session watcher and agent collect
// run it, and a resumed session takes its handle back from a lease whose
// holder is gone (holderGone), so no hook waits for every record on the host.
func (e Env) adopt(ctx context.Context, loc Location, kind core.Kind, in HookInput) (Adoption, error) {
	a, err := e.Adopt(ctx, loc, kind, in)
	if err != nil {
		return Adoption{}, err
	}
	if a.LabelErr != nil {
		fmt.Fprintf(Diagnostics, "show handle %s as the pane label: %v\n", a.Agent.Handle, a.LabelErr)
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
