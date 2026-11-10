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
// live agent of the goal's session it looks for mail the injector could not
// deliver for StuckFor, and acts only on the one shape it can fix without
// the user: a pane too small to draw the input box. It moves that pane once,
// to the session's overflow tab, has the user's app render it, puts the
// window the app showed back, verifies the size and an empty box, and hands
// the mail to the injector. Every other stuck shape it only reports to the
// goal agent: a trust or permission dialog, a draft in the box, a box that
// stays missing at a normal size, a pane the user moved meanwhile, no app to
// render with, a restore that did not take, a recovery that did not take.

// actBudget bounds one layout action sequence. It is cut from the parent's
// cancellation, not its deadline: the watcher's tick budget (watchBudget) is
// shorter than a round trip to the app client, and a move must not be left
// half done by it.
const actBudget = 20 * time.Second

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

// look is what the watchdog saw of one agent.
type look struct {
	unread  int
	code    string // the injector's deferral reason
	since   time.Time
	status  core.AgentState
	kind    string
	box     screen.Draft
	trust   string
	columns int
	rows    int
	lines   []string
	window  string
}

func (e Env) observe(ctx context.Context, rec store.Record, unread int, history []core.Attempt) (look, bool, error) {
	l := look{unread: unread}
	var stuck bool
	if l.code, l.since, stuck = core.Stuck(history, unread, e.Now()); !stuck {
		return l, false, nil
	}
	status, _, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return l, false, err
	}
	l.status, l.kind = core.AgentState(status.State), status.Kind
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return l, false, err
	}
	l.box = screen.Check(string(rec.Kind), vt)
	l.trust, _ = screen.TrustScreen(string(rec.Kind), vt)
	l.lines = lastLines(vt, core.ReportLines)
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return l, false, err
	}
	l.columns, l.rows = size.Columns, size.Rows
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return l, false, err
	}
	l.window = blk.WindowID
	return l, true, nil
}

