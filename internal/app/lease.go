package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"

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
	// in the block, or another process does not name the record's session.
	// Adopt again, and never use its mailbox.
	foreign
)

// recordFit compares rec, this block's record, with the hook's evidence.
func (e Env) recordFit(kind core.Kind, rec store.Record, in HookInput) fit {
	if rec.Kind != kind {
		return foreign
	}
	if !e.sameProcess(rec, in) {
		// Only the record's own session vouches for another process: a hook
		// that names none, or a record that holds none, proves nothing.
		if in.SessionID != "" && in.SessionID == rec.AgentSessionID {
			return staleLease
		}
		return foreign
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
	err = runsUnder(rec.Kind, ancestors, rec.AgentPID)
	if errors.Is(err, ErrSharedHarness) {
		fmt.Fprintf(Diagnostics, "%s: %v\n", rec.Handle, err)
	}
	return err == nil
}

// ErrSharedHarness: a Codex hook descends from the pane's agent through
// another Codex process, the shared daemon that runs other panes' threads.
var ErrSharedHarness = errors.New("this Codex hook does not run directly under the pane's agent (a shared Codex daemon?); restart Codex with codex --no-daemon")

// proc is one process of an ancestry.
type proc struct {
	PID  int
	Name string
}

// runsUnder reports whether ancestors, this process first, place the hook
// under the agent process agent. Descent is enough for most kinds. A Codex
// hook must also run in agent itself, not in another Codex process below it:
// without --no-daemon, Codex runs every pane's threads in one shared daemon,
// which the first terminal client started and which inherited that pane's
// Rex location. Another pane's thread then fires hooks that descend from the
// first pane's agent and name its block. A Codex hook runs under shells its
// harness starts (observed: hook ← zsh ← codex), so the first process above
// them is the harness; a process of the harness's own name further up, the
// agent included, means the harness is a second Codex. A launcher of another
// name (node running a native codex) is the agent itself.
//
// An agy hook must run in agent itself, the first process above its shells
// (observed: hook ← sh ← agy). agy's user hooks fire for a headless `agy -p`
// too, which inherits REX_BLOCK from whatever ran it and so descends from the
// pane's agent without being it (observed: hook ← sh ← agy -p ← timeout ←
// bash ← claude). agy is a native binary the pane runs directly, so no
// launcher is allowed: one that spawns agy without a shell (a program's
// subprocess, `sh -c` exec'ing its only command) looks the same as the
// pane's agent running `agy -p`.
func runsUnder(kind core.Kind, ancestors []proc, agent int) error {
	under := slices.ContainsFunc(ancestors, func(p proc) bool { return p.PID == agent })
	if kind == core.Agy && under {
		return agyRunsIn(ancestors, agent)
	}
	if kind != core.Codex {
		if under {
			return nil
		}
		return errNotUnder
	}
	var harness *proc
	for i, p := range ancestors {
		if p.PID != agent && (i == 0 || isShell(p.Name)) {
			continue
		}
		switch {
		case harness == nil && p.PID == agent:
			return nil
		case harness == nil:
			harness = &ancestors[i]
		case p.Name == harness.Name:
			return fmt.Errorf("%w (%s pid %d runs under %s pid %d; the agent is pid %d)", ErrSharedHarness, harness.Name, harness.PID, p.Name, p.PID, agent)
		case p.PID == agent:
			return nil
		}
	}
	return errNotUnder
}

// agyRunsIn reports whether the first process above the hook's shells is
// agent, for an ancestry that holds agent.
func agyRunsIn(ancestors []proc, agent int) error {
	for i, p := range ancestors {
		if p.PID != agent && (i == 0 || isShell(p.Name)) {
			continue
		}
		if p.PID == agent {
			return nil
		}
		return fmt.Errorf("%w (%s pid %d runs the hook; the pane's agent is pid %d)", ErrStartedInPane, p.Name, p.PID, agent)
	}
	return errNotUnder
}

// ErrStartedInPane: an agy hook comes from an agy that a command in the pane
// started (a headless `agy -p`), not from the pane's agent.
var ErrStartedInPane = errors.New("this agy hook comes from an agy started by a command in the pane (agy -p?), not from the pane's agent")

var errNotUnder = errors.New("this hook does not descend from the agent")

// isShell reports whether name is a shell's process name. A login shell's
// name starts with a dash.
func isShell(name string) bool {
	switch strings.TrimPrefix(name, "-") {
	case "sh", "bash", "zsh", "dash", "fish", "ksh", "mksh", "tcsh", "csh", "ash", "busybox":
		return true
	}
	return false
}

func (e Env) ancestors() ([]proc, error) {
	if e.Ancestors != nil {
		return e.Ancestors()
	}
	return processAncestors()
}
