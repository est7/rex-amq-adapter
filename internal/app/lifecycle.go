package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fsutil"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Diagnostics receives problems that do not fail the entry point and must
// not reach the agent's context. Hooks point it at logs/hooks.log, because
// they run with stderr discarded.
var Diagnostics io.Writer = os.Stderr

// ReserveFor is how long a departed agent's handle stays reserved for its
// session to resume.
const ReserveFor = 7 * 24 * time.Hour

// superviseFor bounds waker supervision inside a Stop hook and after a send.
const superviseFor = 1500 * time.Millisecond

// finishDepartureFor bounds one retry of an unfinished departure during
// collection.
const finishDepartureFor = time.Second

// waker is the amq waker that wakes rec's agent. It names the handle, not
// a block: the injector reads the handle's lease when a doorbell fires, so a
// lease that moves keeps its waker.
func (e Env) waker(rec agent) amq.Waker {
	return amq.Waker{InjectVia: e.injector(), Handle: string(rec.Handle), Root: e.Paths.Root()}
}

// injector is the --inject-via every waker runs: a wrapper in the private
// state directory that execs this binary. amq refuses an --inject-via under a
// group- or world-writable directory, which Homebrew's Cellar is; and the
// wrapper's path does not change when the binary moves (go install to brew,
// brew upgrade), so running wakers keep working and need no replacement.
func (e Env) injector() string { return filepath.Join(e.Paths.Home, "bin", "inject") }

