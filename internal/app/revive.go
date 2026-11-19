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
	by, id, err := e.reviver(ctx, getenv, r.Session)
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
	plans, err := on.planRevive(ctx, key, by)
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
	launched, err := on.launchRevived(ctx, key, plans)
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

// reviver is who asks to revive session, decided as for an Epic's
// artifacts: the user (outside Rex, or in a pane with no live agent, past
// HumanGate), or a live personal agent of a Hub the Epic is linked to.
// Anyone else is refused. by is the personal agent, zero for the user.
func (e Env) reviver(ctx context.Context, getenv func(string) string, session string) (by agent, id reviveIdentity, err error) {
	id.Session = session
	loc, err := e.Caller(ctx, getenv)
	switch {
	case errors.Is(err, ErrNotInRex):
		id.Human = true
		return agent{}, id, e.HumanGate(getenv)
	case err != nil:
		return agent{}, id, err
	}
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return agent{}, id, err
	}
	if !ok || !rec.Live() {
		id.Human = true
		return agent{}, id, e.HumanGate(getenv)
	}
	if rec.Role != core.PersonalRole {
		return agent{}, id, fmt.Errorf("%w: an Epic is revived by the user or by a personal agent of a Hub it is linked to; %s is %s", ErrRefused, rec.Handle, whoIs(rec))
	}
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
// revives, named in the resumed agents' first prompt.
func (e Env) planRevive(ctx context.Context, key core.SessionKey, by agent) ([]revivePlan, error) {
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
			p.Action, p.Status = ReviveAlreadyLive, ReviveReady
			plans = append(plans, p)
			continue
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
	cmd, ok := reviveCommand(rec, launch, e.revivePrompt(rec, by), unattended || rec.Unattended)
	if !ok {
		return fmt.Sprintf("%s (%s) cannot be resumed from outside", rec.Handle, rec.Kind), nil
	}
	p.cmd = e.trustGate().Args(p.Cwd, cmd)
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
// as its first turn: its harness's resume command, with the model and
// effort it was launched on, carrying them to its adoption
// (withLaunchEnv), and ME_UNATTENDED=1 when it runs unattended. false for
// a harness the adapter cannot resume from outside.
func reviveCommand(rec agent, l core.Launch, prompt string, unattended bool) ([]string, bool) {
	cmd, ok := resumeCommand(rec.Kind, rec.Session(), prompt)
	if !ok {
		return nil, false
	}
	cmd = withLaunchEnv(l, l.Args(rec.Kind, cmd))
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
	if l, ok, err := e.Store.LeaseAt(key.Server, rec.BlockID); err != nil {
		return "", err
	} else if ok && l.Handle != rec.Handle {
		return fmt.Sprintf("%s's block %s runs %s now", rec.Handle, rec.BlockID, l.Handle), nil
	}
	fg, ok, err := e.Rex.Foreground(ctx, rec.BlockID)
	if errors.Is(err, rex.ErrNotFound) {
		return fmt.Sprintf("%s's block %s is gone", rec.Handle, rec.BlockID), nil
	}
	if err != nil {
		return "", err
	}
	if !ok || !isShell(fg.Name) {
		name := fg.Name
		if !ok {
			name = "no process"
		}
		return fmt.Sprintf("%s's block %s runs %s, not an idle shell; nothing was typed into it", rec.Handle, rec.BlockID, name), nil
	}
	vt, err := e.Rex.CaptureVT(ctx, rec.BlockID)
	if err != nil {
		return "", err
	}
	if !screen.ShellPromptEmpty(vt) {
		return fmt.Sprintf("%s's block %s shows no empty shell prompt (someone's unsent input, or a prompt not drawn yet); nothing was typed into it: clear it and revive again", rec.Handle, rec.BlockID), nil
	}
	return "", nil
}

// launchRevived starts each planned resume, under the lifecycle lock so no
// adoption takes a block between its check and its write: in its block, the
// pane is checked once more and the command typed into its shell; in a new
// tab, the tab is opened. A failure to start one is that agent's, and the
// others go on.
func (e Env) launchRevived(ctx context.Context, key core.SessionKey, plans []revivePlan) ([]ReviveAgent, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	out := make([]ReviveAgent, 0, len(plans))
	for _, p := range plans {
		a := p.ReviveAgent
		switch a.Action {
		case reviveInPlacePlan:
			if reason, err := e.resumeInPlace(ctx, key, p); err != nil || reason != "" {
				if err != nil {
					reason = err.Error()
				}
				a.Action, a.Status, a.Reason = ReviveRefused, ReviveRefused, reason
			} else {
				a.Action = ReviveResumedInPlace
			}
		case reviveRespawnPlan:
			block, err := e.openTab(ctx, key.SessionID, core.PaneLabel(p.rec.Handle, p.rec.Role, p.rec.Task), p.Cwd, p.cmd)
			if err != nil {
				a.Action, a.Status, a.Reason = ReviveRefused, ReviveRefused, fmt.Sprintf("open a tab for %s: %v", p.rec.Handle, err)
			} else {
				a.Action, a.Block = ReviveRespawned, block
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// resumeInPlace types p's command into its block's shell, after checking
// the pane again. The command goes in a launcher script the shell runs
// with /bin/sh, so whatever shell the user's login shell is (fish quotes
// otherwise than sh), the agent's argv reaches it as written; the script
// removes itself, and a cd that fails leaves the shell, and the pane, as
// they were.
func (e Env) resumeInPlace(ctx context.Context, key core.SessionKey, p revivePlan) (string, error) {
	if reason, err := e.paneFree(ctx, key, p.rec); err != nil || reason != "" {
		return reason, err
	}
	script, err := e.writeLauncher(p.Cwd, p.cmd)
	if err != nil {
		return "", err
	}
	if label := core.PaneLabel(p.rec.Handle, p.rec.Role, p.rec.Task); label != "" {
		if err := e.Rex.RenameBlock(ctx, p.rec.BlockID, label); err != nil {
			fmt.Fprintf(Diagnostics, "label %s for %s: %v\n", p.rec.BlockID, p.rec.Handle, err)
		}
	}
	if err := e.Rex.Write(ctx, p.rec.BlockID, "/bin/sh "+shellQuote(script)+"\r"); err != nil {
		_ = os.Remove(script)
		return "", fmt.Errorf("type the resume of %s into %s: %w", p.rec.Handle, p.rec.BlockID, err)
	}
	return "", nil
}

// writeLauncher writes a one-shot /bin/sh script that removes itself,
// changes to dir, and execs cmd, and returns its path.
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
	body := "#!/bin/sh\n# Written by rex-amq-adapter epic revive: resumes an agent in its own pane.\nrm -f -- " + shellQuote(path) +
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
