package app

import (
	"context"
	"errors"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
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
	rec, ok, err := e.agentAt(block)
	if err != nil {
		return o, err
	}
	if ok && rec.Adopted() {
		o.Handle = rec.Handle
		return o, nil
	}
	if _, _, err := e.Rex.Foreground(ctx, block); errors.Is(err, rex.ErrNotFound) {
		o.BlockGone = true
		return o, nil
	} else if err != nil {
		return o, err
	}
	var liveRec *agent
	if ok && rec.Live() {
		liveRec = &rec
	}
	snap, err := e.assessActivity(ctx, activityRequest{Agent: liveRec, Block: block, Policy: FullScreen})
	if err != nil {
		return o, err
	}
	o.Activity = snap.Activity
	return o, nil
}
