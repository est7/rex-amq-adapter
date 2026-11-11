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

// The watchdog's second look at every live agent of an orchestrator session, mail or
// no mail: the agent's own transcript. Only the harness's structural flags
// are read (core.Assess); the rows go to the orchestrator verbatim, as quoted
// data, and it decides what they mean. Each finding is told once
// (store.HealthMark). When the orchestrator's own transcript carries the same
// flag, the notice goes to a live agent of another model family in the
// session instead, since a shared account's limit stops both; with none,
// the user's desktop hears.

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
	if n := len(h.Errors); n > 0 && h.Errors[n-1].ID != mark.LastErrorID {
		newest := h.Errors[n-1]
		mark.LastErrorID = newest.ID
		if !mark.HookToldAt.IsZero() && (newest.At.IsZero() || !newest.At.Before(mark.HookToldAt.Add(-2*time.Second))) {
			mark.HookToldAt = time.Time{}
		} else {
			if err := e.healthNotice(ctx, orch, s, rec, core.NoticeTranscriptError, tail, h, nil, true); err != nil {
				return err
			}
		}
	}
	if h.Hung != nil && h.Hung.ID != mark.LastHungID {
		mark.LastHungID = h.Hung.ID
		if err := e.healthNotice(ctx, orch, s, rec, core.NoticeToolHung, tail, h, e.screenLines(ctx, rec), false); err != nil {
			return err
		}
	}
	if h.Stalled && !tail.LastAt.Equal(mark.LastStallAt) {
		mark.LastStallAt = tail.LastAt
		if err := e.healthNotice(ctx, orch, s, rec, core.NoticeStalled, tail, h, e.screenLines(ctx, rec), false); err != nil {
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

// healthNotice mails the finding in rec's name. shared says the finding may
// come from the account rather than the agent: then an orchestrator whose own
// transcript is flagged too cannot act on it, and another family is told.
// TODO(error-routing): unify with core.ErrorRecipient once watchdog
// transcript_error and hook turn_error share the no-mail-when-orchestrator-flagged
// contract (TestHealthRoutesAroundAFlaggedGoal currently asserts that a
// flagged orchestrator still receives the peer's mail when no third family exists).
func (e Env) healthNotice(ctx context.Context, orch store.Record, s Session, rec store.Record, code string, tail core.Tail, h core.Health, screen []string, shared bool) error {
	subject, body := core.HealthNotice(peerOf(rec), code, tail, h, screen, e.Now())
	to := orch.Handle
	if rec.Handle == orch.Handle || shared && e.flagged(ctx, orch) {
		if other, ok := e.otherFamily(s, rec, orch); ok {
			to = other.Handle
			body += "\n\nSent to you because the orchestrator " + string(orch.Handle) + " is of the same family as the flagged agent and its own transcript is flagged too."
		} else {
			e.desktop("rex-amq-adapter", string(rec.Handle)+": "+code+", and no agent of another model family is in the session")
			e.wdLog(orch, rec, "notice-desktop", "", code+": no other family in the session")
			if rec.Handle == orch.Handle {
				return nil
			}
		}
	}
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(to), subject, body); err != nil {
		return err
	}
	e.seedSentCursor(rec.Handle)
	e.wdLog(orch, rec, "notice", "", fmt.Sprintf("%s -> %s (%d rows)", code, to, len(tail.Rows)))
	return nil
}

// flagged reports whether rec's last turn ended abnormally or its transcript
// tail carries a fresh error flag.
func (e Env) flagged(ctx context.Context, rec store.Record) bool {
	if m, err := e.Store.HealthMark(rec.BlockID); err == nil && !m.TurnErroredAt.IsZero() {
		return true
	}
	tail, ok, err := transcript.Read(ctx, e.TranscriptHome, rec.Kind, rec.AgentSessionID, core.TailRows)
	if err != nil || !ok {
		return false
	}
	h := core.Assess(tail, false, e.Now())
	if len(h.Errors) == 0 {
		return false
	}
	newest := h.Errors[len(h.Errors)-1]
	// A row without a time of its own (Antigravity) cannot be shown fresh
	// from the transcript alone; TurnErroredAt covers its hook path.
	return !newest.At.IsZero() && e.Now().Sub(newest.At) < core.StalledFor
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

// codexTurnError reports whether a Codex Stop hook is ending an errored turn:
// Codex fires Stop on both normal completion and turn errors, writing the
// error row to its rollout before running the hook.
func (e Env) codexTurnError(ctx context.Context, loc Location, in HookInput) (detail string, ok bool) {
	rec, _, _ := e.Store.Record(loc.Block)
	tail, found := e.readHookTail(ctx, core.Codex, in, rec)
	if !found || len(tail.Rows) == 0 {
		return "", false
	}
	if in.TurnID != "" {
		for i := len(tail.Rows) - 1; i >= 0; i-- {
			r := tail.Rows[i]
			if r.Error && r.Kind == "error" && r.ToolID == in.TurnID {
				return r.Text, true
			}
		}
	}
	last := tail.Rows[len(tail.Rows)-1]
	if last.Error && last.Kind == "error" && (last.ToolID == "" || in.TurnID == "" || last.ToolID == in.TurnID) {
		if m, err := e.Store.HealthMark(loc.Block); err == nil && last.ID != "" && last.ID == m.LastErrorID {
			return "", false
		}
		return last.Text, true
	}
	return "", false
}

// clearTurnError clears TurnErroredAt and HookToldAt on a normal turn stop,
// touching disk only when one of them was set.
func (e Env) clearTurnError(ctx context.Context, block string) {
	m, err := e.Store.HealthMark(block)
	if err != nil || (m.TurnErroredAt.IsZero() && m.HookToldAt.IsZero()) {
		return
	}
	unlock, err := e.Store.LockHealth(ctx, block)
	if err != nil {
		return
	}
	defer unlock()
	m, err = e.Store.HealthMark(block)
	if err != nil || (m.TurnErroredAt.IsZero() && m.HookToldAt.IsZero()) {
		return
	}
	m.TurnErroredAt = time.Time{}
	m.HookToldAt = time.Time{}
	_ = e.Store.PutHealthMark(block, m)
}

// reportTurnError records and routes a turn-ending error or limit from a stop
// hook, deduplicating with the goal watchdog's transcript check via HealthMark.
// The dedup marks (LastErrorID / HookToldAt) are committed only after the
// notice is delivered (or posted to desktop), so a failed send leaves the row
// untold for the watchdog backstop.
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
	unlock, err := e.Store.LockHealth(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	defer unlock()
	mark, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return err
	}
	now := e.Now().UTC()
	tail, _ := e.readHookTail(ctx, kind, in, rec)
	h := core.Assess(tail, false, now)
	wantLastErrorID := mark.LastErrorID
	wantHookToldAt := mark.HookToldAt
	alreadyTold := false
	if n := len(h.Errors); n > 0 {
		newest := h.Errors[n-1]
		freshForTurn := false
		switch kind {
		case core.Codex:
			freshForTurn = newest.ID != "" && ((in.TurnID != "" && newest.ToolID == in.TurnID) ||
				(in.TurnID == "" && !newest.At.IsZero() && !newest.At.Before(now.Add(-2*watchEvery))))
		case core.Claude, core.Pi:
			freshForTurn = newest.ID != "" && !newest.At.IsZero() && !newest.At.Before(now.Add(-2*watchEvery))
		}
		switch {
		case freshForTurn && newest.ID == mark.LastErrorID && kind == core.Codex:
			alreadyTold = true
		case freshForTurn && newest.ID != mark.LastErrorID:
			wantLastErrorID = newest.ID
			wantHookToldAt = time.Time{}
		case kind != core.Agy:
			wantHookToldAt = now
		}
		if detail == "" && freshForTurn {
			detail = newest.Text
		}
	} else if kind != core.Agy {
		wantHookToldAt = now
	}
	if alreadyTold {
		if code == core.NoticeTurnError {
			mark.TurnErroredAt = now
			return e.Store.PutHealthMark(rec.BlockID, mark)
		}
		return nil
	}
	peers, byHandle, crewLead := e.sessionPeersForError(rec)
	to, why, found := core.ErrorRecipient(peerOf(rec), peers, crewLead, code, func(h core.Handle) bool {
		cand, ok := byHandle[h]
		return ok && e.flagged(ctx, cand)
	})
	if !found {
		msg := string(rec.Handle) + ": " + code
		if detail != "" {
			msg += ": " + detail
		}
		e.desktop("rex-amq-adapter", msg)
		mark.LastErrorID = wantLastErrorID
		mark.HookToldAt = wantHookToldAt
		if code == core.NoticeTurnError {
			mark.TurnErroredAt = now
		}
		return e.Store.PutHealthMark(rec.BlockID, mark)
	}
	subject, body := core.ErrorNotice(peerOf(rec), code, detail, why, tail)
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(to.Handle), subject, body); err != nil {
		if code == core.NoticeTurnError {
			mark.TurnErroredAt = now
			_ = e.Store.PutHealthMark(rec.BlockID, mark)
		}
		return err
	}
	e.seedSentCursor(rec.Handle)
	mark.LastErrorID = wantLastErrorID
	mark.HookToldAt = wantHookToldAt
	if code == core.NoticeTurnError {
		mark.TurnErroredAt = now
	}
	return e.Store.PutHealthMark(rec.BlockID, mark)
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
