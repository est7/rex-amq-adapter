package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Session is a Rex session as its layout stands now, the boundary of
// discovery: the agents of one task, in any of its tabs. FindSession is the
// only way to get one.
type Session struct {
	server string
	id     string
	// windowOf places every block of the session.
	windowOf map[string]string
	// tabs labels the session's windows.
	tabs map[string]string
}

// FindSession resolves the session block is in now. Discovery has no global
// fallback, so an unknown session is an error.
func (e Env) FindSession(ctx context.Context, block string) (Session, error) {
	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return Session{}, err
	}
	blk, err := e.Rex.InspectBlock(ctx, block)
	if err != nil {
		return Session{}, err
	}
	if srv.Endpoint == "" || blk.SessionID == "" || blk.WindowID == "" && blk.Placement != "detached" {
		return Session{}, errors.New("cannot determine this Rex session")
	}
	return e.loadSession(ctx, srv.Endpoint, blk.SessionID)
}

// loadSession reads the layout of session id on server.
func (e Env) loadSession(ctx context.Context, server, id string) (Session, error) {
	blocks, err := e.Rex.ListBlocks(ctx, id)
	if err != nil {
		return Session{}, err
	}
	windowOf := make(map[string]string, len(blocks))
	tabs := map[string]string{}
	for _, b := range blocks {
		windowOf[b.ID] = b.WindowID
		tabs[b.WindowID] = ""
	}
	// Tab labels are for display; a session Rex cannot describe still has
	// its layout, and its tabs show by id.
	if rs, err := e.Rex.InspectSession(ctx, id); err == nil {
		for _, w := range rs.Windows {
			tabs[w.ID] = w.Label
		}
	}
	return Session{server: server, id: id, windowOf: windowOf, tabs: tabs}, nil
}

// recorded reports whether r's agent was last recorded here. A record is only
// as current as the last reconciliation of its session, so a moved agent's
// may be out of date.
func (s Session) recorded(r store.Record) bool {
	return r.Server == s.server && r.SessionID == s.id
}

// holds reports whether r belongs to this session: a live agent by the
// layout, a departed one, which has no block to place, by its record.
func (s Session) holds(r store.Record) bool {
	if r.Live() {
		_, placed := s.windowOf[r.BlockID]
		return r.Server == s.server && placed
	}
	return s.recorded(r)
}

// reconciles reports whether collecting in this session covers r: everything
// it holds, and an agent recorded here whose block left the session's layout.
// That block closed or moved to another session; collectOne tells which.
func (s Session) reconciles(r store.Record) bool {
	_, placed := s.windowOf[r.BlockID]
	return s.holds(r) || (s.recorded(r) && !placed)
}

// together reports whether a and b were last recorded in the same Rex
// session.
func together(a, b store.Record) bool { return a.SessionID != "" && a.SessionID == b.SessionID }

// placed reports whether r was recorded anywhere it can be told about.
func placed(r store.Record) bool { return r.SessionID != "" }

// tab is how who names window w: its label, else its id.
func (s Session) tab(w string) string {
	if l := s.tabs[w]; l != "" {
		return l
	}
	return w
}

// CollectSession reconciles records with Rex as Collect does, for s only,
// and brings the records of s's agents up to date with its layout.
func (e Env) CollectSession(ctx context.Context, s Session) error {
	return errors.Join(e.followMoves(ctx, s), e.collect(ctx, s.reconciles))
}

// followMoves records the session and window each agent's block is in now. A
// departure is told to the recorded session, so a record must not lag a move
// for longer than it takes someone to look. A block outside s keeps its
// record.
func (e Env) followMoves(ctx context.Context, s Session) error {
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
	for _, r := range recs {
		placed, ok := s.windowOf[r.BlockID]
		if !ok || (r.WindowID == placed && r.SessionID == s.id) {
			continue
		}
		r.WindowID, r.SessionID = placed, s.id
		if err := e.Store.PutRecord(r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Handle, err))
		}
	}
	return errors.Join(errs...)
}

// SuperviseSession brings back the dead wakers of the live agents s holds. The
// Stop hook cannot: an idle agent runs no hooks, so mail to it would wait unseen
// until the agent's next turn. A peer whose mail goes unanswered runs who.
func (e Env) SuperviseSession(ctx context.Context, s Session) error {
	// A waker that will not come up is waited for under the lock every
	// adoption needs.
	ctx, cancel := context.WithTimeout(ctx, superviseFor)
	defer cancel()
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
	for _, r := range recs {
		if r.Live() && s.holds(r) {
			if err := e.ensureWaker(ctx, r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Handle, err))
			}
		}
	}
	return errors.Join(errs...)
}

// SessionAgents projects the running agents s holds, and with departed the
// ones reserved for a resume: who's rows. It reads records and mailboxes
// only.
func (e Env) SessionAgents(s Session, departed bool) ([]core.Agent, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, err
	}
	v := view{scope: Scope{Kind: ScopeSession, Server: s.server, SessionID: s.id}, sessions: []Session{s}}
	var held []store.Record
	for _, r := range recs {
		if v.holds(r) && (r.Live() || departed) {
			held = append(held, r)
		}
	}
	agents, err := e.observeAgents(v, held, nil)
	if err != nil {
		return nil, err
	}
	return core.Project(core.Observations{Agents: agents}).Agents, nil
}
