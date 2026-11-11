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
	tail, ok, err := transcript.Read(ctx, e.TranscriptHome, rec.Kind, rec.AgentSessionID, core.TailRows)
	if err != nil || !ok {
		return err
	}
	working := false
	if st, ok, err := e.Rex.ProgramStatus(ctx, rec.BlockID); err == nil && ok {
		working = core.AgentState(st.State) == core.StateWorking
	}
	now := e.Now()
	h := core.Assess(tail, working, now)
	unlock, err := e.Store.LockHealth(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	defer unlock()
	mark, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return err
	}
	if h.Hung != nil && h.Hung.ID != mark.LastHungID {
		mark.LastHungID = h.Hung.ID
		if err := e.healthNotice(ctx, orch, s, rec, core.NoticeToolHung, tail, h, e.screenLines(ctx, rec)); err != nil {
			return err
		}
	}
	if h.Stalled && !tail.LastAt.Equal(mark.LastStallAt) {
		mark.LastStallAt = tail.LastAt
		if err := e.healthNotice(ctx, orch, s, rec, core.NoticeStalled, tail, h, e.screenLines(ctx, rec)); err != nil {
			return err
		}
	}
	return e.Store.PutHealthMark(rec.BlockID, mark)
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
// or to the user's desktop when there is none.
func (e Env) healthNotice(ctx context.Context, orch store.Record, s Session, rec store.Record, code string, tail core.Tail, h core.Health, screen []string) error {
	subject, body := core.HealthNotice(peerOf(rec), code, tail, h, screen, e.Now())
	to := orch.Handle
	if rec.Handle == orch.Handle {
		other, ok := e.otherFamily(s, rec, orch)
		if !ok {
			e.desktop("rex-amq-adapter", string(rec.Handle)+": "+code+", and no agent of another model family is in the session")
			e.wdLog(orch, rec, "notice-desktop", "", code+": no other family in the session")
			return nil
		}
		to = other.Handle
		body += "\n\nSent to you because the finding is about the orchestrator " + string(orch.Handle) + " itself."
	}
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(to), subject, body); err != nil {
		return err
	}
	e.seedSentCursor(rec.Handle)
	e.wdLog(orch, rec, "notice", "", fmt.Sprintf("%s -> %s (%d rows)", code, to, len(tail.Rows)))
	return nil
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
	m, err := e.Store.HealthMark(rec.BlockID)
	return err == nil && !m.TurnErroredAt.IsZero() && m.TurnErroredBy == turnKey(rec) && e.Now().Sub(m.TurnErroredAt) < core.StalledFor
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
	t, ok, err := transcript.Read(ctx, e.TranscriptHome, kind, sid, core.TailRows)
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
	rec, _, _ := e.Store.Record(loc.Block)
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

// clearTurnError ends the agent's error mark when a new turn starts or a turn
// ends normally, touching disk only when one is set.
func (e Env) clearTurnError(ctx context.Context, block string) {
	if m, err := e.Store.HealthMark(block); err != nil || m.TurnErroredAt.IsZero() {
		return
	}
	unlock, err := e.Store.LockHealth(ctx, block)
	if err != nil {
		return
	}
	defer unlock()
	m, err := e.Store.HealthMark(block)
	if err != nil || m.TurnErroredAt.IsZero() {
		return
	}
	m.TurnErroredAt, m.TurnErroredBy = time.Time{}, ""
	_ = e.Store.PutHealthMark(block, m)
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
	if !ok || !rec.Adopted() || rec.Kind != kind || (in.SessionID != "" && rec.AgentSessionID != "" && in.SessionID != rec.AgentSessionID) {
		if a, adopted, _ := e.adoptUnlessBackingOff(ctx, kind, loc, in, onTurnEnd); adopted {
			rec, ok = a.Record, true
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
	if code == core.NoticeTurnError {
		if err := e.markTurnError(ctx, rec); err != nil {
			return err
		}
	}
	peers, byHandle, crewLead := e.sessionPeersForError(rec)
	to, why, found := core.ErrorRecipient(peerOf(rec), peers, crewLead, code, func(h core.Handle) bool {
		cand, ok := byHandle[h]
		return ok && e.flagged(cand)
	})
	summary := string(rec.Handle) + ": " + code
	if detail != "" {
		summary += ": " + detail
	}
	if !found {
		e.desktop("rex-amq-adapter", summary)
		return nil
	}
	subject, body := core.ErrorNotice(peerOf(rec), code, detail, why, tail)
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(to.Handle), subject, body); err != nil {
		e.desktop("rex-amq-adapter", summary+" (mail to "+string(to.Handle)+" failed)")
		return err
	}
	e.seedSentCursor(rec.Handle)
	return nil
}

// markTurnError records that rec's turn ended in an error, for flagged.
func (e Env) markTurnError(ctx context.Context, rec store.Record) error {
	unlock, err := e.Store.LockHealth(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return err
	}
	m.TurnErroredAt, m.TurnErroredBy = e.Now().UTC(), turnKey(rec)
	return e.Store.PutHealthMark(rec.BlockID, m)
}

// sessionPeersForError reads only stored records (no Rex IPC inside the stop
// hook budget): peers in rec's session via together(), and rec's crew lead from
// rec.Crew and rec.Cwd.
func (e Env) sessionPeersForError(rec store.Record) ([]core.Peer, map[core.Handle]store.Record, string) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, nil, ""
	}
	byHandle := make(map[core.Handle]store.Record, len(recs))
	var peers []core.Peer
	for _, r := range recs {
		if !r.Live() || r.Handle == rec.Handle || !together(r, rec) {
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

// otherFamily finds a live agent in s of a model family other than rec's
// and the orchestrator's.
func (e Env) otherFamily(s Session, rec, orch store.Record) (store.Record, bool) {
	recs, err := e.Store.Records()
	if err != nil {
		return store.Record{}, false
	}
	for _, r := range recs {
		if r.Live() && s.holds(r) && r.Kind != rec.Kind && r.Kind != orch.Kind && r.Handle != rec.Handle {
			return r, true
		}
	}
	return store.Record{}, false
}

// desktop reaches the user's desktop through Env.Notify.
func (e Env) desktop(title, text string) {
	if e.Notify != nil {
		e.Notify(title, text)
		return
	}
	notifyDesktop(title, text)
}
