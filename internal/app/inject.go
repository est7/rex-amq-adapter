package app

import (
	"context"
	"errors"
	"syscall"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// InjectRequest is what amq wake passes the injector: the handle to wake and
// its mailbox root. Where the handle runs is its lease's, read when the
// doorbell fires, so a waker survives its agent moving between blocks. amq's
// notification payload is not used: it previews the sender's subject, which
// must not be typed into a prompt (see core.Doorbell).
type InjectRequest struct {
	Handle core.Handle
	Root   string
}

const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
	// ConfirmBudget bounds confirming and cleaning up after a write. It is
	// separate from the caller's deadline so a slow observation before the
	// write cannot leave a doorbell in the box uncleaned.
	ConfirmBudget = 4 * time.Second
	confirmFor    = 3 * time.Second
	pollEvery     = 150 * time.Millisecond
)

// Inject types a doorbell into an agent at rest and confirms it started.
// Every outcome, including errors, is reported as a progress marker: amq
// needs one to decide whether to retry.
func (e Env) Inject(ctx context.Context, r InjectRequest) core.Outcome {
	o := e.injectLeased(ctx, r)
	_ = e.Store.RecordInject(r.Handle, o, e.Now()) // the log is diagnostic; the marker is the contract
	return o
}

// injectLeased finds the block r's handle runs in from its lease (spec §4.3),
// and rings the doorbell there under that block's lock. It only reads the
// lease: a holder found gone is the collector's to release.
func (e Env) injectLeased(ctx context.Context, r InjectRequest) core.Outcome {
	rec, o, ok := e.leasedAgent(r.Handle)
	if !ok {
		return o
	}
	if reason, gone := incarnationGone(rec.Server, rec.lease.Incarnation); gone {
		return core.Outcome{Progress: core.Failed, Code: "holder_gone", Note: "the lease's process is gone (" + reason + "); collection releases it"}
	}
	unlock, err := e.Store.LockInject(ctx, rec.BlockID)
	if err != nil {
		return deferredErr("inject_locked", err)
	}
	defer unlock()
	// The lease may have moved while this waited for the block: the block
	// locked is then not the agent's, and the doorbell is rung again there.
	now, o, ok := e.leasedAgent(r.Handle)
	if !ok {
		return o
	}
	if now.Server != rec.Server || now.BlockID != rec.BlockID {
		return core.Outcome{Progress: core.Deferred, Code: "lease_moved", Note: "the lease moved from " + rec.BlockID + " to " + now.BlockID}
	}
	on := e
	if e.RexOn != nil && now.Server != "" {
		on.Rex = e.RexOn(now.Server)
	}
	return on.inject(ctx, r, now)
}

// leasedAgent is h with its lease; ok is false with the outcome to report
// when h runs nowhere or its state cannot be read.
func (e Env) leasedAgent(h core.Handle) (agent, core.Outcome, bool) {
	rec, ok, err := e.agentOf(h)
	switch {
	case err != nil:
		return agent{}, deferredErr("state_unreadable", err), false
	case !ok || !rec.Live():
		return agent{}, core.Outcome{Progress: core.Failed, Code: "no_lease", Note: "the handle runs nowhere; its next adoption announces the mail"}, false
	}
	return rec, core.Outcome{}, true
}

func (e Env) inject(ctx context.Context, r InjectRequest, rec agent) core.Outcome {
	if o, ok := e.checkBinding(ctx, rec); !ok {
		return o
	}
	if o, ok := e.clearLeftover(ctx, rec); !ok {
		return o
	}
	snap, err := e.assessActivity(ctx, activityRequest{Agent: &rec, Block: rec.BlockID, Policy: FullScreen})
	if err != nil {
		var jerr jobStateError
		if errors.As(err, &jerr) {
			return deferredErr("state_unreadable", jerr.err)
		}
		return rexOutcome(err)
	}
	reports := snap.External.ReportsStatus()
	if o, ok := core.DecideWrite(snap.Activity); !ok {
		return o
	}
	unread, err := e.admit(ctx, rec)
	if err != nil {
		return deferredErr("inbox_unreadable", err)
	}
	if len(unread) == 0 {
		return core.Outcome{Progress: core.Accepted, Code: "nothing_unread", Note: "the mail was drained before delivery"}
	}

	doorbell := core.Doorbell(len(unread), r.Handle, e.SelfBin)
	// Recorded before the write: if this process dies after writing, the
	// next attempt still knows which text in the box is ours.
	if err := e.Store.SetPending(rec.BlockID, doorbell); err != nil {
		return deferredErr("state_unwritable", err)
	}
	// One write, bracketed: Codex treats a burst of typed text followed by
	// Enter as a paste and swallows the Enter.
	if err := e.Rex.Write(ctx, rec.BlockID, bracketedPasteStart+doorbell+bracketedPasteEnd+"\r"); err != nil {
		return rexOutcome(err)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ConfirmBudget)
	defer cancel()
	o := e.confirm(cctx, rec, doorbell, reports)
	if o.Progress == core.Accepted {
		e.markAnnounced(cctx, r.Root, r.Handle, unread)
	}
	return o
}

