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

// The orchestrator watchdog runs inside the orchestrator watcher's tick (orchestrator.go). For every
// live agent of the orchestrator's session it looks for mail the injector could not
// deliver for StuckFor, and acts only on the one shape it can fix without
// the user: a pane too small to draw the input box. Without touching what
// the user looks at, it detaches that block from its window (the program
// keeps running and no client owns the terminal any more), resizes it to
// RecoverColumns x RecoverRows, which a terminal nobody owns takes at once,
// verifies the size and an empty box, hands the mail to the injector, and
// places the block in the session's overflow tab, where it keeps that size
// until the user opens the tab. Every other stuck shape it only reports to
// the orchestrator: a trust or permission dialog, a draft in the box, a box
// that stays missing at a normal size, a pane the user moved meanwhile, a
// recovery that did not take.

// actBudget bounds one layout action sequence. It is cut from the parent's
// cancellation, not its deadline: the watcher's tick budget (watchBudget)
// must not leave a block detached halfway.
const actBudget = 20 * time.Second

// settleFor bounds waiting for the agent to redraw after a resize.
const settleFor = 3 * time.Second

// watchdog runs one look over every live agent in s. Errors are reported and
// swallowed: a failing watchdog must not stop the orchestrator watcher.
func (e Env) watchdog(ctx context.Context, orch store.Record, s Session) {
	recs, err := e.Store.Records()
	if err != nil {
		e.wdLog(orch, store.Record{}, "records", "", err.Error())
		return
	}
	for _, r := range recs {
		if !r.Live() || !s.holds(r) {
			continue
		}
		if !r.Kind.InProcess() {
			if err := e.watchPeer(ctx, orch, s, r); err != nil {
				e.wdLog(orch, r, "error", "", err.Error())
			}
		}
		if err := e.health(ctx, orch, s, r); err != nil {
			e.wdLog(orch, r, "error", "", err.Error())
		}
		if err := e.blocked(ctx, orch, s, r); err != nil {
			e.wdLog(orch, r, "error", "", err.Error())
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

// watchPeer decides and acts for one agent. Whatever a step did, the look
// ends by settling the episode: a block out of every window is placed again,
// and an episode ends only once its block is in a window.
func (e Env) watchPeer(ctx context.Context, orch store.Record, s Session, rec store.Record) (err error) {
	defer func() {
		if serr := e.settleEpisode(ctx, orch, rec, s); serr != nil && err == nil {
			err = serr
		}
	}()
	return e.stepPeer(ctx, orch, s, rec)
}

func (e Env) stepPeer(ctx context.Context, orch store.Record, s Session, rec store.Record) error {
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
		// The journal says what was attempted; the block says what took,
		// whoever's run the episode was: a detach it did not journal still
		// leaves a block to place.
		if err := e.reconcile(ctx, orch, rec, &ep); err != nil {
			return err
		}
	}
	if open && !ep.Of(rec.Handle, rec.AgentPID) {
		if ep.Placement == core.Detached {
			return nil // settled (placed, then ended) before this agent gets one
		}
		e.wdLog(orch, rec, "episode-end", ep.ID, "another agent runs in the block")
		if err := e.Store.ClearEpisode(rec.BlockID); err != nil {
			return err
		}
		open = false
	}
	if open && (len(unread) == 0 || core.Delivered(history)) {
		return nil // the mail arrived: settling ends the episode
	}
	l, stuck, err := e.observe(ctx, rec, len(unread), history)
	if err != nil || !stuck {
		return err
	}
	if !open {
		now := e.Now()
		ep = core.Episode{ID: fmt.Sprintf("%s-%d", rec.Handle, now.Unix()), Handle: rec.Handle, Block: rec.BlockID, PID: rec.AgentPID, Session: s.id, StartedAt: now, Reported: map[string]time.Time{}}
	}
	e.wdLog(orch, rec, "observe", ep.ID, fmt.Sprintf("unread %d, deferred %s since %s, %dx%d owner %q in %q, status %s/%s, box %s", l.unread, l.code, core.Ago(e.Now().Sub(l.since)), l.columns, l.rows, l.owner, l.window, l.status, l.kind, l.box))

	switch {
	case ep.Placement == core.Theirs:
		return e.report(ctx, orch, s, rec, &ep, core.ReportMovedByUser, fmt.Sprintf("Its pane is no longer where the watchdog put it (the %s tab), so the watchdog leaves it alone.", core.OverflowTab), l.lines)
	// Zero keystrokes on any dialog or draft. A trust screen or a blocked
	// prompt is the blocked look's to report (blocked.go), mail or not.
	case l.trust != "", l.status == core.StateBlocked:
		return nil
	case l.box == screen.Typed:
		return e.report(ctx, orch, s, rec, &ep, core.ReportDraft, "Its input box holds a draft, so no doorbell can be typed.", l.lines)
	case ep.Moved() && l.code == core.CodeBoxNotFound:
		return e.continueRecovery(ctx, orch, s, rec, &ep, l)
	}
	if !core.MayMove(l.code, l.columns, l.rows) {
		if l.code == core.CodeBoxNotFound {
			return e.report(ctx, orch, s, rec, &ep, core.ReportBoxNotFound, fmt.Sprintf("Its pane measures %dx%d, large enough, yet doorbells find no input box.", l.columns, l.rows), l.lines)
		}
		return e.report(ctx, orch, s, rec, &ep, core.ReportUndelivered, fmt.Sprintf("Every doorbell since %s deferred with %s.", core.Ago(e.Now().Sub(l.since)), l.code), l.lines)
	}
	if ep.Attempts >= core.MaxAttempts {
		return e.report(ctx, orch, s, rec, &ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane could not be recovered after %d attempts.", ep.Attempts), l.lines)
	}
	v, err := e.recover(ctx, orch, rec, &ep, s, l)
	return e.handOff(ctx, orch, rec, &ep, v, err)
}

// continueRecovery continues a recovery whose block the watchdog moved and whose mail
// is still undelivered: resize it if that never took and nobody renders it,
// verify it, and hand the mail over. A recovery already reported failed, or
// out of attempts, is left to the injector, which keeps ringing on its own.
func (e Env) continueRecovery(ctx context.Context, orch store.Record, s Session, rec store.Record, ep *core.Episode, l look) error {
	if _, failed := ep.Reported[core.ReportRecoveryFailed]; failed {
		return nil
	}
	if ep.Attempts >= core.MaxAttempts {
		return e.report(ctx, orch, s, rec, ep, core.ReportRecoveryFailed, fmt.Sprintf("Its pane could not be recovered after %d attempts.", ep.Attempts), l.lines)
	}
	var v verdict
	var err error
	if !ep.Resized && l.owner == "" {
		v, err = e.resize(ctx, orch, rec, ep)
	} else {
		v, err = e.verify(ctx, orch, rec, ep.ID)
	}
	if err == nil && v == notReady {
		detail := fmt.Sprintf("A client renders its pane at %dx%d, yet doorbells still find no input box.", l.columns, l.rows)
		if ep.Resized {
			detail = fmt.Sprintf("Its pane was resized to %dx%d, yet doorbells still find no input box (it measures %dx%d now).", core.RecoverColumns, core.RecoverRows, l.columns, l.rows)
		}
		return e.report(ctx, orch, s, rec, ep, core.ReportRecoveryFailed, detail, l.lines)
	}
	return e.handOff(ctx, orch, rec, ep, v, err)
}

// handOff gives the mail to the injector once a step found the pane ready;
// a step that failed counts an attempt. It runs after that step returned, so
// the injector takes the block's lock itself. Placement and the end of the
// episode are settleEpisode's.
func (e Env) handOff(ctx context.Context, orch store.Record, rec store.Record, ep *core.Episode, v verdict, err error) error {
	if err != nil {
		ep.Attempts++
		_ = e.Store.PutEpisode(*ep)
		return err
	}
	if v != ready {
		return nil
	}
	o := e.Inject(ctx, InjectRequest{Block: rec.BlockID, Handle: rec.Handle, Root: e.Paths.Root()})
	e.wdLog(orch, rec, "inject", ep.ID, string(o.Progress)+" "+o.Code)
	return nil
}

// settleEpisode ends every look at rec: a block out of every window is
// placed (one try per look; a failure counts and is retried next look), and
// an episode whose mail arrived, or that another agent's run left, ends once
// its block is in a window.
func (e Env) settleEpisode(ctx context.Context, orch, rec store.Record, s Session) error {
	ep, open, err := e.Store.Episode(rec.BlockID)
	if err != nil || !open {
		return err
	}
	if ep.Placement == core.Detached {
		if err := e.place(ctx, orch, rec, &ep, s); err != nil {
			return err
		}
	}
	if !ep.Of(rec.Handle, rec.AgentPID) {
		e.wdLog(orch, rec, "episode-end", ep.ID, "another agent runs in the block")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return err
	}
	history, err := e.Store.InjectHistory(rec.Handle)
	if err != nil {
		return err
	}
	if len(unread) == 0 || core.Delivered(history) {
		e.wdLog(orch, rec, "episode-end", ep.ID, "mail delivered")
		return e.Store.ClearEpisode(rec.BlockID)
	}
	return nil
}

// recover detaches rec's block and resizes it, once, under the injector's
// lock and after re-reading everything the decision rested on.
func (e Env) recover(ctx context.Context, orch store.Record, rec store.Record, ep *core.Episode, s Session, l look) (v verdict, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
	defer cancel()
	unlock, err := e.Store.LockInject(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	defer unlock()
	// Precondition, re-read under the lock.
	cur, ok, err := e.Store.Record(rec.BlockID)
	if err != nil {
		return notReady, err
	}
	if !ok || cur.Handle != rec.Handle || cur.AgentPID != rec.AgentPID || cur.AgentSessionID != rec.AgentSessionID || !cur.Live() {
		e.wdLog(orch, rec, "abort", ep.ID, "the record changed under the lock")
		return notReady, nil
	}
	if o, ok := e.checkBinding(ctx, rec); !ok {
		e.wdLog(orch, rec, "abort", ep.ID, "the pane does not show the agent: "+o.Code)
		return notReady, nil
	}
	status, _, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	if st := core.AgentState(status.State); st == core.StateWorking || st == core.StateBlocked {
		e.wdLog(orch, rec, "abort", ep.ID, "the agent is "+status.State)
		return notReady, nil
	}
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	if !core.TinyPane(size.Columns, size.Rows) {
		e.wdLog(orch, rec, "abort", ep.ID, fmt.Sprintf("the pane grew to %dx%d on its own", size.Columns, size.Rows))
		return notReady, nil
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	// At this size the box cannot be seen, so a draft cannot be ruled out
	// here; the capture after the resize is the gate that matters (verify).
	if _, trust := screen.TrustScreen(string(rec.Kind), vt); trust || screen.Check(string(rec.Kind), vt) == screen.Typed {
		e.wdLog(orch, rec, "abort", ep.ID, "a dialog or draft appeared")
		return notReady, nil
	}
	if pending, ok, _ := e.Store.Pending(rec.BlockID); ok && pending != "" {
		e.wdLog(orch, rec, "abort", ep.ID, "the injector has a doorbell pending in this box")
		return notReady, nil
	}
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	if blk.WindowID != l.window {
		e.wdLog(orch, rec, "abort", ep.ID, "the pane moved between the look and the lock")
		return notReady, nil
	}
	ep.Window = blk.WindowID
	// The journal goes down before the detach, so a restart finds the block
	// either still placed (nothing happened) or detached (continue).
	if err := e.Store.PutEpisode(*ep); err != nil {
		return notReady, err
	}
	e.wdLog(orch, rec, "detach", ep.ID, fmt.Sprintf("from %s (was %dx%d, owner %q)", ep.Window, size.Columns, size.Rows, size.Owner))
	if err := e.Rex.DetachBlock(ctx, s.id, rec.BlockID); err != nil {
		return notReady, fmt.Errorf("detach: %w", err)
	}
	ep.Placement = core.Detached
	if err := e.Store.PutEpisode(*ep); err != nil {
		return notReady, err
	}
	return e.resize(ctx, orch, rec, ep)
}

// resize gives the detached block its recovery size; a terminal still owned
// by a client would ignore it, so that is checked first.
func (e Env) resize(ctx context.Context, orch store.Record, rec store.Record, ep *core.Episode) (verdict, error) {
	size, err := e.Rex.Size(ctx, rec.BlockID)
	if err != nil {
		return notReady, err
	}
	if size.Owner != "" {
		return notReady, fmt.Errorf("resize: the terminal is still owned by %s after detaching", size.Owner)
	}
	if err := e.Rex.ResizeBlock(ctx, rec.BlockID, core.RecoverColumns, core.RecoverRows); err != nil {
		return notReady, fmt.Errorf("resize: %w", err)
	}
	ep.Resized = true
	if err := e.Store.PutEpisode(*ep); err != nil {
		return notReady, err
	}
	e.wdLog(orch, rec, "resize", ep.ID, fmt.Sprintf("%dx%d requested", core.RecoverColumns, core.RecoverRows))
	return e.verify(ctx, orch, rec, ep.ID)
}

// verdict is what verify saw of a pane after a layout step.
type verdict int

const (
	// notReady: still too small, or no empty box yet.
	notReady verdict = iota
	// ready: large enough with an empty box; the mail can be handed over.
	ready
	// draftShown: the box holds a draft; the next look's observe reports
	// it, by then with the block placed.
	draftShown
)

// verify waits, briefly, for the agent to redraw at its size: large enough
// with an empty box is ready; a draft is never typed over. It only observes:
// it may run under the injector's lock, before the block is placed.
func (e Env) verify(ctx context.Context, orch store.Record, rec store.Record, epID string) (verdict, error) {
	var size rexSize
	var box screen.Draft
	// Bounded by polls, not the clock: tests freeze Env.Now.
	for left := int(settleFor / pollEvery); ; left-- {
		sz, err := e.Rex.Size(ctx, rec.BlockID)
		if err != nil {
			return notReady, err
		}
		vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
		if err != nil {
			return notReady, err
		}
		size, box = rexSize{sz.Columns, sz.Rows}, screen.Check(string(rec.Kind), vt)
		if box == screen.Typed {
			e.wdLog(orch, rec, "verify", epID, fmt.Sprintf("%dx%d, a draft in the box", sz.Columns, sz.Rows))
			return draftShown, nil
		}
		if !core.TinyPane(size.columns, size.rows) && box == screen.Empty {
			break
		}
		if left <= 0 || !sleep(ctx, pollEvery) {
			e.wdLog(orch, rec, "verify", epID, fmt.Sprintf("%dx%d, box %s: not ready", size.columns, size.rows, box))
			return notReady, nil
		}
	}
	e.wdLog(orch, rec, "verify", epID, fmt.Sprintf("%dx%d, box empty", size.columns, size.rows))
	return ready, nil
}

type rexSize struct{ columns, rows int }

// reconcile brings the journal up to what the block shows: a detach that
// took before the journal said so, a block that sits in the overflow tab, a
// block the user placed elsewhere (theirs from then on).
func (e Env) reconcile(ctx context.Context, orch store.Record, rec store.Record, ep *core.Episode) error {
	if ep.Placement == core.Theirs {
		return nil
	}
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	next := found(*ep, blk.WindowID, blk.Placement == "detached")
	if next == ep.Placement {
		return nil
	}
	e.wdLog(orch, rec, "reconcile", ep.ID, fmt.Sprintf("the block is %s; the journal said %s", placementName(next), placementName(ep.Placement)))
	ep.Placement = next
	return e.Store.PutEpisode(*ep)
}

// found is where ep's block is, read from Rex: out of every window, in the
// overflow tab chosen for it, still in its own window when nothing was
// moved, and otherwise wherever the user put it.
func found(ep core.Episode, window string, detached bool) core.Placement {
	switch {
	case detached || window == "":
		return core.Detached
	case ep.Placement == core.InWindow:
		return core.InWindow
	case ep.Overflow != "" && window == ep.Overflow:
		return core.InOverflow
	default:
		return core.Theirs
	}
}

func placementName(p core.Placement) string {
	if p == core.InWindow {
		return "in its window"
	}
	return string(p)
}

// place puts the detached block in the session's overflow tab, which is not
// rendered until the user opens it, so the block keeps its size. The tab is
// journaled before the move, so only a block found there counts as placed;
// one found elsewhere was placed by the user. A failure is reported once
// under its own code, closes the shell a new tab was made with, and is
// retried on every look; it spends none of the recovery's attempts.
func (e Env) place(ctx context.Context, orch store.Record, rec store.Record, ep *core.Episode, s Session) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actBudget)
	defer cancel()
	blk, err := e.Rex.InspectBlock(ctx, rec.BlockID)
	if err != nil {
		return err
	}
	if blk.WindowID != "" {
		ep.Placement = found(*ep, blk.WindowID, false)
		if ep.Placement == core.Theirs {
			e.wdLog(orch, rec, "place", ep.ID, "the user placed the block in "+blk.WindowID+"; it is theirs")
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
	if shell != "" {
		// The tab's first shell only anchored the move; a later look finds
		// the tab by its label, or makes it again.
		if cerr := e.Rex.CloseBlock(ctx, shell); cerr != nil {
			e.wdLog(orch, rec, "warn", ep.ID, "the overflow tab's shell stays open: "+cerr.Error())
		}
	}
	if err != nil {
		if rerr := e.report(ctx, orch, s, rec, ep, core.ReportPlaceFailed, "Its pane is detached from every window and could not be placed in the "+core.OverflowTab+" tab: "+err.Error()+". The watchdog retries; `rex move` places it by hand.", nil); rerr != nil {
			return rerr
		}
		return err
	}
	ep.Placement = core.InOverflow
	e.wdLog(orch, rec, "place", ep.ID, "in "+overflow)
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

// report tells the orchestrator once per code per episode, in the stuck
// peer's name; a report about the orchestrator's own pane goes where
// findingRecipient says, since its own inbox is the one that is stuck.
func (e Env) report(ctx context.Context, orch store.Record, s Session, rec store.Record, ep *core.Episode, code, detail string, lines []string) error {
	if ep.Reported == nil {
		ep.Reported = map[string]time.Time{}
	}
	if _, done := ep.Reported[code]; done {
		return nil
	}
	subject, body := core.WatchdogReport(peerOf(rec), *ep, code, detail, lines)
	to, ok := e.findingRecipient(s, rec, orch)
	if !ok {
		e.desktop("rex-amq-adapter", string(rec.Handle)+": "+code+", and no agent of another model family is in the session")
		e.wdLog(orch, rec, "report-desktop", ep.ID, code+": no other family in the session")
		ep.Reported[code] = e.Now()
		return e.Store.PutEpisode(*ep)
	}
	if to != orch.Handle {
		body += "\n\nSent to you because the finding is about the orchestrator " + string(orch.Handle) + " itself."
	}
	if err := e.sendSystem(ctx, rec.Handle, to, subject, body); err != nil {
		return err
	}
	ep.Reported[code] = e.Now()
	if err := e.Store.PutEpisode(*ep); err != nil {
		return err
	}
	e.wdLog(orch, rec, "report", ep.ID, code+": "+detail)
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
func (e Env) wdLog(orch, rec store.Record, step, episode, detail string) {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "orchestrator": orch.Handle, "handle": rec.Handle, "block": rec.BlockID, "step": step, "detail": detail}
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
