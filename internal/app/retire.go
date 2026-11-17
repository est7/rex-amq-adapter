package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"time"

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

// What a retire did with the agent's pane.
const (
	PaneClosed   = "closed"
	PaneGone     = "gone"      // Rex no longer had the block
	PaneLeftOpen = "left_open" // it shows something else now, or did not close
)

// retireRexFor bounds each Rex call a retire makes under the lifecycle lock,
// which every adoption and departure on the host waits for.
const retireRexFor = 2 * time.Second

// RetireRequest is what the caller asks.
type RetireRequest struct {
	Handle core.Handle
	// Force retires an agent that is working, waiting on a dialog, holds
	// unread mail, or reports no state.
	Force bool
	// DryRun runs every guard, the busy check included, and stops before
	// the departure: nothing is marked, retired, or closed.
	DryRun bool
}

// RetireResult is what was retired.
type RetireResult struct {
	Handle       core.Handle `json:"handle"`
	Block        string      `json:"block"`
	Action       string      `json:"action"`
	DepartReason string      `json:"depart_reason"`
	// Pane is PaneClosed, PaneGone, or PaneLeftOpen, with PaneNote saying
	// why it was left open; both are empty when nothing was retired.
	Pane     string `json:"pane,omitempty"`
	PaneNote string `json:"pane_note,omitempty"`
	// DryRun marks a preview: Action says what would happen.
	DryRun bool `json:"dry_run,omitempty"`
}

// Retire retires r.Handle for the agent in loc: it marks the record departed
// for retired, retires its waker, clears its window notes, and closes its
// block on the record's server. It refuses a caller that is not an adopted
// agent, a target outside the caller's Rex session, the caller itself, and a
// narrator, personal, or orchestrator target; unless forced, also a target
// that is not at rest. A target already departed is a no-op
// (RetireAlreadyDeparted). A block that no longer shows the agent (it left
// the pane, its process exited, its background job is not shown there) is
// left open, since closing it would close someone else's pane. When the
// block does not close, the departure stands and the error names the block
// to close by hand.
func (e Env) Retire(ctx context.Context, loc Location, r RetireRequest) (RetireResult, error) {
	res := RetireResult{Handle: r.Handle}
	if r.Handle == "" {
		return res, fmt.Errorf("%w: name the agent to retire", ErrUsage)
	}
	target, on, err := e.retireLocked(ctx, loc, r, &res)
	if err != nil || (res.Pane != PaneClosed && res.Pane != PaneGone) {
		return res, err
	}
	// Unlocked: the block is gone, so no hook of the agent can adopt it again.
	// A tab it shared may now hold one agent, whose label it then follows.
	tabs := e
	tabs.Rex = on
	tabs.followTab(ctx, target.SessionID, target.WindowID)
	return res, nil
}

