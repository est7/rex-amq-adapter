package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Closing an Epic is the second half of a two-phase end: the orchestrator
// hands back and the narrator tells the personal agent, who asks the user;
// on the user's words the personal agent closes the Epic's Rex session.
// Nothing closes an Epic on its own: a hand-back can leave items only the
// user can act on, and a killed session's screens are gone for good.

// CloseRequest is what a personal agent asks.
type CloseRequest struct {
	Session string // the Epic's Rex session id
	// Asked is the user's own words asking to close it, its grant.
	Asked string
	// Force closes while an agent of the Epic is working, waiting on a
	// dialog, holds unread mail, or reports no state.
	Force bool
	// OpID names this request, so a retry with the same OpID finishes the
	// close the first run started, even once its link is gone; "" for none.
	OpID string
}

// CloseResult is what was closed. Replayed says a retry of OpID found the
// close its first run started and finished it.
type CloseResult struct {
	SessionID      string        `json:"session_id"`
	Parked         []core.Handle `json:"parked"`
	Unlinked       bool          `json:"unlinked"`
	OpID           string        `json:"op_id,omitempty"`
	LifecycleState string        `json:"lifecycle_state,omitempty"`
	Replayed       bool          `json:"replayed,omitempty"`
}

// CloseEpic closes the Epic session r.Session for the personal agent in
// loc: it kills the Rex session, parks every live agent recorded in it as
// departed for epic_closed (handles stay reserved for a resume, ReserveFor),
// removes the caller's link to it, and records the Epic's lifecycle closed.
// It refuses a caller that is not a live personal agent, one without the
// user's words, and a session not linked to the caller; unless forced, also
// while an agent other than a Stealth one is not at rest or holds unread
// mail. With r.OpID, the close is recorded once those guards pass and before
// the kill, so a retry of the same request finishes it whatever the first
// run got to, the unlink included; the op id reused for another request is
// refused. Without one, a retry before the unlink finishes what is left: a
// session already gone is not an error.
func (e Env) CloseEpic(ctx context.Context, loc Location, r CloseRequest) (CloseResult, error) {
	res := CloseResult{SessionID: r.Session, OpID: r.OpID}
	caller, ok, err := e.agentIn(loc)
	if err != nil {
		return res, err
	}
	if !ok || !caller.Live() {
		return res, errors.New("no AMQ identity for this pane, so nothing can close an Epic")
	}
	if caller.Role != core.PersonalRole {
		return res, fmt.Errorf("%w: only a %s agent closes Epics, and %s is %q", ErrRefused, core.PersonalRole, caller.Handle, caller.Role)
	}
	if strings.TrimSpace(r.Asked) == "" {
		return res, fmt.Errorf("%w: closing an Epic kills its Rex session and what its panes show; it needs the user's own words asking for it (--asked \"<their words>\")", ErrRefused)
	}
	if err := ValidOpID(r.OpID); err != nil {
		return res, err
	}
	hash := requestHash(closeIdentity{Personal: caller.Endpoint().Session(), Session: r.Session})
	// A recorded close is looked up before the link: the run that recorded
	// it may have removed the link before it died.
	if r.OpID != "" {
		if out, done, err := e.replayClose(ctx, caller, r.OpID, hash); done || err != nil {
			return out, err
		}
	}
	link, err := e.linkTo(caller, r.Session)
	if err != nil {
		return res, err
	}
	on := e.Rex
	if e.RexOn != nil && link.Epic.Server != "" {
		on = e.RexOn(link.Epic.Server)
	}
	if !r.Force {
		if busy, err := e.unsettled(ctx, on, link.Epic); err != nil {
			return res, err
		} else if len(busy) > 0 {
			return res, fmt.Errorf("%w: not every agent of the Epic is at rest (%s); ask the user again, or pass --force when they say to close it anyway", ErrRefused, strings.Join(busy, "; "))
		}
	}
	unlockEpic, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlockEpic()
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	var op *store.EpicOpRecord
	if r.OpID != "" {
		prev, found, err := e.Store.EpicOp(store.EpicOpClose, r.OpID)
		switch {
		case err != nil:
			return res, err
		case found && prev.RequestHash != hash:
			return res, errOpConflict(store.EpicOpClose, r.OpID)
		case found:
			op = &prev // a run of the same close got here meanwhile
		default:
			now := e.Now().UTC()
			op = &store.EpicOpRecord{Kind: store.EpicOpClose, OpID: r.OpID, RequestHash: hash, SessionID: link.Epic.SessionID, Server: link.Epic.Server,
				PersonalHandle: caller.Handle, PersonalSessionID: caller.SessionID, State: store.EpicClosing, CreatedAt: now, UpdatedAt: now}
			if err := e.Store.PutEpicOp(*op); err != nil {
				return res, fmt.Errorf("record the close under --op-id %q: %w", r.OpID, err)
			}
		}
	}
	return e.finishClose(ctx, on, link, caller, op)
}

