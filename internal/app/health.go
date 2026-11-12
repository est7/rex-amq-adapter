package app

import (
	"context"
	"fmt"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/transcript"
)

// The watchdog's second look at every live agent of an orchestrator session,
// mail or no mail: the agent's own transcript, for what no hook reports
// because the turn has not ended: a tool call with no result (hung) and a
// working agent whose transcript stopped growing (stalled). A turn that ends
// in an error or a limit is reported by its stop hook (reportTurnError), with
// the harness's own reason. Only the harness's structural flags are read
// (core.Assess); the rows go to the orchestrator verbatim, as quoted data, and
// it decides what they mean. Each finding is told once (store.HealthMark).

func (e Env) health(ctx context.Context, orch store.Record, s Session, rec store.Record) error {
	tail, ok, err := transcript.Read(ctx, e.harnessDirs(), rec.Kind, rec.AgentSessionID, core.TailRows)
	if err != nil || !ok {
		return err
	}
	working := false
	if st, ok, err := e.Rex.ProgramStatus(ctx, rec.BlockID); err == nil && ok {
		working = core.AgentState(st.State) == core.StateWorking
	}
	h := core.Assess(tail, working, e.Now())
	if h.Hung == nil && !h.Stalled {
		return nil
	}
	screen := e.screenLines(ctx, rec)
	desktop, err := e.tellFindings(ctx, orch, s, rec, tail, h, screen)
	for _, text := range desktop {
		e.desktop("rex-amq-adapter", text)
	}
	return err
}

// tellFindings tells each finding of h not yet told about rec's agent run,
// and records it only once it is sent, so a failed send is tried again on
// the next poll. Rex is read before and the desktop told after, never under
// LockHealth; it returns what the desktop should hear.
func (e Env) tellFindings(ctx context.Context, orch store.Record, s Session, rec store.Record, tail core.Tail, h core.Health, screen []string) (desktop []string, err error) {
	unlock, err := e.Store.LockHealth(ctx, rec.BlockID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	mark, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return nil, err
	}
	if key := turnKey(rec); mark.HealthBy != key {
		// Another run's findings: this run starts clean.
		mark.HealthBy, mark.LastHungID, mark.LastStallAt = key, "", time.Time{}
	}
	if h.Hung != nil && h.Hung.ToolID != mark.LastHungID {
		text, err := e.healthNotice(ctx, orch, s, rec, core.NoticeToolHung, tail, h, screen)
		if err != nil {
			return desktop, err
		}
		desktop = append(desktop, text...)
		mark.LastHungID = h.Hung.ToolID
		if err := e.Store.PutHealthMark(rec.BlockID, mark); err != nil {
			return desktop, err
		}
	}
	if h.Stalled && !tail.LastAt.Equal(mark.LastStallAt) {
		text, err := e.healthNotice(ctx, orch, s, rec, core.NoticeStalled, tail, h, screen)
		if err != nil {
			return desktop, err
		}
		desktop = append(desktop, text...)
		mark.LastStallAt = tail.LastAt
		if err := e.Store.PutHealthMark(rec.BlockID, mark); err != nil {
			return desktop, err
		}
	}
	return desktop, nil
}

func (e Env) screenLines(ctx context.Context, rec store.Record) []string {
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return nil
	}
	return lastLines(vt, core.ReportLines)
}

// healthNotice mails the finding in rec's name to the orchestrator; a finding
// about the orchestrator itself goes to a live agent of another model family,
// or, when there is none, to the user's desktop: it returns that text for the
// caller to show once it holds no lock.
func (e Env) healthNotice(ctx context.Context, orch store.Record, s Session, rec store.Record, code string, tail core.Tail, h core.Health, screen []string) (desktop []string, err error) {
	subject, body := core.HealthNotice(peerOf(rec), code, tail, h, screen, e.Now())
	to, ok := e.findingRecipient(s, rec, orch)
	if !ok {
		e.wdLog(orch, rec, "notice-desktop", "", code+": no other family in the session")
		return []string{string(rec.Handle) + ": " + code + ", and no agent of another model family is in the session"}, nil
	}
	if to != orch.Handle {
		body += "\n\nSent to you because the finding is about the orchestrator " + string(orch.Handle) + " itself."
	}
	if err := e.sendSystem(ctx, rec.Handle, to, subject, body); err != nil {
		return nil, err
	}
	e.wdLog(orch, rec, "notice", "", fmt.Sprintf("%s -> %s (%d rows)", code, to, len(tail.Rows)))
	return nil, nil
}

