package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The watchdog's third look, at every live agent of an orchestrator session, the
// orchestrator included: one sitting on a prompt only the user can answer, mail
// pending or not. It is the one owner of these two shapes (blocked status,
// trust screen); the undelivered-mail path leaves them to it, so each is
// reported once and the orchestrator's own is never mailed to itself. The signal is structural (program status blocked, or a trust
// screen while the agent reports nothing else), held for StuckFor; the
// notice says so once per episode, and a line lands in logs/blocked.log
// with the time, so the user finds when the run stopped. A blocked
// orchestrator cannot read mail: another family in the session is told, else the
// desktop. Zero keys.

func (e Env) blocked(ctx context.Context, orch store.Record, s Session, rec store.Record) error {
	unlock, err := e.Store.LockHealth(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	defer unlock()
	mark, err := e.Store.HealthMark(rec.BlockID)
	if err != nil {
		return err
	}
	what := ""
	st, reports, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	switch {
	case reports && core.AgentState(st.State) == core.StateBlocked:
		what = "its status reports blocked"
		if st.Kind != "" {
			what += " (" + st.Kind + ")"
		}
	case !reports || core.AgentState(st.State) != core.StateWorking:
		if phrase, ok := screen.TrustScreen(string(rec.Kind), vt); ok {
			what = "a trust screen is up: " + phrase
		}
	}
	now := e.Now()
	if what == "" {
		// The prompt is gone: the episode ends. Mail coming or going does
		// not end it, and does not change who reports it.
		if !mark.BlockedSince.IsZero() {
			mark.BlockedSince, mark.BlockedTold = time.Time{}, false
			return e.Store.PutHealthMark(rec.BlockID, mark)
		}
		return nil
	}
	if mark.BlockedSince.IsZero() {
		mark.BlockedSince = now
		return e.Store.PutHealthMark(rec.BlockID, mark)
	}
	if mark.BlockedTold || now.Sub(mark.BlockedSince) < core.StuckFor {
		return nil
	}
	lines := lastLines(vt, core.ReportLines)
	e.blockedLog(orch, rec, what, mark.BlockedSince, lines)
	subject, body := core.BlockedNotice(peerOf(rec), what, mark.BlockedSince, lines, now)
	if rec.Handle == orch.Handle {
		if other, ok := e.otherFamily(s, rec, orch); ok {
			body += "\n\nSent to you because the orchestrator " + string(orch.Handle) + " is the one waiting and cannot read mail."
			if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(other.Handle), subject, body); err != nil {
				return err
			}
			e.seedSentCursor(rec.Handle)
		}
		e.desktop("rex-amq-adapter", "orchestrator "+string(rec.Handle)+" waits on you: "+what)
	} else {
		if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(orch.Handle), subject, body); err != nil {
			return err
		}
		e.seedSentCursor(rec.Handle)
		if strings.Contains(strings.ToLower(what), "permission") {
			e.desktop("rex-amq-adapter", string(rec.Handle)+" waits on a permission prompt")
		}
	}
	e.wdLog(orch, rec, "blocked", "", what)
	mark.BlockedTold = true
	return e.Store.PutHealthMark(rec.BlockID, mark)
}

// blockedLog keeps one line per blocked episode in logs/blocked.log: when
// it began, who, what, and the screen's last lines.
func (e Env) blockedLog(orch, rec store.Record, what string, since time.Time, lines []string) {
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "blocked.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s since %s: %s (orchestrator %s) %s | %s\n", e.Now().UTC().Format(time.RFC3339), since.UTC().Format(time.RFC3339), rec.Handle, orch.Handle, what, strings.Join(lines, " | "))
}
