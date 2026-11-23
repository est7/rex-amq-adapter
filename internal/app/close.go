package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/est7/rex-amq-adapter/internal/fault"
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
	// DryRun runs every guard, the busy check included, and stops before
	// anything is recorded, killed, parked, or unlinked.
	DryRun bool
}

// CloseResult is what was closed. Replayed says a retry of OpID found the
// close its first run started and finished it. Changed is false when the
// Epic was closed already (by another personal agent of the Hub, or another
// op id): nothing was killed, parked, or unlinked again.
type CloseResult struct {
	SessionID      string        `json:"session_id"`
	Changed        bool          `json:"changed"`
	Parked         []core.Handle `json:"parked"`
	Unlinked       bool          `json:"unlinked"`
	OpID           string        `json:"op_id,omitempty"`
	LifecycleState string        `json:"lifecycle_state,omitempty"`
	Replayed       bool          `json:"replayed,omitempty"`
	// DryRun marks a preview of a close every guard allowed.
	DryRun bool `json:"dry_run,omitempty"`
}

// CloseEpic closes the Epic session r.Session for the personal agent in
// loc: it kills the Rex session, parks every live agent recorded in it as
// departed for epic_closed (handles stay reserved for a resume, ReserveFor),
// removes every link to it the caller's Hub holds, and records the Epic's
// lifecycle closed. Any personal agent of the Hub the Epic is linked to may
// close it (core.LinkMatchesPersonal); the lifecycle's personal agent is a
// delivery preference, never the one allowed.
//
// It refuses a caller that is not a live personal agent, one without the
// user's words, and a session not linked to the caller's Hub; unless
// forced, also while an agent other than a Stealth one is not at rest or
// holds unread mail. Membership, the link, and the busy check are decided
// under LockEpicLifecycle and then LockLifecycle, so two closes of one Epic
// (two tabs of the Hub, two op ids) converge on one termination: the second
// finds the closed lifecycle (its tombstone) and returns changed false,
// killing nothing. With r.OpID (whose identity is the Hub, the Epic, and
// --force), the close is recorded once those guards pass and before the
// kill, so a retry of the same request finishes it whatever the first run
// got to, the unlink included; the op id reused for another request is
// refused. Without one, a retry before the lifecycle is closed finishes
// what is left: a session already gone is not an error.
func (e Env) CloseEpic(ctx context.Context, loc Location, r CloseRequest) (CloseResult, error) {
	res, err := e.closeEpic(ctx, loc, r)
	if r.DryRun || r.Session == "" {
		return res, err
	}
	// Whichever way it ended (closed now, replayed, or closed already), any
	// quiet departure of the Epic left unfinished is finished here.
	err = errors.Join(err, e.finishQuietly(ctx, e.unfinishedQuietIn(r.Session)))
	// A closed Epic is never revived: its handles leave the amq root, and
	// their identities go. A handle whose mailbox holds mail stays; the
	// roster sweep tries again.
	if res.LifecycleState == store.EpicClosed {
		if rep, rerr := e.retireEpicRoster(ctx, r.Session); rerr != nil || len(rep.Kept) > 0 {
			fmt.Fprintf(Diagnostics, "retire the roster of %s: pruned %v, kept %v: %v\n", r.Session, rep.Pruned, rep.Kept, rerr)
		}
	}
	return res, err
}

// unfinishedQuietIn lists the handles departed quietly from session whose
// departures are unfinished: their wakers may still run.
func (e Env) unfinishedQuietIn(session string) []core.Handle {
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "list the departures of %s: %v\n", session, err)
		return nil
	}
	var hs []core.Handle
	for _, r := range recs {
		if p := r.departure(); p != nil && p.Unfinished && r.SessionID == session && quietDeparture(p.DepartReason) {
			hs = append(hs, r.Handle)
		}
	}
	return hs
}

