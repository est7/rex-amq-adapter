package app

import (
	"context"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/rex"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Adoption is the result of adopting the agent in a block.
type Adoption struct {
	Record store.Record
	Unread int
	// Decision is what happened to the waker; zero for in-process kinds,
	// which have none.
	Decision amq.WakeDecision
	// Known is set when this agent session already held the handle: it was
	// told the handle when it first took it, so it needs no identity notice.
	Known bool
	// Mates are the live agents in the same Rex window.
	Mates []core.Peer
	// Role is what the pane label said the agent is for; Window and
	// Session are where its block is. They choose its briefing.
	Role, Window, Session string
	// LabelErr is set when the handle could not be shown as the block
	// label. The handle works regardless; callers report it.
	LabelErr error
}

// Adopt gives the agent running in loc a handle, a mailbox, and a live
// waker, and records the binding. The handle follows the agent session (see
// claim): a session resumed in a new pane gets its handle back, and adopting
// an agent already adopted keeps everything as is.
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
	}
	blk, err := e.Rex.InspectBlock(ctx, loc.Block)
	if err != nil {
		return Adoption{}, err
	}
	recs, err := e.Store.Records()
	if err != nil {
		return Adoption{}, err
	}
	own, hasOwn := claim(recs, loc.Block, agentPID, kind, lineage(recs, kind, in))
	// A background job shown in another pane than its record's moved there
	// (jobLocation): it keeps its handle and is not departing anywhere.
	moved := false
	if !hasOwn && in.Job {
		for _, r := range recs {
			if r.Live() && r.Kind == kind && r.AgentSessionID == in.SessionID && r.BlockID != loc.Block {
				own, hasOwn, moved = r, true, true
			}
		}
	}
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
		handle = core.NameHandle(kind, blk.Label, taken)
	}
	// Any other live record of this block is an agent this one replaced, or
	// the session this process switched away from: it keeps its handle as a
	// reservation for a resume.
	for _, r := range recs {
		if r.BlockID == loc.Block && r.Live() && r.Handle != handle {
			if err := e.park(ctx, r); err != nil {
				return Adoption{}, fmt.Errorf("park %s, replaced in this block: %w", r.Handle, err)
			}
		}
	}
	// The moved job's waker follows it: amq.Ensure below replaces the one
	// that targets its old block.
	if moved && own.WindowID != blk.WindowID {
		e.leaveWindow(ctx, own)
	}
	if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), string(handle)); err != nil {
		return Adoption{}, err
	}
	if !hasOwn {
		e.seedSentCursor(handle)
	}
	server := srv.Endpoint
	if server == "" {
		server = loc.Server
	}
	// What this adoption means. The live record of this block coming back is a
	// repeated start hook or a compaction: nothing changes, unless the adoption
	// that wrote it never finished. Then the agent was not told, and nor was
	// its window. One that departed was announced as gone, so taking its handle
	// back is news again.
	again := hasOwn && own.Live() && own.BlockID == loc.Block
	unfinished := hasOwn && own.Unfinished == store.Adopting
	arrived := !again || unfinished
	if moved {
		arrived = own.WindowID != blk.WindowID || unfinished // a move within the tab is no news
	}
	known := hasOwn && in.SessionID != "" && own.AgentSessionID == in.SessionID && !unfinished
	rec := store.Record{
		Handle:         handle,
		Kind:           kind,
		Server:         server,
		BlockID:        loc.Block,
		WindowID:       blk.WindowID,
		AgentPID:       agentPID,
		AgentSessionID: in.SessionID,
		AdoptedAt:      e.Now().UTC(),
	}
	if hasOwn {
		rec.Task = own.Task
	}
	if again && own.AgentPID == agentPID {
		rec.AdoptedAt = own.AdoptedAt
	}
	// Recorded as unfinished until the waker and label follow: a move within
	// the tab tells no one but is a transition all the same.
	if !again || unfinished {
		rec.Unfinished = store.Adopting
	}
	// Recorded before the waker starts: whatever happens next, departure and
	// collection find this record and retire the block's waker, which amq
	// identifies by the block its saved target names, and a later hook finishes
	// the adoption.
	if err := e.Store.PutRecord(rec); err != nil {
		return Adoption{}, err
	}
	var decision amq.WakeDecision
	if kind.InProcess() {
		// The agent's own extension delivers. A waker left from an earlier
		// agent in this block would type doorbells into it: remove it.
		if _, err := amq.RetireFor(ctx, e.AmqBin, e.Paths.Root(), string(handle), loc.Block); err != nil {
			return Adoption{}, err
		}
	} else {
		if err := e.prepareInjector(); err != nil {
			return Adoption{}, err
		}
		if _, decision, err = amq.Ensure(ctx, e.AmqBin, e.waker(rec), e.Paths.Logs()); err != nil {
			return Adoption{}, err
		}
	}

	if err := e.ensureGoalWatcher(rec); err != nil {
		fmt.Fprintf(Diagnostics, "%v\n", err)
	}
	// The handle carries the role, so briefing and goal behaviour read one fact:
	// the label is overwritten below, and a resumed agent may land in any pane.
	out := Adoption{Decision: decision, Known: known, Role: core.Role(string(handle)), Window: blk.WindowID, Session: blk.SessionID}
	out.Mates = e.joinWindow(ctx, rec, blk, recs, arrived)
	if label := core.PaneLabel(handle, rec.Task); blk.Label != label {
		out.LabelErr = e.Rex.RenameBlock(ctx, loc.Block, label)
	}
	unread, err := amq.Unread(e.Paths.Root(), string(handle))
	if err != nil {
		return Adoption{}, err
	}
	out.Unread = len(unread)
	if rec.Unfinished != "" {
		rec.Unfinished = ""
		if err := e.Store.PutRecord(rec); err != nil {
			return Adoption{}, err
		}
	}
	out.Record = rec
	return out, nil
}

