package core

import (
	"fmt"

	"github.com/est7/rex-amq-adapter/internal/screen"
)

// AgentState is the root record of the agent's OSC 7501 program status.
// StateAbsent means the terminal holds no root record (the agent has not
// reported, or the record was cleared).
type AgentState string

const (
	StateAbsent  AgentState = ""
	StateIdle    AgentState = "idle"
	StateWorking AgentState = "working"
	StateDone    AgentState = "done"
	StateBlocked AgentState = "blocked"
	StateError   AgentState = "error"
)

// AtRest reports whether the agent sits at its prompt: the only state in
// which a doorbell is typed. Claude ends a turn in done, Codex in idle.
func (s AgentState) AtRest() bool {
	return s == StateIdle || s == StateDone || s == StateError
}

// Progress is the marker amq wake reads from the injector's stderr.
type Progress string

const (
	Accepted Progress = "accepted"
	Deferred Progress = "deferred"
	Failed   Progress = "failed"
)

// Outcome is one injector result. Code is a stable machine reason.
type Outcome struct {
	Progress Progress `json:"progress"`
	Code     string   `json:"code,omitempty"`
	Note     string   `json:"note,omitempty"`
}

// ExitCode follows amq's contract: only accepted exits 0. A deferred marker
// with exit 0 is classified "uncertain", which amq never retries.
func (o Outcome) ExitCode() int {
	if o.Progress == Accepted {
		return 0
	}
	return 1
}

// Binding is what the injector observes about the block it was told to wake.
type Binding struct {
	BlockGone     bool
	ForegroundPID int
	RecordPID     int
	// AgentGone: the adopted agent process is known to have exited. Known only
	// for a local server, where the pid can be probed.
	AgentGone bool
}

// CheckBinding fails only when the agent is provably gone: its block no
// longer exists or its process exited. A different foreground process with
// the agent still alive (an editor or pager it opened) is transient and
// deferred, because a failed cohort is not retried until new mail arrives.
func CheckBinding(b Binding) (Outcome, bool) {
	switch {
	case b.BlockGone:
		return Outcome{Progress: Failed, Code: "block_gone", Note: "the agent's Rex block no longer exists"}, false
	case b.ForegroundPID == b.RecordPID:
		return Outcome{}, true
	case b.AgentGone:
		return Outcome{Progress: Failed, Code: "agent_gone", Note: "the adopted agent process exited"}, false
	}
	return Outcome{Progress: Deferred, Code: "agent_not_foreground", Note: "another process holds the agent's terminal"}, false
}

// Readiness is what the injector observes before typing.
type Readiness struct {
	State       AgentState
	BlockedKind string
	Box         screen.Draft
	Trust       bool
}

// DecideWrite says whether a doorbell may be typed now. Typing happens only
// when the agent is at rest (or reports nothing) and its input box is on
// screen and empty. A working agent is reached by its hooks instead.
func DecideWrite(r Readiness) (Outcome, bool) {
	switch {
	case r.State == StateWorking:
		return Outcome{Progress: Deferred, Code: "agent_working", Note: "the agent is working; its hooks announce mail in-band"}, false
	case r.State == StateBlocked:
		note := "the agent waits on the user"
		if r.BlockedKind != "" {
			note += " (" + r.BlockedKind + ")"
		}
		return Outcome{Progress: Deferred, Code: "agent_blocked", Note: note}, false
	case !r.State.AtRest() && r.State != StateAbsent:
		return Outcome{Progress: Deferred, Code: "agent_state_unknown", Note: "unrecognised program status " + string(r.State)}, false
	case r.Trust:
		return Outcome{Progress: Deferred, Code: "trust_screen", Note: "a trust dialog is on screen"}, false
	case r.Box == screen.Typed:
		return Outcome{Progress: Deferred, Code: "draft_in_box", Note: "the input box holds unsent text"}, false
	case r.Box != screen.Empty:
		return Outcome{Progress: Deferred, Code: "box_not_found", Note: "the input box is not on screen"}, false
	}
	return Outcome{}, true
}

// Confirmation is what the injector observes after writing the doorbell.
type Confirmation struct {
	// Reports: the agent reported a program status before the write, so its
	// silence afterwards means something.
	Reports bool
	// Started: the agent reported working after the write.
	Started bool
	// EmptiedTwice: two consecutive polls after the write saw an empty box.
	EmptiedTwice bool
	Box          screen.Draft
	// OursInBox: the unsent text on screen is the doorbell we wrote.
	OursInBox bool
}

// Cleanup is what the injector must do after an unconfirmed write.
type Cleanup int

const (
	NoCleanup Cleanup = iota
	// ClearOurText: erase the doorbell left in the box, so the next attempt
	// does not merge with it.
	ClearOurText
)

// DecideConfirm classifies a written doorbell. Text left in the box is only
// erased when it is provably ours; anything else might be the user's.
//
// An unconfirmed write is deferred, never failed: amq does not retry a
// failed cohort until new mail arrives, and an idle agent fires no hook, so
// a failure would strand the mail. A repeated doorbell costs one empty drain.
func DecideConfirm(c Confirmation) (Outcome, Cleanup) {
	switch {
	case c.Started:
		return Outcome{Progress: Accepted}, NoCleanup
	case !c.Reports && c.EmptiedTwice:
		// Without program status an emptied box is the only evidence of
		// submission available; two observations rule out a paste the
		// harness has not drawn yet.
		return Outcome{Progress: Accepted, Code: "box_emptied", Note: "submitted; the agent reports no program status"}, NoCleanup
	case c.Box == screen.Typed && c.OursInBox:
		return Outcome{Progress: Deferred, Code: "not_submitted", Note: "the doorbell stayed in the input box; cleared for a retry"}, ClearOurText
	case c.Box == screen.Typed:
		return Outcome{Progress: Deferred, Code: "draft_in_box", Note: "someone typed into the box during delivery; left untouched"}, NoCleanup
	}
	return Outcome{Progress: Deferred, Code: "unconfirmed", Note: "the doorbell was written but the agent did not report starting"}, NoCleanup
}

// Doorbell is the line typed into an idle agent. It is built from the
// unread count alone: amq's notification payload previews the sender's
// subject, and text another agent controls must never be typed into a
// prompt as if the user wrote it.
func Doorbell(unread int, h Handle, envCmd string) string {
	return fmt.Sprintf("AMQ doorbell: %d unread message(s) for %s (you are %s in Rex; skill: rex-amq-adapter). Run eval \"$(%s)\" && amq drain --include-body, then act on each message.", unread, h, h, envCmd)
}
