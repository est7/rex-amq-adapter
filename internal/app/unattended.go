package app

import (
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/core"
	"github.com/est7/rex-amq-adapter/internal/store"
)

// An Epic runs unattended (Claim C23): every agent its session launches or
// resumes, other than a personal agent, carries ME_UNATTENDED=1 in its own
// command's environment, so its shell tools stage outward writes for the
// human instead of sending them. Panes Rex creates do not inherit the
// adapter's environment, so the marker always goes in the argv's env prefix.
// The adapter never sets, clears, or reads it for a personal agent, nor
// changes its own process environment: a personal agent that inherited the
// marker keeps it.

// UnattendedEnv marks an agent's shells as running unattended; its value is
// "1".
const UnattendedEnv = "ME_UNATTENDED"

// withUnattendedEnv adds UnattendedEnv=1 to cmd's env prefix, which cmd has
// (withLaunchEnv), so the program stays the first word after the
// assignments.
func withUnattendedEnv(cmd []string) []string {
	if len(cmd) == 0 || cmd[0] != "env" {
		return append([]string{"env", UnattendedEnv + "=1"}, cmd...)
	}
	return append([]string{"env", UnattendedEnv + "=1"}, cmd[1:]...)
}

// launchUnattended reports whether an agent of role launched or resumed in
// session by caller runs unattended: never a personal agent; always an
// orchestrator or narrator; otherwise when the session's Epic policy says so,
// or the caller or the departed agent (nil for a start) runs unattended. An
// unreadable policy is an error, never an attended launch.
func (e Env) launchUnattended(session, role string, caller store.Record, departed *store.Record) (bool, error) {
	if role == core.PersonalRole {
		return false, nil
	}
	if role == core.OrchestratorRole || role == core.NarratorRole || caller.Unattended || departed != nil && departed.Unattended {
		return true, nil
	}
	p, _, err := e.Store.EpicPolicy(session)
	if err != nil {
		return false, fmt.Errorf("read the Epic policy of %s: %w", session, err)
	}
	return p.Unattended, nil
}

// adoptUnattended reports whether rec, being adopted in session, runs
// unattended: never a personal agent; otherwise when its hook's environment
// carries the marker (in), the handle ran unattended before (was), or the
// session's Epic policy says so.
func (e Env) adoptUnattended(rec store.Record, in HookInput, was bool) (bool, error) {
	if rec.Role == core.PersonalRole {
		return false, nil
	}
	if in.Unattended || was {
		return true, nil
	}
	p, _, err := e.Store.EpicPolicy(rec.SessionID)
	if err != nil {
		return false, fmt.Errorf("read the Epic policy of %s: %w", rec.SessionID, err)
	}
	return p.Unattended, nil
}