// checkBinding confirms the block still runs the adopted agent process.
func (e Env) checkBinding(ctx context.Context, rec agent) (core.Outcome, bool) {
	b, err := e.observeBinding(ctx, rec)
	if err != nil {
		return rexOutcome(err), false
	}
	return core.CheckBinding(b)
}

// observeBinding reads what CheckBinding decides on. The agent is gone when
// its lease's process is (a local server's pid exited, or names a process
// that started at another time), or when the lease's Rex server restarted
// since: a restored block keeps its id but runs none of the old processes.
// The server is not asked when the block's foreground is the lease's own
// live local process, which it shows whatever the server's instance.
func (e Env) observeBinding(ctx context.Context, rec agent) (core.Binding, error) {
	fg, ok, err := e.Rex.Foreground(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		return core.Binding{BlockGone: true}, nil
	}
	if err != nil {
		return core.Binding{}, err
	}
	var inc store.Incarnation
	if rec.lease != nil {
		inc = rec.lease.Incarnation
	}
	_, gone := incarnationGone(rec.Server, inc)
	b := core.Binding{RecordPID: rec.pid(), AgentGone: gone}
	if ok {
		b.ForegroundPID = fg.PID
	}
	shown := localServer(rec.Server) && b.RecordPID > 0 && b.ForegroundPID == b.RecordPID
	if !b.AgentGone && !shown {
		if b.AgentGone, err = e.serverRestarted(ctx, rec); err != nil {
			return core.Binding{}, err
		}
	}
	if rec.Kind == core.Claude && ok {
		if err := e.claudeBinding(ctx, rec, &b); err != nil {
			return core.Binding{}, err
		}
	}
	return b, nil
}

// serverRestarted reports whether rec's Rex server is another instance than
// the one its lease was acquired on. A lease that recorded none cannot tell.
func (e Env) serverRestarted(ctx context.Context, rec agent) (bool, error) {
	if rec.lease == nil || rec.lease.ServerInstance == "" {
		return false, nil
	}
	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return false, err
	}
	return srv.InstanceID != "" && srv.InstanceID != rec.lease.ServerInstance, nil
}

// clearLeftover erases a doorbell an earlier attempt left in the box. Text in
// the box that is not exactly that doorbell is someone's draft and stays.
func (e Env) clearLeftover(ctx context.Context, rec agent) (core.Outcome, bool) {
	pending, ok, err := e.Store.Pending(rec.BlockID)
	if err != nil {
		return deferredErr("state_unreadable", err), false
	}
	if !ok {
		return core.Outcome{}, true
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return rexOutcome(err), false
	}
	if screen.BoxHolds(string(rec.Kind), vt, pending) {
		if o, ok := e.erase(ctx, rec); !ok {
			return o, false
		}
	}
	if err := e.Store.ClearPending(rec.BlockID); err != nil {
		return deferredErr("state_unwritable", err), false
	}
	return core.Outcome{}, true
}

// erase clears the input box and verifies it emptied.
func (e Env) erase(ctx context.Context, rec agent) (core.Outcome, bool) {
	if err := e.Rex.SendKeys(ctx, rec.BlockID, "ctrl+u"); err != nil {
		return rexOutcome(err), false
	}
	if !sleep(ctx, pollEvery) {
		return deferredErr("cleanup_unverified", ctx.Err()), false
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return rexOutcome(err), false
	}
	if screen.Check(string(rec.Kind), vt) != screen.Empty {
		return core.Outcome{Progress: core.Deferred, Code: "cleanup_unverified", Note: "the input box did not empty after erasing the doorbell"}, false
	}
	return core.Outcome{}, true
}

