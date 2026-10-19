package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Diagnostics receives problems that do not fail the entry point and must
// not reach the agent's context. Hooks point it at logs/hooks.log, because
// they run with stderr discarded.
var Diagnostics io.Writer = os.Stderr

// ReserveFor is how long a departed agent's handle stays reserved.
const ReserveFor = 24 * time.Hour

// Depart retires the waker of the agent that ended in loc and keeps its
// record as a reservation. A SessionEnd from an agent session other than the
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
			return e.Store.DeleteRecord(rec.BlockID)
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
