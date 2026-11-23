package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Adoption is the result of adopting the agent in a block.
type Adoption struct {
	Agent  agent
	Unread int
	// Decision is what happened to the waker; zero for in-process kinds,
	// which have none.
	Decision amq.WakeDecision
	// Known is set when this agent session already held the handle: it was
	// told the handle when it first took it, so it needs no identity notice.
	Known bool
	// Mates are the live agents in the same Rex session.
	Mates []core.Peer
	// Role is what the pane label said the agent is for; Window and
	// Session are where its block is. The role and the session's crew choose
	// its briefing.
	Role, Window, Session string
	// LabelErr is set when the handle could not be shown as the block
	// label. The handle works regardless; callers report it.
	LabelErr error
}

// Adopt gives the agent running in loc a handle, a mailbox, and a live
// waker, and records the binding. The handle follows the agent session (see
// resolveIdentity): a session resumed in a new pane gets its handle back, and
// adopting an agent already adopted keeps everything as is. A session another
// pane's live agent runs is refused with a *DuplicateSessionError, before
// anything changes.
//
// It runs in three phases so that the lifecycle lock, which every hook on the
// host waits for, holds local file I/O only: Probe asks Rex (and reads the
// process table) with no lock; Commit decides and writes the records under
// the lock; Reconcile makes the mailbox, the waker, the labels, and the
// notes follow, with no lock. Commit marks the lease adopting, so a hook cut
// short in Reconcile leaves the next hook to finish it. A probe the records
// moved away from while it ran (errStaleProbe) is taken again.
func (e Env) Adopt(ctx context.Context, loc Location, kind core.Kind, in HookInput) (Adoption, error) {
	for attempt := 1; ; attempt++ {
		p, err := e.probeAdoption(ctx, loc, kind, in)
		if err != nil {
			return Adoption{}, err
		}
		c, err := e.commitAdoption(ctx, p, kind, in)
		if err == nil {
			var a Adoption
			if a, err = e.reconcileAdoption(ctx, p, c); err == nil {
				return a, nil
			}
		}
		if !errors.Is(err, errStaleProbe) || attempt >= adoptProbes {
			return Adoption{}, err
		}
	}
}

// adoptProbes bounds how often an adoption probes Rex again because the
// records changed under it.
const adoptProbes = 3

// errStaleProbe is a Commit that found the records no longer those its
// probe judged: another adoption or departure of the same handle ran in
// between.
var errStaleProbe = errors.New("the records changed while this adoption asked Rex; probe again")

// errSuperseded is a Reconcile whose binding another adoption or a
// departure replaced before it finished: it neither labels the pane nor
// reports the agent adopted.
var errSuperseded = errors.New("another adoption or a departure replaced this adoption's binding before it finished")

// samePlacement reports whether now is still the record probed was, as
// placed: the same binding (sameBinding), in the same Rex session and window,
// behind the same fence. A session reconciliation (followMoves) that moved
// the record since makes a probe of it stale, so Commit never writes back a
// placement older than one Rex reported later.
func samePlacement(now, probed agent) bool {
	return now.Handle == probed.Handle && sameBinding(now, probed) && now.Place == probed.Place && now.Fence == probed.Fence
}

// liveIn is the agent live in block of server among recs (I3: at most one).
func liveIn(recs []agent, server, block string) (agent, bool) {
	for _, r := range recs {
		if r.Live() && r.Server == server && r.BlockID == block {
			return r, true
		}
	}
	return agent{}, false
}

// adoptionProbe is what Rex and the process table said about the agent in
// loc, gathered with no lock held.
type adoptionProbe struct {
	loc    Location
	srv    rex.Server
	server string
	// occupant is the agent live in the block before Rex was asked; nil
	// when none was.
	occupant *agent
	agentPID int
	inc      store.Incarnation
	cwd      string
	blk      rex.Block
	// holder judges the lease the conversation's handle had when probed, held
	// by another process; nil when it was not.
	holder *holderVerdict
	// jobShown is, for a background job, whether loc's block shows it, as
	// judged with the record of jobOf ("" for none).
	jobShown bool
	jobOf    core.Handle
}