// closeEpic is CloseEpic up to the agents' quiet departures, which it
// leaves to finish with no lock held.
func (e Env) closeEpic(ctx context.Context, loc Location, r CloseRequest) (CloseResult, error) {
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
		return res, fault.New(fault.ConfirmationRequired, "%s: closing an Epic kills its Rex session and what its panes show; it needs the user's own words asking for it (--asked \"<their words>\")", ErrRefused).
			WithCause(ErrRefused).WithNext("rex-amq-adapter epic close " + r.Session + " --asked \"<the user's words>\"")
	}
	if err := ValidOpID(r.OpID); err != nil {
		return res, err
	}
	if r.Session == "" || r.Session == caller.SessionID {
		return res, fmt.Errorf("%w: name an Epic session other than your own", ErrUsage)
	}
	me, err := e.gatewayEndpoint(caller)
	if err != nil {
		return res, err
	}
	hash := requestHash(closeIdentityOf(me, r))
	// A recorded close is looked up before the link: the run that recorded
	// it may have removed the link before it died.
	if r.OpID != "" {
		if out, done, err := e.replayClose(ctx, caller, me, r.OpID, hash); done || err != nil {
			return out, err
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
	if done, err := e.closedAlready(caller, me, r, hash, &res); done || err != nil {
		return res, err
	}
	link, err := e.linkTo(caller, me, r.Session)
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
			return res, fault.New(fault.Busy, "%s: not every agent of the Epic is at rest (%s); ask the user again", ErrRefused, strings.Join(busy, "; ")).
				WithCause(ErrRefused).With("reason", "not_at_rest").With("busy", busy).
				WithHint("--force closes it anyway; use it only when the user says to close it while its agents work").
				WithNext("rex-amq-adapter epic close " + r.Session + " --asked \"<the user's words>\" --force")
		}
	}
	if r.DryRun {
		res.DryRun, res.Parked, res.Changed = true, []core.Handle{}, true
		return res, nil
	}
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
	return e.finishClose(ctx, on, link.Epic, caller, me, op)
}

// closeIdentityOf is the identity of close request r from personal agent
// me: its Hub (its session when it has none), the Epic, and --force.
func closeIdentityOf(me core.Endpoint, r CloseRequest) closeIdentity {
	id := closeIdentity{Hub: string(me.Hub), Session: r.Session, Force: r.Force}
	if me.Hub == "" {
		id.Personal = me.Session()
	}
	return id
}

// closedAlready answers a close of an Epic whose lifecycle is closed (its
// tombstone) for a caller of the Hub (or the session) that it was linked to:
// changed false, nothing killed again; with an op id not seen yet it is
// recorded closed, so its replay answers the same. done is false when the
// Epic is not closed, or the tombstone names another Hub. The caller holds
// LockEpicLifecycle and then LockLifecycle.
func (e Env) closedAlready(caller agent, me core.Endpoint, r CloseRequest, hash string, res *CloseResult) (bool, error) {
	lc, ok, err := e.Store.EpicLifecycle(r.Session)
	if err != nil || !ok || lc.State != store.EpicClosed {
		return false, err
	}
	if !(lc.Hub != "" && lc.Hub == me.Hub) && !(lc.PersonalSessionID != "" && lc.PersonalSessionID == caller.SessionID && lc.Server == caller.Server) {
		return false, nil
	}
	res.LifecycleState, res.Parked = lc.State, []core.Handle{}
	if r.DryRun || r.OpID == "" {
		res.DryRun = r.DryRun
		return true, nil
	}
	prev, found, err := e.Store.EpicOp(store.EpicOpClose, r.OpID)
	switch {
	case err != nil:
		return true, err
	case found && prev.RequestHash != hash:
		return true, errOpConflict(store.EpicOpClose, r.OpID)
	case found:
		return true, nil
	}
	now := e.Now().UTC()
	return true, e.Store.PutEpicOp(store.EpicOpRecord{Kind: store.EpicOpClose, OpID: r.OpID, RequestHash: hash, SessionID: r.Session, Server: lc.Server,
		PersonalHandle: caller.Handle, PersonalSessionID: caller.SessionID, State: store.EpicClosed, CreatedAt: now, UpdatedAt: now})
}

