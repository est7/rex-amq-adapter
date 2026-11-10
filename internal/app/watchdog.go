package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// The goal watchdog runs inside the goal watcher's tick (goal.go). For every
// live agent of the goal's session it looks for mail that cannot be
// delivered, and acts only on the one shape it can fix without the user: a
// pane too small to draw the input box. It moves that pane once, to the
// session's overflow tab, has the user's app render it, restores the window
// the app showed, verifies the size and an empty box, and hands the mail to
// the injector. Anything else it only reports to the goal agent: a trust or
// permission dialog, a draft in the box, a box that stays missing at a normal
// size, a pane the user moved meanwhile, a recovery that did not take.

// recoverBudget bounds one episode's layout calls, which include a
// round trip to the app client.
const recoverBudget = 20 * time.Second

// watchdogErr wraps a watchdog failure so the goal watcher logs it without
// counting it towards giving up.
type watchdogErr struct{ error }

// watchdog runs one look over every live agent in s. Errors are reported and
// swallowed: a failing watchdog must not stop the goal watcher.
func (e Env) watchdog(ctx context.Context, goal store.Record, s Session) {
	recs, err := e.Store.Records()
	if err != nil {
		e.wdLog(goal, store.Record{}, "records", "", err.Error())
		return
	}
	for _, r := range recs {
		if !r.Live() || !s.holds(r) || r.Kind.InProcess() {
			continue
		}
		if err := e.watchPeer(ctx, goal, s, r); err != nil {
			e.wdLog(goal, r, "error", "", err.Error())
		}
	}
}

