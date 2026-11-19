package app

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/est7/rex-amq-adapter/internal/fault"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// Reasons a guard_denied refusal names in details.reason.
const (
	GuardHumanOnly  = "human_only"
	GuardUnattended = "unattended"
)

// HumanGate refuses, failing closed, every caller but the human's, for the
// commands that act as the user or change every harness's configuration
// (trust grant, trust root add|remove, hub add|remove, install setup|remove,
// stream panel, and epic list|show from outside Rex):
//
//  1. a harness session the environment names (sessionVars), adopted or not;
//  2. ME_UNATTENDED=1;
//  3. a REX_BLOCK holding a live agent of any role, personal, orchestrator,
//     and narrator included;
//  4. this process descending from a live agent of any role, so `env -i`
//     under an agent is refused too; a walk up the process tree that stopped
//     at its bound (maxAncestors) before the root proves nothing, so it is
//     refused as well.
//
// A refusal is guard_denied (exit 6) and still matches ErrRefused.
func (e Env) HumanGate(getenv func(string) string) error {
	for _, v := range sessionVars {
		if getenv(v.name) != "" {
			return humanRefused(GuardHumanOnly, "this command is the human's, and it runs in a %s session (%s is set)", v.kind, v.name)
		}
	}
	if getenv(UnattendedEnv) == "1" {
		return humanRefused(GuardUnattended, "this command is the human's, and it runs unattended (%s=1)", UnattendedEnv)
	}
	if block := getenv("REX_BLOCK"); block != "" {
		rec, ok, err := e.agentAt(block)
		if err != nil {
			return err
		}
		if ok && rec.Live() {
			return agentRefused(rec)
		}
	}
	recs, err := e.agents()
	if err != nil {
		return err
	}
	ancestors, err := e.ancestors()
	if err != nil {
		return fmt.Errorf("tell which agent runs this command: %w", err)
	}
	if rec, ok := agentAncestor(ancestors, recs, func(agent) bool { return true }); ok {
		return agentRefused(rec)
	}
	// The walkers stop at maxAncestors without an error, so a full walk may
	// have left an agent above it unread. A chain that reaches the root in
	// exactly maxAncestors steps is refused too: no human's pane is that deep.
	if len(ancestors) >= maxAncestors {
		return humanRefused(GuardHumanOnly, "this command is the human's, and it runs at least %d processes deep, too deep to tell that no agent started it", maxAncestors)
	}
	return nil
}

// HumanGate is Env.HumanGate on the default state directory. It needs no
// Rex or amq binary: a refusal must not wait on, or be hidden by, a
// missing dependency.
func HumanGate(getenv func(string) string) error {
	paths, err := store.DefaultPaths()
	if err != nil {
		return err
	}
	e := Env{Paths: paths, Store: store.Store{P: paths}, Now: time.Now, Getenv: os.Getenv, LookPath: exec.LookPath}
	return e.HumanGate(getenv)
}

func humanRefused(reason, format string, a ...any) error {
	return fault.New(fault.GuardDenied, "%s: %s", ErrRefused, fmt.Sprintf(format, a...)).
		WithCause(ErrRefused).With("reason", reason).
		WithHint("the user runs this command in a terminal of their own; an agent asks the user instead")
}

func agentRefused(rec agent) error {
	role := rec.Role
	if role == "" {
		role = "no role"
	}
	return humanRefused(GuardHumanOnly, "this command is the human's, and it runs as or under %s (%s)", rec.Handle, role)
}
