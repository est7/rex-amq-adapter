package app

import (
	"context"
	"errors"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
)

const awaitEvery = 250 * time.Millisecond

// AwaitStartup waits for an outcome of the agent started in block, until ctx
// ends. It is addressed by a block id the caller already holds, so it reveals
// nothing beyond that block and works in any tab.
func (e Env) AwaitStartup(ctx context.Context, block string) (core.Startup, error) {
	for {
		o, err := e.observeStartup(ctx, block)
		if err != nil {
			if ctx.Err() != nil {
				return core.StartupTimedOut(), nil
			}
			return core.Startup{}, err
		}
		if s, done := core.DecideStartup(o); done {
			return s, nil
		}
		if !sleep(ctx, awaitEvery) {
			return core.StartupTimedOut(), nil
		}
	}
}

func (e Env) observeStartup(ctx context.Context, block string) (core.StartupObservation, error) {
	var o core.StartupObservation
	rec, ok, err := e.Store.Record(block)
	if err != nil {
		return o, err
	}
	if ok && rec.Live() {
		o.Handle = rec.Handle
		return o, nil
	}
	if _, _, err := e.Rex.Foreground(ctx, block); errors.Is(err, rex.ErrNotFound) {
		o.BlockGone = true
		return o, nil
	} else if err != nil {
		return o, err
	}
	status, _, err := e.Rex.ProgramStatus(ctx, block)
	if err != nil {
		return o, err
	}
	o.State, o.BlockedKind = core.AgentState(status.State), status.Kind
	vt, err := e.Rex.CaptureVT(ctx, block)
	if err != nil {
		return o, err
	}
	// The kind is unknown before adoption: any agent's dialog counts.
	o.TrustPhrase, _ = screen.TrustScreen("", vt)
	return o, nil
}