// holderVerdict is holderGone's answer about one lease.
type holderVerdict struct {
	holder agent // the record judged, its lease and placement as probed
	reason string
	gone   bool
}

// probeAdoption asks Rex where the agent of loc runs and what holds its
// conversation, without the lifecycle lock: each question can take seconds
// on a loaded server.
func (e Env) probeAdoption(ctx context.Context, loc Location, kind core.Kind, in HookInput) (adoptionProbe, error) {
	p := adoptionProbe{loc: loc}
	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return p, err
	}
	p.srv, p.server = srv, serverOf(srv, loc)
	// The records as they are before Rex is asked about the block: Commit
	// reads them again under the lock, and what changed in between (the
	// block taken by another agent, the conversation's lease moved) makes
	// this probe stale.
	recs, err := e.agents()
	if err != nil {
		return p, err
	}
	if r, ok := liveIn(recs, p.server, loc.Block); ok {
		p.occupant = &r
	}
	// The agent is the block's foreground process. Claude Code reports its
	// own pid (CLAUDE_PID), which must be that process: a `claude -p` that
	// another program in the pane runs is not the pane's agent. A background
	// job is the exception: a terminal client is its pane's foreground, and
	// its title placed it (jobLocation). The same answer gives the directory
	// it works in, for admission and a later resume.
	fg, fgOK, fgErr := e.Rex.Foreground(ctx, loc.Block)
	agentPID := in.AgentPID
	if agentPID <= 0 || kind == core.Claude && !in.Job {
		if fgErr != nil {
			return p, fgErr
		}
		if !fgOK {
			return p, fmt.Errorf("block %s reports no foreground process", loc.Block)
		}
		if kind == core.Claude && agentPID > 0 && fg.PID != agentPID {
			return p, fmt.Errorf("the hook's agent (pid %d) is not the foreground of block %s (pid %d), so it is not that pane's agent", agentPID, loc.Block, fg.PID)
		}
		agentPID = fg.PID
		// A Codex hook reports no pid, and the block is the one its harness
		// inherited: a shared daemon runs other panes' threads under the first
		// pane's location. Taking the foreground on trust would hand that
		// pane's handle and mail to another thread (resolveIdentity matches
		// it by process).
		// An agy hook reports no pid either, and a headless `agy -p` that a
		// command in the pane runs fires the user's hooks with the pane's
		// location: its foreground is the agent that ran the command.
		if (kind == core.Codex || kind == core.Agy) && localServer(serverOf(srv, loc)) {
			ancestors, err := e.ancestors()
			switch {
			case err != nil && kind == core.Agy:
				// An agy hook that cannot place itself adopts the foreground,
				// as before this check: an unreadable ancestry must not leave
				// the pane's agy without mail (descendsFrom).
				fmt.Fprintf(Diagnostics, "read this hook's process ancestry: %v\n", err)
			case err != nil:
				return p, fmt.Errorf("read this hook's process ancestry: %w", err)
			default:
				if err := runsUnder(kind, ancestors, agentPID); err != nil {
					return p, fmt.Errorf("block %s: its foreground (pid %d) is not this hook's agent: %w", loc.Block, agentPID, err)
				}
			}
		}
	}
	if fgErr == nil && fgOK {
		p.cwd = fg.Cwd
	}
	if p.blk, err = e.Rex.InspectBlock(ctx, loc.Block); err != nil {
		return p, err
	}
	if err := e.awaitEpicPolicy(ctx, p.blk.SessionID); err != nil {
		return p, err
	}
	p.agentPID = agentPID
	p.inc = incarnation(p.server, agentPID)
	own, hasOwn := resolveIdentity(recs, p.server, loc.Block, p.inc, kind, lineage(recs, kind, in))
	if hasOwn && heldElsewhere(own, p.server, loc.Block, p.inc) {
		reason, gone := e.holderGone(ctx, own)
		p.holder = &holderVerdict{holder: own, reason: reason, gone: gone}
	}
	// A background job is adopted only into a pane that shows it: a hook on
	// every tool call trusts the block of the job's record, and that record
	// may be stale or an older adapter's.
	if in.Job {
		if fgErr != nil {
			return p, fgErr
		}
		job, _, err := e.claudeJob(in.SessionID)
		if err != nil {
			return p, err
		}
		if p.jobShown, err = e.showsJobWith(ctx, loc.Block, fg, fgErr == nil && fgOK, own, job); err != nil {
			return p, err
		}
		p.jobOf = own.Handle
	}
	return p, nil
}