// prepareInjector writes the wrapper for the binary that is running now, when
// it differs; it is normally a no-op.
func (e Env) prepareInjector() error {
	want := "#!/bin/sh\n# Written by rex-amq-adapter: amq wake runs this to deliver a doorbell.\nexec " + shellQuote(e.SelfBin) + " \"$@\"\n"
	path := e.injector()
	if b, err := os.ReadFile(path); err == nil && string(b) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(path, []byte(want)); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

// SuperviseWaker keeps the waker of the agent in loc alive. amq wake has no
// supervisor of its own, and an agent ending its turn is the moment it starts
// depending on its waker: a dead one would leave it deaf until it restarts.
// Keeping a live waker costs one amq wake check. A waker running an older
// adapter or amq binary is replaced on the way.
func (e Env) SuperviseWaker(ctx context.Context, loc Location) error {
	ctx, cancel := context.WithTimeout(ctx, superviseFor)
	defer cancel()
	rec, ok, err := e.agentIn(loc)
	if err != nil || !ok || !rec.Live() {
		return err
	}
	if err := e.ensureSessionWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	return e.ensureWaker(ctx, rec)
}

// ensureWaker brings back rec's waker if it died. An in-process agent has none.
func (e Env) ensureWaker(ctx context.Context, rec agent) error {
	_, err := e.reconcileWaker(ctx, rec.Handle)
	return err
}

// reconcileWaker makes h's waker follow h's lease, which the lifecycle lock
// guards and the waker is derived from: a live lease of an agent that is not
// in-process keeps exactly one live waker, and any other state (departed,
// in-process, forgotten) keeps none. It runs under h's own lock, and reads
// the lease under it, so of an adoption and a departure that race, the one
// that reconciles last leaves the waker matching the lease they both wrote
// (each writer reconciles after its write). The lifecycle lock is never held
// while waiting for it: amq wake check and start take seconds under load.
//
// Lock order: epic-lifecycle, epic-opening, handles/<h>, lifecycle, ledger.
// A holder of the lifecycle lock never waits for a handle's lock.
func (e Env) reconcileWaker(ctx context.Context, h core.Handle) (amq.WakeDecision, error) {
	unlock, err := e.Store.LockHandle(ctx, h)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return e.reconcileWakerHeld(ctx, h)
}

// reconcileWakerHeld is reconcileWaker for a caller holding h's lock.
func (e Env) reconcileWakerHeld(ctx context.Context, h core.Handle) (amq.WakeDecision, error) {
	rec, ok, err := e.agentOf(h)
	if err != nil {
		return 0, err
	}
	keep := ok && rec.Live() && !rec.Kind.InProcess()
	if !ok || !rec.Kind.InProcess() {
		// amq wake retire checks the waker's --inject-via path exists; a
		// wrapper that was removed (or never written, as when an adoption
		// failed on it) would make the retire fail and keep the waker.
		if err := e.prepareInjector(); err != nil {
			return 0, fmt.Errorf("write the injector wrapper for %s's waker: %w", h, err)
		}
	}
	if !keep {
		_, err := e.AMQ.RetireHandle(ctx, e.Paths.Root(), string(h))
		return 0, err
	}
	_, decision, err := e.AMQ.Ensure(ctx, e.waker(rec), e.Paths.Logs())
	return decision, err
}

// Depart releases the lease of the agent that ended in loc (by its epoch, I4)
// and retires its waker; its identity stays a reservation for its session.
// A SessionEnd from an agent session other than the recorded one (a late hook
// from a replaced agent), or that does not prove it comes from the lease's
// process (endsOwnLease), changes nothing: collection releases a lease whose
// process is gone.
func (e Env) Depart(ctx context.Context, loc Location, in HookInput) error {
	rec, ok, err := e.agentIn(loc)
	if err != nil || !ok || !rec.Live() {
		return err
	}
	if in.SessionID != "" && rec.Session() != "" && in.SessionID != rec.Session() {
		return nil
	}
	if !e.endsOwnLease(rec, in) {
		return nil
	}
	return e.park(ctx, rec, store.DepartSessionEnd)
}

// park retires rec's waker and ends its lease for reason
// (store.Placement.DepartReason), if rec still holds the binding it was read
// with, and settles the departure. A waker that cannot be retired fails the
// park, and the record stays live for the next hook or collection to try
// again. The release is the departure: what follows can fail without undoing
// it, and the departure stays unfinished for collection to finish. A binding
// that changed since rec was read (a takeover, a move, a new process, a new
// conversation in the same process) is left alone: whatever rec's reader
// observed was about another one.
func (e Env) park(ctx context.Context, rec agent, reason string) error {
	left, released, err := e.departHeld(ctx, rec, reason)
	if err != nil || !released {
		return err
	}
	e.settleDeparture(ctx, left)
	return nil
}

// departHeld, under rec's handle lock so no waker comes back in between,
// retires rec's waker and then, under the lifecycle lock, releases its lease
// as unfinished, each only while rec still holds the binding it was read
// with (sameBinding). A retire that succeeds before a binding that changed
// meanwhile is left to the newer binding's adoption, which reconciles its
// waker after this lock is released.
func (e Env) departHeld(ctx context.Context, rec agent, reason string) (agent, bool, error) {
	unlockHandle, err := e.Store.LockHandle(ctx, rec.Handle)
	if err != nil {
		return rec, false, err
	}
	defer unlockHandle()
	if now, ok, err := e.agentOf(rec.Handle); err != nil || !ok || !sameBinding(now, rec) {
		return rec, false, err
	}
	if !rec.Kind.InProcess() {
		if err := e.prepareInjector(); err != nil {
			return rec, false, fmt.Errorf("restore the injector wrapper to retire %s's waker: %w", rec.Handle, err)
		}
	}
	if _, err := e.AMQ.RetireHandle(ctx, e.Paths.Root(), string(rec.Handle)); err != nil {
		return rec, false, err
	}
	left, released, err := e.releaseBound(ctx, rec, reason)
	if !released {
		// The lease stays live (its binding changed, or the lock could not be
		// had): its waker follows it again, within a bound of its own, since
		// the caller's context may be what ended.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), superviseFor)
		defer cancel()
		if _, rerr := e.reconcileWakerHeld(rctx, rec.Handle); rerr != nil {
			fmt.Fprintf(Diagnostics, "bring back the waker of %s, whose lease stays: %v\n", rec.Handle, rerr)
		}
	}
	return left, released, err
}

// releaseBound releases rec's lease as departed for reason, unfinished,
// under the lifecycle lock, while rec still holds its binding.
func (e Env) releaseBound(ctx context.Context, rec agent, reason string) (agent, bool, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return rec, false, err
	}
	defer unlock()
	now, ok, err := e.agentOf(rec.Handle)
	if err != nil || !ok || !sameBinding(now, rec) {
		return rec, false, err
	}
	left, err := e.releaseLease(now, store.Departure{Reason: reason, At: e.Now().UTC(), Unfinished: true})
	return left, err == nil, err
}