// confirm waits for evidence that the doorbell was submitted, and erases a
// doorbell left unsubmitted in the box. The pending record is dropped once
// the doorbell is known to be submitted or erased.
func (e Env) confirm(ctx context.Context, rec agent, doorbell string, reports bool) core.Outcome {
	c := core.Confirmation{Reports: reports}
	empties := 0
	for deadline := time.Now().Add(confirmFor); time.Now().Before(deadline) && !c.Started && !c.EmptiedTwice; {
		if !sleep(ctx, pollEvery) {
			break
		}
		// Keep status/supervisor polling and the empty-box fallback independent
		// so a failing status RPC never blocks two-empty-box confirmation for a
		// statusless agent, and a failing VT capture never hides a working status.
		if snap, err := e.assessActivity(ctx, activityRequest{Agent: &rec, Block: rec.BlockID, Policy: NoScreen}); err == nil {
			if _, working := snap.Activity.(core.InTurn); working {
				// Only working counts: an agent at rest may refresh its status
				// without starting, and no turn completes inside one poll interval.
				c.Started = true
				break
			}
		}
		if !reports {
			if surf, _, err := e.inspectSurface(ctx, e.Rex, rec.Kind, rec.BlockID, ""); err == nil && surf.Box == screen.BoxEmpty {
				empties++
			} else {
				empties = 0
			}
			c.EmptiedTwice = empties >= 2
		}
	}
	if !c.Started && !c.EmptiedTwice {
		surf, _, err := e.inspectSurface(ctx, e.Rex, rec.Kind, rec.BlockID, doorbell)
		if err != nil {
			return rexOutcome(err) // pending stays: the next attempt clears it
		}
		c.Box = draftFromBox(surf.Box)
		c.OursInBox = surf.OursInBox
	}
	o, cleanup := core.DecideConfirm(c)
	if cleanup == core.ClearOurText {
		if eo, ok := e.erase(ctx, rec); !ok {
			return eo // pending stays: the next attempt clears it
		}
	}
	// Here the doorbell was submitted, erased, or is not in the box.
	_ = e.Store.ClearPending(rec.BlockID) // a stale pending only costs one extra screen check
	return o
}

func draftFromBox(box screen.BoxState) screen.Draft {
	switch box {
	case screen.BoxEmpty:
		return screen.Empty
	case screen.BoxDraft:
		return screen.Typed
	default:
		return screen.Unknown
	}
}

// markAnnounced records ids as announced, keeping only ids still unread.
// Failure to update the ledger only risks a repeated announcement.
func (e Env) markAnnounced(ctx context.Context, root string, h core.Handle, ids []string) {
	unlock, err := e.Store.LockLedger(ctx, h)
	if err != nil {
		return
	}
	defer unlock()
	prior, err := e.Store.Announced(h)
	if err != nil {
		return
	}
	still, err := amq.Unread(root, string(h))
	if err != nil {
		return
	}
	_ = e.Store.SetAnnounced(h, core.MergeAnnounced(prior.IDs, ids, still), e.Now())
}

// rexOutcome maps a rex failure: a vanished block is terminal for this
// cohort, anything else (server unreachable, timeout) is retried.
func rexOutcome(err error) core.Outcome {
	if errors.Is(err, rex.ErrNotFound) {
		return core.Outcome{Progress: core.Failed, Code: core.CodeBlockGone, Note: err.Error()}
	}
	return deferredErr("rex_error", err)
}

func deferredErr(code string, err error) core.Outcome {
	note := "unknown error"
	if err != nil {
		note = err.Error()
	}
	return core.Outcome{Progress: core.Deferred, Code: code, Note: note}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// localServer reports whether a Rex address is a Unix socket on this machine,
// where agent pids can be probed.
func localServer(server string) bool {
	return len(server) >= 7 && server[:7] == "unix://"
}

// processGone reports whether pid is known not to exist. Permission errors
// mean it exists.
func processGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
