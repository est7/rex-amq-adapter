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

// The watchdog's third look, at every live agent of a goal session, the goal
// agent included: one sitting on a prompt only the user can answer while no
// mail is pending for it. With mail pending the injector records the
// deferral and the undelivered-mail path reports it; without, nothing
// would. The signal is structural (program status blocked, or a trust
// screen while the agent reports nothing else), held for StuckFor; the
// notice says so once per episode, and a line lands in logs/blocked.log
// with the time, so the user finds when the run stopped. A blocked goal
// agent cannot read mail: another family in the session is told, else the
// desktop. Zero keys.

func (e Env) blocked(ctx context.Context, goal store.Record, s Session, rec store.Record) error {
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return err
	}
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
	if what == "" || len(unread) > 0 {
		// Not blocked, or the undelivered-mail path owns it: the episode ends.
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
	e.blockedLog(goal, rec, what, mark.BlockedSince, lines)
	subject, body := core.BlockedNotice(peerOf(rec), what, mark.BlockedSince, lines, now)
	if rec.Handle == goal.Handle {
		if other, ok := e.otherFamily(s, rec, goal); ok {
			body += "\n\nSent to you because the goal agent " + string(goal.Handle) + " is the one waiting and cannot read mail."
			if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(other.Handle), subject, body); err != nil {
				return err
			}
			e.seedSentCursor(rec.Handle)
		}
		e.desktop("rex-amq-adapter", "goal agent "+string(rec.Handle)+" waits on you: "+what)
	} else {
		if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(goal.Handle), subject, body); err != nil {
			return err
		}
		e.seedSentCursor(rec.Handle)
		if strings.Contains(strings.ToLower(what), "permission") {
			e.desktop("rex-amq-adapter", string(rec.Handle)+" waits on a permission prompt")
		}
	}
	e.wdLog(goal, rec, "blocked", "", what)
	mark.BlockedTold = true
	return e.Store.PutHealthMark(rec.BlockID, mark)
}

// blockedLog keeps one line per blocked episode in logs/blocked.log: when
// it began, who, what, and the screen's last lines.
func (e Env) blockedLog(goal, rec store.Record, what string, since time.Time, lines []string) {
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "blocked.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s since %s: %s (goal %s) %s | %s\n", e.Now().UTC().Format(time.RFC3339), since.UTC().Format(time.RFC3339), rec.Handle, goal.Handle, what, strings.Join(lines, " | "))
}