// sameBinding reports whether now, read again, is still live in the binding
// rec was read with: the same lease and the same conversation. A process
// keeps its lease's epoch across a new conversation (/clear), so the lease
// alone does not tell the old conversation's SessionEnd from the new one.
func sameBinding(now, rec agent) bool {
	return now.Live() && rec.lease != nil && sameLease(*now.lease, *rec.lease) && now.Session() == rec.Session()
}

// sameLease reports whether a and b are one lease: one epoch, in one block,
// held by one process.
func sameLease(a, b store.Lease) bool {
	return a.Epoch == b.Epoch && a.Server == b.Server && a.BlockID == b.BlockID && a.Incarnation == b.Incarnation
}

// settleDeparture does what follows a released lease, outside the lifecycle
// lock: the session hears that left's agent left, a tab it shared follows
// the agent left in it, and the departure is finished.
func (e Env) settleDeparture(ctx context.Context, left agent) {
	e.leaveSession(ctx, left)
	// The tab is on the record's server, which collection may not be on.
	on := e
	if e.RexOn != nil && left.Server != "" {
		on.Rex = e.RexOn(left.Server)
	}
	on.followTab(ctx, left.SessionID, left.WindowID)
	if err := e.finishDeparture(ctx, left); err != nil {
		fmt.Fprintf(Diagnostics, "finish the departure of %s: %v\n", left.Handle, err)
	}
}

// finishDeparture does what a departure must not lose: its waker retired,
// its window notes cleared, and, unless it was quiet (quietDeparture), an
// idle orchestrator told and a personal agent's narrator mail delivered
// again. Nothing else would ever tell an idle orchestrator, so this runs
// until it succeeds, which may tell it twice. Notes about leaving are advice
// and are not repeated here. It runs under the handle's lock, so two
// finishers of one departure do not interleave, and only for the departure
// rec read (sameDeparture): a handle taken back, or forgotten and named
// afresh, since owes nothing for it.
func (e Env) finishDeparture(ctx context.Context, rec agent) error {
	unlockHandle, err := e.Store.LockHandle(ctx, rec.Handle)
	if err != nil {
		return err
	}
	defer unlockHandle()
	if _, err := e.reconcileWakerHeld(ctx, rec.Handle); err != nil {
		return fmt.Errorf("retire the waker: %w", err)
	}
	// A second finisher of the same departure (two collectors from one
	// snapshot) finds it finished and owes nothing.
	if now, ok, err := e.agentOf(rec.Handle); err != nil || !ok || !sameDeparture(now, rec) || !now.departure().Unfinished {
		return err
	}
	// Notes about who joined are about the session it was in; a resume may
	// be elsewhere, much later.
	if err := e.clearWindowNotes(ctx, rec.Handle); err != nil {
		return err
	}
	if !quietDeparture(rec.departReason()) {
		if err := e.wakeForDeparture(ctx, rec); err != nil {
			return err
		}
		// A personal agent's unread narrator mail goes to the Hub's personal
		// agent that is live, or waits for the next one (C26).
		if err := e.redeliverFrom(ctx, rec); err != nil {
			return err
		}
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if now, ok, err := e.agentOf(rec.Handle); err != nil || !ok || !sameDeparture(now, rec) {
		return err
	}
	return e.Store.FinishDeparture(rec.Handle)
}

// quietDeparture reports whether a departure for reason tells nobody: an
// Epic closed (everyone it would tell was in the session just killed) or a
// peer retired as finished (Retiring a peer).
func quietDeparture(reason string) bool {
	return reason == store.DepartEpicClosed || reason == store.DepartRetired
}

// sameDeparture reports whether now is still departed from the placement
// rec departed from. A handle forgotten and named afresh starts its epochs
// over, so the placement's start and the departure's time tell them apart.
func sameDeparture(now, rec agent) bool {
	a, b := now.departure(), rec.departure()
	return a != nil && b != nil && a.Epoch == b.Epoch && a.AcquiredAt.Equal(b.AcquiredAt) && a.DepartedAt.Equal(b.DepartedAt)
}

func (e Env) clearWindowNotes(ctx context.Context, h core.Handle) error {
	unlock, err := e.Store.LockLedger(ctx, h)
	if err != nil {
		return err
	}
	defer unlock()
	return e.Store.ClearWindowNotes(h)
}

// leaveSession queues, for each live agent recorded in rec's session, a note
// that rec's agent left. It reads only records, so a crash found by
// collection, whose block is gone, is told like a SessionEnd. Like a join
// it is advice: a failure is reported, and the departure stands. A Stealth
// agent (core.Stealth) neither is told nor tells.
func (e Env) leaveSession(ctx context.Context, rec agent) {
	if !placed(rec) || core.Stealth(rec.Role) {
		return
	}
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "tell the session that %s left: %v\n", rec.Handle, err)
		return
	}
	note := core.LeftNotice(peerOf(rec))
	for _, r := range recs {
		if r.Live() && r.Handle != rec.Handle && together(r, rec) && !core.Stealth(r.Role) {
			if err := e.noteWindow(ctx, r.Handle, note); err != nil {
				fmt.Fprintf(Diagnostics, "tell %s that %s left: %v\n", r.Handle, rec.Handle, err)
			}
		}
	}
}

