package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// A conversation is one identity, and one process speaks for it at a time.
// When a second pane resumes a conversation whose agent still runs in another
// pane, the pane that holds it keeps it (incumbent-wins): the newcomer gets no
// identity, is told why, and takes the handle over at its next hook once the
// incumbent is provably gone.

// DuplicateSessionError: the conversation a hook or a command speaks for is
// live as Handle in Block, held by another process than the caller's.
type DuplicateSessionError struct {
	Session string
	Handle  core.Handle
	Block   string
}

func (e *DuplicateSessionError) Error() string {
	return fmt.Sprintf("conversation %s is live as %s in %s; this pane has no AMQ identity until that pane closes", e.Session, e.Handle, e.Block)
}

// duplicateOf is the refusal for a caller speaking for session, which held's
// agent runs as in its block.
func duplicateOf(session string, held agent) *DuplicateSessionError {
	if session == "" {
		session = held.Session()
	}
	return &DuplicateSessionError{Session: session, Handle: held.Handle, Block: held.BlockID}
}

// resolveIdentity finds the handle the agent of a hook already is, if any.
// sessions is its lineage: its session, then the sessions its conversation
// continues, nearest first. inc is its process.
//
//  1. The identity of a session in the lineage, nearest first, of the same
//     kind, wherever it runs or ran: a conversation is one identity (I1 keeps
//     at most one identity per session), so a resume takes its handle back,
//     and one still running elsewhere is the same agent, whose lease decides
//     who speaks for it (heldElsewhere, holderGone in Adopt).
//  2. Otherwise this block's live lease of the same process and kind: a new
//     session in the same process (/clear) stays the same agent.
//
// An agent that resolves to nothing is named afresh: a new session in a pane
// does not inherit the handle of the pane's previous session.
func resolveIdentity(recs []agent, block string, inc store.Incarnation, kind core.Kind, sessions []string) (agent, bool) {
	for _, session := range sessions {
		if i := slices.IndexFunc(recs, func(r agent) bool { return r.Kind == kind && slices.Contains(r.Sessions, session) }); i >= 0 {
			return recs[i], true
		}
	}
	for _, r := range recs {
		if r.BlockID == block && r.Live() && r.lease.Incarnation.Same(inc) && r.Kind == kind {
			return r, true
		}
	}
	return agent{}, false
}

// heldElsewhere reports whether own's lease is held by another process than
// inc, the hook's. A lease in this block whose process is unknown (pid 0) is
// taken as the hook's own, as before incarnations were recorded; in another
// block an unknown process proves nothing, and is another.
func heldElsewhere(own agent, block string, inc store.Incarnation) bool {
	if !own.Live() {
		return false
	}
	if !own.lease.Incarnation.Same(inc) {
		return true
	}
	return own.BlockID != block && (inc.PID <= 0 || own.pid() <= 0)
}

// holderGone reports whether the process holding q's lease is provably no
// longer where the lease places it, and the reason, so another process may
// take the lease over (§3.2). Each proof is one of:
//
//   - its process exited, or its pid now names another process (local only);
//   - its Rex server restarted (the server's instance id changed);
//   - its block no longer exists;
//   - its block's foreground is no longer it (for a background job: its pane
//     no longer shows it), or the conversation left the pane.
//
// A server that cannot be reached, or cannot say, proves nothing: the
// incumbent keeps its lease.
func (e Env) holderGone(ctx context.Context, q agent) (reason string, gone bool) {
	inc := q.lease.Incarnation
	if localServer(q.Server) && inc.PID > 0 {
		if processGone(inc.PID) {
			return "agent_gone", true
		}
		if inc.StartTime != 0 {
			if st, err := processStartTime(inc.PID); err == nil && st != inc.StartTime {
				return "incarnation_gone", true
			}
		}
	}
	on := e
	if e.RexOn != nil && q.Server != "" {
		on.Rex = e.RexOn(q.Server)
	}
	if q.lease.ServerInstance != "" {
		srv, err := on.Rex.Whoami(ctx)
		if err != nil {
			return "", false
		}
		if srv.InstanceID != "" && srv.InstanceID != q.lease.ServerInstance {
			return "server_restarted", true
		}
	}
	b, err := on.observeBinding(ctx, q)
	if err != nil {
		return "", false
	}
	o, bound := core.CheckBinding(b)
	switch {
	case bound:
		return "", false
	case o.Progress == core.Failed:
		return o.Code, true
	}
	// A doorbell waits out another foreground (an editor the agent opened);
	// a takeover does not: the conversation's own process now runs elsewhere.
	return o.Code, true
}

// callerUnder checks that this command runs under rec's agent process, the
// incarnation its lease records (§3.3): a pane that resumed the conversation
// while rec's agent runs in another pane names the same session in its tool
// shells, and must not speak for it. A remote server's processes are not
// this machine's and cannot be checked. A Claude Code background job's lease
// is its own process (CLAUDE_PID), which its tool shells run under.
func (e Env) callerUnder(rec agent, session string) error {
	if !rec.Live() || !localServer(rec.Server) || rec.pid() <= 0 {
		return nil
	}
	ancestors, err := e.ancestors()
	if err != nil {
		return fmt.Errorf("tell whether this command runs under %s's agent: %w", rec.Handle, err)
	}
	if slices.ContainsFunc(ancestors, func(p proc) bool { return p.PID == rec.pid() }) && sameStart(rec.lease.Incarnation) {
		return nil
	}
	return duplicateOf(session, rec)
}

// sameStart reports whether inc's pid, which is alive, is still the process
// that started at inc's start time. An unknown start time matches.
func sameStart(inc store.Incarnation) bool {
	if inc.StartTime == 0 {
		return true
	}
	st, err := processStartTime(inc.PID)
	return err != nil || st == inc.StartTime
}

// refusedDuplicate reports err's duplicate-session refusal, and logs it: the
// refused pane's hooks run with no identity, and hooks.log is where the user
// finds why.
func refusedDuplicate(err error) (*DuplicateSessionError, bool) {
	var dup *DuplicateSessionError
	if !errors.As(err, &dup) {
		return nil, false
	}
	fmt.Fprintf(Diagnostics, "%v\n", dup)
	return dup, true
}
