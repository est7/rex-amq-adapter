package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/screen"
	"github.com/est7/rex-amq-adapter/internal/store"
	"github.com/est7/rex-amq-adapter/internal/transcript"
)

// A Rex server that restarts (or a machine that reboots and Rex restores
// its sessions) keeps an Epic's session, tabs, and block ids, and kills the
// agents in them: each block comes back as an idle login shell under a new
// server instance, while the adapter's records keep who ran there and the
// conversation each ran as. Reviving the Epic resumes those conversations
// in the blocks they ran in, so the user's layout, the agents' handles, and
// their mail are what they were; only a lead's or narrator's block that was
// closed gets a tab of its own instead.

// Revive actions, one per agent. A dry run reports the action a live run
// would take: resume_in_place, respawn_window.
const (
	ReviveAlreadyLive    = "already_live"
	ReviveResumedInPlace = "resumed_in_place"
	ReviveRespawned      = "respawned_window"
	ReviveRefused        = "refused"
	reviveInPlacePlan    = "resume_in_place"
	reviveRespawnPlan    = "respawn_window"
)

// ReviveReady is the status of an agent that runs live in the Epic once the
// revive ends: one it found live, or one it resumed whose adoption it saw.
const ReviveReady = "ready"

// ReviveRequest is `epic revive`: the Epic session to revive, the user's
// words when they asked for it (logged as the caller's attestation), an op
// id that names the request, how long to wait for the resumed agents, and a
// dry run that reports the plan and touches nothing.
type ReviveRequest struct {
	Session string
	Asked   string
	OpID    string
	Timeout time.Duration
	DryRun  bool
}

