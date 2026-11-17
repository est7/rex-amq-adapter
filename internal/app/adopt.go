package app

import (
	"context"
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
func (e Env) Adopt(ctx context.Context, loc Location, kind core.Kind, in HookInput) (Adoption, error) {
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return Adoption{}, err
	}
	defer unlock()

	srv, err := e.Rex.Whoami(ctx)
	if err != nil {
		return Adoption{}, err
	}
	// The agent is the block's foreground process. Claude Code reports its
	// own pid (CLAUDE_PID), which must be that process: a `claude -p` that
	// another program in the pane runs is not the pane's agent. A background
	// job is the exception: a terminal client is its pane's foreground, and
	// its title placed it (jobLocation).
	agentPID := in.AgentPID
	if agentPID <= 0 || kind == core.Claude && !in.Job {
		fg, ok, err := e.Rex.Foreground(ctx, loc.Block)
		if err != nil {
			return Adoption{}, err
		}
		if !ok {
			return Adoption{}, fmt.Errorf("block %s reports no foreground process", loc.Block)
		}
		if kind == core.Claude && agentPID > 0 && fg.PID != agentPID {
			return Adoption{}, fmt.Errorf("the hook's agent (pid %d) is not the foreground of block %s (pid %d), so it is not that pane's agent", agentPID, loc.Block, fg.PID)
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
				return Adoption{}, fmt.Errorf("read this hook's process ancestry: %w", err)
			default:
				if err := runsUnder(kind, ancestors, agentPID); err != nil {
					return Adoption{}, fmt.Errorf("block %s: its foreground (pid %d) is not this hook's agent: %w", loc.Block, agentPID, err)
				}
			}
		}
	}
	blk, err := e.Rex.InspectBlock(ctx, loc.Block)
	if err != nil {
		return Adoption{}, err
	}
	recs, err := e.agents()
	if err != nil {
		return Adoption{}, err
	}
	server := serverOf(srv, loc)
	inc := incarnation(server, agentPID)
	own, hasOwn := resolveIdentity(recs, loc.Block, inc, kind, lineage(recs, kind, in))
	// Who speaks for the conversation is settled before anything changes: a
	// refused pane parks no agent, names no handle, and makes no mailbox.
	if hasOwn && heldElsewhere(own, server, loc.Block, inc) {
		reason, gone := e.holderGone(ctx, own)
		if !gone {
			return Adoption{}, duplicateOf(in.SessionID, own)
		}
		// The lease is taken over below: AcquireLease gives this process the
		// next epoch, so a late release by the old one is a no-op (I4).
		fmt.Fprintf(Diagnostics, "%s takes over %s from pid %d in %s (%s)\n", loc.Block, own.Handle, own.pid(), own.BlockID, reason)
	}
	// The lease follows the agent to this block: a background job shown in
	// another pane than its lease's (jobLocation), or a takeover from a
	// process gone from another pane. It keeps its handle and is not
	// departing anywhere.
	moved := hasOwn && own.Live() && own.BlockID != loc.Block
	// A background job is adopted only into a pane that shows it: a hook on
	// every tool call trusts the block of the job's record, and that record
	// may be stale or an older adapter's.
	if in.Job {
		job, _, err := e.claudeJob(in.SessionID)
		if err != nil {
			return Adoption{}, err
		}
		shown, err := e.showsJob(ctx, loc.Block, own, job)
		if err != nil {
			return Adoption{}, err
		}
		if !shown {
			return Adoption{}, fmt.Errorf("%w: block %s shows something else", ErrJobNotShown, loc.Block)
		}
	}
	handle := own.Handle
	if !hasOwn {
		taken, err := e.takenHandles(recs)
		if err != nil {
			return Adoption{}, err
		}
		handle = core.NameHandle(kind, taken)
	}
	// Any other live record of this block is an agent this one replaced, or
	// the session this process switched away from: it keeps its handle as a
	// reservation for a resume.
	for _, r := range recs {
		if r.BlockID == loc.Block && r.Live() && r.Handle != handle {
			if err := e.park(ctx, r, store.DepartReplaced); err != nil {
				return Adoption{}, fmt.Errorf("park %s, replaced in this block: %w", r.Handle, err)
			}
		}
	}
	// The moved agent keeps its waker: the waker names the handle, and finds
	// the block from the lease when it rings (amq.Ensure below keeps it).
	shownAt := agent{SessionID: blk.SessionID}
	if moved && !together(own, shownAt) {
		e.leaveSession(ctx, own)
	}
	if err := e.AMQ.EnsureMailbox(ctx, e.Paths.Root(), string(handle)); err != nil {
		return Adoption{}, err
	}
	if !hasOwn {
		e.seedSentCursor(handle)
	}
	// What this adoption means. The live agent of this block coming back is a
	// repeated start hook or a compaction: nothing changes, unless the adoption
	// that leased it never finished. Then the agent was not told, and nor was
	// its window. One that departed was announced as gone, so taking its handle
	// back is news again.
	again := hasOwn && own.Live() && own.BlockID == loc.Block
	unfinished := hasOwn && own.Live() && !own.Adopted()
	arrived := !again || unfinished
	if moved {
		arrived = !together(own, shownAt) || unfinished // a move within the session is no news
	}
	known := hasOwn && in.SessionID != "" && own.Session() == in.SessionID && !unfinished
	now := e.Now().UTC()
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
		if id.Role == "" && core.LauncherLabel(blk.Label) {
			id.Role = core.Role(blk.Label)
		}
	} else {
		id.Task, id.Role = core.InitialTask(blk.Label), core.Role(blk.Label)
	}
	id = id.WithSession(in.SessionID)
	lease := store.Lease{
		Handle:         handle,
		Place:          store.Place{Server: server, SessionID: blk.SessionID, WindowID: blk.WindowID, BlockID: loc.Block},
		ServerInstance: srv.InstanceID,
		Incarnation:    inc,
		AcquiredAt:     now,
		// Unfinished until the waker and label follow: a move within the tab
		// tells no one but is a transition all the same.
		Adopting: !again || unfinished,
	}
	if again && own.pid() == agentPID {
		lease.AcquiredAt = own.adoptedAt()
	}
	// Where it runs, for a later resume; and who summoned it, for the guards.
	if fg, ok, err := e.Rex.Foreground(ctx, loc.Block); err == nil && ok && fg.Cwd != "" {
		id.Cwd = fg.Cwd
	}
	if id.Cwd == "" && hasOwn {
		id.Cwd = own.Cwd
	}
	// What the adapter launched it on; an agent started by hand, or a
	// repeated hook of one launched before this version, keeps its own.
	if in.Launch != nil {
		id.Launched, id.Model, id.Effort = true, in.Launch.Model, in.Launch.Effort
	}
	if id.Unattended, err = e.adoptUnattended(id.Role, blk.SessionID, in, hasOwn && own.Unattended); err != nil {
		return Adoption{}, err
	}
	sm, summoned, err := e.Store.Summon(in.SummonToken)
	if err != nil {
		fmt.Fprintf(Diagnostics, "read the summon of %s: %v\n", in.SummonToken, err)
	} else if summoned && !again {
		id.Parent, id.Depth, id.SummonedFor = sm.Parent, sm.Depth, sm.For
	}
	// Leased before the waker starts: whatever happens next, departure and
	// collection find this lease and retire the handle's waker, which amq
	// identifies by the handle its saved target names, and a later hook
	// finishes the adoption.
	if err := e.Store.PutIdentity(id); err != nil {
		return Adoption{}, err
	}
	if lease, err = e.Store.AcquireLease(lease, now); err != nil {
		return Adoption{}, err
	}
	rec, _, err := e.agentOf(handle)
	if err != nil {
		return Adoption{}, err
	}
	// The committed identity preserves lineage if token cleanup fails or the
	// process exits here; a retry keeps that lineage and finishes cleanup.
	if summoned {
		if err := e.Store.DeleteSummon(in.SummonToken); err != nil {
			return Adoption{}, fmt.Errorf("consume committed summon %s: %w", in.SummonToken, err)
		}
	}
	// Before the waker starts, so it reads them from the start.
	e.alignHolds(ctx, rec, own.Role)
	var decision amq.WakeDecision
	if kind.InProcess() {
		// The agent's own extension delivers. A waker left for this handle
		// would type doorbells into it: remove it.
		if _, err := e.AMQ.RetireHandle(ctx, e.Paths.Root(), string(handle)); err != nil {
			return Adoption{}, err
		}
	} else {
		if err := e.prepareInjector(); err != nil {
			return Adoption{}, err
		}
		if _, decision, err = e.AMQ.Ensure(ctx, e.waker(rec), e.Paths.Logs()); err != nil {
			return Adoption{}, err
		}
	}

	if err := e.ensureSessionWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	// The identity carries the role, so briefing and orchestrator behaviour read one
	// fact: the label is overwritten below, and a resumed agent may land in
	// any pane.
	out := Adoption{Decision: decision, Known: known, Role: rec.Role, Window: blk.WindowID, Session: blk.SessionID}
	out.Mates = e.joinSession(ctx, rec, blk, recs, arrived)
	if label := core.PaneLabel(handle, rec.Role, rec.Task); blk.Label != label {
		out.LabelErr = e.Rex.RenameBlock(ctx, loc.Block, label)
	}
	// The tab follows even when the pane label was right already: Rex names
	// a new tab "Window <8-hex>", and a summoned agent's tab its launch label.
	e.followTab(ctx, blk.SessionID, blk.WindowID)
	// The gate runs before the agent is told of its mail, and before a Pi
	// extension, handed the inbox, reads it.
	unread, err := e.admit(rec)
	if err != nil {
		return Adoption{}, err
	}
	out.Unread = len(unread)
	if lease.Adopting {
		if err := e.Store.FinishAdoption(handle, lease.Epoch); err != nil {
			return Adoption{}, err
		}
		rec.lease.Adopting = false
	}
	out.Agent = rec
	return out, nil
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
	unlock, err := e.Store.LockLifecycle(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	rec, ok, err := e.agentAt(loc.Block)
	if err != nil {
		return "", err
	}
	if !ok || !rec.Live() {
		return "", fmt.Errorf("no AMQ identity for this Rex block (%s), so there is no pane label to set", loc.Block)
	}
	rec.Task = core.Task(task)
	if err := e.Store.PutIdentity(rec.Identity); err != nil {
		return "", err
	}
	e.refreshTitle(ctx, rec)
	label := core.PaneLabel(rec.Handle, rec.Role, rec.Task)
	if err := e.Rex.RenameBlock(ctx, loc.Block, label); err != nil {
		return "", fmt.Errorf("the task is recorded, but the pane label could not be set: %w", err)
	}
	e.followBlockTab(ctx, loc.Block)
	return label, nil
}

// Exports is the shell text `rex-amq-adapter env` prints for the agent in loc.
func (e Env) Exports(loc Location) (string, error) {
	rec, ok, err := e.agentAt(loc.Block)
	if err != nil {
		return "", err
	}
	if !ok || !rec.Live() {
		return "", fmt.Errorf("no AMQ identity for this Rex block (%s); it is assigned when an agent's first turn starts here with the adapter's hooks installed", loc.Block)
	}
	// An unattended agent's shells carry the marker even when its harness
	// dropped it; nothing here ever clears it (unattended.go).
	unattended := ""
	if rec.Unattended && rec.Role != core.PersonalRole {
		unattended = " " + UnattendedEnv + "=1"
	}
	return fmt.Sprintf("export AM_ROOT=%s AM_ME=%s%s\n", shellQuote(e.Paths.Root()), shellQuote(string(rec.Handle)), unattended), nil
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
