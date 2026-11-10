package app

import (
	"context"
	"fmt"

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
	mark, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return err
	}
	if n := len(h.Errors); n > 0 && h.Errors[n-1].ID != mark.LastErrorID {
		mark.LastErrorID = h.Errors[n-1].ID
		if err := e.healthNotice(ctx, orch, s, rec, core.NoticeTranscriptError, tail, h, nil, true); err != nil {
			return err
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

// flagged reports whether rec's own transcript tail carries a fresh error
// flag.
func (e Env) flagged(ctx context.Context, rec store.Record) bool {
	tail, ok, err := transcript.Read(ctx, e.TranscriptHome, rec.Kind, rec.AgentSessionID, core.TailRows)
	if err != nil || !ok {
		return false
	}
	h := core.Assess(tail, false, e.Now())
	if len(h.Errors) == 0 {
		return false
	}
	newest := h.Errors[len(h.Errors)-1]
	// A row without a time of its own (Antigravity) cannot be shown fresh.
	return !newest.At.IsZero() && e.Now().Sub(newest.At) < core.StalledFor
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
