package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A live goal agent makes its window unattended: it owns the goal, so it
// hears at once when an agent there departs, even when idle. A window without
// one has the user present and gets nothing here. Window notes wait for a
// prompt, so these go as mail, which rings a doorbell.

const (
	// watchEvery bounds how late a crash or a closed pane is noticed.
	watchEvery = 5 * time.Second
	// watchBudget bounds one look, including waiting for the lifecycle lock.
	watchBudget = 10 * time.Second
	// watchGiveUp is how many looks in a row may fail before the watcher
	// exits: without a Rex server it would otherwise run forever. The goal
	// agent's next turn starts a new one.
	watchGiveUp = 12
)

// wakeForDeparture tells rec's window that rec departed, when a goal agent
// is involved. Like a note about leaving it is advice: a failure is reported,
// and the departure stands.
func (e Env) wakeForDeparture(ctx context.Context, rec store.Record) {
	if rec.WindowID == "" {
		return
	}
	recs, err := e.Store.Records()
	if err != nil {
		fmt.Fprintf(Diagnostics, "tell the goal agent that %s left: %v\n", rec.Handle, err)
		return
	}
	var goals, mates []core.Handle
	for _, r := range recs {
		if !r.Live() || r.Handle == rec.Handle || r.WindowID != rec.WindowID {
			continue
		}
		if core.IsGoalAgent(r.Handle) {
			goals = append(goals, r.Handle)
		} else {
			mates = append(mates, r.Handle)
		}
	}
	left := core.Peer{Handle: rec.Handle, Kind: rec.Kind, Block: rec.BlockID}
	var to []core.Handle
	var subject, body string
	switch {
	case len(goals) > 0:
		to = goals
		subject, body = core.GoalPeerLeft(left, rec.AgentSessionID)
	case core.IsGoalAgent(rec.Handle) && len(mates) > 0:
		// The goal is unattended, so someone has to surface that it stopped.
		to = mates[:1]
		subject, body = core.GoalAgentLeft(left)
	}
	for _, h := range to {
		if err := amq.Send(ctx, e.AmqBin, e.Paths.Root(), string(rec.Handle), string(h), subject, body); err != nil {
			fmt.Fprintf(Diagnostics, "tell %s that %s left: %v\n", h, rec.Handle, err)
		}
	}
}

// ensureGoalWatcher starts the watcher of a goal agent unless one runs. An
// agent that departs without a hook leaves nothing to react to, and a goal
// agent is often idle, so its window needs a process of its own. Hooks call
// this; the watcher ends with its goal agent.
func (e Env) ensureGoalWatcher(rec store.Record) error {
	if !core.IsGoalAgent(rec.Handle) {
		return nil
	}
	done, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled context tries the lock once
	unlock, err := e.Store.LockWatch(done, rec.Handle)
	if err != nil {
		return nil // held: a watcher runs
	}
	unlock()
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "watch-"+string(rec.Handle)+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(e.SelfBin, "__internal", "watch", rec.Server, string(rec.Handle))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the goal watcher: %w", err)
	}
	return cmd.Process.Release()
}

// WatchGoal reconciles h's window until h is no longer a live goal agent, so
// its departure and its peers' are noticed without a hook. A second watcher
// finds the lock held and returns.
func (e Env) WatchGoal(ctx context.Context, h core.Handle) error {
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	unlock, err := e.Store.LockWatch(lockCtx, h)
	if err != nil {
		return nil
	}
	defer unlock()
	for failures := 0; ; {
		rec, ok, err := e.liveRecord(h)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := e.watchOnce(ctx, rec); err != nil {
			fmt.Fprintf(Diagnostics, "watch %s: %v\n", h, err)
			if failures++; failures >= watchGiveUp {
				return err
			}
		} else {
			failures = 0
		}
		if !sleep(ctx, watchEvery) {
			return ctx.Err()
		}
	}
}

func (e Env) liveRecord(h core.Handle) (store.Record, bool, error) {
	recs, err := e.Store.Records()
	if err != nil {
		return store.Record{}, false, err
	}
	for _, r := range recs {
		if r.Handle == h && r.Live() {
			return r, true, nil
		}
	}
	return store.Record{}, false, nil
}

func (e Env) watchOnce(ctx context.Context, rec store.Record) error {
	ctx, cancel := context.WithTimeout(ctx, watchBudget)
	defer cancel()
	w, err := e.FindWindow(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		// The goal agent's own pane closed; its window can no longer be found.
		return e.collect(ctx, func(r store.Record) bool { return r.Handle == rec.Handle })
	}
	if err != nil {
		return err
	}
	return e.CollectWindow(ctx, w)
}
