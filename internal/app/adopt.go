package app

import (
	"context"
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/amq"
	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Adoption is the result of adopting the agent in a block.
type Adoption struct {
	Record store.Record
	Unread int
	// Decision is what happened to the waker; zero for in-process kinds,
	// which have none.
	Decision amq.WakeDecision
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
	agentPID := in.AgentPID
	if agentPID <= 0 {
		fg, ok, err := e.Rex.Foreground(ctx, loc.Block)
		if err != nil {
			return Adoption{}, err
		}
		if !ok {
			return Adoption{}, fmt.Errorf("block %s reports no foreground process", loc.Block)
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
	own, hasOwn := claim(recs, loc.Block, agentPID, kind, in.SessionID)
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
	if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), string(handle)); err != nil {
		return Adoption{}, err
	}
	server := srv.Endpoint
	if server == "" {
		server = loc.Server
	}
	rec := store.Record{
		Handle:         handle,
		Kind:           kind,
		Server:         server,
		SessionID:      loc.Session,
		BlockID:        loc.Block,
		AgentPID:       agentPID,
		AgentSessionID: in.SessionID,
		AdoptedAt:      e.Now().UTC(),
	}
	if hasOwn && own.Live() && own.BlockID == loc.Block && own.AgentPID == agentPID {
		rec.AdoptedAt = own.AdoptedAt
	}
	// Recorded before the waker starts: whatever happens next, departure and
	// collection find this record and retire the block's waker, which amq
	// identifies by the block its saved target names.
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

	out := Adoption{Record: rec, Decision: decision}
	if blk.Label != string(handle) {
		out.LabelErr = e.Rex.RenameBlock(ctx, loc.Block, string(handle))
	}
	unread, err := amq.Unread(e.Paths.Root(), string(handle))
	if err != nil {
		return Adoption{}, err
	}
	out.Unread = len(unread)
	return out, nil
}

// claim finds the handle this agent already holds, if any:
//
//  1. The record of the same agent session and kind, when no other block's
//     agent is running it: a resume, in this block or any other, takes its
//     handle back, so peers' mail and the conversation's references to it
//     still reach it.
//  2. Otherwise this block's live record of the same process: a repeated
//     hook, a compaction, or a new session in the same process (/clear).
//
// An agent that claims nothing is named afresh: a new session in a pane
// does not inherit the handle of the pane's previous session.
func claim(recs []store.Record, block string, pid int, kind core.Kind, session string) (store.Record, bool) {
	var resumed store.Record
	found := false
	for _, r := range recs {
		if session == "" || r.AgentSessionID != session || r.Kind != kind {
			continue
		}
		if r.Live() && r.BlockID != block {
			continue // running in another block: never shared
		}
		if !found || r.AdoptedAt.After(resumed.AdoptedAt) {
			resumed, found = r, true
		}
	}
	if found {
		return resumed, true
	}
	for _, r := range recs {
		if r.BlockID == block && r.Live() && r.AgentPID == pid {
			return r, true
		}
	}
	return store.Record{}, false
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
