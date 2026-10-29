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
	if err != nil || !ok || !rec.Live() || rec.Kind.InProcess() {
		return err
	}
	if err := e.prepareInjector(); err != nil {
		return err
	}
	_, _, err = amq.Ensure(ctx, e.AmqBin, e.waker(rec), e.Paths.Logs())
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
	return e.park(ctx, rec)
}

// park retires the block's waker and marks the record departed.
func (e Env) park(ctx context.Context, rec store.Record) error {
	if _, err := amq.RetireFor(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), rec.BlockID); err != nil {
		return err
	}
	now := e.Now().UTC()
	rec.DepartedAt = &now
	return e.Store.PutRecord(rec)
}

// Collect reconciles records with Rex: an agent whose block is gone or whose
// process exited departed without a SessionEnd (it crashed, or its pane was
// closed) and is parked like a departure; a departed record past its
// reservation is deleted. An agent that is alive but not in the foreground
// (an editor it opened) stays live, and a server that cannot be reached
// leaves its records alone: absence cannot be proven.
func (e Env) Collect(ctx context.Context) error {
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
		return nil
	}
	on := e
	on.Rex = e.RexOn(rec.Server)
	b, err := on.observeBinding(ctx, rec)
	if err != nil {
		return nil // unreachable or failing server: cannot prove the agent is gone
	}
	if b.BlockGone || (b.ForegroundPID != rec.AgentPID && b.AgentGone) {
		return e.park(ctx, rec)
	}
	return nil
}
