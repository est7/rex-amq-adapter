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

// superviseFor bounds waker supervision inside a Stop hook, leaving the rest
// of the hook's budget when another agent's adoption holds the lifecycle lock.
const superviseFor = 1500 * time.Millisecond

// finishDepartureFor bounds one retry of an unfinished departure: collection
// runs inside every adoption's hook budget.
const finishDepartureFor = time.Second

// waker is the amq waker that wakes rec's agent.
func (e Env) waker(rec store.Record) amq.Waker {
	return amq.Waker{InjectVia: e.injector(), Server: rec.Server, Block: rec.BlockID, Handle: string(rec.Handle), Root: e.Paths.Root()}
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
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil || !ok || !rec.Live() {
		return err
	}
	if err := e.ensureOrchestratorWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	return e.ensureWaker(ctx, rec)
}

// ensureWaker brings back rec's waker if it died. An in-process agent has none.
func (e Env) ensureWaker(ctx context.Context, rec store.Record) error {
	if rec.Kind.InProcess() {
		return nil
	}
	if err := e.prepareInjector(); err != nil {
		return err
	}
	_, _, err := amq.Ensure(ctx, e.AmqBin, e.waker(rec), e.Paths.Logs())
	return err
}

// Depart retires the waker of the agent that ended in loc and keeps its
// record as a reservation for its session. A SessionEnd from an agent session other than the
// recorded one (a late hook from a replaced agent) changes nothing.
func (e Env) Depart(ctx context.Context, loc Location, in HookInput) error {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil || !ok || !rec.Live() {
		return err
	}
	if in.SessionID != "" && rec.AgentSessionID != "" && in.SessionID != rec.AgentSessionID {
		return nil
	}
	return e.park(ctx, rec, store.DepartSessionEnd)
}

// park retires the handle's waker and marks the record departed, for reason
// (store.Record.DepartReason). A moved job whose adoption failed after its
// record took the new block still has a waker targeting the old one, so the
// handle's waker is retired whichever block it wakes (amq.RetireHandle).
// What follows can fail without undoing that: the departure stands, and
// collection finishes it.
func (e Env) park(ctx context.Context, rec store.Record, reason string) error {
	if !rec.Kind.InProcess() {
		// amq wake retire checks the waker's --inject-via path exists; a
		// wrapper that was removed (or never written, as when the adoption
		// failed on it) would make the retire fail and keep the waker. A
		// failure here is returned, so the record stays live and the next
		// hook or collection tries again.
		if err := e.prepareInjector(); err != nil {
			return fmt.Errorf("restore the injector wrapper to retire %s's waker: %w", rec.Handle, err)
		}
	}
	if _, err := amq.RetireHandle(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle)); err != nil {
		return err
	}
	now := e.Now().UTC()
	rec.DepartedAt = &now
	rec.DepartReason = reason
	rec.Unfinished = store.Departing
	if err := e.Store.PutRecord(rec); err != nil {
		return err
	}
	e.leaveSession(ctx, rec)
	// A tab it shared may now hold one agent, whose label it then follows.
	// The tab is on the record's server, which collection may not be on.
	on := e
	if e.RexOn != nil && rec.Server != "" {
		on.Rex = e.RexOn(rec.Server)
	}
	on.followTab(ctx, rec.SessionID, rec.WindowID)
	if err := e.finishDeparture(ctx, rec); err != nil {
		fmt.Fprintf(Diagnostics, "finish the departure of %s: %v\n", rec.Handle, err)
	}
	return nil
}

// finishDeparture does what a departure must not lose. Nothing else would ever
// tell an idle orchestrator, so this runs until it succeeds, which may tell it
// twice. Notes about leaving are advice and are not repeated here.
func (e Env) finishDeparture(ctx context.Context, rec store.Record) error {
	// Notes about who joined are about the session it was in; a resume may
	// be elsewhere, much later.
	if err := e.clearWindowNotes(ctx, rec.Handle); err != nil {
		return err
	}
	if err := e.wakeForDeparture(ctx, rec); err != nil {
		return err
	}
	rec.Unfinished = ""
	return e.Store.PutRecord(rec)
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
// it is advice: a failure is reported, and the departure stands.
func (e Env) leaveSession(ctx context.Context, rec store.Record) {
	if !placed(rec) {
		return
	}
	recs, err := e.Store.Records()
	if err != nil {
		fmt.Fprintf(Diagnostics, "tell the session that %s left: %v\n", rec.Handle, err)
		return
	}
	note := core.LeftNotice(peerOf(rec))
	for _, r := range recs {
		if r.Live() && r.Handle != rec.Handle && together(r, rec) {
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
// cannot be proven.
func (e Env) Collect(ctx context.Context) error {
	return e.collect(ctx, func(store.Record) bool { return true })
}

// collect reconciles the records covers selects. A failed record does not
// stop the rest.
func (e Env) collect(ctx context.Context, covers func(store.Record) bool) error {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := e.Store.Records()
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

func (e Env) collectOne(ctx context.Context, rec store.Record) error {
	if !rec.Live() {
		if e.Now().Sub(*rec.DepartedAt) > ReserveFor {
			return e.Store.DeleteRecord(rec.Handle)
		}
		if rec.Unfinished == store.Departing {
			ctx, cancel := context.WithTimeout(ctx, finishDepartureFor)
			defer cancel()
			return e.finishDeparture(ctx, rec)
		}
		return nil
	}
	_, err := e.parkIfGone(ctx, rec)
	return err
}

// parkIfGone parks the live agent of rec if Rex shows it is gone. observed is
// false when Rex could not say: an unreachable or failing server cannot prove
// the agent is gone, and the record stands.
func (e Env) parkIfGone(ctx context.Context, rec store.Record) (observed bool, err error) {
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