// Collect reconciles records with Rex: an agent whose block is gone or whose
// process exited departed without a SessionEnd (it crashed, or its pane was
// closed) and is parked like a departure; a departed record past its
// reservation is deleted, and an unfinished departure is finished. An agent
// that is alive but not in the foreground (an editor it opened) stays live,
// and a server that cannot be reached leaves its records alone: absence
// cannot be proven. Rex is asked with no lock held; each change is made
// under the lifecycle lock only if the record is still what was observed.
func (e Env) Collect(ctx context.Context) error {
	return e.collect(ctx, func(agent) bool { return true })
}

// collect reconciles the records covers selects. A failed record does not
// stop the rest.
func (e Env) collect(ctx context.Context, covers func(agent) bool) error {
	recs, err := e.agents()
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range recs {
		if !covers(rec) {
			continue
		}
		if err := e.collectOne(ctx, rec); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rec.Handle, err))
		}
	}
	return errors.Join(errs...)
}

func (e Env) collectOne(ctx context.Context, rec agent) error {
	if !rec.Live() {
		if e.reservationOver(rec) {
			return e.forgetIfOver(ctx, rec.Handle)
		}
		if p := rec.departure(); p != nil && p.Unfinished {
			ctx, cancel := context.WithTimeout(ctx, finishDepartureFor)
			defer cancel()
			return e.finishDeparture(ctx, rec)
		}
		return nil
	}
	_, err := e.parkIfGone(ctx, rec)
	return err
}

// reservationOver reports whether the departed rec's handle is no longer
// reserved for a resume: ReserveFor after it departed, or after it was first
// adopted when an adoption died between writing the identity and acquiring
// its lease (it was never placed anywhere).
func (e Env) reservationOver(rec agent) bool {
	if rec.Live() {
		return false
	}
	if p := rec.departure(); p != nil {
		return e.Now().Sub(p.DepartedAt) > ReserveFor
	}
	return e.Now().Sub(rec.FirstAdoptedAt) > ReserveFor
}

// forgetIfOver deletes h's identity, under the lifecycle lock, when its
// record read there is still past its reservation: a resume, or a new
// identity of the same name, since the unlocked read keeps it.
func (e Env) forgetIfOver(ctx context.Context, h core.Handle) error {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	now, ok, err := e.agentOf(h)
	if err != nil || !ok || !e.reservationOver(now) {
		return err
	}
	return e.Store.ForgetIdentity(h)
}

// parkIfGone parks the live agent of rec if Rex shows it is gone. observed is
// false when Rex could not say: an unreachable or failing server cannot prove
// the agent is gone, and the record stands.
func (e Env) parkIfGone(ctx context.Context, rec agent) (observed bool, err error) {
	on := e
	on.Rex = e.RexOn(rec.Server)
	b, err := on.observeBinding(ctx, rec)
	if err != nil {
		return false, nil
	}
	if o, bound := core.CheckBinding(b); !bound && o.Progress == core.Failed {
		return true, e.park(ctx, rec, o.Code)
	}
	return true, nil
}
