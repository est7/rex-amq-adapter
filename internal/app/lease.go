package app

import (
	"fmt"
	"slices"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 64

// fit is how the block's record compares with what a hook knows first-hand:
// the kind it was installed for, the session in its payload, and the process
// it descends from. The record is a lease taken at adoption; a harness that
// does not announce a change leaves it stale, and only a hook can tell.
type fit int

const (
	// own: the record is this agent's, held by its current process.
	own fit = iota
	// staleLease: this agent's session, but the record names a process the
	// hook does not descend from. The conversation was resumed in a new
	// process (agy fires no SessionStart for that), or the recorded pid now
	// belongs to another program. Adopt again; the mailbox is still its own.
	staleLease
	// foreign: the record belongs to another agent: another kind replaced it
	// in the block, or another process holds another session. Adopt again,
	// and never use its mailbox.
	foreign
)

// recordFit compares rec, this block's record, with the hook's evidence.
func (e Env) recordFit(kind core.Kind, rec store.Record, in HookInput) fit {
	if rec.Kind != kind {
		return foreign
	}
	if !e.sameProcess(rec, in) {
		if in.SessionID != "" && rec.AgentSessionID != "" && in.SessionID != rec.AgentSessionID {
			return foreign
		}
		return staleLease
	}
	// Another session from the record's own process is not taken as a
	// switch: agy runs each subagent as a conversation of its own in the same
	// process, and its hooks carry the subagent's id from the parent's pane.
	return own
}

// sameProcess reports whether the hook comes from rec's agent process. A
// harness that reports its own pid (Claude Code's CLAUDE_PID, Pi's
// process.pid) is compared exactly: a session started inside the agent (a
// `claude -p` it runs) descends from it but is not it. Others are known by
// ancestry.
func (e Env) sameProcess(rec store.Record, in HookInput) bool {
	if in.AgentPID > 0 && rec.AgentPID > 0 && localServer(rec.Server) {
		return in.AgentPID == rec.AgentPID
	}
	return e.descendsFrom(rec)
}

// descendsFrom reports whether this process descends from rec's agent
// process. Only a local server's pids are this machine's; a remote record
// cannot be checked and is taken as is. Unreadable ancestry is not taken as a
// match: adopting again costs one hook's time, trusting a stale record costs
// the agent its mail.
func (e Env) descendsFrom(rec store.Record) bool {
	if !localServer(rec.Server) || rec.AgentPID <= 0 {
		return true
	}
	ancestors, err := e.ancestors()
	if err != nil {
		fmt.Fprintf(Diagnostics, "read this hook's process ancestry: %v\n", err)
		return false
	}
	return slices.Contains(ancestors, rec.AgentPID)
}

func (e Env) ancestors() ([]int, error) {
	if e.Ancestors != nil {
		return e.Ancestors()
	}
	return processAncestors()
}