// awaitEpicPolicy waits, when session has no policy, until no Epic is being
// opened: its agents start as Rex creates its session, and one adopted
// before the Epic's policy, crew, and grant are recorded would be briefed
// without them and never lead (newEpicSession).
func (e Env) awaitEpicPolicy(ctx context.Context, session string) error {
	if session == "" {
		return nil
	}
	if _, ok, err := e.Store.SessionPolicy(session); err != nil || ok {
		return err
	}
	return e.Store.AwaitEpicOpenings(ctx)
}

// adoptionCommit is what Commit recorded, for Reconcile to make follow.
type adoptionCommit struct {
	rec     agent
	own     agent
	hasOwn  bool
	recs    []agent // the records Commit decided on, before its writes
	arrived bool
	known   bool
	// leftOwn: the agent moved here from a session it is no longer in.
	leftOwn bool
	// replaced are the agents this one replaced in its block, departed.
	replaced []agent
}

// commitAdoption decides the adoption and writes it, under the lifecycle
// lock, reading and writing local files only. The probe's verdicts count
// only for the records they were made about: a takeover verdict about
// another lease than the one held now, or a job judged with another record,
// is errStaleProbe.
func (e Env) commitAdoption(ctx context.Context, p adoptionProbe, kind core.Kind, in HookInput) (adoptionCommit, error) {
	var c adoptionCommit
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return c, err
	}
	defer unlock()
	loc, blk, server, inc := p.loc, p.blk, p.server, p.inc
	recs, err := e.agents()
	if err != nil {
		return c, err
	}
	// The block's agent changed since the probe began: what the probe saw
	// of the block's foreground may be that agent's, not this hook's.
	if occupant, ok := liveIn(recs, server, loc.Block); ok != (p.occupant != nil) || ok && !samePlacement(occupant, *p.occupant) {
		return c, errStaleProbe
	}
	own, hasOwn := resolveIdentity(recs, server, loc.Block, inc, kind, lineage(recs, kind, in))
	// Who speaks for the conversation is settled before anything changes: a
	// refused pane parks no agent, names no handle, and makes no mailbox.
	if hasOwn && heldElsewhere(own, server, loc.Block, inc) {
		if p.holder == nil || !samePlacement(own, p.holder.holder) {
			return c, errStaleProbe
		}
		if !p.holder.gone {
			return c, duplicateOf(in.SessionID, own)
		}
		// The lease is taken over below: AcquireLease gives this process the
		// next epoch, so a late release by the old one is a no-op (I4).
		fmt.Fprintf(Diagnostics, "%s takes over %s from pid %d in %s (%s)\n", loc.Block, own.Handle, own.pid(), own.BlockID, p.holder.reason)
	}
	if in.Job {
		if p.jobOf != own.Handle {
			return c, errStaleProbe
		}
		if !p.jobShown {
			return c, fmt.Errorf("%w: block %s shows something else", ErrJobNotShown, loc.Block)
		}
	}
	// The lease follows the agent to this block: a background job shown in
	// another pane than its lease's (jobLocation), or a takeover from a
	// process gone from another pane. It keeps its handle and is not
	// departing anywhere.
	moved := hasOwn && own.Live() && own.BlockID != loc.Block
	handle := own.Handle
	if !hasOwn {
		taken, err := e.takenHandles(recs)
		if err != nil {
			return c, err
		}
		handle = core.NameHandle(kind, taken)
	}
	// Where it runs, for admission and a later resume; and who summoned it,
	// for admission and the guards.
	cwd := p.cwd
	if cwd == "" && hasOwn {
		cwd = own.Cwd
	}
	sm, summoned, err := e.Store.Summon(in.SummonToken)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the summon of %s: %v\n", in.SummonToken, err)
		summoned = false
	}
	// Admission (C25) before anything changes: a refused agent parks no
	// agent, and gets no mailbox, identity, or lease.
	role := core.Role(blk.Label)
	if hasOwn {
		if role = own.Role; role == "" && core.LauncherLabel(blk.Label) {
			role = core.Role(blk.Label)
		}
	}
	// An agent that stays in its session keeps its admission, unless the role
	// it takes now is not the one it was admitted with (a launcher label that
	// names a role for an agent that had none): that is admitted again, and a
	// refusal leaves its identity and lease as they were.
	stays := hasOwn && own.Live() && own.SessionID == blk.SessionID
	admits := !stays || role != own.Role
	hubKey := core.HubKey("")
	if admits {
		if role, hubKey, err = e.admitAdoption(recs, server, blk.SessionID, handle, own, hasOwn, role, cwd, summoned || in.SummonToken != "", in.Unattended); err != nil {
			return c, err
		}
	}
	now := e.Now().UTC()
	// Any other live record of this block is an agent this one replaced, or
	// the session this process switched away from: it keeps its handle as a
	// reservation for a resume. Reconcile retires its waker and tells its
	// session.
	for _, r := range recs {
		if r.BlockID == loc.Block && r.Server == server && r.Live() && r.Handle != handle {
			left, err := e.releaseLease(r, store.Departure{Reason: store.DepartReplaced, At: now, Unfinished: true})
			if err != nil {
				return c, fmt.Errorf("release %s, replaced in this block: %w", r.Handle, err)
			}
			c.replaced = append(c.replaced, left)
		}
	}
	shownAt := agent{SessionID: blk.SessionID}
	c.leftOwn = moved && !together(own, shownAt)
	if !hasOwn {
		e.seedSentCursor(handle)
	}
	// What this adoption means. The live agent of this block coming back is a
	// repeated start hook or a compaction: nothing changes, unless the adoption
	// that leased it never finished. Then the agent was not told, and nor was
	// its window. One that departed was announced as gone, so taking its handle
	// back is news again. A handle never placed (its adoption died before its
	// lease) was never told either.
	again := hasOwn && own.Live() && own.BlockID == loc.Block
	unfinished := hasOwn && !own.Adopted() && own.departure() == nil
	c.arrived = !again || unfinished
	if moved {
		c.arrived = !together(own, shownAt) || unfinished // a move within the session is no news
	}
	c.known = hasOwn && in.SessionID != "" && own.Session() == in.SessionID && !unfinished
	// A resumed agent keeps who it is: its task and role, in any pane, and
	// the sessions and placements it ran as. One without a role takes the one
	// its launcher wrote in the label; a label the adapter wrote is an earlier
	// agent's and gives it nothing. A new agent takes the role the pane is
	// labelled with, whoever wrote it, so a pane keeps its purpose across
	// agents.
	id := store.Identity{Handle: handle, Kind: kind, FirstAdoptedAt: now}
	if hasOwn {
		id = own.Identity
		id.FirstAdoptedAt = own.InitialAdoptedAt()
	} else {
		id.Task = core.InitialTask(blk.Label)
	}
	id.Role = role
	id = id.WithSession(in.SessionID)
	lease := store.Lease{
		Handle:         handle,
		Place:          store.Place{Server: server, SessionID: blk.SessionID, WindowID: blk.WindowID, BlockID: loc.Block},
		ServerInstance: p.srv.InstanceID,
		Incarnation:    inc,
		AcquiredAt:     now,
		// Unfinished until the waker and label follow: a move within the tab
		// tells no one but is a transition all the same.
		Adopting: !again || unfinished,
	}
	if again && own.pid() == p.agentPID {
		lease.AcquiredAt = own.adoptedAt()
	}
	if cwd != "" {
		id.Cwd = cwd
	}
	// What the adapter launched it on; an agent started by hand, or a
	// repeated hook of one launched before this version, keeps its own.
	if in.Launch != nil {
		id.Launched, id.Model, id.Effort = true, in.Launch.Model, in.Launch.Effort
	}
	if id.Unattended, err = e.adoptUnattended(id.Role, blk.SessionID, in, hasOwn && own.Unattended); err != nil {
		return c, err
	}
	if summoned && !again {
		id.Parent, id.Depth, id.SummonedFor = sm.Parent, sm.Depth, sm.For
	}
	// A personal agent makes its session a Hub's before it is published, so
	// no admission ever sees it in a session of no policy.
	if id.Role == core.PersonalRole && admits {
		if err := e.ensureHubPolicy(blk.SessionID, server, hubKey, handle); err != nil {
			return c, err
		}
	}
	// Leased before the waker starts: whatever happens next, departure and
	// collection find this lease and retire the handle's waker, which amq
	// identifies by the handle its saved target names, and a later hook
	// finishes the adoption.
	if err := e.Store.PutIdentity(id); err != nil {
		return c, err
	}
	if lease, err = e.Store.AcquireLease(lease, now); err != nil {
		return c, err
	}
	rec, _, err := e.agentOf(handle)
	if err != nil {
		return c, err
	}
	// The first agent of the Epic's lead role nobody summoned becomes its
	// lead, before anything reads who leads (its watcher, its notice).
	if err := e.claimLead(rec); err != nil {
		return c, err
	}
	// The committed identity preserves lineage if token cleanup fails or the
	// process exits here; a retry keeps that lineage and finishes cleanup.
	if summoned {
		if err := e.Store.DeleteSummon(in.SummonToken); err != nil {
			return c, fmt.Errorf("consume committed summon %s: %w", in.SummonToken, err)
		}
	}
	c.rec, c.own, c.hasOwn, c.recs = rec, own, hasOwn, recs
	return c, nil
}

