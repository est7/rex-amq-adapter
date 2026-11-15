package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Retiring is how a session's agent closes a peer whose work is done. A raw
// `rex block close` looks like a crash to collection, which parks the record
// for block_gone and mails the orchestrator that the peer quit or crashed,
// and the narrator hears an urgent "left". A retire marks the record
// departed for store.DepartRetired first, telling nobody, and only then
// closes the block: the SessionEnd hook and collection that follow find a
// departed record and do nothing, and the narrator hears a routine
// "retired".

// Actions a retire reports.
const (
	RetireRetired         = "retired"
	RetireAlreadyDeparted = "already_departed"
)

// RetireRequest is what the caller asks.
type RetireRequest struct {
	Handle core.Handle
	// Force retires an agent that is working, waiting on a dialog, holds
	// unread mail, or reports no state.
	Force bool
}

// RetireResult is what was retired.
type RetireResult struct {
	Handle       core.Handle `json:"handle"`
	Block        string      `json:"block"`
	Action       string      `json:"action"`
	DepartReason string      `json:"depart_reason"`
}

// Retire retires r.Handle for the agent in loc: it marks the record departed
// for retired, retires its waker, clears its window notes, and closes its
// block on the record's server. It refuses a caller that is not an adopted
// agent, a target outside the caller's Rex session, the caller itself, and a
// narrator, personal, or orchestrator target; unless forced, also a target
// that is not at rest. A target already departed is a no-op
// (RetireAlreadyDeparted). When the block does not close, the departure
// stands and the error names the block to close by hand.
func (e Env) Retire(ctx context.Context, loc Location, r RetireRequest) (RetireResult, error) {
	res := RetireResult{Handle: r.Handle}
	if r.Handle == "" {
		return res, fmt.Errorf("%w: name the agent to retire", ErrUsage)
	}
	caller, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return res, err
	}
	if !ok || !caller.Adopted() {
		return res, errors.New("no AMQ identity for this pane, so nothing can retire an agent")
	}
	target, err := e.retireTarget(caller, r.Handle)
	if err != nil {
		return res, err
	}
	res.Block = target.BlockID
	on := e.Rex
	if e.RexOn != nil && target.Server != "" {
		on = e.RexOn(target.Server)
	}
	if target.Live() && !r.Force {
		why, err := e.unrest(ctx, on, target)
		if err != nil {
			return res, err
		}
		if why != "" {
			return res, fmt.Errorf("%w: not at rest (%s); wait for it, or pass --force to retire it anyway", ErrRefused, why)
		}
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, err
	}
	// The lock is held through the close, as an Epic's close holds it through
	// the kill: the target's own hooks, its SessionEnd included, wait and
	// then find a departed record.
	defer unlock()
	target, ok, err = e.recordByHandle(target.Handle)
	if err != nil {
		return res, err
	}
	if ok {
		res.Block = target.BlockID
	}
	if !ok || !target.Live() {
		res.Action = RetireAlreadyDeparted
		res.DepartReason = target.DepartReason
		return res, nil
	}
	if err := e.departQuietly(ctx, target, store.DepartRetired); err != nil {
		return res, fmt.Errorf("retire %s: %w", r.Handle, err)
	}
	res.Action, res.DepartReason = RetireRetired, store.DepartRetired
	if err := on.CloseBlock(ctx, target.BlockID); err != nil && !errors.Is(err, rex.ErrNotFound) {
		// The agent still runs, and its next turn takes its handle back
		// (a resume): then a raw close would read as a crash again.
		return res, fmt.Errorf("%s is retired, but its pane did not close (%w); close it with `rex block close %s`, or, once `rex-amq-adapter who` lists it again, retire it again", r.Handle, err, target.BlockID)
	}
	// A tab it shared may now hold one agent, whose label it then follows.
	tabs := e
	tabs.Rex = on
	tabs.followTab(ctx, target.SessionID, target.WindowID)
	return res, nil
}

// retireTarget is the record caller may retire as h, refused per Retire. A
// departed one is returned for the no-op.
func (e Env) retireTarget(caller store.Record, h core.Handle) (store.Record, error) {
	if h == caller.Handle {
		return store.Record{}, fmt.Errorf("%w: an agent does not retire itself; end your turn, and whoever briefed you retires you", ErrRefused)
	}
	target, ok, err := e.recordByHandle(h)
	if err != nil {
		return store.Record{}, err
	}
	if !ok || target.Server != caller.Server || !placed(caller) || !together(target, caller) {
		return store.Record{}, fmt.Errorf("%w: no agent %s in your Rex session (`rex-amq-adapter who --all` lists them)", ErrRefused, h)
	}
	switch target.Role {
	case core.NarratorRole, core.PersonalRole, core.OrchestratorRole:
		return store.Record{}, fmt.Errorf("%w: %s is the session's %s agent, which retire does not close", ErrRefused, h, target.Role)
	}
	return target, nil
}

// recordByHandle is h's record; ok is false when there is none.
func (e Env) recordByHandle(h core.Handle) (store.Record, bool, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return store.Record{}, false, err
	}
	for _, r := range recs {
		if r.Handle == h {
			return r, true, nil
		}
	}
	return store.Record{}, false, nil
}
