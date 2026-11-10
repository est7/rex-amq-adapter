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
// the user: a pane too small to draw the input box. Without touching what
// the user looks at, it detaches that block from its window (the program
// keeps running and no client owns the terminal any more), resizes it to
// RecoverColumns x RecoverRows, which a terminal nobody owns takes at once,
// verifies the size and an empty box, hands the mail to the injector, and
// places the block in the session's overflow tab, where it keeps that size
// until the user opens the tab. Every other stuck shape it only reports to
// the goal agent: a trust or permission dialog, a draft in the box, a box
// that stays missing at a normal size, a pane the user moved meanwhile, a
// recovery that did not take.

// actBudget bounds one layout action sequence. It is cut from the parent's
// cancellation, not its deadline: the watcher's tick budget (watchBudget)
// must not leave a block detached halfway.
const actBudget = 20 * time.Second

// settleFor bounds waiting for the agent to redraw after a resize.
const settleFor = 3 * time.Second

// watchdog runs one look over every live agent in s. Errors are reported and
// swallowed: a failing watchdog must not stop the goal watcher.
func (e Env) watchdog(ctx context.Context, goal store.Record, s Session) {
	recs, err := e.Store.Records()
	if err != nil {
		e.wdLog(goal, store.Record{}, "records", "", err.Error())
		return
	}
	for _, r := range recs {
		if !r.Live() || !s.holds(r) {
			continue
		}
		if !r.Kind.InProcess() {
			if err := e.watchPeer(ctx, goal, s, r); err != nil {
				e.wdLog(goal, r, "error", "", err.Error())
			}
		}
		if err := e.health(ctx, goal, s, r); err != nil {
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
	owner   string
	lines   []string
	window  string // "" when detached
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
	l.columns, l.rows, l.owner = size.Columns, size.Rows, size.Owner
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return l, false, err
	}
	l.window = blk.WindowID
	return l, true, nil
}

// watchPeer decides and acts for one agent. Whatever happens after a
// detach, the block is placed in a window again before the look ends.
func (e Env) watchPeer(ctx context.Context, goal store.Record, s Session, rec store.Record) (err error) {
	defer func() {
		ep, open, perr := e.Store.Episode(rec.BlockID)
		if perr != nil || !open || !ep.Detached || ep.Placed || ep.Taken {
			return
		}
		if perr := e.place(ctx, goal, rec, &ep, s); perr != nil && err == nil {
			err = perr
		}
	}()
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
	if open {
		// The journal says what was attempted; the block says what took.
		if err := e.reconcile(ctx, goal, rec, &ep); err != nil {
			return err
		}
		if ep.Detached && ep.Resized && !ep.Placed && !ep.Taken {
			// A resized block is placed before anything else, delivered or
			// not: the user must be able to find it. One not yet resized
			// gets its resize first (below), and is placed when the look
			// ends (the deferred placement above).
			if err := e.place(ctx, goal, rec, &ep, s); err != nil {
				return err
			}
		}
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
	e.wdLog(goal, rec, "observe", ep.ID, fmt.Sprintf("unread %d, deferred %s since %s, %dx%d owner %q in %q, status %s/%s, box %s", l.unread, l.code, core.Ago(e.Now().Sub(l.since)), l.columns, l.rows, l.owner, l.window, l.status, l.kind, l.box))

	// A pane the user took (placed themselves, or moved out of the overflow
	// tab) is theirs, whatever its size.
	if ep.Taken || ep.Placed && l.window != ep.Overflow {
		ep.Taken = true
		return e.report(ctx, goal, rec, &ep, core.ReportMovedByUser, fmt.Sprintf("Its pane is no longer where the watchdog put it (the %s tab), so the watchdog leaves it alone.", core.OverflowTab), l.lines)
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

	if ep.Detached {
		// Detached earlier, still undelivered: resize if that never took,
		// then verify again (the agent may have redrawn since), else report.
		var inject bool
		var err error
		if !ep.Resized && l.window == "" {
			inject, err = e.resize(ctx, goal, rec, &ep)
		} else {
			inject, err = e.verify(ctx, goal, rec, &ep)
		}
		if err != nil {
			return e.handOff(ctx, goal, rec, &ep, s, false, err)
		}
		if inject {
			return e.handOff(ctx, goal, rec, &ep, s, true, nil)
		}
		return e.report(ctx, goal, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane was resized to %dx%d, yet doorbells still find no input box (it measures %dx%d now).", core.RecoverColumns, core.RecoverRows, l.columns, l.rows), l.lines)
	}
	if !core.MayMove(l.code, l.columns, l.rows) {
		if l.code == "box_not_found" {
			return e.report(ctx, goal, rec, &ep, core.ReportBoxNotFound, fmt.Sprintf("Its pane measures %dx%d, large enough, yet doorbells find no input box.", l.columns, l.rows), l.lines)
		}
		return e.report(ctx, goal, rec, &ep, core.ReportUndelivered, fmt.Sprintf("Every doorbell since %s deferred with %s.", core.Ago(e.Now().Sub(l.since)), l.code), l.lines)
	}
	if ep.Attempts >= core.MaxAttempts {
		return e.report(ctx, goal, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane could not be recovered after %d attempts.", ep.Attempts), l.lines)
	}
	ep.Columns, ep.Rows = l.columns, l.rows
	inject, err := e.recover(ctx, goal, rec, &ep, s, l)
	return e.handOff(ctx, goal, rec, &ep, s, inject, err)
}

// handOff gives the mail to the injector once a step verified the pane,
// then places the pane. It runs after that step returned, so the injector
// takes the block's lock itself.
func (e Env) handOff(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, s Session, inject bool, err error) error {
	if err != nil {
		ep.Attempts++
		_ = e.Store.PutEpisode(*ep)
		return err
	}
	if !inject || ep.Taken {
		return nil
	}
	o := e.Inject(ctx, InjectRequest{Block: rec.BlockID, Handle: rec.Handle, Root: e.Paths.Root()})
	e.wdLog(goal, rec, "inject", ep.ID, string(o.Progress)+" "+o.Code)
	if o.Progress == core.Accepted {
		ep.Outcome = "recovered"
	}
	if err := e.Store.PutEpisode(*ep); err != nil {
		return err
	}
	if !ep.Placed {
		if err := e.place(ctx, goal, rec, ep, s); err != nil {
			return err
		}
	}
	if o.Progress == core.Accepted {
		e.wdLog(goal, rec, "episode-end", ep.ID, "recovered: mail delivered")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	return nil
}

// recover detaches rec's block and resizes it, once, under the injector's
// lock and after re-reading everything the decision rested on.
func (e Env) recover(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, s Session, l look) (inject bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
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
	// here; the capture after the resize is the gate that matters (verify).
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
	// The journal goes down before the detach, so a restart finds the block
	// either still placed (nothing happened) or detached (continue).
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	e.wdLog(goal, rec, "detach", ep.ID, fmt.Sprintf("from %s (was %dx%d, owner %q)", ep.Window, size.Columns, size.Rows, size.Owner))
	if err := e.Rex.DetachBlock(ctx, s.id, rec.BlockID); err != nil {
		return false, fmt.Errorf("detach: %w", err)
	}
	ep.Detached = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	return e.resize(ctx, goal, rec, ep)
}

// resize gives the detached block its recovery size; a terminal still owned
// by a client would ignore it, so that is checked first.
func (e Env) resize(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) (inject bool, err error) {
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return false, err
	}
	if size.Owner != "" {
		return false, fmt.Errorf("resize: the terminal is still owned by %s after detaching", size.Owner)
	}
	if err := e.Rex.ResizeBlock(ctx, rec.BlockID, core.RecoverColumns, core.RecoverRows); err != nil {
		return false, fmt.Errorf("resize: %w", err)
	}
	ep.Resized = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return false, err
	}
	e.wdLog(goal, rec, "resize", ep.ID, fmt.Sprintf("%dx%d requested", core.RecoverColumns, core.RecoverRows))
	return e.verify(ctx, goal, rec, ep)
}

// verify waits, briefly, for the agent to redraw at the new size; large
// enough with an empty box means the mail can be handed to the injector.
func (e Env) verify(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) (inject bool, err error) {
	var size rexSize
	var box screen.Draft
	// Bounded by polls, not the clock: tests freeze Env.Now.
	for left := int(settleFor / pollEvery); ; left-- {
		sz, err := e.Rex.Size(ctx, rec.BlockID)
		if err != nil {
			return false, err
		}
		vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
		if err != nil {
			return false, err
		}
		size, box = rexSize{sz.Columns, sz.Rows}, screen.Check(string(rec.Kind), vt)
		if box == screen.Typed {
			return false, e.report(ctx, goal, rec, ep, core.ReportDraft, "After the resize, its input box holds a draft, so no doorbell can be typed.", lastLines(vt, core.ReportLines))
		}
		if !core.TinyPane(size.columns, size.rows) && box == screen.Empty {
			break
		}
		if left <= 0 || !sleep(ctx, pollEvery) {
			e.wdLog(goal, rec, "verify", ep.ID, fmt.Sprintf("%dx%d, box %s: not ready", size.columns, size.rows, box))
			return false, nil
		}
	}
	e.wdLog(goal, rec, "verify", ep.ID, fmt.Sprintf("%dx%d, box empty", size.columns, size.rows))
	ep.Verified = true
	return true, e.Store.PutEpisode(*ep)
}

type rexSize struct{ columns, rows int }

// reconcile brings the journal up to what the block shows: a detach that
// took before the journal said so, a placement by the user, a block that
// sits where the watchdog wanted it.
func (e Env) reconcile(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode) error {
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	changed := false
	switch {
	case blk.Placement == "detached" || blk.WindowID == "":
		if !ep.Detached {
			ep.Detached, changed = true, true
			e.wdLog(goal, rec, "reconcile", ep.ID, "the block is detached; the journal did not say so")
		}
	case ep.Detached && !ep.Placed && !ep.Taken:
		changed = true
		if ep.Overflow != "" && blk.WindowID == ep.Overflow {
			ep.Placed = true
			e.wdLog(goal, rec, "reconcile", ep.ID, "the block sits in the overflow tab; the journal did not say so")
		} else {
			ep.Taken = true
			e.wdLog(goal, rec, "reconcile", ep.ID, "the user placed the block in "+blk.WindowID+"; it is theirs")
		}
	}
	if changed {
		return e.Store.PutEpisode(*ep)
	}
	return nil
}

// place puts the detached block in the session's overflow tab, which is not
// rendered until the user opens it, so the block keeps its size. The tab is
// journaled before the move, so only a block found there counts as placed;
// one found elsewhere was placed by the user. A failure is reported once
// and retried on the next look.
func (e Env) place(ctx context.Context, goal store.Record, rec store.Record, ep *core.Episode, s Session) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
	defer cancel()
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	if blk.WindowID != "" {
		if ep.Overflow != "" && blk.WindowID == ep.Overflow {
			ep.Placed = true
		} else {
			ep.Taken = true
			e.wdLog(goal, rec, "place", ep.ID, "the user placed the block in "+blk.WindowID+"; it is theirs")
		}
		return e.Store.PutEpisode(*ep)
	}
	overflow, anchor, shell, err := e.overflowTab(ctx, s.id)
	if err == nil {
		ep.Overflow = overflow
		err = e.Store.PutEpisode(*ep)
	}
	if err == nil {
		err = e.Rex.MoveBlock(ctx, s.id, rec.BlockID, anchor)
	}
	if err != nil {
		ep.Attempts++
		_ = e.Store.PutEpisode(*ep)
		if rerr := e.report(ctx, goal, rec, ep, core.ReportRecoveryFailed, "Its pane is detached from every window and could not be placed in the "+core.OverflowTab+" tab: "+err.Error()+". The watchdog retries; `rex move` places it by hand.", nil); rerr != nil {
			return rerr
		}
		return err
	}
	if shell != "" {
		if err := e.Rex.CloseBlock(ctx, shell); err != nil {
			e.wdLog(goal, rec, "warn", ep.ID, "the overflow tab's shell stays open: "+err.Error())
		}
	}
	ep.Placed = true
	e.wdLog(goal, rec, "place", ep.ID, "in "+overflow)
	return e.Store.PutEpisode(*ep)
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
		e.desktop("rex-amq-adapter", string(rec.Handle)+" waits on a permission prompt")
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
