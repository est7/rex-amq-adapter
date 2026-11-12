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

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A live orchestrator coordinates its Rex session: it owns the goal, so it
// hears at once when an agent there departs, even when idle. A session
// without one reports departures to the user and gets nothing here. Session notes wait
// for a prompt, so these go as mail, which rings a doorbell.

const (
	// watchEvery bounds how late a crash or a closed pane is noticed.
	watchEvery = 5 * time.Second
	// watchBudget bounds one look, including waiting for the lifecycle lock.
	watchBudget = 10 * time.Second
	// watchGiveUp is how many looks in a row may fail before the watcher
	// exits: without a Rex server it would otherwise run forever. The
	// orchestrator's next turn starts a new one.
	watchGiveUp = 12
)

// wakeForDeparture tells rec's session that rec departed, when an orchestrator
// is involved.
func (e Env) wakeForDeparture(ctx context.Context, rec store.Record) error {
	if !placed(rec) {
		return nil
	}
	recs, err := e.Store.Records()
	if err != nil {
		return err
	}
	var others []core.Peer
	for _, r := range recs {
		if r.Live() && together(r, rec) {
			others = append(others, peerOf(r))
		}
	}
	left := peerOf(rec)
	to, subject, body := core.DepartureMail(left, rec.AgentSessionID, others)
	var errs []error
	for _, h := range to {
		if err := e.sendAs(ctx, rec.Handle, h, true, "--kind", "status", "--priority", "normal", "--subject", subject, "--body", body); err != nil {
			errs = append(errs, fmt.Errorf("tell %s: %w", h, err))
		}
	}
	// The mail went out in rec's name, which is not rec's doing: should rec
	// resume, it is not told about it.
	e.seedSentCursor(rec.Handle)
	return errors.Join(errs...)
}

// ensureOrchestratorWatcher starts the watcher of an orchestrator unless one runs. An
// agent that departs without a hook leaves nothing to react to, and an
// orchestrator is often idle, so its session needs a process of its own. Hooks call
// this; the watcher ends with its orchestrator.
func (e Env) ensureOrchestratorWatcher(rec store.Record) error {
	if rec.Role != core.OrchestratorRole {
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
		return fmt.Errorf("start the orchestrator watcher: %w", err)
	}
	return cmd.Process.Release()
}

// WatchOrchestrator reconciles h's session until h is no longer a live orchestrator, so
// its departure and its peers' are noticed without a hook. A second watcher
// finds the lock held and returns.
func (e Env) WatchOrchestrator(ctx context.Context, h core.Handle) error {
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
		if !ok || rec.Role != core.OrchestratorRole {
			return nil // gone, or it joined a crew in another role
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
	s, err := e.FindSession(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		// The orchestrator's own pane closed; its session can no longer be found.
		return e.collect(ctx, func(r store.Record) bool { return r.Handle == rec.Handle })
	}
	if err != nil {
		return err
	}
	if err := e.CollectSession(ctx, s); err != nil {
		return err
	}
	// The watchdog's own failures are logged, never counted against the
	// watcher: a layout call that times out must not end the session's watch.
	e.watchdog(ctx, rec, s)
	return nil
}