// watchPeer decides and acts for one agent.
func (e Env) watchPeer(ctx context.Context, goal store.Record, s Session, rec store.Record) error {
	now := e.Now()
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return err
	}
	history, err := e.Store.InjectHistory(rec.Handle)
	if err != nil {
		return err
	}
	ep, open, err := e.Store.Episode(rec.BlockID)
	if err != nil {
		return err
	}
	if open && (len(unread) == 0 || core.Delivered(history)) {
		// The mail arrived: the episode is over, whatever it did.
		ep.Outcome = "delivered"
		e.wdLog(goal, rec, "episode-end", ep.ID, "mail delivered")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	since, stuck := core.Undelivered(history, len(unread), now)
	if !stuck {
		return nil
	}
	if !open {
		ep = core.Episode{ID: fmt.Sprintf("%s-%d", rec.Handle, now.Unix()), Handle: rec.Handle, Block: rec.BlockID, PID: rec.AgentPID, Session: s.id, StartedAt: now, Reported: map[string]time.Time{}}
	}
	// The look: what the injector would see, plus the size.
	status, _, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	lines := lastLines(vt, core.ReportLines)
	obs := fmt.Sprintf("unread %d, box_not_found since %s, %dx%d, status %s/%s, box %s", len(unread), core.Ago(now.Sub(since)), size.Columns, size.Rows, status.State, status.Kind, screen.Check(string(rec.Kind), vt))
	e.wdLog(goal, rec, "observe", ep.ID, obs)

	// Zero keystrokes on any dialog: these are reported, never answered.
	if phrase, ok := screen.TrustScreen(string(rec.Kind), vt); ok {
		return e.report(ctx, goal, rec, &ep, core.ReportTrustScreen, "A trust dialog is on its screen: "+phrase+".", lines, false)
	}
	if core.AgentState(status.State) == core.StateBlocked {
		permission := strings.Contains(strings.ToLower(status.Kind), "permission")
		return e.report(ctx, goal, rec, &ep, core.ReportBlocked, "It waits on the user ("+status.Kind+").", lines, permission)
	}
	if screen.Check(string(rec.Kind), vt) == screen.Typed {
		return e.report(ctx, goal, rec, &ep, core.ReportDraft, "Its input box holds a draft, so no doorbell can be typed.", lines, false)
	}
	if !core.TinyPane(size.Columns, size.Rows) {
		if ep.Moved {
			if !ep.Verified {
				inject, err := e.verify(ctx, goal, rec, &ep)
				return e.handOff(ctx, goal, rec, &ep, inject, err)
			}
			return e.report(ctx, goal, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane was moved to the %s tab and measures %dx%d, yet doorbells still find no input box.", core.OverflowTab, size.Columns, size.Rows), lines, false)
		}
		return e.report(ctx, goal, rec, &ep, core.ReportBoxNotFound, fmt.Sprintf("Its pane measures %dx%d, large enough, yet doorbells find no input box.", size.Columns, size.Rows), lines, false)
	}
	if ep.Moved {
		// Moved and still tiny: rendering did not take, or the user moved it.
		if !ep.Rendered {
			inject, err := e.render(ctx, goal, rec, &ep)
			return e.handOff(ctx, goal, rec, &ep, inject, err)
		}
		return e.report(ctx, goal, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane was moved to the %s tab and rendered, yet still measures %dx%d.", core.OverflowTab, size.Columns, size.Rows), lines, false)
	}
	ep.Columns, ep.Rows = size.Columns, size.Rows
	inject, err := e.recover(ctx, goal, rec, &ep, s)
	return e.handOff(ctx, goal, rec, &ep, inject, err)
}

// handOff gives the mail to the injector once a step verified the pane. It
// runs after that step returned, so the injector takes the block's lock
// itself.
func (e Env) handOff(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, inject bool, err error) error {
	if err != nil || !inject {
		return err
	}
	o := e.Inject(ctx, InjectRequest{Block: rec.BlockID, Handle: rec.Handle, Root: e.Paths.Root()})
	e.wdLog(goal, rec, "inject", ep.ID, string(o.Progress)+" "+o.Code)
	if o.Progress == core.Accepted {
		ep.Outcome = "recovered"
		e.wdLog(goal, rec, "episode-end", ep.ID, "recovered: mail delivered")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	return nil
}

// recover moves rec's pane to the overflow tab, once, under the injector's
// lock and after re-reading everything the decision rested on.
func (e Env) recover(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, s Session) (inject bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, recoverBudget)
	defer cancel()
	unlock, err := e.Store.LockInject(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	defer unlock()
	// Precondition, re-read under the lock.
	cur, ok, err := e.Store.Record(rec.BlockID)
	if err != nil {
		return false, err
	}
	if !ok || cur.Handle != rec.Handle || cur.AgentPID != rec.AgentPID || cur.AgentSessionID != rec.AgentSessionID || !cur.Live() {
		e.wdLog(goal, rec, "abort", ep.ID, "the record changed under the lock")
		return false, nil
	}
	status, _, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	if core.AgentState(status.State) == core.StateWorking || core.AgentState(status.State) == core.StateBlocked {
		e.wdLog(goal, rec, "abort", ep.ID, "the agent is "+status.State)
		return false, nil
	}
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	if !core.TinyPane(size.Columns, size.Rows) {
		e.wdLog(goal, rec, "abort", ep.ID, fmt.Sprintf("the pane grew to %dx%d on its own", size.Columns, size.Rows))
		return false, nil
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	// At this size the box cannot be seen, so a draft cannot be ruled out
	// here; the capture after rendering is the gate that matters (verify).
	if _, trust := screen.TrustScreen(string(rec.Kind), vt); trust || screen.Check(string(rec.Kind), vt) == screen.Typed {
		e.wdLog(goal, rec, "abort", ep.ID, "a dialog or draft appeared")
		return false, nil
	}
	if pending, ok, _ := e.Store.Pending(rec.BlockID); ok && pending != "" {
		e.wdLog(goal, rec, "abort", ep.ID, "the injector has a doorbell pending in this box")
		return false, nil
	}
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	ep.Window = blk.WindowID
	if ep.PrevActive, err = e.Rex.ActiveWindow(ctx, s.id); err != nil {
		return false, err
	}
	overflow, anchor, shell, err := e.overflowTab(ctx, s.id)
	if err != nil {
		return false, err
	}
	ep.Overflow = overflow
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	e.wdLog(goal, rec, "move", ep.ID, fmt.Sprintf("from %s to %s beside %s (was %dx%d)", ep.Window, overflow, anchor, size.Columns, size.Rows))
	if err := e.Rex.MoveBlock(ctx, s.id, rec.BlockID, anchor); err != nil {
		ep.Outcome = "move failed: " + err.Error()
		_ = e.Store.PutEpisode(*ep)
		return false, err
	}
	ep.Moved = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	if shell != "" {
		if err := e.Rex.CloseBlock(ctx, shell); err != nil {
			e.wdLog(goal, rec, "warn", ep.ID, "the overflow tab's shell stays open: "+err.Error())
		}
	}
	return e.render(ctx, goal, rec, ep)
}

// overflowTab finds the session's overflow window and a block in it to
// anchor on, creating the window (with a shell to close afterwards) when it
// does not exist.
func (e Env) overflowTab(ctx context.Context, session string) (window, anchor, shell string, err error) {
	sess, err := e.Rex.InspectSession(ctx, session)
	if err != nil {
		return "", "", "", err
	}
	for _, w := range sess.Windows {
		if w.Label != core.OverflowTab {
			continue
		}
		blocks, err := e.Rex.ListBlocks(ctx, session)
		if err != nil {
			return "", "", "", err
		}
		for _, b := range blocks {
			if b.WindowID == w.ID {
				return w.ID, b.ID, "", nil
			}
		}
	}
	window, shell, err = e.Rex.NewTab(ctx, session, core.OverflowTab)
	return window, shell, shell, err
}

// render has the user's app show the overflow tab, which is what lays the
// moved pane out, then puts the window the app showed back at once.
func (e Env) render(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) (inject bool, err error) {
	clients, err := e.Rex.AppClients(ctx, ep.Session)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		e.wdLog(goal, rec, "warn", ep.ID, "no app client shows this session; nothing can render the pane")
		return false, nil
	}
	for _, c := range clients {
		if err := e.Rex.SelectView(ctx, c, ep.Session, ep.Overflow); err != nil {
			return false, err
		}
		if ep.PrevActive != "" && ep.PrevActive != ep.Overflow {
			if err := e.Rex.SelectView(ctx, c, ep.Session, ep.PrevActive); err != nil {
				e.wdLog(goal, rec, "warn", ep.ID, "the app could not be put back on "+ep.PrevActive+": "+err.Error())
			}
		}
	}
	ep.Rendered = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	restored := "unknown"
	if active, err := e.Rex.ActiveWindow(ctx, ep.Session); err == nil {
		restored = active
	}
	e.wdLog(goal, rec, "render", ep.ID, fmt.Sprintf("shown %s on %d client(s); active window now %s (was %s)", ep.Overflow, len(clients), restored, ep.PrevActive))
	return e.verify(ctx, goal, rec, ep)
}

// verify measures the pane after rendering and, when it is large enough with
// an empty box, hands the mail to the injector.
func (e Env) verify(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) (inject bool, err error) {
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	box := screen.Check(string(rec.Kind), vt)
	e.wdLog(goal, rec, "verify", ep.ID, fmt.Sprintf("%dx%d, box %s", size.Columns, size.Rows, box))
	if core.TinyPane(size.Columns, size.Rows) {
		return false, nil // still tiny: the next look reports recovery_failed
	}
	if box == screen.Typed {
		return false, e.report(ctx, goal, rec, ep, core.ReportDraft, "After rendering, its input box holds a draft, so no doorbell can be typed.", lastLines(vt, core.ReportLines), false)
	}
	if box != screen.Empty {
		return false, nil // not drawn yet: looked at again next tick
	}
	ep.Verified = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	return true, nil
}

// report tells the goal agent once per code per episode, in the stuck peer's
// name, and notifies the user's desktop when a permission prompt waits.
func (e Env) report(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, code, detail string, lines []string, desktop bool) error {
	if ep.Reported == nil {
		ep.Reported = map[string]time.Time{}
	}
	if _, done := ep.Reported[code]; done {
		return nil
	}
	subject, body := core.WatchdogReport(peerOf(rec), code, detail, lines)
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(goal.Handle), subject, body); err != nil {
		return err
	}
	e.seedSentCursor(rec.Handle)
	ep.Reported[code] = e.Now()
	if err := e.Store.PutEpisode(*ep); err != nil {
		return err
	}
	e.wdLog(goal, rec, "report", ep.ID, code+": "+detail)
	if desktop {
		notifyDesktop("rex-amq-adapter", string(rec.Handle)+" waits on a permission prompt")
	}
	return nil
}

// notifyDesktop is best effort and macOS only.
func notifyDesktop(title, text string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", text, title)
	_ = exec.Command("osascript", "-e", script).Run()
}

// lastLines are the last n non-empty plain lines of a screen.
func lastLines(vt string, n int) []string {
	var out []string
	for _, l := range strings.Split(screen.PlainText(vt), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// wdLog appends one JSON line per step to logs/watchdog.log: who looked,
// at whom, what was seen, what was done. Drafts and raw screens stay out.
func (e Env) wdLog(goal, rec store.Record, step, episode, detail string) {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "goal": goal.Handle, "handle": rec.Handle, "block": rec.BlockID, "step": step, "detail": detail}
	if episode != "" {
		entry["episode"] = episode
	}
	b, _ := json.Marshal(entry)
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err == nil {
		if f, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if step == "error" {
		fmt.Fprintf(Diagnostics, "watchdog %s: %s\n", rec.Handle, detail)
	}
}