// turnKey names one agent run in a pane: a new agent in the same pane, or a
// new session of the same agent, does not inherit its predecessor's mark.
func turnKey(rec store.Record) string {
	return string(rec.Handle) + "/" + rec.AgentSessionID
}

// flagged reports whether rec's own last turn ended in an error recently
// enough that it may still be unable to act (a usage limit on its account);
// its next turn, or core.StalledFor, ends that.
func (e Env) flagged(rec store.Record) bool {
	te, ok, err := e.Store.TurnError(rec.BlockID)
	return err == nil && ok && te.By == turnKey(rec) && e.Now().Sub(te.At) < core.StalledFor
}

func (e Env) readHookTail(ctx context.Context, kind core.Kind, in HookInput, rec store.Record) (core.Tail, bool) {
	if path, err := in.transcript(); err == nil && path != "" {
		if t, ok, err := transcript.ReadPath(ctx, path, kind, core.TailRows); err == nil && ok {
			return t, true
		}
	}
	sid := in.SessionID
	if sid == "" {
		sid = rec.AgentSessionID
	}
	t, ok, err := transcript.Read(ctx, e.harnessDirs(), kind, sid, core.TailRows)
	if err != nil {
		return core.Tail{}, false
	}
	return t, ok
}

// codexTurnError reports whether a Codex Stop hook is ending an errored turn.
// Codex runs Stop on normal completion and on a turn error alike, after
// writing the error row to its rollout; an error it recovered from within
// the turn is followed by more rows, so only a last row that is the turn's
// own error counts.
func (e Env) codexTurnError(ctx context.Context, loc Location, in HookInput) (detail string, ok bool) {
	rec, has, _ := e.Store.Record(loc.Block)
	if has && e.recordFit(core.Codex, rec, in) == foreign {
		rec = store.Record{} // another agent's rollout is not this hook's
	}
	tail, found := e.readHookTail(ctx, core.Codex, in, rec)
	if !found || len(tail.Rows) == 0 {
		return "", false
	}
	last := tail.Rows[len(tail.Rows)-1]
	if !last.Error || last.Kind != "error" {
		return "", false
	}
	if in.TurnID != "" && last.ToolID != "" && last.ToolID != in.TurnID {
		return "", false
	}
	return last.Text, true
}

// clearTurnError ends the pane agent's error mark when a new turn of it
// starts or ends normally, touching disk only when one is set. A hook from
// another agent (a `claude -p` the pane's agent runs, a Codex thread of a
// shared daemon) leaves it alone.
func (e Env) clearTurnError(kind core.Kind, block string, in HookInput) {
	if _, ok, err := e.Store.TurnError(block); err != nil || !ok {
		return
	}
	if rec, ok, err := e.Store.Record(block); err != nil || !ok || e.recordFit(kind, rec, in) == foreign {
		return
	}
	_ = e.Store.ClearTurnError(block)
}

// reportTurnError tells the agent that should act (core.ErrorRecipient) that
// rec's turn ended in an error or hit a limit, with the harness's own reason
// and the last transcript rows. A stop hook runs once per turn, so it is
// reported once; when the mail cannot be sent, the user's desktop hears.
func (e Env) reportTurnError(ctx context.Context, kind core.Kind, loc Location, in HookInput, code, detail string) error {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return err
	}
	// The pane's agent only, as announce decides it: a hook from another
	// agent that cannot be adopted here reports nothing in its name.
	f := own
	if ok {
		f = e.recordFit(kind, rec, in)
	}
	if !ok || !rec.Adopted() || f != own {
		a, adopted, _ := e.adoptUnlessBackingOff(ctx, kind, loc, in, onTurnEnd)
		switch {
		case adopted:
			rec, ok = a.Record, true
		case f == foreign:
			return nil
		}
	}
	if !ok || !rec.Live() {
		return nil
	}
	tail, _ := e.readHookTail(ctx, kind, in, rec)
	if detail == "" {
		if h := core.Assess(tail, false, e.Now()); len(h.Errors) > 0 {
			detail = h.Errors[len(h.Errors)-1].Text
		}
	}
	summary := string(rec.Handle) + ": " + code
	if detail != "" {
		summary += ": " + detail
	}
	if code == core.NoticeTurnError {
		if err := e.markTurnError(rec); err != nil {
			// Unmarked, the agent would be routed further error notices
			// though it may be unable to act: the user hears instead.
			e.desktop("rex-amq-adapter", summary+" (turn-error flag failed: "+err.Error()+")")
			return err
		}
	}
	peers, byHandle, crewLead := e.sessionPeersForError(rec)
	to, why, found := core.ErrorRecipient(peerOf(rec), peers, crewLead, code, func(h core.Handle) bool {
		cand, ok := byHandle[h]
		return ok && e.flagged(cand)
	})
	if !found {
		e.desktop("rex-amq-adapter", summary)
		return nil
	}
	subject, body := core.ErrorNotice(peerOf(rec), code, detail, why, tail)
	if err := e.sendSystem(ctx, rec.Handle, to.Handle, subject, body); err != nil {
		e.desktop("rex-amq-adapter", summary+" (mail to "+string(to.Handle)+" failed)")
		return err
	}
	return nil
}