// reconcileAdoption makes everything that follows the committed records
// follow them, with no lifecycle lock: the replaced agents' departures, the
// mailbox, the doorbell holds, the waker, the session watcher, the mates'
// notes, the labels, and the arrival gate; then it marks the lease adopted.
func (e Env) reconcileAdoption(ctx context.Context, p adoptionProbe, c adoptionCommit) (Adoption, error) {
	rec, blk, handle := c.rec, p.blk, c.rec.Handle
	for _, r := range c.replaced {
		e.settleDeparture(ctx, r)
	}
	if c.leftOwn {
		e.leaveSession(ctx, c.own)
	}
	// Before the mailbox and the waker, which can run out of the hook's
	// time: a lead's or narrator's watcher then finishes its waker
	// (superviseWatched) though the idle agent runs no further hook.
	if err := e.ensureSessionWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), string(handle)); err != nil {
		return Adoption{}, err
	}
	// Before the waker starts, so it reads them from the start.
	e.alignHolds(ctx, rec, c.own.Role)
	// The moved agent keeps its waker: the waker names the handle, and finds
	// the block from the lease when it rings. An in-process agent's own
	// extension delivers, and a waker left for its handle would type
	// doorbells into it: reconcileWaker removes it.
	decision, err := e.reconcileWaker(ctx, handle)
	if err != nil {
		return Adoption{}, err
	}
	out := Adoption{Decision: decision, Known: c.known}
	out.Mates = e.joinSession(ctx, rec, blk, c.recs, c.arrived)
	if out.LabelErr, err = e.labelPane(ctx, c, p.loc.Block); err != nil {
		return Adoption{}, err
	}
	// The tab follows even when the pane label was right already: Rex names
	// a new tab "Window <8-hex>", and a summoned agent's tab its launch label.
	e.followTab(ctx, blk.SessionID, blk.WindowID)
	// The arrival gate, then the lease marked adopted, both only while the
	// record is still the one this adoption committed, placement included:
	// a gate that fails leaves the adoption unfinished for the next hook, a
	// superseded one reports failure (its hook tells nobody of anything),
	// and one moved meanwhile is adopted again from a new probe.
	admitted, unread, err := e.admitHeld(ctx, c)
	if err != nil {
		return Adoption{}, err
	}
	if rec, err = e.finishAdoption(ctx, admitted); err != nil {
		return Adoption{}, err
	}
	// The identity carries the role, so briefing and orchestrator behaviour
	// read one fact: the label was overwritten above, and a resumed agent
	// may land in any pane. Where it is comes from the same record.
	out.Unread, out.Role, out.Window, out.Session = unread, rec.Role, rec.WindowID, rec.SessionID
	if rec.Role == core.PersonalRole && c.arrived {
		// Mail its Hub's departed personal agents left pending (C26).
		e.redeliverPending(ctx, rec)
	}
	out.Agent = rec
	return out, nil
}

