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

// Outcome codes that more than one site emits or matches. The values are
// wire format (who, the events stream, e2e JSON).
const (
	// CodeAgentWorking is the deferral of a doorbell to a ledger- or
	// supervisor-backed working agent, which its hooks tell about mail in-band.
	CodeAgentWorking = "agent_working"
	// CodeWorkingUnconfirmed is the deferral of a doorbell when only an
	// unbacked external status reports working (no hook turn ledger or
	// supervisor backing it); unlike CodeAgentWorking it is subject to
	// watchdog StuckFor so a frozen OSC signal never defers silently forever.
	CodeWorkingUnconfirmed = "working_unconfirmed"
	// CodeBoxNotFound: no input box is on screen, so nothing can be typed.
	CodeBoxNotFound = "box_not_found"
	// CodeDraftInBox: the input box holds text someone else typed.
	CodeDraftInBox = "draft_in_box"
	// CodeBlockGone: the agent's Rex block no longer exists.
	CodeBlockGone = "block_gone"
)

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
	// JobShown: the agent is a background session hosted by its harness's
	// supervisor, and the block's foreground (a terminal client) shows it.
	JobShown bool
	// LeftPane: the block's foreground is the agent's process, but its title
	// says it shows the harness's list of background sessions, or another
	// conversation: the agent's conversation was moved to the background and
	// is no longer here.
	LeftPane bool
}

// CheckBinding fails only when the agent is provably gone: its block no
// longer exists or its process exited. A different foreground process with
// the agent still alive (an editor or pager it opened) is transient and
// deferred, because a failed cohort is not retried until new mail arrives.
// A gone agent is gone before its pid is compared: the foreground may be
// another program that reuses it.
func CheckBinding(b Binding) (Outcome, bool) {
	switch {
	case b.BlockGone:
		return Outcome{Progress: Failed, Code: CodeBlockGone, Note: "the agent's Rex block no longer exists"}, false
	case b.LeftPane:
		return Outcome{Progress: Failed, Code: "agent_left_pane", Note: "the agent's conversation was moved to the background; its pane shows something else"}, false
	case b.AgentGone:
		return Outcome{Progress: Failed, Code: "agent_gone", Note: "the adopted agent process exited"}, false
	case b.RecordPID > 0 && b.ForegroundPID == b.RecordPID, b.JobShown:
		return Outcome{}, true
	}
	return Outcome{Progress: Deferred, Code: "agent_not_foreground", Note: "another process holds the agent's terminal"}, false
}

// DecideWrite says whether a doorbell may be typed now. Typing happens only
// when the agent is at its prompt (or reports no signal at all) and its input
// box is on screen and empty. A working agent is reached by its hooks instead.
func DecideWrite(act AgentActivity) (Outcome, bool) {
	switch a := act.(type) {
	case InTurn:
		if a.Backed {
			return Outcome{Progress: Deferred, Code: CodeAgentWorking, Note: "the agent is working; its hooks announce mail in-band"}, false
		}
		return Outcome{Progress: Deferred, Code: CodeWorkingUnconfirmed, Note: "external status reports working without a hook turn ledger"}, false
	case AwaitingUser:
		if a.Kind == "trust_screen" {
			return Outcome{Progress: Deferred, Code: "trust_screen", Note: "a trust dialog is on screen"}, false
		}
		note := "the agent waits on the user"
		if a.Kind != "" {
			note += " (" + a.Kind + ")"
		}
		return Outcome{Progress: Deferred, Code: "agent_blocked", Note: note}, false
	case AtPrompt:
		return decideWriteBox(a.Box)
	case Unknown:
		if a.Why == "no_signal" {
			return decideWriteBox(a.Box)
		}
		if a.Unrecognized != "" {
			return Outcome{Progress: Deferred, Code: "agent_state_unknown", Note: "unrecognised program status " + string(a.Unrecognized)}, false
		}
		return Outcome{Progress: Deferred, Code: "agent_state_unknown", Note: "agent activity unknown: " + a.Why}, false
	default:
		return Outcome{Progress: Deferred, Code: "agent_state_unknown", Note: "agent activity unknown"}, false
	}
}

