package core

import "fmt"

// StartupStatus is what became of an agent started in a block.
type StartupStatus string

const (
	// StartupAdopted: it has a mailbox, so mail is safe. Its waker may still be
	// starting and its first prompt may be unread.
	StartupAdopted StartupStatus = "adopted"
	// StartupTrust: a trust or hook-approval dialog needs the user.
	StartupTrust StartupStatus = "trust_screen"
	// StartupBlocked: the agent waits on the user.
	StartupBlocked StartupStatus = "blocked"
	// StartupExited: the block is gone.
	StartupExited StartupStatus = "exited"
	// StartupTimeout: none of these was seen in time.
	StartupTimeout StartupStatus = "timeout"
)

// Startup is the observed outcome of starting an agent.
type Startup struct {
	Status StartupStatus `json:"status"`
	Handle Handle        `json:"handle,omitempty"`
	Note   string        `json:"note,omitempty"`
}

// StartupObservation is one look at a block an agent was started in.
type StartupObservation struct {
	Handle      Handle // of the block's live record, if any
	BlockGone   bool
	State       AgentState
	BlockedKind string
	TrustPhrase string
}

// DecideStartup says whether the startup has an outcome yet. A dialog ends the
// wait because only the user can clear it.
func DecideStartup(o StartupObservation) (Startup, bool) {
	switch {
	case o.Handle != "":
		return Startup{Status: StartupAdopted, Handle: o.Handle}, true
	case o.BlockGone:
		return Startup{Status: StartupExited, Note: "the block closed before the agent was adopted"}, true
	case o.TrustPhrase != "":
		return Startup{Status: StartupTrust, Note: fmt.Sprintf("the user must answer %q in that pane", o.TrustPhrase)}, true
	case o.State == StateBlocked:
		note := "the agent waits on the user"
		if o.BlockedKind != "" {
			note += " (" + o.BlockedKind + ")"
		}
		return Startup{Status: StartupBlocked, Note: note}, true
	}
	return Startup{}, false
}

// StartupTimedOut is the outcome when nothing was decided in time.
func StartupTimedOut() Startup {
	return Startup{Status: StartupTimeout, Note: "no hook ran: the agent is still starting, or its hooks were never approved (Codex: /hooks)"}
}