// labelPane shows the handle of c's agent, with the role and task its
// record has now, as block's label, under the block's pane lock and only
// while c's binding holds the block: the last writer of the label (this
// adoption, a replacement's, an `agent title`) reads the record under the
// same lock and writes it whatever the pane showed before, so the pane never
// ends on an older handle or task. The first error is Rex's, reported, never
// fatal; the second is errSuperseded.
func (e Env) labelPane(ctx context.Context, c adoptionCommit, block string) (error, error) {
	unlock, err := e.Store.LockPane(ctx, block)
	if err != nil {
		return nil, err
	}
	defer unlock()
	now, ok, err := e.agentOf(c.rec.Handle)
	if err != nil {
		return nil, err
	}
	if !ok || !sameBinding(now, c.rec) {
		return nil, errSuperseded
	}
	return e.Rex.RenameBlock(ctx, block, core.PaneLabel(now.Handle, now.Role, now.Task)), nil
}

// stillCommitted is nil when now is the record c committed, placement
// included; errStaleProbe when the same binding moved since (a session
// reconciliation), so the adoption is taken again; errSuperseded otherwise.
func stillCommitted(now agent, ok bool, c adoptionCommit) error {
	switch {
	case ok && samePlacement(now, c.rec):
		return nil
	case ok && sameBinding(now, c.rec):
		return errStaleProbe
	}
	return errSuperseded
}