// replayClose finishes the close recorded under opID, when there is one: done
// is false when there is none, and the close goes on as a new one. The op id
// of another request (another caller, another session) is refused.
func (e Env) replayClose(ctx context.Context, caller agent, opID, hash string) (res CloseResult, done bool, err error) {
	unlockEpic, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return res, true, err
	}
	defer unlockEpic()
	op, found, err := e.Store.EpicOp(store.EpicOpClose, opID)
	if err != nil || !found {
		return res, err != nil, err
	}
	if op.RequestHash != hash {
		return CloseResult{OpID: opID}, true, errOpConflict(store.EpicOpClose, opID)
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, true, err
	}
	defer unlock()
	on := e.Rex
	if e.RexOn != nil && op.Server != "" {
		on = e.RexOn(op.Server)
	}
	link := core.Link{Personal: caller.Endpoint().Session(), Epic: core.SessionKey{Server: op.Server, SessionID: op.SessionID}}
	res, err = e.finishClose(ctx, on, link, caller, &op)
	res.Replayed = true
	return res, true, err
}

// linkTo is caller's link to the Epic session, refused when there is none.
func (e Env) linkTo(caller agent, session string) (core.Link, error) {
	if session == "" || session == caller.SessionID {
		return core.Link{}, fmt.Errorf("%w: name an Epic session other than your own", ErrUsage)
	}
	links, err := e.Store.Links()
	if err != nil {
		return core.Link{}, err
	}
	for _, l := range links {
		if l.Personal == caller.Endpoint().Session() && l.Epic.SessionID == session {
			return l, nil
		}
	}
	return core.Link{}, fmt.Errorf("%w: %s is not an Epic linked to %s (`rex-amq-adapter link` lists yours); a session you did not open is the user's to close", ErrRefused, session, caller.Handle)
}

// unsettled names each live agent of epic, Stealth ones aside, that is not
// at rest (unrest).
func (e Env) unsettled(ctx context.Context, on Terminal, epic core.SessionKey) ([]string, error) {
	recs, err := e.agents()
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID || core.Stealth(rec.Role) {
			continue
		}
		why, err := e.unrest(ctx, on, rec)
		if err != nil {
			return nil, err
		}
		if why != "" {
			busy = append(busy, why)
		}
	}
	return busy, nil
}

// unrest says, "<handle>: <why>", how rec's agent is not at rest: working,
// waiting on a dialog, with unread mail, or reporting no state, which cannot
// be told from working. It is "" at rest, and when its block is gone, which
// leaves nothing in the way. A local Claude Code background job is judged by
// its supervisor's state (jobStatus): the client showing it reports none of
// its own, or a stale one.
func (e Env) unrest(ctx context.Context, on Terminal, rec agent) (string, error) {
	var st rex.Status
	isJob := false
	if localServer(rec.Server) {
		var err error
		if st, _, isJob, err = e.jobStatus(rec); err != nil {
			return fmt.Sprintf("%s: state unknown (%v)", rec.Handle, err), nil
		}
	}
	if !isJob {
		var ok bool
		var err error
		st, ok, err = on.ProgramStatus(ctx, rec.BlockID)
		switch {
		case errors.Is(err, rex.ErrNotFound):
			return "", nil
		case err != nil:
			return fmt.Sprintf("%s: state unknown (%v)", rec.Handle, err), nil
		case !ok:
			return fmt.Sprintf("%s: reports no state", rec.Handle), nil
		}
	}
	if !core.AgentState(st.State).AtRest() {
		return fmt.Sprintf("%s: %s", rec.Handle, st.State), nil
	}
	unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
	if err != nil {
		return "", err
	}
	if len(unread) > 0 {
		return fmt.Sprintf("%s: %d unread message(s)", rec.Handle, len(unread)), nil
	}
	return "", nil
}

// parkClosed parks each live agent recorded in epic, the caller holding the
// lifecycle lock, as departed for epic_closed. Unlike a departure (park) it
// tells nobody: everyone it would tell was in the session that was just
// killed.
func (e Env) parkClosed(ctx context.Context, epic core.SessionKey) ([]core.Handle, error) {
	recs, err := e.agents()
	if err != nil {
		return nil, err
	}
	var parked []core.Handle
	var errs []error
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID {
			continue
		}
		if err := e.departQuietly(ctx, rec, store.DepartEpicClosed); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rec.Handle, err))
			continue
		}
		parked = append(parked, rec.Handle)
	}
	return parked, errors.Join(errs...)
}

// departQuietly marks rec's agent departed for reason, the caller holding the
// lifecycle lock: it retires the handle's waker, clears its window notes, and
// tells nobody. The departure is left finished (it owes no notices), so collection
// and a later SessionEnd find nothing to do. A failure leaves the record live.
func (e Env) departQuietly(ctx context.Context, rec agent, reason string) error {
	if !rec.Kind.InProcess() {
		if err := e.prepareInjector(); err != nil {
			return err
		}
	}
	if _, err := e.AMQ.RetireHandle(ctx, e.Paths.Root(), string(rec.Handle)); err != nil {
		return err
	}
	if err := e.clearWindowNotes(ctx, rec.Handle); err != nil {
		return err
	}
	_, err := e.releaseLease(rec, store.Departure{Reason: reason, At: e.Now().UTC()})
	return err
}
