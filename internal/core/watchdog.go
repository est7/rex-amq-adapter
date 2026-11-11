package core

import (
	"fmt"
	"time"
)

// The orchestrator watchdog's decisions. In a session nobody watches, a peer whose pane
// is too small cannot draw its input box, so every doorbell defers with
// box_not_found and its mail never arrives: on 2026-11-09 two Claude panes
// of 84x3 and 84x4 sat that way for 25 minutes and 2 hours, and drew the box
// again at 172x17. The watchdog moves such a pane once to a tab of its own,
// has the user's app render it, and hands it back to the injector. Every
// other stuck shape is reported to the orchestrator, never touched.

const (
	// TinyRows and TinyColumns bound the sizes at which the CLIs cannot draw
	// their box; both measured cases were under them, every working pane over.
	TinyRows    = 8
	TinyColumns = 40
	// StuckFor is how long box_not_found must repeat before the pane counts
	// as stuck, so one slow redraw does not move a pane.
	StuckFor = 30 * time.Second
	// OverflowTab is the window the watchdog places recovered panes in.
	OverflowTab = "overflow"
	// RecoverColumns and RecoverRows are the size a detached pane is given:
	// enough for every CLI's input box, small enough for any screen.
	RecoverColumns = 120
	RecoverRows    = 30
	// ReportLines is how many plain screen lines a report carries.
	ReportLines = 12
)

// TinyPane reports whether a pane of that size cannot draw an input box.
func TinyPane(columns, rows int) bool {
	return rows > 0 && rows < TinyRows || columns > 0 && columns < TinyColumns
}

// Stuck reports whether mail is undelivered: it is unread and every doorbell
// for at least StuckFor deferred with the same code. code is that reason as
// the injector recorded it (box_not_found, draft_in_box, agent_blocked,
// trust_screen, …) and since when the run of outcomes began. Reports follow
// from any code; a move follows only from box_not_found on a tiny pane. A run
// of agent_working is never stuck: a working agent hears of mail through its
// own hooks and drains it when its step ends.
func Stuck(history []Attempt, unread int, now time.Time) (code string, since time.Time, ok bool) {
	if unread == 0 {
		return "", time.Time{}, false
	}
	last, since, ok := lastRun(history)
	if !ok || last.Progress != Deferred || last.Code == CodeAgentWorking {
		return "", time.Time{}, false
	}
	return last.Code, since, now.Sub(since) >= StuckFor
}

// MayMove reports whether a stuck pane is the one shape the watchdog fixes
// itself: doorbells find no input box and the pane is too small to draw one.
func MayMove(code string, columns, rows int) bool {
	return code == CodeBoxNotFound && TinyPane(columns, rows)
}

// Delivered reports whether the last doorbell was accepted.
func Delivered(history []Attempt) bool {
	return len(history) > 0 && history[len(history)-1].Progress == Accepted
}

// Placement is where an episode last knows its block to be. Only the
// watchdog's own steps and what it reads back from Rex move it.
type Placement string

const (
	// InWindow: the block is where it was; nothing was moved.
	InWindow Placement = ""
	// Detached: the block is out of every window. An episode never ends here.
	Detached Placement = "detached"
	// InOverflow: the block sits in the session's overflow tab.
	InOverflow Placement = "overflow"
	// Theirs: the user placed or moved the block; the watchdog leaves it.
	Theirs Placement = "user"
)

// Episode is one block's recovery, persisted before and after each step so a
// restarted watchdog continues it (or ends it) instead of repeating a move.
// It belongs to one agent run (Handle and PID): another agent in the block
// starts its own.
type Episode struct {
	ID        string    `json:"id"`
	Handle    Handle    `json:"handle"`
	Block     string    `json:"block"`
	PID       int       `json:"pid"`
	Session   string    `json:"session"`
	StartedAt time.Time `json:"started_at"`
	// Window is where the block was; Overflow the tab chosen for it.
	Window    string    `json:"window,omitempty"`
	Overflow  string    `json:"overflow,omitempty"`
	Placement Placement `json:"placement,omitempty"`
	// Resized: the block was given RecoverColumns x RecoverRows, which only
	// a detached block (one nobody renders) takes.
	Resized bool `json:"resized,omitempty"`
	// Attempts counts layout actions that failed; at MaxAttempts the
	// watchdog stops acting and reports.
	Attempts int `json:"attempts,omitempty"`
	// Reported holds each report code sent to the orchestrator and when.
	Reported map[string]time.Time `json:"reported,omitempty"`
}

// Moved reports whether the watchdog took the block out of its window.
func (ep Episode) Moved() bool { return ep.Placement != InWindow }

// Of reports whether the episode is the run of handle with process pid.
func (ep Episode) Of(handle Handle, pid int) bool {
	return ep.Handle == handle && ep.PID == pid
}

// Report codes the orchestrator receives. A report about a doorbell's
// deferral carries the injector's own code.
const (
	ReportDraft          = CodeDraftInBox
	ReportBoxNotFound    = CodeBoxNotFound
	ReportRecoveryFailed = "recovery_failed"
	// ReportPlaceFailed: a detached block could not be placed in a tab; the
	// watchdog retries every look, apart from the recovery's attempts.
	ReportPlaceFailed = "place_failed"
	ReportMovedByUser = "moved_by_user"
	ReportUndelivered = "undelivered"
)

// MaxAttempts bounds the layout actions one episode may retry.
const MaxAttempts = 2

// WatchdogReport is the mail a stuck peer's watchdog sends its orchestrator.
// It says what the watchdog did to the pane so far, so a moved pane is not
// taken for an untouched one.
func WatchdogReport(peer Peer, ep Episode, code, detail string, screen []string) (subject, body string) {
	subject = "watchdog: " + string(peer.Handle) + " " + code
	body = "The orchestrator watchdog found " + peer.named() + " stuck: " + code + "."
	if detail != "" {
		body += " " + detail
	}
	body += " It pressed no key. " + ep.done()
	body += " The call is yours (the user still decides a dialog that asks for trust or a permission).\n\nLast lines on its screen:\n"
	for _, l := range screen {
		body += "  " + l + "\n"
	}
	return subject, body
}

// done says what the watchdog did to the pane so far, from the episode alone.
func (ep Episode) done() string {
	size := ""
	if ep.Resized {
		size = fmt.Sprintf(", resized it to %dx%d", RecoverColumns, RecoverRows)
	}
	switch ep.Placement {
	case Detached:
		return "It had detached the pane from its window" + size + "; it is not placed in a tab yet."
	case InOverflow:
		return "It had detached the pane" + size + " and placed it in the " + OverflowTab + " tab."
	case Theirs:
		return "It had detached the pane" + size + "; the user has placed it since."
	default:
		return "It moved nothing."
	}
}