// watchPeer decides and acts for one agent.
func (e Env) watchPeer(ctx context.Context, goal store.Record, s Session, rec store.Record) error {
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
		e.wdLog(goal, rec, "episode-end", ep.ID, "mail delivered")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	l, stuck, err := e.observe(ctx, rec, len(unread), history)
	if err != nil || !stuck {
		return err
	}
	if !open {
		now := e.Now()
		ep = core.Episode{ID: fmt.Sprintf("%s-%d", rec.Handle, now.Unix()), Handle: rec.Handle, Block: rec.BlockID, PID: rec.AgentPID, Session: s.id, StartedAt: now, Reported: map[string]time.Time{}}
	}
	e.wdLog(goal, rec, "observe", ep.ID, fmt.Sprintf("unread %d, deferred %s since %s, %dx%d in %s, status %s/%s, box %s", l.unread, l.code, core.Ago(e.Now().Sub(l.since)), l.columns, l.rows, l.window, l.status, l.kind, l.box))

	// An episode that moved the pane: first, is the pane still where the
	// watchdog put it? A pane the user moved out is theirs, whatever its size.
	if ep.Moved || ep.Overflow != "" {
		if l.window != ep.Overflow {
			if !ep.Moved {
				// Journal written, move not confirmed, pane not there: the move never took.
				ep.Overflow, ep.PrevActive = "", ""
			} else {
				return e.report(ctx, goal, rec, &ep, core.ReportMovedByUser, fmt.Sprintf("Its pane left the %s tab after the watchdog moved it there, so the watchdog leaves it alone.", core.OverflowTab), l.lines)
			}
		} else if !ep.Moved {
			// The move took before the journal could say so (a restart or a
			// timed-out call); continue from there, never move again.
			ep.Moved = true
			if err := e.Store.PutEpisode(ep); err != nil {
				return err
			}
			e.wdLog(goal, rec, "reconcile", ep.ID, "the pane is already in the overflow tab")
		}
	}

	// Zero keystrokes on any dialog or draft: these are reported, never answered.
	switch {
	case l.trust != "":
		return e.report(ctx, goal, rec, &ep, core.ReportTrustScreen, "A trust dialog is on its screen: "+l.trust+".", l.lines)
	case l.status == core.StateBlocked:
		return e.report(ctx, goal, rec, &ep, core.ReportBlocked, "It waits on the user ("+l.kind+").", l.lines)
	case l.box == screen.Typed:
		return e.report(ctx, goal, rec, &ep, core.ReportDraft, "Its input box holds a draft, so no doorbell can be typed.", l.lines)
	}

	if ep.Moved {
		return e.continueEpisode(ctx, goal, rec, &ep, l)
	}
	if !core.MayMove(l.code, l.columns, l.rows) {
		if l.code == "box_not_found" {
			return e.report(ctx, goal, rec, &ep, core.ReportBoxNotFound, fmt.Sprintf("Its pane measures %dx%d, large enough, yet doorbells find no input box.", l.columns, l.rows), l.lines)
		}
		return e.report(ctx, goal, rec, &ep, core.ReportUndelivered, fmt.Sprintf("Every doorbell since %s deferred with %s.", core.Ago(e.Now().Sub(l.since)), l.code), l.lines)
	}
	if ep.Attempts >= core.MaxAttempts {
		return e.report(ctx, goal, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane could not be moved after %d attempts.", ep.Attempts), l.lines)
	}
	ep.Columns, ep.Rows = l.columns, l.rows
	inject, err := e.recover(ctx, goal, rec, &ep, s, l)
	return e.handOff(ctx, goal, rec, &ep, inject, err)
}

// continueEpisode carries a moved pane through render, restore and verify,
// each step once per look, and reports what does not take.
func (e Env) continueEpisode(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, l look) error {
	if !ep.Rendered {
		if ep.Attempts >= core.MaxAttempts {
			return e.report(ctx, goal, rec, ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane sits in the %s tab but the app could not render it after %d attempts.", core.OverflowTab, ep.Attempts), l.lines)
		}
		inject, err := e.render(ctx, goal, rec, ep)
		return e.handOff(ctx, goal, rec, ep, inject, err)
	}
	if !ep.Restored {
		e.restore(ctx, goal, rec, ep)
	}
	if !ep.Verified {
		inject, err := e.verify(ctx, goal, rec, ep)
		if err != nil {
			return err
		}
		if inject {
			return e.handOff(ctx, goal, rec, ep, true, nil)
		}
		if core.TinyPane(l.columns, l.rows) {
			return e.report(ctx, goal, rec, ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane was moved to the %s tab and rendered, yet still measures %dx%d.", core.OverflowTab, l.columns, l.rows), l.lines)
		}
		return nil // drawn large, box not seen yet: looked at again next tick
	}
	return e.report(ctx, goal, rec, ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane was moved to the %s tab and measures %dx%d, yet doorbells still find no input box.", core.OverflowTab, l.columns, l.rows), l.lines)
}

// handOff gives the mail to the injector once a step verified the pane. It
// runs after that step returned, so the injector takes the block's lock
// itself.
func (e Env) handOff(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, inject bool, err error) error {
	if err != nil {
		ep.Attempts++
		_ = e.Store.PutEpisode(*ep)
		return err
	}
	if !inject {
		return nil
	}
	o := e.Inject(ctx, InjectRequest{Block: rec.BlockID, Handle: rec.Handle, Root: e.Paths.Root()})
	e.wdLog(goal, rec, "inject", ep.ID, string(o.Progress)+" "+o.Code)
	if o.Progress == core.Accepted {
		ep.Outcome = "recovered"
		e.wdLog(goal, rec, "episode-end", ep.ID, "recovered: mail delivered")
		if err := e.Store.PutEpisode(*ep); err != nil {
			return err
		}
		if !ep.Restored {
			// Delivered, but the app may still show the overflow tab: say so once.
			return e.report(ctx, goal, rec, ep, core.ReportRestoreFailed, "Its mail was delivered, but the app could not be put back on "+ep.PrevActive+".", nil)
		}
		return e.Store.ClearEpisode(rec.BlockID)
	}
	return nil
}

// recover moves rec's pane to the overflow tab, once, under the injector's
// lock and after re-reading everything the decision rested on.
func (e Env) recover(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, s Session, l look) (inject bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
	defer cancel()
	// Nothing can render the pane without an app: report before moving.
	clients, err := e.Rex.AppClients(ctx, s.id)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		return false, e.report(ctx, goal, rec, ep, core.ReportNoClient, "No app client shows this session, so nothing can render its pane; it was not moved.", l.lines)
	}
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
	if o, ok := e.checkBinding(ctx, rec); !ok {
		e.wdLog(goal, rec, "abort", ep.ID, "the pane does not show the agent: "+o.Code)
		return false, nil
	}
	status, _, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	if st := core.AgentState(status.State); st == core.StateWorking || st == core.StateBlocked {
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
	if blk.WindowID != l.window {
		e.wdLog(goal, rec, "abort", ep.ID, "the pane moved between the look and the lock")
		return false, nil
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
	// The journal goes down before the move, so a restart finds the pane
	// either where it was (no journal effect) or in the overflow tab.
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	e.wdLog(goal, rec, "move", ep.ID, fmt.Sprintf("from %s to %s beside %s (was %dx%d)", ep.Window, overflow, anchor, size.Columns, size.Rows))
	if err := e.Rex.MoveBlock(ctx, s.id, rec.BlockID, anchor); err != nil {
		// A timed-out call may have taken: the next look reconciles.
		return false, fmt.Errorf("move: %w", err)
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
// moved pane out, then puts the window the app showed back.
func (e Env) render(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) (inject bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
	defer cancel()
	clients, err := e.Rex.AppClients(ctx, ep.Session)
	if err != nil {
		return false, err
	}
	if len(clients) == 0 {
		return false, e.report(ctx, goal, rec, ep, core.ReportNoClient, "No app client shows this session, so nothing can render its pane.", nil)
	}
	for _, c := range clients {
		if err := e.Rex.SelectView(ctx, c, ep.Session, ep.Overflow); err != nil {
			return false, fmt.Errorf("show the overflow tab on %s: %w", c, err)
		}
	}
	ep.Rendered = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	e.wdLog(goal, rec, "render", ep.ID, fmt.Sprintf("shown %s on %d client(s)", ep.Overflow, len(clients)))
	e.restore(ctx, goal, rec, ep)
	return e.verify(ctx, goal, rec, ep)
}

// restore puts the app back on the window the session showed before the
// render, with a budget of its own. What the app showed is known only for
// this session (Rex keeps no per-client view), so an app that was on
// another session comes back to this one's previous window. A restore that
// fails is retried on later looks and reported once.
func (e Env) restore(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ep.PrevActive == "" || ep.PrevActive == ep.Overflow {
		ep.Restored = true
		_ = e.Store.PutEpisode(*ep)
		return
	}
	clients, err := e.Rex.AppClients(ctx, ep.Session)
	if err == nil {
		for _, c := range clients {
			if err = e.Rex.SelectView(ctx, c, ep.Session, ep.PrevActive); err != nil {
				break
			}
		}
	}
	if err == nil {
		var active string
		if active, err = e.Rex.ActiveWindow(ctx, ep.Session); err == nil && active != ep.PrevActive {
			err = fmt.Errorf("the session still shows %s", active)
		}
	}
	if err != nil {
		e.wdLog(goal, rec, "restore-failed", ep.ID, err.Error())
		_ = e.report(ctx, goal, rec, ep, core.ReportRestoreFailed, "After rendering its pane, the app could not be put back on "+ep.PrevActive+" ("+err.Error()+"); the watchdog keeps trying.", nil)
		return
	}
	ep.Restored = true
	_ = e.Store.PutEpisode(*ep)
	e.wdLog(goal, rec, "restore", ep.ID, "the app shows "+ep.PrevActive+" again")
}

// verify measures the pane after rendering; large enough with an empty box
// means the mail can be handed to the injector.
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
		return false, e.report(ctx, goal, rec, ep, core.ReportDraft, "After rendering, its input box holds a draft, so no doorbell can be typed.", lastLines(vt, core.ReportLines))
	}
	if box != screen.Empty {
		return false, nil // not drawn yet: looked at again next tick
	}
	ep.Verified = true
	return true, e.Store.PutEpisode(*ep)
}

// report tells the goal agent once per code per episode, in the stuck peer's
// name, and notifies the user's desktop when a permission prompt waits.
func (e Env) report(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, code, detail string, lines []string) error {
	if ep.Reported == nil {
		ep.Reported = map[string]time.Time{}
	}
	if _, done := ep.Reported[code]; done {
		return nil
	}
	subject, body := core.WatchdogReport(peerOf(rec), *ep, code, detail, lines)
	if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(goal.Handle), subject, body); err != nil {
		return err
	}
	e.seedSentCursor(rec.Handle)
	ep.Reported[code] = e.Now()
	if err := e.Store.PutEpisode(*ep); err != nil {
		return err
	}
	e.wdLog(goal, rec, "report", ep.ID, code+": "+detail)
	if code == core.ReportBlocked && strings.Contains(strings.ToLower(detail), "permission") {
		notifyDesktop("rex-amq-adapter", string(rec.Handle)+" waits on a permission prompt")
	}
	return nil
}

// notifyDesktop is best effort and macOS only.
func notifyDesktop(title, text string) {
	if runtime.GOOS != "darwin" || os.Getenv("REX_AMQ_ADAPTER_NO_DESKTOP") != "" {
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
