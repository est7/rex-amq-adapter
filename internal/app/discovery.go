package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/store"
)

// Window is a Rex window (tab) as its layout stands now, the boundary of
// discovery. FindWindow is the only way to get one.
type Window struct {
	server string
	id     string
	// windowOf places every block of the window's session.
	windowOf map[string]string
}

// FindWindow resolves the window block is in now. Discovery has no global
// fallback, so an unknown window is an error.
func (e Env) FindWindow(ctx context.Context, block string) (Window, error) {
	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return Window{}, err
	}
	blk, err := e.Rex.InspectBlock(ctx, block)
	if err != nil {
		return Window{}, err
	}
	if srv.Endpoint == "" || blk.WindowID == "" || blk.SessionID == "" {
		return Window{}, errors.New("cannot determine this Rex tab")
	}
	blocks, err := e.Rex.ListBlocks(ctx, blk.SessionID)
	if err != nil {
		return Window{}, err
	}
	windowOf := make(map[string]string, len(blocks))
	for _, b := range blocks {
		windowOf[b.ID] = b.WindowID
	}
	return Window{server: srv.Endpoint, id: blk.WindowID, windowOf: windowOf}, nil
}

// recorded reports whether r's agent was last recorded here. The record
// keeps the window of its adoption, so a moved agent's is out of date.
func (w Window) recorded(r store.Record) bool {
	return r.Server == w.server && r.WindowID == w.id
}

// holds reports whether r belongs to this window: a live agent by the layout,
// a departed one, which has no block to place, by its record.
func (w Window) holds(r store.Record) bool {
	if r.Live() {
		return r.Server == w.server && w.windowOf[r.BlockID] == w.id
	}
	return w.recorded(r)
}

// reconciles reports whether collecting in this window covers r: everything
// it holds, and an agent recorded here whose block left the session's layout.
// That block closed or moved to another session; collectOne tells which.
func (w Window) reconciles(r store.Record) bool {
	_, placed := w.windowOf[r.BlockID]
	return w.holds(r) || (w.recorded(r) && !placed)
}

// CollectWindow reconciles records with Rex as Collect does, for w only.
func (e Env) CollectWindow(ctx context.Context, w Window) error {
	return e.collect(ctx, w.reconciles)
}

// SuperviseWindow brings back the dead wakers of the live agents w holds. The
// Stop hook cannot: an idle agent runs no hooks, so mail to it would wait unseen
// until the agent's next turn. A peer whose mail goes unanswered runs who.
func (e Env) SuperviseWindow(ctx context.Context, w Window) error {
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
		if r.Live() && w.holds(r) {
			if err := e.ensureWaker(ctx, r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Handle, err))
			}
		}
	}
	return errors.Join(errs...)
}

// WindowAgents returns the records w holds, departed ones included.
func (e Env) WindowAgents(w Window) ([]store.Record, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return nil, err
	}
	var out []store.Record
	for _, r := range recs {
		if w.holds(r) {
			out = append(out, r)
		}
	}
	return out, nil
}