// retireLocked does a retire's work under the lifecycle lock, filling in
// res: every guard reads the records as they are under it, so a resume,
// a move, a role change, or mail arriving before it was taken is seen. The
// agent's own hooks, its SessionEnd included, wait for the close and then
// find a departed record. It returns the target and the terminal of its
// server.
func (e Env) retireLocked(ctx context.Context, loc Location, r RetireRequest, res *RetireResult) (agent, Terminal, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return agent{}, nil, err
	}
	defer unlock()
	caller, ok, err := e.agentIn(loc)
	if err != nil {
		return agent{}, nil, err
	}
	if !ok || !caller.Adopted() {
		return agent{}, nil, errors.New("no AMQ identity for this pane, so nothing can retire an agent")
	}
	target, err := e.retireTarget(caller, r.Handle)
	if err != nil {
		return agent{}, nil, err
	}
	res.Block = target.BlockID
	if !target.Live() {
		res.Action, res.DepartReason = RetireAlreadyDeparted, target.departReason()
		return target, nil, nil
	}
	on := e.Rex
	if e.RexOn != nil && target.Server != "" {
		on = e.RexOn(target.Server)
	}
	if !r.Force {
		rctx, cancel := context.WithTimeout(ctx, retireRexFor)
		why, err := e.unrest(rctx, on, target)
		cancel()
		if err != nil {
			return target, on, err
		}
		if why != "" {
			return target, on, fault.New(fault.Busy, "%s: %s is not at rest (%s); wait for it to finish", ErrRefused, r.Handle, why).
				WithCause(ErrRefused).With("reason", "not_at_rest").
				WithHint("--force retires it anyway; use it only when the user says to retire it while it works").
				WithNext("rex-amq-adapter agent retire " + string(r.Handle) + " --force")
		}
	}
	if r.DryRun {
		res.Action, res.DryRun = RetireRetired, true
		return target, on, nil
	}
	if err := e.departQuietly(ctx, target, store.DepartRetired); err != nil {
		return target, on, fmt.Errorf("retire %s: %w", r.Handle, err)
	}
	res.Action, res.DepartReason = RetireRetired, store.DepartRetired
	pane, note, err := e.paneOfRetired(ctx, on, target)
	if err != nil {
		res.Pane, res.PaneNote = PaneLeftOpen, fmt.Sprintf("what it shows could not be read: %v", err)
		return target, on, fmt.Errorf("%s is retired, but its pane was not closed: what it shows could not be read (%w); look at it, then close it with `rex block close %s` if it still shows the agent", r.Handle, err, target.BlockID)
	}
	if pane != "" {
		res.Pane, res.PaneNote = pane, note
		return target, on, nil
	}
	cctx, cancel := context.WithTimeout(ctx, retireRexFor)
	err = on.CloseBlock(cctx, target.BlockID)
	cancel()
	switch {
	case errors.Is(err, rex.ErrNotFound):
		res.Pane = PaneGone
	case err != nil:
		res.Pane, res.PaneNote = PaneLeftOpen, fmt.Sprintf("the close failed: %v", err)
		// The agent still runs, and its next turn takes its handle back
		// (a resume): then a raw close would read as a crash again.
		return target, on, fmt.Errorf("%s is retired, but its pane did not close (%w); close it with `rex block close %s`, or, once `rex-amq-adapter who` lists it again, retire it again", r.Handle, err, target.BlockID)
	default:
		res.Pane = PaneClosed
	}
	return target, on, nil
}

// paneOfRetired says what to do with rec's block, read on its server: ""
// when it still shows rec's agent, to be closed; PaneGone when Rex no longer
// has it; PaneLeftOpen, with why, when it shows something else.
func (e Env) paneOfRetired(ctx context.Context, on Terminal, rec agent) (pane, note string, err error) {
	ctx, cancel := context.WithTimeout(ctx, retireRexFor)
	defer cancel()
	probe := e
	probe.Rex = on
	b, err := probe.observeBinding(ctx, rec)
	if err != nil {
		return "", "", err
	}
	isJob := false
	if localServer(rec.Server) {
		if _, _, isJob, err = e.jobStatus(rec); err != nil {
			return "", "", err
		}
	}
	switch {
	case b.BlockGone:
		return PaneGone, "", nil
	case b.LeftPane:
		return PaneLeftOpen, "its conversation moved to the background; the pane shows something else", nil
	case b.AgentGone && b.ForegroundPID != 0:
		return PaneLeftOpen, "its process exited; another program holds the pane", nil
	case isJob && !b.JobShown:
		return PaneLeftOpen, "the pane no longer shows its background session", nil
	}
	return "", "", nil
}

// retireTarget is the record caller may retire as h, refused per Retire. A
// departed one is returned for the no-op.
func (e Env) retireTarget(caller agent, h core.Handle) (agent, error) {
	if h == caller.Handle {
		return agent{}, fmt.Errorf("%w: an agent does not retire itself; end your turn, and whoever briefed you retires you", ErrRefused)
	}
	target, ok, err := e.recordByHandle(h)
	if err != nil {
		return agent{}, err
	}
	if !ok || target.Server != caller.Server || !placed(caller) || !together(target, caller) {
		return agent{}, fmt.Errorf("%w: no agent %s in your Rex session (`rex-amq-adapter who --all` lists them)", ErrRefused, h)
	}
	switch target.Role {
	case core.NarratorRole, core.PersonalRole, core.OrchestratorRole:
		return agent{}, fmt.Errorf("%w: %s is the session's %s agent, which retire does not close", ErrRefused, h, target.Role)
	}
	return target, nil
}

// recordByHandle is h's record; ok is false when there is none.
func (e Env) recordByHandle(h core.Handle) (agent, bool, error) {
	recs, err := e.agents()
	if err != nil {
		return agent{}, false, err
	}
	for _, r := range recs {
		if r.Handle == h {
			return r, true, nil
		}
	}
	return agent{}, false, nil
}