// admitHeld runs the arrival gate (admit) on c's agent under its handle's
// lock, so its departure cannot run meanwhile, while it is still the record
// c committed. The gate reads every unread message and may quarantine each,
// so it holds no lock every hook on the host waits for. It returns the record
// the gate judged with and how much mail it left unread.
func (e Env) admitHeld(ctx context.Context, c adoptionCommit) (agent, int, error) {
	unlock, err := e.Store.LockHandle(ctx, c.rec.Handle)
	if err != nil {
		return agent{}, 0, err
	}
	defer unlock()
	now, ok, err := e.agentOf(c.rec.Handle)
	if err != nil {
		return agent{}, 0, err
	}
	if err := stillCommitted(now, ok, c); err != nil {
		return agent{}, 0, err
	}
	unread, err := e.admit(now)
	if errors.Is(err, errReaderMoved) {
		err = errStaleProbe
	}
	return now, len(unread), err
}

// finishAdoption marks the lease of admitted (the record the arrival gate
// judged with) adopted, under the lifecycle lock, while the record is still
// that one, placement included; otherwise it is stillCommitted's error. It
// returns the record as it is now.
func (e Env) finishAdoption(ctx context.Context, admitted agent) (agent, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return admitted, err
	}
	defer unlock()
	now, ok, err := e.agentOf(admitted.Handle)
	if err != nil {
		return admitted, err
	}
	if err := stillCommitted(now, ok, adoptionCommit{rec: admitted}); err != nil {
		return admitted, err
	}
	if now.lease.Adopting {
		if err := e.Store.FinishAdoption(now.Handle, now.lease.Epoch); err != nil {
			return now, err
		}
		now.lease.Adopting = false
	}
	return now, nil
}

// admitAdoption decides, under the lifecycle lock, the role of an agent
// arriving in session (a new one, or one resumed or moved there): role is
// what its record or its pane label says, which an unlabelled launch in a
// Hub directory (or a Hub session) turns into personal (core.AutoPersonal).
// A summoned agent never enters a Hub session. A refusal is a *HubRefusal;
// hubKey is the Hub a personal agent joins.
func (e Env) admitAdoption(recs []agent, server, session string, h core.Handle, own agent, hasOwn bool, role, cwd string, summoned, unattended bool) (string, core.HubKey, error) {
	f, p, err := e.sessionFacts(session, recs, h)
	if err != nil {
		return "", "", err
	}
	key, inHub, err := e.hubOf(server, cwd)
	if err != nil {
		return "", "", fmt.Errorf("resolve the Hub directories: %w", err)
	}
	if role == "" && core.AutoPersonal(f, false, summoned, unattended, inHub) {
		role = core.PersonalRole
	}
	from := ""
	if hasOwn {
		from = own.Role
	}
	reason := core.AdmitRole(f, from, role)
	if reason == "" && summoned && f.Hub() {
		reason = core.ReasonSessionPure
	}
	warnConflict(session, f)
	if reason != "" {
		notice := core.HubRefusalNotice
		if reason == core.ReasonNoPersonal {
			notice = "AMQ: this Rex session is an Epic's, and a personal agent lives only in a Hub session, so this agent got no AMQ identity, mailbox, or lease. Start the personal agent in a Rex session of its own, in the Hub directory."
		}
		return "", "", &HubRefusal{Reason: reason, Notice: notice}
	}
	if p.Hub() && p.HubKey != "" {
		key = p.HubKey
	}
	return role, key, nil
}