// joinWindow returns the live agents in the window of rec's block and, when
// the agent has just arrived in its block, queues a note for each of them that
// it started beside them. The window is advice, not a binding: when
// Rex cannot list it, adoption stands without mates and the failure is
// reported.
func (e Env) joinWindow(ctx context.Context, rec store.Record, blk rex.Block, recs []store.Record, arrived bool) []core.Peer {
	if blk.WindowID == "" || blk.SessionID == "" {
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
			peers = append(peers, core.Peer{Handle: r.Handle, Kind: r.Kind, Block: r.BlockID})
		}
	}
	mates := core.WindowMates(rec.BlockID, blk.WindowID, windowOf, peers)
	if !arrived {
		return mates
	}
	note := core.JoinedNotice(core.Peer{Handle: rec.Handle, Kind: rec.Kind, Block: rec.BlockID})
	for _, m := range mates {
		if err := e.noteWindow(ctx, m.Handle, note); err != nil {
			fmt.Fprintf(Diagnostics, "tell %s that %s joined: %v\n", m.Handle, rec.Handle, err)
		}
	}
	return mates
}

func (e Env) noteWindow(ctx context.Context, h core.Handle, note string) error {
	unlock, err := e.Store.LockLedger(ctx, h)
	if err != nil {
		return err
	}
	defer unlock()
	return e.Store.AddWindowNote(h, note)
}

// claim finds the handle this agent already holds, if any. sessions is the
// agent's lineage: its session, then the sessions its conversation continues.
//
//  1. The record of a session in the lineage, nearest first, of the same kind:
//     a resume, in this block or any other, takes its handle back, so peers'
//     mail and the conversation's references to it still reach it. A session
//     another block's agent is running ends the search: that agent is this
//     conversation's continuation too, and owns what came before it.
//  2. Otherwise this block's live record of the same process and kind: a
//     repeated hook, a compaction, a new session in the same process
//     (/clear), or an agy subagent, which runs as a conversation of its own in
//     the parent's process; the pane keeps its handle through it.
//
// An agent that claims nothing is named afresh: a new session in a pane
// does not inherit the handle of the pane's previous session.
func claim(recs []store.Record, block string, pid int, kind core.Kind, sessions []string) (store.Record, bool) {
	for _, session := range sessions {
		r, ok, running := resumable(recs, block, kind, session)
		if ok {
			return r, true
		}
		if running {
			break
		}
	}
	for _, r := range recs {
		if r.BlockID == block && r.Live() && r.AgentPID == pid && r.Kind == kind {
			return r, true
		}
	}
	return store.Record{}, false
}

// resumable is the newest record of session that the agent in block may take.
// running reports that another block's agent holds the session: a handle is
// never shared.
func resumable(recs []store.Record, block string, kind core.Kind, session string) (resumed store.Record, found, running bool) {
	for _, r := range recs {
		if r.AgentSessionID != session || r.Kind != kind {
			continue
		}
		if r.Live() && r.BlockID != block {
			running = true
			continue
		}
		if !found || r.AdoptedAt.After(resumed.AdoptedAt) {
			resumed, found = r, true
		}
	}
	return resumed, found, running
}

// takenHandles are the handles a new name must avoid: every record's (live,
// or reserved for a departed session), and every mailbox holding undrained
// mail, so a new agent never inherits someone else's unread messages.
func (e Env) takenHandles(recs []store.Record) (map[core.Handle]bool, error) {
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
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return "", err
	}
	if !ok || !rec.Live() {
		return "", fmt.Errorf("no AMQ identity for this Rex block (%s), so there is no pane label to set", loc.Block)
	}
	rec.Task = core.Task(task)
	if err := e.Store.PutRecord(rec); err != nil {
		return "", err
	}
	label := core.PaneLabel(rec.Handle, rec.Task)
	if err := e.Rex.RenameBlock(ctx, loc.Block, label); err != nil {
		return "", fmt.Errorf("the task is recorded, but the pane label could not be set: %w", err)
	}
	return label, nil
}

// Exports is the shell text `rex-amq-adapter env` prints for the agent in loc.
func (e Env) Exports(loc Location) (string, error) {
	rec, ok, err := e.Store.Record(loc.Block)
	if err != nil {
		return "", err
	}
	if !ok || !rec.Live() {
		return "", fmt.Errorf("no AMQ identity for this Rex block (%s); it is assigned when Claude Code or Codex starts here with the adapter's hooks installed", loc.Block)
	}
	return fmt.Sprintf("export AM_ROOT=%s AM_ME=%s\n", shellQuote(e.Paths.Root()), shellQuote(string(rec.Handle))), nil
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