func decideWriteBox(box screen.BoxState) (Outcome, bool) {
	switch box {
	case screen.BoxEmpty:
		return Outcome{}, true
	case screen.BoxDraft:
		return Outcome{Progress: Deferred, Code: CodeDraftInBox, Note: "the input box holds unsent text"}, false
	default:
		return Outcome{Progress: Deferred, Code: CodeBoxNotFound, Note: "the input box is not on screen"}, false
	}
}

// Confirmation is what the injector observes before and after writing the
// doorbell. Submission is confirmed only by a causal post-write transition,
// never by a pre-existing frozen OSC working level.
type Confirmation struct {
	// Pre-write snapshot captured before typing the doorbell.
	PreTurnSeq    uint64
	PreOSCWorking bool
	PreOSCReports bool

	// Post-write transitions observed while polling.
	PostTurnSeq    uint64
	PostOSCWorking bool
	// EmptiedTwice: two consecutive polls after the write saw an empty box.
	EmptiedTwice bool

	Box screen.BoxState
	// OursInBox: the unsent text on screen is the doorbell we wrote.
	OursInBox bool
}

// Confirmed reports whether a causal post-write transition proves the doorbell
// was submitted: a new turn opened in the hook ledger, external status rose
// from non-working to working, or (when external status is absent or was
// already stuck working before the write) two consecutive polls saw an empty box.
// An observed still-pending doorbell in the input box disqualifies confirmation.
func (c Confirmation) Confirmed() bool {
	if c.Box == screen.BoxDraft && c.OursInBox {
		return false
	}
	return c.PostTurnSeq > c.PreTurnSeq ||
		(!c.PreOSCWorking && c.PostOSCWorking) ||
		((!c.PreOSCReports || c.PreOSCWorking) && c.EmptiedTwice)
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
	case c.Box == screen.BoxDraft && c.OursInBox:
		return Outcome{Progress: Deferred, Code: "not_submitted", Note: "the doorbell stayed in the input box; cleared for a retry"}, ClearOurText
	case c.PostTurnSeq > c.PreTurnSeq:
		return Outcome{Progress: Accepted}, NoCleanup
	case !c.PreOSCWorking && c.PostOSCWorking:
		return Outcome{Progress: Accepted}, NoCleanup
	case (!c.PreOSCReports || c.PreOSCWorking) && c.EmptiedTwice:
		// Without an external rising edge (no OSC status, or OSC already stuck
		// working before the write), two consecutive empty-box polls confirm
		// submission while ruling out a paste the harness has not drawn yet.
		note := "submitted; the agent reports no program status"
		if c.PreOSCWorking {
			note = "submitted; input box emptied while external status stayed working"
		}
		return Outcome{Progress: Accepted, Code: "box_emptied", Note: note}, NoCleanup
	case c.Box == screen.BoxDraft:
		return Outcome{Progress: Deferred, Code: CodeDraftInBox, Note: "someone typed into the box during delivery; left untouched"}, NoCleanup
	}
	return Outcome{Progress: Deferred, Code: "unconfirmed", Note: "the doorbell was written but the agent did not report starting"}, NoCleanup
}

// Doorbell is the line typed into an idle agent. It is built from the
// unread count alone: amq's notification payload previews the sender's
// subject, and text another agent controls must never be typed into a
// prompt as if the user wrote it.
// bin is the adapter's absolute path: its drain finds the agent's identity
// itself and reads only mail the gateway admitted.
func Doorbell(unread int, h Handle, bin string) string {
	return fmt.Sprintf("AMQ doorbell: %d unread message(s) for %s (you are %s in Rex; skill: rex-amq-adapter). Run %s mail drain --include-body, then act on each message.", unread, h, h, bin)
}