// peerOf is r's agent as the session policy sees it.
func peerOf(r agent) core.Peer {
	return core.Peer{
		Handle:    r.Handle,
		Kind:      r.Kind,
		Role:      r.Role,
		Block:     r.BlockID,
		Window:    r.WindowID,
		Parent:    r.Parent,
		AdoptedAt: r.InitialAdoptedAt(),
	}
}

// serverOf is the address a record names: the server's own endpoint, else
// the caller's.
func serverOf(srv rex.Server, loc Location) string {
	if srv.Endpoint != "" {
		return srv.Endpoint
	}
	return loc.Server
}

// joinSession returns the live agents in the session of rec's block and,
// when the agent has just arrived in its block, queues a note for each of them
// that it started beside them. The session is advice, not a binding: when Rex
// cannot list it, adoption stands without mates and the failure is reported.
func (e Env) joinSession(ctx context.Context, rec agent, blk rex.Block, recs []agent, arrived bool) []core.Peer {
	if blk.SessionID == "" {
		return nil
	}
	blocks, err := e.Rex.ListBlocks(ctx, blk.SessionID)
	if err != nil {
		fmt.Fprintf(Diagnostics, "list the blocks of %s: %v\n", blk.SessionID, err)
		return nil
	}
	windowOf := make(map[string]string, len(blocks))
	for _, b := range blocks {
		windowOf[b.ID] = b.WindowID
	}
	var peers []core.Peer
	for _, r := range recs {
		if r.Live() && r.Handle != rec.Handle {
			peers = append(peers, peerOf(r))
		}
	}
	mates := core.SessionMates(rec.BlockID, windowOf, peers)
	if !arrived || core.Stealth(rec.Role) {
		// A narrator or personal agent arrives unannounced.
		return mates
	}
	e.tellMates(ctx, peerOf(rec), mates, core.JoinedNotice(peerOf(rec)))
	return mates
}

// tellMates queues note for each of mates, saying of joined, which just
// started or took a role. When joined is an orchestrator, each mate that is not
// one also hears who now stands in for the human, naming every orchestrator in
// the session; another orchestrator keeps its own goal.
func (e Env) tellMates(ctx context.Context, joined core.Peer, mates []core.Peer, note string) {
	orch := ""
	if joined.Role == core.OrchestratorRole {
		orch = core.OrchestratorNotice(append(slices.Clone(mates), joined))
	}
	for _, m := range mates {
		n := note
		if orch != "" && m.Role != core.OrchestratorRole {
			n = joinNotices(n, orch)
		}
		if n == "" {
			continue
		}
		if err := e.noteWindow(ctx, m.Handle, n); err != nil {
			fmt.Fprintf(Diagnostics, "tell %s about %s: %v\n", m.Handle, joined.Handle, err)
		}
	}
}

func (e Env) noteWindow(ctx context.Context, h core.Handle, note string) error {
	unlock, err := e.Store.LockLedger(ctx, h)
	if err != nil {
		return err
	}
	defer unlock()
	return e.Store.AddWindowNote(h, note)
}

