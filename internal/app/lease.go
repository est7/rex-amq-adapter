package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/est7/rex-amq-adapter/internal/core"
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
func (e Env) recordFit(kind core.Kind, rec agent, in HookInput) fit {
	if rec.Kind != kind {
		return foreign
	}
	if !e.sameProcess(rec, in) {
		// Only the record's own session vouches for another process: a hook
		// that names none, or a record that holds none, proves nothing.
		if in.SessionID != "" && in.SessionID == rec.Session() {
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
// ancestry. Either way the pid must still be the process that started at the
// lease's start time: a reused pid is another process.
func (e Env) sameProcess(rec agent, in HookInput) bool {
	if in.AgentPID > 0 && rec.pid() > 0 && localServer(rec.Server) {
		return in.AgentPID == rec.pid() && sameStart(rec.lease.Incarnation)
	}
	return e.descendsFrom(rec)
}

// endsOwnLease reports whether a SessionEnd hook proves it comes from rec's
// agent process: the authority to release its lease at once. A local lease's
// process is known as sameProcess knows it, by the reported pid or else by
// ancestry. A remote lease's ancestry cannot be read (descendsFrom takes it
// as is), so only the agent's own reported pid proves it; without that
// proof the release is collection's.
func (e Env) endsOwnLease(rec agent, in HookInput) bool {
	if !localServer(rec.Server) {
		return in.AgentPID > 0 && in.AgentPID == rec.pid()
	}
	return e.sameProcess(rec, in)
}

// descendsFrom reports whether this process descends from rec's agent
// process, the incarnation its lease records. Only a local server's pids are this machine's; a remote record
// cannot be checked and is taken as is. Unreadable ancestry is not taken as a
// match: adopting again costs one hook's time, trusting a stale record costs
// the agent its mail.
func (e Env) descendsFrom(rec agent) bool {
	if !localServer(rec.Server) || rec.pid() <= 0 {
		return true
	}
	ancestors, err := e.ancestors()
	if err != nil {
		fmt.Fprintf(Diagnostics, "read this hook's process ancestry: %v\n", err)
		return false
	}
	err = runsUnder(rec.Kind, ancestors, rec.pid())
	if errors.Is(err, ErrSharedHarness) {
		fmt.Fprintf(Diagnostics, "%s: %v\n", rec.Handle, err)
	}
	return err == nil && sameStart(rec.lease.Incarnation)
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
// in the agent process agent itself: the first process above the hook's own
// shells must be agent, or a launcher of agent's that runs it without a
// shell (node running a native codex is the agent itself).
//
// A harness between them is another agent. One under a shell of the agent's
// was started by a command in the pane: a tool shell's `codex exec`,
// `claude -p`, `agy -p`, or `pi -p`, which inherits REX_BLOCK and so names
// the agent's pane (observed: hook ← sh ← codex exec ← bash ← claude, F14;
// hook ← sh ← agy -p ← timeout ← bash ← claude). A Codex hook under another
// Codex process with no shell between is Codex's shared daemon: without
// --no-daemon, Codex runs every pane's threads in one daemon, which the first
// terminal client started and which inherited that pane's Rex location
// (observed: hook ← zsh ← codex ← codex). Another kind's harness of its own
// name with no shell between was started by the agent itself.
//
// agy is a native binary the pane runs directly, so no launcher is allowed:
// one that spawns agy without a shell (a program's subprocess, `sh -c`
// exec'ing its only command) looks the same as the pane's agent running
// `agy -p`.
func runsUnder(kind core.Kind, ancestors []proc, agent int) error {
	if !slices.ContainsFunc(ancestors, func(p proc) bool { return p.PID == agent }) {
		return errNotUnder
	}
	var harness *proc
	for i, p := range ancestors {
		if harness == nil {
			if p.PID == agent {
				return nil
			}
			if i == 0 || isShell(p.Name) {
				continue
			}
			harness = &ancestors[i]
			if kind == core.Agy {
				break
			}
			continue
		}
		switch {
		case p.Name == harness.Name && kind == core.Codex:
			return fmt.Errorf("%w (%s pid %d runs under %s pid %d; the agent is pid %d)", ErrSharedHarness, harness.Name, harness.PID, p.Name, p.PID, agent)
		case p.Name == harness.Name:
			return fmt.Errorf("%w (%s pid %d runs under %s pid %d; the pane's agent is pid %d)", ErrStartedInPane, harness.Name, harness.PID, p.Name, p.PID, agent)
		case p.PID == agent:
			return nil
		case isShell(p.Name):
			return fmt.Errorf("%w (%s pid %d runs under the shell %s pid %d; the pane's agent is pid %d)", ErrStartedInPane, harness.Name, harness.PID, p.Name, p.PID, agent)
		}
	}
	return fmt.Errorf("%w (%s pid %d runs the hook; the pane's agent is pid %d)", ErrStartedInPane, harness.Name, harness.PID, agent)
}

// ErrStartedInPane: a hook comes from a harness that a command in the pane
// started (a headless `agy -p`, `codex exec`, `claude -p`, `pi -p`), not
// from the pane's agent.
var ErrStartedInPane = errors.New("this hook comes from a harness a command in the pane started (agy -p, codex exec, claude -p, pi -p?), not from the pane's agent")

// startedUnderOwner reports whether this hook comes from a harness that a
// command started under the live agent recorded in block: its tool shell's
// `codex exec`, `claude -p`, `agy -p`, or `pi -p`, whose hooks name the
// agent's pane because they inherit REX_BLOCK. Such a hook is no agent of the
// pane's: it must not adopt, park, flag, or read anything there (F14). The
// agent's own hooks, a harness that does not run under the agent, and a hook
// whose ancestry cannot be read are not: muting the pane's agent costs more
// than a stray adoption, which the agent's next hook takes back.
func (e Env) startedUnderOwner(kind core.Kind, block string) (agent, bool) {
	rec, ok, err := e.agentAt(block)
	if err != nil || !ok || !rec.Live() || rec.pid() <= 0 || !localServer(rec.Server) {
		return rec, false
	}
	ancestors, err := e.ancestors()
	if err != nil || len(ancestors) < 2 {
		return rec, false
	}
	// The hook itself is never its own agent: a test process standing in
	// for an agent is ancestors[0].
	if !slices.ContainsFunc(ancestors[1:], func(p proc) bool { return p.PID == rec.pid() }) {
		return rec, false
	}
	if kind != rec.Kind {
		return rec, true
	}
	return rec, errors.Is(runsUnder(kind, ancestors, rec.pid()), ErrStartedInPane)
}

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