// ReviveAgent is one agent of the Epic and what the revive did with it.
type ReviveAgent struct {
	Handle         core.Handle `json:"handle"`
	Kind           core.Kind   `json:"kind"`
	Role           string      `json:"role"`
	Block          string      `json:"block"`
	AgentSessionID string      `json:"agent_session_id"`
	Cwd            string      `json:"cwd"`
	Action         string      `json:"action"`
	// Status is ReviveReady, how a resumed agent's startup ended
	// (core.StartupStatus), or ReviveRefused; "" in a dry run.
	Status string `json:"status,omitempty"`
	// Reason says why an agent was refused; Note what its startup showed.
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// ReviveResult is what the revive found and did. Replayed says a retry of
// OpID found the revive it names already recorded (and resumed again only
// what is not live); Linked that a Hub link to the Epic exists.
type ReviveResult struct {
	SessionID      string        `json:"session_id"`
	OpID           string        `json:"op_id,omitempty"`
	Replayed       bool          `json:"replayed,omitempty"`
	LifecycleState string        `json:"lifecycle_state"`
	ServerInstance string        `json:"server_instance,omitempty"`
	Linked         bool          `json:"linked"`
	ViewRefreshed  bool          `json:"view_refreshed,omitempty"`
	DryRun         bool          `json:"dry_run,omitempty"`
	Agents         []ReviveAgent `json:"agents"`
}

// Ready reports whether every agent the revive covers runs live in the
// Epic now.
func (r ReviveResult) Ready() bool {
	for _, a := range r.Agents {
		if a.Status != ReviveReady {
			return false
		}
	}
	return true
}

// Touched reports whether the revive typed into a block or opened a tab:
// from then on, what it did is the result even when part of it failed.
func (r ReviveResult) Touched() bool {
	return slices.ContainsFunc(r.Agents, func(a ReviveAgent) bool {
		return a.Action == ReviveResumedInPlace || a.Action == ReviveRespawned
	})
}

// reviveIdentity is what a revive request is: the Epic, and who asked (a
// personal agent's session, or the user from outside an agent).
type reviveIdentity struct {
	Session  string          `json:"session"`
	Personal core.SessionKey `json:"personal,omitzero"`
	Human    bool            `json:"human,omitempty"`
}

// ReviveEpic resumes, after a Rex restart, the agents of Epic session
// r.Session: its lead and narrator always, and each other agent of it that
// died with the server rather than leaving (reviveCandidate). It is the
// user's, or a personal agent's whose Hub the Epic is linked to (reviver).
// It refuses a session with no Epic lifecycle, a closed Epic, and a session
// Rex no longer has. Stale leases are reconciled first (collect), so an
// agent still live afterwards is left alone (already_live), and a retry
// resumes only what is still down. Each one is resumed by its latest
// conversation id, on what it was launched on, unattended as every Epic
// agent is, in its own block when that block still holds an idle shell with
// nothing typed on its prompt line; a lead or narrator whose block was
// closed gets a tab of its own. It then waits, up to r.Timeout, for each to
// be adopted, makes sure the session's watcher runs, and rebuilds the
// view.
func (e Env) ReviveEpic(ctx context.Context, getenv func(string) string, r ReviveRequest) (ReviveResult, error) {
	res := ReviveResult{SessionID: r.Session, OpID: r.OpID, DryRun: r.DryRun, Agents: []ReviveAgent{}}
	if err := ValidOpID(r.OpID); err != nil {
		return res, err
	}
	by, id, err := e.reviver(getenv, r.Session)
	if err != nil {
		return res, err
	}
	hash := requestHash(id)
	var release func()
	if !r.DryRun {
		// Held until every pane is written: a close cannot kill the session
		// between the check that it is open and the launches.
		if release, err = e.Store.LockEpicLifecycle(ctx); err != nil {
			return res, err
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	lc, ok, err := e.Store.EpicLifecycle(r.Session)
	switch {
	case err != nil:
		return res, err
	case !ok:
		return res, fault.New(fault.NotFound, "%s is no Epic: it has no lifecycle (`rex-amq-adapter epic list` lists the Epics linked to you)", r.Session).With("kind", "session")
	case lc.State == store.EpicClosed:
		return res, fault.New(fault.Conflict, "the Epic %s is closed, and a closed Epic is never revived: open a new one", r.Session).With("reason", "epic_closed")
	}
	res.LifecycleState = lc.State
	if r.OpID != "" {
		prev, found, err := e.Store.EpicOp(store.EpicOpRevive, r.OpID)
		switch {
		case err != nil:
			return res, err
		case found && prev.RequestHash != hash:
			return res, errOpConflict(store.EpicOpRevive, r.OpID)
		}
		res.Replayed = found
	}
	on := e
	if e.RexOn != nil && lc.Server != "" {
		on.Rex = e.RexOn(lc.Server)
	}
	on.preview = r.DryRun
	if _, err := on.Rex.InspectSession(ctx, r.Session); errors.Is(err, rex.ErrNotFound) {
		return res, fault.New(fault.NotFound, "the Epic's Rex session %s is gone: a server that started without restoring its sessions keeps nothing to revive in", r.Session).With("kind", "session")
	} else if err != nil {
		return res, err
	}
	srv, err := on.Rex.Whoami(ctx)
	if err != nil {
		return res, err
	}
	res.ServerInstance = srv.InstanceID
	key := core.SessionKey{Server: lc.Server, SessionID: r.Session}
	if key.Server == "" {
		key.Server = serverOf(srv, Location{})
	}
	if !r.DryRun {
		// Leases of agents that died with the server are released, as the
		// session's watcher would, so what is still live afterwards is.
		if err := on.collect(ctx, func(a agent) bool { return a.Server == key.Server && a.SessionID == key.SessionID }); err != nil {
			return res, fmt.Errorf("reconcile the Epic's leases: %w", err)
		}
	}
	pending, err := e.Store.PendingRevives(key.SessionID)
	if err != nil {
		return res, err
	}
	plans, err := on.planRevive(ctx, key, by, pending, srv.InstanceID)
	if err != nil {
		return res, err
	}
	if res.Linked, err = e.epicLinked(key); err != nil {
		return res, err
	}
	if r.DryRun {
		for _, p := range plans {
			res.Agents = append(res.Agents, p.ReviveAgent)
		}
		return res, nil
	}
	// Each resumed handle's mailbox first, outside the lifecycle lock the
	// launches take (provision.go). An agent whose mailbox cannot be made is
	// not resumed, and says why; the others go on.
	for i, p := range plans {
		if p.Action != reviveInPlacePlan && p.Action != reviveRespawnPlan {
			continue
		}
		if err := e.provisionMailbox(ctx, p.rec.Handle); err != nil {
			plans[i].Action, plans[i].Status, plans[i].Reason = ReviveRefused, ReviveRefused, err.Error()
		}
	}
	launched, err := on.launchRevived(ctx, key, plans, pending, srv.InstanceID)
	res.Agents = launched
	if err == nil && r.OpID != "" && !res.Replayed {
		now := e.Now().UTC()
		op := store.EpicOpRecord{Kind: store.EpicOpRevive, OpID: r.OpID, RequestHash: hash, SessionID: key.SessionID, Server: key.Server,
			PersonalHandle: by.Handle, PersonalSessionID: by.SessionID, State: store.EpicRevived, CreatedAt: now, UpdatedAt: now}
		if perr := e.Store.PutEpicOp(op); perr != nil {
			err = fmt.Errorf("record the revive under --op-id %q: %w", r.OpID, perr)
		}
	}
	release()
	release = nil
	defer func() { e.reviveLog(by, r, res) }()
	if err != nil {
		return res, err
	}
	on.awaitRevived(ctx, r.Timeout, res.Agents)
	on.settleRevived(ctx, key, &res)
	return res, nil
}

// reviver is who asks to revive session: the Operator (ResolveOperator);
// a personal agent only for an Epic linked to its Hub or session. by is the
// personal agent, zero for any other Operator, whose request is the user's.
func (e Env) reviver(getenv func(string) string, session string) (by agent, id reviveIdentity, err error) {
	id.Session = session
	op, err := e.ResolveOperator(getenv)
	if err != nil {
		return agent{}, id, err
	}
	if !op.Personal() {
		id.Human = true
		return agent{}, id, nil
	}
	rec := *op.Agent
	if linked, err := e.personalLinked(rec, session); err != nil {
		return agent{}, id, err
	} else if !linked {
		return agent{}, id, refused(fmt.Sprintf("%s reviving %s", rec.Handle, session), core.Decision{Reason: core.ReasonNotLinked})
	}
	id.Personal = rec.Endpoint().Session()
	return rec, id, nil
}

// epicLinked reports whether a link to the Epic exists: the narrator's
// mail reaches a personal agent only through one.
func (e Env) epicLinked(key core.SessionKey) (bool, error) {
	links, err := e.Store.Links()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(links, func(l core.Link) bool { return l.Epic.SessionID == key.SessionID }), nil
}

// revivePlan is one agent of the Epic and how it is to be resumed (its
// Action): cmd is the whole command, its environment included.
type revivePlan struct {
	ReviveAgent
	rec agent
	cmd []string
}

// planRevive decides, for every agent recorded in key's session that the
// revive covers, what it does, checking everything that can be checked
// before a pane is touched. It reads Rex and the records only; by is who
// revives, named in the resumed agents' first prompt. A launch an earlier
// revive recorded as pending, still starting, is waited for again rather
// than started twice (pendingLaunch); pending entries that no longer apply
// are dropped from pending, which a live run writes back when it launches.
func (e Env) planRevive(ctx context.Context, key core.SessionKey, by agent, pending map[core.Handle]store.PendingRevive, instance string) ([]revivePlan, error) {
	recs, err := e.agents()
	if err != nil {
		return nil, err
	}
	lead, err := e.leadRuleOf(key, recs)
	if err != nil {
		return nil, err
	}
	s, err := e.loadSession(ctx, key.Server, key.SessionID)
	if err != nil {
		return nil, err
	}
	var plans []revivePlan
	for _, rec := range recs {
		if rec.Server != key.Server || rec.SessionID != key.SessionID {
			continue
		}
		required := lead.holds(rec) || rec.Role == core.NarratorRole
		if rec.Live() && e.preview {
			// A dry run collects nothing: a lease whose agent is gone is
			// read as the departure collection would record.
			if why, gone := e.wouldPark(ctx, rec); gone {
				rec = departedAs(rec, why)
			}
		}
		if !reviveCandidate(rec, required) {
			continue
		}
		p := revivePlan{rec: rec, ReviveAgent: ReviveAgent{Handle: rec.Handle, Kind: rec.Kind, Role: rec.Role, Block: rec.BlockID, AgentSessionID: rec.Session(), Cwd: rec.Cwd}}
		if rec.Live() {
			delete(pending, rec.Handle)
			p.Action, p.Status = ReviveAlreadyLive, ReviveReady
			plans = append(plans, p)
			continue
		}
		if pl, ok := pending[rec.Handle]; ok {
			state, why, err := e.reconcileLaunch(ctx, s, rec, &pl, instance)
			if err != nil {
				return nil, fmt.Errorf("reconcile the earlier launch of %s: %w", rec.Handle, err)
			}
			switch state {
			case launchStarting:
				pending[rec.Handle] = pl
				p.Action, p.Block, p.Note = pl.Action, pl.Block, "launched by an earlier revive at "+pl.At.Format(time.RFC3339)+" and still starting: waited for, not started again"
				plans = append(plans, p)
				continue
			case launchUnclear:
				p.Action, p.Status, p.Reason = ReviveRefused, ReviveRefused, why
				plans = append(plans, p)
				continue
			case launchDied:
				// A restart killed it where it ran: that block, kept, is
				// where the agent resumes now (its own, or the tab an
				// earlier revive gave it), after the usual pane checks. The
				// record stays until a launch there replaces it, so a pane
				// refused now is still the agent's on the next revive.
				p.rec.BlockID, p.Block = pl.Block, pl.Block
				pending[rec.Handle] = pl
			default:
				delete(pending, rec.Handle)
			}
		}
		if reason, err := e.planReviveAgent(ctx, key, s, by, required, &p); err != nil {
			return nil, fmt.Errorf("plan the revive of %s: %w", rec.Handle, err)
		} else if reason != "" {
			p.Action, p.Status, p.Reason = ReviveRefused, ReviveRefused, reason
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// launchState is what became of a launch an earlier revive recorded.
type launchState int

const (
	// launchEnded: it never ran, was cancelled, or ran and its agent left;
	// the agent is planned anew.
	launchEnded launchState = iota
	// launchStarting: its harness runs, or its launcher has yet to start,
	// in a known block: the revive waits for that block.
	launchStarting
	// launchUnclear: it cannot be attributed (two tabs carry its label);
	// nothing is started for the agent.
	launchUnclear
	// launchDied: a Rex restart since it was launched killed it, and its
	// block, kept by the restart, holds a fresh login shell: the agent is
	// planned anew in that block.
	launchDied
)

// reconcileLaunch decides what became of pl, the recorded launch of rec,
// on the Rex server instance that runs now, from the session s as Rex
// shows it and pl's launcher, which removes
// itself as it starts (writeLauncher). A launch still possible but not
// running yet (its launcher unclaimed, in place) is cancelled by removing
// the launcher first (cancelLaunch), which leaves its fate certain; a tab
// whose id the earlier revive never learned is found by its label among the
// blocks that were not there before it. pl.Block is filled in when found.
func (e Env) reconcileLaunch(ctx context.Context, s Session, rec agent, pl *store.PendingRevive, instance string) (launchState, string, error) {
	if pl.AgentSessionID != rec.Session() {
		_, err := e.cancelLaunch(pl.Script)
		return launchEnded, "", err
	}
	if pl.ServerInstance != instance {
		// Launched on a server instance that is gone: whatever it started
		// died with it, and its shell pid and its tab's liveness describe
		// that server. The launcher is claimed so it never runs late.
		if _, err := e.cancelLaunch(pl.Script); err != nil {
			return launchEnded, "", err
		}
		if _, here := s.windowOf[pl.Block]; pl.Block != "" && here {
			return launchDied, "", nil
		}
		return launchEnded, "", nil
	}
	if pl.Block == "" {
		found, err := e.newBlocksLabelled(ctx, s.id, pl.Label, pl.PriorBlocks)
		if err != nil {
			return launchEnded, "", err
		}
		switch len(found) {
		case 0:
			// No tab carries its label: Rex never opened it, or it closed as
			// its program ended. Cancelled, it can never run.
			_, err := e.cancelLaunch(pl.Script)
			return launchEnded, "", err
		case 1:
			pl.Block = found[0]
		default:
			return launchUnclear, fmt.Sprintf("%s's earlier launch cannot be told apart: %d new tabs are labelled %q (%s); close the ones that do not run it and revive again", rec.Handle, len(found), pl.Label, strings.Join(found, ", ")), nil
		}
	}
	if _, here := s.windowOf[pl.Block]; !here {
		_, err := e.cancelLaunch(pl.Script)
		return launchEnded, "", err
	}
	if pl.Action == ReviveRespawned {
		// Its own tab runs only the launcher, then the harness, and closes
		// when that ends: while the block is there the launch is on.
		return launchStarting, "", nil
	}
	cancelled, err := e.cancelLaunch(pl.Script)
	if err != nil || cancelled {
		// Typed and never run (the text lost, or the shell never got to
		// it): cancelled, it never will. The pane is checked again before
		// anything else is typed.
		return launchEnded, "", err
	}
	fg, ok, err := e.Rex.Foreground(ctx, pl.Block)
	switch {
	case errors.Is(err, rex.ErrNotFound):
		return launchEnded, "", nil
	case err != nil:
		return launchEnded, "", err
	}
	if ok && (pl.ShellPID != 0 && fg.PID == pl.ShellPID || pl.ShellPID == 0 && isShell(fg.Name)) {
		// The shell it was typed into is back in the foreground: the
		// launch ran and its agent exited.
		return launchEnded, "", nil
	}
	return launchStarting, "", nil
}

// cancelLaunch claims script, a launch's launcher, before the launch can:
// removed, the launcher exits as it starts and the launch never runs
// (cancelled true); already gone, the launch claimed it and runs. A dry run
// removes nothing and reads an unclaimed launcher as cancelled.
func (e Env) cancelLaunch(script string) (bool, error) {
	if script == "" {
		return false, nil
	}
	if e.preview {
		_, err := os.Stat(script)
		return err == nil, nil
	}
	switch err := os.Remove(script); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// newBlocksLabelled lists the blocks of session labelled label that are
// not among prior: the tab a launch opened, whose reply was lost.
func (e Env) newBlocksLabelled(ctx context.Context, session, label string, prior []string) ([]string, error) {
	blocks, err := e.Rex.ListBlocks(ctx, session)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range blocks {
		if b.Label == label && !slices.Contains(prior, b.ID) {
			out = append(out, b.ID)
		}
	}
	return out, nil
}

// reviveCandidate reports whether the revive covers rec: always the Epic's
// lead and narrator (required), never a personal agent, and any other
// agent that is live or died where it ran (its process gone, or its pane
// taken by something else) rather than leaving: retired, ended by its
// harness, closed with its Epic, replaced, or gone with its block.
func reviveCandidate(rec agent, required bool) bool {
	switch {
	case rec.Role == core.PersonalRole:
		return false
	case required, rec.Live():
		return true
	}
	switch rec.departReason() {
	case "agent_gone", "agent_left_pane":
		return true
	}
	return false
}

// wouldPark is the departure reason collection would park rec's live
// agent for (parkIfGone), read without parking it; false when Rex shows it
// bound, or cannot say.
func (e Env) wouldPark(ctx context.Context, rec agent) (string, bool) {
	b, err := e.observeBinding(ctx, rec)
	if err != nil {
		return "", false
	}
	if o, bound := core.CheckBinding(b); !bound && o.Progress == core.Failed {
		return o.Code, true
	}
	return "", false
}

// departedAs is rec as a dry run reads it once its lease ended for reason:
// the record is not written.
func departedAs(rec agent, reason string) agent {
	p := store.Placement{Place: rec.Place, Epoch: rec.lease.Epoch, AcquiredAt: rec.lease.AcquiredAt, DepartReason: reason}
	rec.LastPlacement, rec.lease = &p, nil
	return rec
}

// planReviveAgent fills in p, a departed agent of the Epic, with the command
// that resumes it and where: its own block of session s when it is still
// there, else (for a required agent only) a tab of its own. refusal says
// why it cannot be resumed.
func (e Env) planReviveAgent(ctx context.Context, key core.SessionKey, s Session, by agent, required bool, p *revivePlan) (refusal string, err error) {
	rec := p.rec
	sid := rec.Session()
	if err := core.ValidateResumeSessionID(rec.Kind, sid); err != nil {
		return fmt.Sprintf("%s cannot be resumed: %v", rec.Handle, err), nil
	}
	if p.Cwd == "" {
		p.Cwd = transcript.Cwd(ctx, e.harnessDirs(), rec.Kind, sid)
	}
	if p.Cwd == "" {
		return fmt.Sprintf("%s cannot be resumed: its working directory is not recorded and its transcript does not name one", rec.Handle), nil
	}
	launch, err := e.resumeLaunch(p.Cwd, rec)
	if err != nil {
		return fmt.Sprintf("%s cannot be resumed on what it was launched on: %v", rec.Handle, err), nil
	}
	if refusal, err := e.preflight(ctx, p.Cwd, rec.Kind); err != nil || refusal != "" {
		return refusal, err
	}
	unattended, err := e.launchUnattended(key.SessionID, rec.Role, agent{}, &rec)
	if err != nil {
		return "", err
	}
	cmd, ok := reviveCommand(rec, launch, e.revivePrompt(rec, by), unattended || rec.Unattended, func(argv []string) []string { return e.trustGate().Args(p.Cwd, argv) })
	if !ok {
		return fmt.Sprintf("%s (%s) cannot be resumed from outside", rec.Handle, rec.Kind), nil
	}
	p.cmd = cmd
	if _, here := s.windowOf[rec.BlockID]; !here || rec.BlockID == "" {
		if !required {
			return fmt.Sprintf("%s's block %s is no longer in the session", rec.Handle, rec.BlockID), nil
		}
		p.Action = reviveRespawnPlan
		return "", nil
	}
	if reason, err := e.paneFree(ctx, key, rec); err != nil || reason != "" {
		return reason, err
	}
	p.Action = reviveInPlacePlan
	return "", nil
}

// reviveCommand is the command that resumes rec's conversation with prompt
// as its first turn: its harness's resume command with the session trust
// trust gives the bare argv (agy --settings, pi --approve, which name the
// program first: trust.Gate.Args, as launch applies it), the model and
// effort it was launched on, carrying them to its adoption
// (withLaunchEnv), and ME_UNATTENDED=1 when it runs unattended. false for
// a harness the adapter cannot resume from outside.
func reviveCommand(rec agent, l core.Launch, prompt string, unattended bool, trust func([]string) []string) ([]string, bool) {
	cmd, ok := resumeCommand(rec.Kind, rec.Session(), prompt)
	if !ok {
		return nil, false
	}
	cmd = withLaunchEnv(l, trust(l.Args(rec.Kind, cmd)))
	if unattended {
		cmd = withUnattendedEnv(cmd)
	}
	return cmd, true
}

func (e Env) revivePrompt(rec agent, by agent) string {
	who := "the user"
	if by.Handle != "" {
		who = string(by.Handle)
	}
	role := rec.Role
	if role == "" {
		role = "an agent"
	}
	return fmt.Sprintf("Rex restarted, and %s revived this Epic: you are %s, its %s, resumed in your own pane. Mail may have waited for you: run %s mail drain --include-body and act on it, then carry on with your task where you left it.", who, rec.Handle, role, e.SelfBin)
}

// paneFree is why rec's block cannot take its resumed conversation now, or
// "": no other agent holds it, its foreground is a shell, and that shell's
// prompt line holds nothing typed, so the command typed in clobbers
// nobody's input.
func (e Env) paneFree(ctx context.Context, key core.SessionKey, rec agent) (string, error) {
	reason, _, err := e.freeShell(ctx, key, rec)
	return reason, err
}

// freeShell is paneFree that also returns the pid of the shell found
// waiting in rec's block.
func (e Env) freeShell(ctx context.Context, key core.SessionKey, rec agent) (string, int, error) {
	if l, ok, err := e.Store.LeaseAt(key.Server, rec.BlockID); err != nil {
		return "", 0, err
	} else if ok && l.Handle != rec.Handle {
		return fmt.Sprintf("%s's block %s runs %s now", rec.Handle, rec.BlockID, l.Handle), 0, nil
	}
	fg, ok, err := e.Rex.Foreground(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		return fmt.Sprintf("%s's block %s is gone", rec.Handle, rec.BlockID), 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if !ok || !isShell(fg.Name) {
		name := fg.Name
		if !ok {
			name = "no process"
		}
		return fmt.Sprintf("%s's block %s runs %s, not an idle shell; nothing was typed into it", rec.Handle, rec.BlockID, name), 0, nil
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return "", 0, err
	}
	if !screen.ShellPromptEmpty(vt) {
		return fmt.Sprintf("%s's block %s shows no fresh shell's empty prompt (someone's unsent input, output of a command run there, or a prompt not drawn yet); nothing was typed into it: empty the prompt, clear the pane with Rex's Clear Terminal (it drops the scrollback, which Ctrl+L keeps), and revive again", rec.Handle, rec.BlockID), 0, nil
	}
	return "", fg.PID, nil
}

// launchRevived starts each planned resume, under the lifecycle lock so no
// adoption takes a block between its check and its write. Every launch runs
// a one-shot launcher (writeLauncher) and is recorded in pending, with that
// launcher, before Rex is asked for anything (the caller holds
// LockEpicLifecycle): a retry after a lost reply, a timeout, or a revive
// that died part way finds it there and reconciles it (reconcileLaunch)
// instead of starting the conversation again. In place, the pane is checked
// once more and the launcher typed into its shell; for a lead or narrator
// whose block was closed, a tab is opened running it. A launch whose reply
// is an error stays recorded: whether Rex did it is for the retry to tell.
// A failure to start one is that agent's, and the others go on.
func (e Env) launchRevived(ctx context.Context, key core.SessionKey, plans []revivePlan, pending map[core.Handle]store.PendingRevive, instance string) ([]ReviveAgent, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := e.Store.PutPendingRevives(key.SessionID, pending); err != nil {
		return nil, fmt.Errorf("record the Epic's pending revives: %w", err)
	}
	record := func(h core.Handle, pl store.PendingRevive) error {
		pl.ServerInstance = instance
		pending[h] = pl
		if err := e.Store.PutPendingRevives(key.SessionID, pending); err != nil {
			return fmt.Errorf("record the launch of %s before it: %w", h, err)
		}
		return nil
	}
	out := make([]ReviveAgent, 0, len(plans))
	for _, p := range plans {
		a := p.ReviveAgent
		var reason string
		var err error
		switch a.Action {
		case reviveInPlacePlan:
			if reason, err = e.resumeInPlace(ctx, key, p, record); err == nil && reason == "" {
				a.Action = ReviveResumedInPlace
			}
		case reviveRespawnPlan:
			var block string
			if block, err = e.respawn(ctx, key, p, record); err == nil {
				a.Action, a.Block = ReviveRespawned, block
			}
		}
		if err != nil {
			reason = err.Error()
		}
		if reason != "" {
			a.Action, a.Status, a.Reason = ReviveRefused, ReviveRefused, reason
		}
		out = append(out, a)
	}
	return out, nil
}

// resumeInPlace types p's launcher into its block's shell, after checking
// the pane again, once the launch is recorded. The launcher runs with
// /bin/sh, so whatever shell the user's login shell is (fish quotes
// otherwise than sh), the agent's argv reaches it as written; it removes
// itself, and a cd that fails leaves the shell, and the pane, as they were.
// A write that fails leaves the record: the retry cancels the launcher if
// the text never ran.
func (e Env) resumeInPlace(ctx context.Context, key core.SessionKey, p revivePlan, record func(core.Handle, store.PendingRevive) error) (string, error) {
	reason, shell, err := e.freeShell(ctx, key, p.rec)
	if err != nil || reason != "" {
		return reason, err
	}
	script, err := e.writeLauncher(p.Cwd, p.cmd)
	if err != nil {
		return "", err
	}
	pl := store.PendingRevive{Block: p.rec.BlockID, AgentSessionID: p.AgentSessionID, Action: ReviveResumedInPlace, Script: script, ShellPID: shell, At: e.Now().UTC()}
	if err := record(p.rec.Handle, pl); err != nil {
		_ = os.Remove(script)
		return "", err
	}
	if label := core.PaneLabel(p.rec.Handle, p.rec.Role, p.rec.Task); label != "" {
		if err := e.Rex.RenameBlock(ctx, p.rec.BlockID, label); err != nil {
			fmt.Fprintf(Diagnostics, "label %s for %s: %v\n", p.rec.BlockID, p.rec.Handle, err)
		}
	}
	if err := e.Rex.Write(ctx, p.rec.BlockID, "/bin/sh "+shellQuote(script)+"\r"); err != nil {
		return "", fmt.Errorf("type the resume of %s into %s (a retry tells whether it ran): %w", p.rec.Handle, p.rec.BlockID, err)
	}
	return "", nil
}

// respawn opens a tab of its own for p, a lead or narrator whose block was
// closed, running its launcher, once the launch is recorded with the
// session's blocks before it and the tab's label, which tell the tab apart
// should Rex's reply be lost; the block is recorded once known.
func (e Env) respawn(ctx context.Context, key core.SessionKey, p revivePlan, record func(core.Handle, store.PendingRevive) error) (string, error) {
	blocks, err := e.Rex.ListBlocks(ctx, key.SessionID)
	if err != nil {
		return "", err
	}
	prior := make([]string, len(blocks))
	for i, b := range blocks {
		prior[i] = b.ID
	}
	script, err := e.writeLauncher(p.Cwd, p.cmd)
	if err != nil {
		return "", err
	}
	label := core.PaneLabel(p.rec.Handle, p.rec.Role, p.rec.Task)
	pl := store.PendingRevive{AgentSessionID: p.AgentSessionID, Action: ReviveRespawned, Script: script, Label: label, PriorBlocks: prior, At: e.Now().UTC()}
	if err := record(p.rec.Handle, pl); err != nil {
		_ = os.Remove(script)
		return "", err
	}
	block, err := e.openTab(ctx, key.SessionID, label, p.Cwd, []string{"/bin/sh", script})
	if err != nil {
		return "", fmt.Errorf("open a tab for %s (a retry tells whether Rex opened it): %w", p.rec.Handle, err)
	}
	pl.Block = block
	if err := record(p.rec.Handle, pl); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	return block, nil
}

// writeLauncher writes a one-shot /bin/sh script that removes itself,
// changes to dir, and execs cmd, and returns its path. The removal is the
// launch's claim: a retry that removes the script first (cancelLaunch)
// makes it exit at once, so a launch runs at most once.
func (e Env) writeLauncher(dir string, cmd []string) (string, error) {
	d := filepath.Join(e.Paths.Home, "revive")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	path := filepath.Join(d, token+".sh")
	if strings.ContainsAny(path, "\\\n\r") {
		return "", fmt.Errorf("the state directory %s cannot be typed into a shell", e.Paths.Home)
	}
	quoted := make([]string, len(cmd))
	for i, a := range cmd {
		quoted[i] = shellQuote(a)
	}
	body := "#!/bin/sh\n# Written by rex-amq-adapter epic revive: resumes an agent in its own pane.\nrm -- " + shellQuote(path) + " 2>/dev/null || exit 0" +
		"\ncd -- " + shellQuote(dir) + " || exit 1\nexec " + strings.Join(quoted, " ") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// awaitRevived waits, until timeout or ctx ends, for every resumed agent to
// be adopted as itself, and records how each startup ended.
func (e Env) awaitRevived(ctx context.Context, timeout time.Duration, agents []ReviveAgent) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for i := range agents {
		a := &agents[i]
		if a.Action != ReviveResumedInPlace && a.Action != ReviveRespawned {
			continue
		}
		s, err := e.AwaitStartup(ctx, a.Block)
		switch {
		case err != nil:
			a.Status, a.Note = string(core.StartupTimeout), err.Error()
		case s.Status == core.StartupAdopted && s.Handle == a.Handle:
			a.Status = ReviveReady
		case s.Status == core.StartupAdopted:
			a.Status, a.Note = string(s.Status), fmt.Sprintf("the block was adopted as %s, not %s", s.Handle, a.Handle)
		default:
			a.Status, a.Note = string(s.Status), s.Note
		}
	}
}

// settleRevived makes sure, once the agents are back, that the session's
// watcher runs (a hook of its lead or narrator starts one too) and that the
// narrator's view shows them. Both are advice: a failure is reported, and
// the agents stay revived.
func (e Env) settleRevived(ctx context.Context, key core.SessionKey, res *ReviveResult) {
	if err := e.dropPendingRevives(ctx, key, res.Agents); err != nil {
		fmt.Fprintf(Diagnostics, "drop the revived agents' pending launches: %v\n", err)
	}
	recs, err := e.agents()
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the revived Epic's agents: %v\n", err)
		return
	}
	for _, r := range recs {
		if !r.Live() || r.Server != key.Server || r.SessionID != key.SessionID {
			continue
		}
		if err := e.ensureSessionWatcher(r); err != nil {
			fmt.Fprintf(Diagnostics, "the watcher of %s: %v\n", key.SessionID, err)
		}
		if r.Role == core.NarratorRole && !res.ViewRefreshed {
			if _, err := e.refreshView(ctx, r, key, ""); err != nil {
				fmt.Fprintf(Diagnostics, "refresh the view of %s: %v\n", key.SessionID, err)
			} else {
				res.ViewRefreshed = true
			}
		}
	}
}

// dropPendingRevives forgets the pending launches of the agents now ready:
// their adoption took the block. One still starting stays for a retry.
func (e Env) dropPendingRevives(ctx context.Context, key core.SessionKey, agents []ReviveAgent) error {
	unlock, err := e.Store.LockEpicLifecycle(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	pending, err := e.Store.PendingRevives(key.SessionID)
	if err != nil || len(pending) == 0 {
		return err
	}
	for _, a := range agents {
		if a.Status == ReviveReady {
			delete(pending, a.Handle)
		}
	}
	return e.Store.PutPendingRevives(key.SessionID, pending)
}

// reviveLog appends one JSON line per live revive to logs/revive.log, with
// the user's words when the caller relayed them: an audit trail, so a
// failure is reported to Diagnostics and never fails the revive.
func (e Env) reviveLog(by agent, r ReviveRequest, res ReviveResult) {
	entry := map[string]any{"at": e.Now().UTC().Format(time.RFC3339), "by": by.Handle, "session": r.Session, "asked": r.Asked, "op_id": r.OpID, "replayed": res.Replayed, "agents": res.Agents}
	if by.Handle == "" {
		entry["by"] = "human"
	}
	b, _ := json.Marshal(entry)
	if err := appendLine(filepath.Join(e.Paths.Logs(), "revive.log"), b); err != nil {
		fmt.Fprintf(Diagnostics, "revive log: %v\n", err)
	}
}

func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
