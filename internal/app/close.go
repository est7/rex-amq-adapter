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
}

// CloseResult is what was closed.
type CloseResult struct {
	SessionID string        `json:"session_id"`
	Parked    []core.Handle `json:"parked"`
	Unlinked  bool          `json:"unlinked"`
}

// CloseEpic closes the Epic session r.Session for the personal agent in
// loc: it kills the Rex session, parks every live agent recorded in it as
// departed for epic_closed (handles stay reserved for a resume, ReserveFor),
// and removes the caller's link to it. It refuses a caller that is not a
// live personal agent, one without the user's words, and a session not
// linked to the caller; unless forced, also while an agent other than a
// Stealth one is not at rest or holds unread mail. Run again after a
// failure part way, it finishes what is left: a session already gone is not
// an error.
func (e Env) CloseEpic(ctx context.Context, loc Location, r CloseRequest) (CloseResult, error) {
	res := CloseResult{SessionID: r.Session}
	caller, ok, err := e.Store.Record(loc.Block)
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
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return res, err
	}
	defer unlock()
	// Killed first: parking first would retire the wakers of agents that
	// stay alive if the kill fails. The lock keeps the session's watcher
	// and the dying agents' SessionEnd hooks from parking them meanwhile.
	if err := on.KillSession(ctx, link.Epic.SessionID); err != nil && !errors.Is(err, rex.ErrNotFound) {
		return res, fmt.Errorf("kill the Epic's session: %w", err)
	}
	if res.Parked, err = e.parkClosed(ctx, link.Epic); err != nil {
		return res, err
	}
	if _, err := e.Store.DeleteLink(link); err != nil {
		return res, fmt.Errorf("unlink the Epic: %w", err)
	}
	res.Unlinked = true
	return res, nil
}

// linkTo is caller's link to the Epic session, refused when there is none.
func (e Env) linkTo(caller store.Record, session string) (core.Link, error) {
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
// at rest: working, waiting on a dialog, with unread mail, or reporting no
// state, which cannot be told from working. An agent whose block is gone
// is not in the way.
func (e Env) unsettled(ctx context.Context, on Terminal, epic core.SessionKey) ([]string, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, err
	}
	var busy []string
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID || core.Stealth(rec.Role) {
			continue
		}
		st, ok, err := on.ProgramStatus(ctx, rec.BlockID)
		switch {
		case errors.Is(err, rex.ErrNotFound):
			continue
		case err != nil:
			busy = append(busy, fmt.Sprintf("%s: state unknown (%v)", rec.Handle, err))
			continue
		case !ok:
			busy = append(busy, fmt.Sprintf("%s: reports no state", rec.Handle))
			continue
		case !core.AgentState(st.State).AtRest():
			busy = append(busy, fmt.Sprintf("%s: %s", rec.Handle, st.State))
			continue
		}
		unread, err := amq.Unread(e.Paths.Root(), string(rec.Handle))
		if err != nil {
			return nil, err
		}
		if len(unread) > 0 {
			busy = append(busy, fmt.Sprintf("%s: %d unread message(s)", rec.Handle, len(unread)))
		}
	}
	return busy, nil
}

// parkClosed parks each live agent recorded in epic, the caller holding the
// lifecycle lock, as departed for epic_closed. Unlike a departure (park) it
// tells nobody: everyone it would tell was in the session that was just
// killed.
func (e Env) parkClosed(ctx context.Context, epic core.SessionKey) ([]core.Handle, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, err
	}
	var parked []core.Handle
	var errs []error
	for _, rec := range recs {
		if !rec.Live() || rec.Server != epic.Server || rec.SessionID != epic.SessionID {
			continue
		}
		if err := e.retireClosed(ctx, rec); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rec.Handle, err))
			continue
		}
		parked = append(parked, rec.Handle)
	}
	return parked, errors.Join(errs...)
}

func (e Env) retireClosed(ctx context.Context, rec store.Record) error {
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
	now := e.Now().UTC()
	rec.DepartedAt, rec.DepartReason, rec.Unfinished = &now, store.DepartEpicClosed, ""
	return e.Store.PutRecord(rec)
}
