package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
)

// A live Epic lead (sessionLead: an orchestrator, a pair Epic's implementer,
// a design Epic's architect) coordinates its Rex session: it owns the goal,
// so it hears at once when an agent there departs, even when idle. A session
// without one reports departures to the user. Session notes wait for a
// prompt, so these go as mail, which rings a doorbell. One watcher per
// session reconciles it while its lead or a narrator is live there.

const (
	// watchEvery bounds how late a crash or a closed pane is noticed.
	watchEvery = 5 * time.Second
	// watchBudget bounds one look, including waiting for the lifecycle lock.
	watchBudget = 10 * time.Second
	// watchGiveUp is how many looks in a row may fail before the watcher
	// exits: without a Rex server it would otherwise run forever. The
	// next turn of the session's orchestrator or narrator starts a new one.
	watchGiveUp = 12
)

// wakeForDeparture tells rec's session that rec departed, when its lead is
// involved (core.DepartureMail).
func (e Env) wakeForDeparture(ctx context.Context, rec agent) error {
	if !placed(rec) {
		return nil
	}
	recs, err := e.agents()
	if err != nil {
		return err
	}
	rule, err := e.leadRuleOf(rec.Endpoint().Session(), recs)
	if err != nil {
		return err
	}
	var others []core.Peer
	for _, r := range recs {
		if r.Live() && together(r, rec) {
			p := peerOf(r)
			p.Lead = rule.holds(r)
			others = append(others, p)
		}
	}
	left := peerOf(rec)
	left.Lead = rule.holds(rec)
	to, subject, body := core.DepartureMail(left, rec.Session(), others)
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

// watches reports whether r keeps its session's watcher running under its
// session's lead rule: its live lead (the watchdog reports to it) or a live
// narrator (the watcher tells it what changed).
func watches(r agent, lead leadRule) bool {
	return r.Live() && placed(r) && (r.Role == core.NarratorRole || lead.holds(r))
}

// ensureSessionWatcher starts the watcher of rec's session unless one runs.
// An agent that departs without a hook leaves nothing to react to, and an
// orchestrator or narrator is often idle, so its session needs a process of
// its own. Hooks call this; the watcher is the session's, not rec's, so it
// outlives an orchestrator while a narrator stays, and ends when neither is
// left.
func (e Env) ensureSessionWatcher(rec agent) error {
	if !rec.Live() || !placed(rec) {
		return nil
	}
	lead, err := e.leadRuleOf(rec.Endpoint().Session(), nil)
	if err != nil {
		return err
	}
	if !watches(rec, lead) {
		return nil
	}
	key := core.SessionKey{Server: rec.Server, SessionID: rec.SessionID}
	done, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled context tries the lock once
	unlock, err := e.Store.LockWatch(done, key)
	if err != nil {
		return nil // held: a watcher runs
	}
	unlock()
	if err := os.MkdirAll(e.Paths.Logs(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(e.Paths.Logs(), "watch-"+fileName(rec.SessionID)+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(e.SelfBin, "__internal", "watch", rec.Server, rec.SessionID)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the session watcher: %w", err)
	}
	return cmd.Process.Release()
}

// fileName makes a session id safe as part of a file name.
func fileName(id string) string { return strings.NewReplacer(":", "_", "/", "_").Replace(id) }

// WatchSession reconciles session key until neither its lead nor a narrator
// is live in it, so departures are noticed without a hook, the lead's
// watchdog runs, and the narrator hears what changed. A second watcher finds
// the lock held and returns.
func (e Env) WatchSession(ctx context.Context, key core.SessionKey) error {
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	unlock, err := e.Store.LockWatch(lockCtx, key)
	if err != nil {
		return nil
	}
	defer unlock()
	for failures := 0; ; {
		recs, err := e.agents()
		if err != nil {
			return err
		}
		lead, err := e.leadRuleOf(key, recs)
		if err != nil {
			return err
		}
		var keepers []agent
		for _, r := range recs {
			if watches(r, lead) && r.Server == key.Server && r.SessionID == key.SessionID {
				keepers = append(keepers, r)
			}
		}
		if len(keepers) == 0 {
			return nil // nobody left to report to
		}
		if err := e.watchOnce(ctx, key, keepers); err != nil {
			fmt.Fprintf(Diagnostics, "watch %s: %v\n", key.SessionID, err)
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

// superviseWatched brings back the wakers no hook would: an idle lead's or
// narrator's (keepers), which runs no hook until mail wakes it, and that of
// any agent of the session whose adoption ran out of its hook's time before
// its waker came up, which only its next hook would finish. A failure is
// logged; the next look tries again.
func (e Env) superviseWatched(ctx context.Context, key core.SessionKey, keepers []agent) {
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "supervise the wakers of %s: %v\n", key.SessionID, err)
		return
	}
	for _, r := range recs {
		if !r.Live() || r.Server != key.Server || r.SessionID != key.SessionID {
			continue
		}
		if r.Adopted() && !slices.ContainsFunc(keepers, func(k agent) bool { return k.Handle == r.Handle }) {
			continue
		}
		if err := e.ensureWaker(ctx, r); err != nil {
			fmt.Fprintf(Diagnostics, "supervise the waker of %s: %v\n", r.Handle, err)
		}
	}
}

// watchOnce looks at session key once: it reconciles the session, runs the
// watchdog for its live lead (among keepers), and tells each live narrator
// what changed.
func (e Env) watchOnce(ctx context.Context, key core.SessionKey, keepers []agent) error {
	ctx, cancel := context.WithTimeout(ctx, watchBudget)
	defer cancel()
	lead, err := e.leadRuleOf(key, nil)
	if err != nil {
		return err
	}
	s, err := e.loadSession(ctx, key.Server, key.SessionID)
	if errors.Is(err, rex.ErrNotFound) {
		// The session itself is gone: its agents' blocks went with it.
		return e.collect(ctx, func(r agent) bool { return r.Server == key.Server && r.SessionID == key.SessionID })
	}
	if err != nil {
		return err
	}
	if err := e.CollectSession(ctx, s); err != nil {
		return err
	}
	e.superviseWatched(ctx, key, keepers)
	for _, r := range keepers {
		// The watchdog's own failures are logged, never counted against the
		// watcher: a layout call that times out must not end the session's watch.
		if lead.holds(r) {
			e.watchdog(ctx, r, s)
		}
	}
	var errs []error
	for _, r := range keepers {
		if r.Role == core.NarratorRole {
			errs = append(errs, e.tellNarrator(ctx, key, r))
		}
	}
	return errors.Join(errs...)
}