// replayClose finishes the close recorded under opID, when there is one: done
// is false when there is none, and the close goes on as a new one. The op id
// of another request (another Hub, another session, another --force) is
// refused.
func (e Env) replayClose(ctx context.Context, caller agent, me core.Endpoint, opID, hash string) (res CloseResult, done bool, err error) {
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
	if op.State == store.EpicClosed {
		// Seen through already: the tombstone answers, and nothing is
		// killed, parked, or unlinked again.
		return CloseResult{SessionID: op.SessionID, OpID: opID, Parked: []core.Handle{}, Unlinked: true, LifecycleState: store.EpicClosed, Replayed: true}, true, nil
	}
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, true, err
	}
	defer unlock()
	// Another close (another op id, or a tab of the Hub with none) may have
	// closed the Epic since this one failed: its tombstone answers, so this
	// op is reconciled to closed and nothing is killed, parked, or unlinked
	// again.
	if lc, ok, err := e.Store.EpicLifecycle(op.SessionID); err != nil {
		return res, true, err
	} else if ok && lc.State == store.EpicClosed {
		if op.Advance(store.EpicClosed, e.Now().UTC()) {
			if err := e.Store.PutEpicOp(op); err != nil {
				return res, true, fmt.Errorf("record the close under --op-id %q: %w", opID, err)
			}
		}
		return CloseResult{SessionID: op.SessionID, OpID: opID, Parked: []core.Handle{}, LifecycleState: store.EpicClosed, Replayed: true}, true, nil
	}
	on := e.Rex
	if e.RexOn != nil && op.Server != "" {
		on = e.RexOn(op.Server)
	}
	res, err = e.finishClose(ctx, on, core.SessionKey{Server: op.Server, SessionID: op.SessionID}, caller, me, &op)
	res.Replayed = true
	return res, true, err
}

// linkTo is a link that grants caller (me, its gateway endpoint) the Epic
// session, refused when there is none.
func (e Env) linkTo(caller agent, me core.Endpoint, session string) (core.Link, error) {
	links, err := e.linkedEpics(me)
	if err != nil {
		return core.Link{}, err
	}
	for _, l := range links {
		if l.Epic.SessionID == session {
			return l, nil
		}
	}
	return core.Link{}, fmt.Errorf("%w: %s is not an Epic linked to %s or its Hub (`rex-amq-adapter epic list` lists them); a session you did not open is the user's to close", ErrRefused, session, caller.Handle)
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
		if isAgyApprovePlan(rec.Kind, st) {
			if vt, err := on.CaptureVT(ctx, rec.BlockID); err == nil {
				st = normalizeProgramStatus(rec.Kind, st, vt)
			}
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
		if _, err := e.departQuietly(rec, store.DepartEpicClosed); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rec.Handle, err))
			continue
		}
		parked = append(parked, rec.Handle)
	}
	return parked, errors.Join(errs...)
}

// departQuietly releases rec's lease as departed for reason, the caller
// holding the lifecycle lock, as unfinished: the caller, once it has released
// the lock, finishes it quietly (finishQuietly), retiring the waker and
// clearing the window notes, and telling nobody (quietDeparture). Waiting
// here for the handle's lock or for amq would make every hook on the host
// wait behind a waker. A departure left unfinished is finished by collection.
func (e Env) departQuietly(rec agent, reason string) (agent, error) {
	return e.releaseLease(rec, store.Departure{Reason: reason, At: e.Now().UTC(), Unfinished: true})
}

// finishQuietly finishes the quiet departures of hs (departQuietly), with no
// lock held. The departures stand whatever happens here: one it cannot finish
// (its waker would not retire) stays unfinished, collection finishes it, and
// the error says so.
func (e Env) finishQuietly(ctx context.Context, hs []core.Handle) error {
	var errs []error
	for _, h := range hs {
		rec, ok, err := e.agentOf(h)
		if err == nil && (!ok || rec.Live()) {
			continue
		}
		if err == nil {
			err = e.finishDeparture(ctx, rec)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s departed, but finishing its departure failed (collection finishes it): %w", h, err))
		}
	}
	return errors.Join(errs...)
}
