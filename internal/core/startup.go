package core

import "fmt"

// StartupStatus is what became of an agent started in a block.
type StartupStatus string

const (
	// StartupAdopted: it has a mailbox and a waker, so mail is safe. Its first
	// prompt may be unread.
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

// HooksReviewPhrase is the line Codex draws while hooks wait for the user's
// approval (screen.TrustScreen reports it).
const HooksReviewPhrase = "Hooks need review"

// DecideStartup says whether the startup has an outcome yet. A dialog ends the
// wait because only the user can clear it.
func DecideStartup(o StartupObservation) (Startup, bool) {
	switch {
	case o.Handle != "":
		return Startup{Status: StartupAdopted, Handle: o.Handle}, true
	case o.BlockGone:
		return Startup{Status: StartupExited, Note: "the block closed before the agent was adopted"}, true
	case o.TrustPhrase == HooksReviewPhrase:
		return Startup{Status: StartupTrust, Note: "Codex stopped on \"Hooks need review\": the user must approve the hooks with /hooks in that pane (nobody approves them for the user); until then it runs none of the adapter's hooks and is never adopted. A summon without --harness passes over a Codex whose hooks are unapproved"}, true
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
	return Startup{Status: StartupTimeout, Note: "no adoption finished: the agent is still starting, or its hooks were never approved (Codex: /hooks)"}
}