// markTurnError records that rec's turn ended in an error, for flagged.
func (e Env) markTurnError(rec store.Record) error {
	return e.Store.PutTurnError(rec.BlockID, store.TurnError{At: e.Now().UTC(), By: turnKey(rec)})
}

// sessionPeersForError reads only stored records (no Rex IPC inside the stop
// hook budget): peers in rec's session via together(), and rec's crew lead from
// rec.Crew and rec.Cwd. A Stealth agent (core.Stealth) is never told of
// another's error, and its own goes to the desktop: no worker hears of it.
func (e Env) sessionPeersForError(rec store.Record) ([]core.Peer, map[core.Handle]store.Record, string) {
	if core.Stealth(rec.Role) {
		return nil, nil, ""
	}
	recs, err := e.Store.Records()
	if err != nil {
		return nil, nil, ""
	}
	byHandle := make(map[core.Handle]store.Record, len(recs))
	var peers []core.Peer
	for _, r := range recs {
		if !r.Live() || r.Handle == rec.Handle || !together(r, rec) || core.Stealth(r.Role) {
			continue
		}
		byHandle[r.Handle] = r
		peers = append(peers, peerOf(r))
	}
	crewLead := ""
	if rec.Crew != "" && e.Crews != nil {
		if c, ok, err := e.Crews(rec.Cwd).Crew(rec.Crew); err == nil && ok {
			crewLead = c.Lead
		}
	}
	return peers, byHandle, crewLead
}

// findingRecipient is who hears a watchdog finding about rec: the
// orchestrator, or, for a finding about the orchestrator itself, a live agent
// of another model family in s. ok is false when the finding is about the
// orchestrator and no such agent exists: the caller tells the desktop.
func (e Env) findingRecipient(s Session, rec, orch store.Record) (to core.Handle, ok bool) {
	if rec.Handle != orch.Handle {
		return orch.Handle, true
	}
	other, ok := e.otherFamily(s, rec, orch)
	return other.Handle, ok
}

// otherFamily finds a live agent in s of a model family other than rec's and
// the orchestrator's, whose own last turn did not end in an error. A Stealth
// agent never takes over a stalled goal.
func (e Env) otherFamily(s Session, rec, orch store.Record) (store.Record, bool) {
	recs, err := e.Store.Records()
	if err != nil {
		return store.Record{}, false
	}
	for _, r := range recs {
		if r.Live() && s.holds(r) && !core.Stealth(r.Role) && r.Kind != rec.Kind && r.Kind != orch.Kind && r.Handle != rec.Handle && !e.flagged(r) {
			return r, true
		}
	}
	return store.Record{}, false
}

// sendSystem mails a report or notice in from's name as system mail
// (amq.SystemLabel), which the bounce check never reads as from's own.
func (e Env) sendSystem(ctx context.Context, from, to core.Handle, subject, body string) error {
	return e.sendAs(ctx, from, to, true, "--kind", "status", "--priority", "normal", "--labels", amq.SystemLabel, "--subject", subject, "--body", body)
}

// desktop reaches the user's desktop through Env.Notify.
func (e Env) desktop(title, text string) {
	if e.Notify != nil {
		e.Notify(title, text)
		return
	}
	notifyDesktop(title, text)
}
