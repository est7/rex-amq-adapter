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

// InjectRequest is what amq wake passes the injector. amq's notification
// payload is not used: it previews the sender's subject, which must not be
// typed into a prompt (see core.Doorbell).
type InjectRequest struct {
	Block  string
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
	o := e.inject(ctx, r)
	_ = e.Store.RecordInject(r.Handle, o, e.Now()) // the log is diagnostic; the marker is the contract
	return o
}

func (e Env) inject(ctx context.Context, r InjectRequest) core.Outcome {
	rec, ok, err := e.Store.Record(r.Block)
	switch {
	case err != nil:
		return deferredErr("state_unreadable", err)
	case !ok || rec.Handle != r.Handle || !rec.Live():
		return core.Outcome{Progress: core.Failed, Code: "not_adopted", Note: "no live record binds this handle to the block"}
	}
	if o, ok := e.checkBinding(ctx, rec); !ok {
		return o
	}
	if o, ok := e.clearLeftover(ctx, rec); !ok {
		return o
	}
	status, reports, err := e.Rex.ProgramStatus(ctx, r.Block)
	if err != nil {
		return rexOutcome(err)
	}
	vt, err := e.Rex.CaptureVT(ctx, r.Block)
	if err != nil {
		return rexOutcome(err)
	}
	_, trust := screen.TrustScreen(string(rec.Kind), vt)
	if o, ok := core.DecideWrite(core.Readiness{
		State:       core.AgentState(status.State),
		BlockedKind: status.Kind,
		Box:         screen.Check(string(rec.Kind), vt),
		Trust:       trust,
	}); !ok {
		return o
	}
	unread, err := amq.Unread(r.Root, string(r.Handle))
	if err != nil {
		return deferredErr("inbox_unreadable", err)
	}
	if len(unread) == 0 {
		return core.Outcome{Progress: core.Accepted, Code: "nothing_unread", Note: "the mail was drained before delivery"}
	}

	doorbell := core.Doorbell(len(unread), r.Handle, e.EnvCommand())
	// Recorded before the write: if this process dies after writing, the
	// next attempt still knows which text in the box is ours.
	if err := e.Store.SetPending(r.Block, doorbell); err != nil {
		return deferredErr("state_unwritable", err)
	}
	// One write, bracketed: Codex treats a burst of typed text followed by
	// Enter as a paste and swallows the Enter.
	if err := e.Rex.Write(ctx, r.Block, bracketedPasteStart+doorbell+bracketedPasteEnd+"\r"); err != nil {
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
func (e Env) checkBinding(ctx context.Context, rec store.Record) (core.Outcome, bool) {
	b, err := e.observeBinding(ctx, rec)
	if err != nil {
		return rexOutcome(err), false
	}
	return core.CheckBinding(b)
}

// observeBinding reads what CheckBinding decides on. Whether the agent
// process exited can only be probed for a local server.
func (e Env) observeBinding(ctx context.Context, rec store.Record) (core.Binding, error) {
	fg, ok, err := e.Rex.Foreground(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		return core.Binding{BlockGone: true}, nil
	}
	if err != nil {
		return core.Binding{}, err
	}
	b := core.Binding{RecordPID: rec.AgentPID, AgentGone: localServer(rec.Server) && processGone(rec.AgentPID)}
	if ok {
		b.ForegroundPID = fg.PID
	}
	return b, nil
}

// clearLeftover erases a doorbell an earlier attempt left in the box. Text in
// the box that is not exactly that doorbell is someone's draft and stays.
func (e Env) clearLeftover(ctx context.Context, rec store.Record) (core.Outcome, bool) {
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
func (e Env) erase(ctx context.Context, rec store.Record) (core.Outcome, bool) {
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
func (e Env) confirm(ctx context.Context, rec store.Record, doorbell string, reports bool) core.Outcome {
	c := core.Confirmation{Reports: reports}
	empties := 0
	for deadline := time.Now().Add(confirmFor); time.Now().Before(deadline) && !c.Started && !c.EmptiedTwice; {
		if !sleep(ctx, pollEvery) {
			break
		}
		st, ok, err := e.Rex.ProgramStatus(ctx, rec.BlockID)
		if err == nil && ok && st.State == string(core.StateWorking) {
			// Only working counts: an agent at rest may refresh its status
			// without starting, and no turn completes inside one poll interval.
			c.Started = true
			break
		}
		if !reports {
			if vt, err := e.Rex.CaptureVT(ctx, rec.BlockID); err == nil && screen.Check(string(rec.Kind), vt) == screen.Empty {
				empties++
			} else {
				empties = 0
			}
			c.EmptiedTwice = empties >= 2
		}
	}
	if !c.Started && !c.EmptiedTwice {
		vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
		if err != nil {
			return rexOutcome(err) // pending stays: the next attempt clears it
		}
		c.Box = screen.Check(string(rec.Kind), vt)
		c.OursInBox = screen.BoxHolds(string(rec.Kind), vt, doorbell)
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
		return core.Outcome{Progress: core.Failed, Code: "block_gone", Note: err.Error()}
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
