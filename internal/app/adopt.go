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
// waker, and records the binding. It is idempotent: a SessionStart for an
// agent already adopted (resume, clear, compact) keeps everything as is.
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
	prev, hasPrev, err := e.Store.Record(loc.Block)
	if err != nil {
		return Adoption{}, err
	}
	req, err := e.handleRequest(kind, blk.Label, loc.Block, prev, hasPrev)
	if err != nil {
		return Adoption{}, err
	}
	choice := core.ChooseHandle(req)

	if hasPrev && prev.Handle != choice.Handle {
		if _, err := amq.RetireFor(ctx, e.AmqBin, e.Paths.Root(), string(prev.Handle), loc.Block); err != nil {
			return Adoption{}, fmt.Errorf("retire the waker of the block's previous handle %s: %w", prev.Handle, err)
		}
	}
	if err := amq.EnsureMailbox(ctx, e.AmqBin, e.Paths.Root(), string(choice.Handle)); err != nil {
		return Adoption{}, err
	}
	server := srv.Endpoint
	if server == "" {
		server = loc.Server
	}
	rec := store.Record{
		Handle:         choice.Handle,
		Kind:           kind,
		Server:         server,
		SessionID:      loc.Session,
		BlockID:        loc.Block,
		AgentPID:       agentPID,
		AgentSessionID: in.SessionID,
		AdoptedAt:      e.Now().UTC(),
	}
	if hasPrev && prev.Live() && prev.AgentPID == agentPID && prev.Handle == choice.Handle {
		rec.AdoptedAt = prev.AdoptedAt
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
		if _, err := amq.RetireFor(ctx, e.AmqBin, e.Paths.Root(), string(choice.Handle), loc.Block); err != nil {
			return Adoption{}, err
		}
	} else {
		if _, decision, err = amq.Ensure(ctx, e.AmqBin, e.waker(rec), e.Paths.Logs()); err != nil {
			return Adoption{}, err
		}
	}

	out := Adoption{Record: rec, Decision: decision}
	if choice.SetLabel && blk.Label != string(choice.Handle) {
		out.LabelErr = e.Rex.RenameBlock(ctx, loc.Block, string(choice.Handle))
	}
	unread, err := amq.Unread(e.Paths.Root(), string(choice.Handle))
	if err != nil {
		return Adoption{}, err
	}
	out.Unread = len(unread)
	return out, nil
}

// handleRequest gathers what handle choice depends on. Live: handles of
// other blocks' running agents. Encumbered: handles reserved by departed
// agents, and handles whose mailbox holds undrained mail with no live owner.
func (e Env) handleRequest(kind core.Kind, label, block string, prev store.Record, hasPrev bool) (core.HandleRequest, error) {
	req := core.HandleRequest{Kind: kind, Label: label, Live: map[core.Handle]bool{}, Encumbered: map[core.Handle]bool{}}
	if hasPrev {
		req.Sticky = prev.Handle
	}
	recs, err := e.Store.Records()
	if err != nil {
		return core.HandleRequest{}, err
	}
	for _, r := range recs {
		switch {
		case r.BlockID == block:
		case r.Live():
			req.Live[r.Handle] = true
		default:
			req.Encumbered[r.Handle] = true
		}
	}
	agents, err := amq.Agents(e.Paths.Root())
	if err != nil {
		return core.HandleRequest{}, err
	}
	for _, a := range agents {
		h := core.Handle(a)
		if req.Live[h] {
			continue
		}
		unread, err := amq.Unread(e.Paths.Root(), a)
		if err != nil {
			return core.HandleRequest{}, err
		}
		if len(unread) > 0 {
			req.Encumbered[h] = true
		}
	}
	return req, nil
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