// takenHandles are the handles a new name must avoid: every record's (live,
// or reserved for a departed session), and every mailbox holding undrained
// mail, so a new agent never inherits someone else's unread messages.
func (e Env) takenHandles(recs []agent) (map[core.Handle]bool, error) {
	taken := map[core.Handle]bool{}
	for _, r := range recs {
		taken[r.Handle] = true
	}
	agents, err := amq.Agents(e.Paths.Root())
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		h := core.Handle(a)
		if taken[h] {
			continue
		}
		unread, err := amq.Unread(e.Paths.Root(), a)
		if err != nil {
			return nil, err
		}
		if len(unread) > 0 {
			taken[h] = true
		}
	}
	return taken, nil
}

// SetTask records what the agent in loc says it is doing and shows it after
// its handle in the pane label. A blank task clears it. It returns the label.
// The record is the source: adoption draws the label from it again, so a
// resumed agent shows its task in whichever pane it lands.
func (e Env) SetTask(ctx context.Context, loc Location, task string) (string, error) {
	rec, err := e.recordTask(ctx, loc, task)
	if err != nil {
		return "", err
	}
	e.refreshTitle(ctx, rec)
	label, err := e.labelCurrent(ctx, loc.Block, rec.Handle)
	if err != nil {
		return "", fmt.Errorf("the task is recorded, but the pane label could not be set: %w", err)
	}
	e.followBlockTab(ctx, loc.Block)
	return label, nil
}

// labelCurrent sets block's label from h's record as it is under the
// block's pane lock, so of two overlapping label writers the last one shows
// the latest task (labelPane). It returns the label.
func (e Env) labelCurrent(ctx context.Context, block string, h core.Handle) (string, error) {
	unlock, err := e.Store.LockPane(ctx, block)
	if err != nil {
		return "", err
	}
	defer unlock()
	now, ok, err := e.agentOf(h)
	if err != nil {
		return "", err
	}
	if !ok || !now.Live() || now.BlockID != block {
		return "", fmt.Errorf("%s no longer runs in %s", h, block)
	}
	label := core.PaneLabel(now.Handle, now.Role, now.Task)
	return label, e.Rex.RenameBlock(ctx, block, label)
}

// recordTask records task for the live agent in loc, under the lifecycle
// lock, and returns its record.
func (e Env) recordTask(ctx context.Context, loc Location, task string) (agent, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return agent{}, err
	}
	defer unlock()
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return agent{}, err
	}
	if !ok || !rec.Live() {
		return agent{}, fmt.Errorf("no AMQ identity for this Rex block (%s), so there is no pane label to set", loc.Block)
	}
	rec.Task = core.Task(task)
	return rec, e.Store.PutIdentity(rec.Identity)
}

// Exports is the shell text `rex-amq-adapter agent env --sh` prints for the agent in loc.
func (e Env) Exports(loc Location) (string, error) {
	se, err := e.ShellEnvOf(loc)
	if err != nil {
		return "", err
	}
	unattended := ""
	if se.Unattended {
		unattended = " " + UnattendedEnv + "=1"
	}
	return fmt.Sprintf("export AM_ROOT=%s AM_ME=%s%s\n", shellQuote(se.AMRoot), shellQuote(se.AMMe), unattended), nil
}

// ShellEnv is the AMQ identity an agent's shell takes: its amq root and
// handle, and whether its shells run unattended.
type ShellEnv struct {
	AMRoot     string `json:"AM_ROOT"`
	AMMe       string `json:"AM_ME"`
	Unattended bool   `json:"ME_UNATTENDED"`
	Role       string `json:"role"`
}

// ShellEnvOf is the shell identity of the live agent in loc.
func (e Env) ShellEnvOf(loc Location) (ShellEnv, error) {
	rec, ok, err := e.agentIn(loc)
	if err != nil {
		return ShellEnv{}, err
	}
	if !ok || !rec.Live() {
		return ShellEnv{}, fmt.Errorf("no AMQ identity for this Rex block (%s); it is assigned when an agent's first turn starts here with the adapter's hooks installed", loc.Block)
	}
	// An unattended agent's shells carry the marker even when its harness
	// dropped it; nothing here ever clears it (unattended.go).
	return ShellEnv{AMRoot: e.Paths.Root(), AMMe: string(rec.Handle), Unattended: rec.Unattended && rec.Role != core.PersonalRole, Role: rec.Role}, nil
}

func shellQuote(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	return out + "'"
}
