package core

import "time"

// The goal watchdog's decisions. In an unattended session a peer whose pane
// is too small cannot draw its input box, so every doorbell defers with
// box_not_found and its mail never arrives: on 2026-11-09 two Claude panes
// of 84x3 and 84x4 sat that way for 25 minutes and 2 hours, and drew the box
// again at 172x17. The watchdog moves such a pane once to a tab of its own,
// has the user's app render it, and hands it back to the injector. Every
// other stuck shape is reported to the goal agent, never touched.

const (
	// TinyRows and TinyColumns bound the sizes at which the CLIs cannot draw
	// their box; both measured cases were under them, every working pane over.
	TinyRows    = 8
	TinyColumns = 40
	// StuckFor is how long box_not_found must repeat before the pane counts
	// as stuck, so one slow redraw does not move a pane.
	StuckFor = 30 * time.Second
	// OverflowTab is the window the watchdog moves tiny panes to.
	OverflowTab = "overflow"
	// ReportLines is how many plain screen lines a report carries.
	ReportLines = 12
)

// TinyPane reports whether a pane of that size cannot draw an input box.
func TinyPane(columns, rows int) bool {
	return rows > 0 && rows < TinyRows || columns > 0 && columns < TinyColumns
}

// Undelivered reports whether mail is stuck on a missing input box: mail is
// unread and every doorbell for at least StuckFor deferred box_not_found.
// since is when that run of outcomes began.
func Undelivered(history []Attempt, unread int, now time.Time) (since time.Time, ok bool) {
	if unread == 0 {
		return time.Time{}, false
	}
	last, since, ok := lastRun(history)
	if !ok || last.Progress != Deferred || last.Code != "box_not_found" {
		return time.Time{}, false
	}
	return since, now.Sub(since) >= StuckFor
}

// Delivered reports whether the last doorbell was accepted.
func Delivered(history []Attempt) bool {
	return len(history) > 0 && history[len(history)-1].Progress == Accepted
}

// Episode is one block's recovery, persisted before and after each step so a
// restarted watchdog continues it (or ends it) instead of repeating a move.
type Episode struct {
	ID        string    `json:"id"`
	Handle    Handle    `json:"handle"`
	Block     string    `json:"block"`
	PID       int       `json:"pid"`
	Session   string    `json:"session"`
	StartedAt time.Time `json:"started_at"`
	// Window is where the block was; PrevActive the session's active window
	// before the app was asked to render Overflow.
	Window     string `json:"window,omitempty"`
	PrevActive string `json:"prev_active,omitempty"`
	Overflow   string `json:"overflow,omitempty"`
	Columns    int    `json:"columns,omitempty"`
	Rows       int    `json:"rows,omitempty"`
	// Moved: the move was issued; Rendered: the app was asked to show the
	// overflow tab and the user's window was restored; Verified: the pane
	// measured large enough with an empty box afterwards.
	Moved    bool `json:"moved,omitempty"`
	Rendered bool `json:"rendered,omitempty"`
	Verified bool `json:"verified,omitempty"`
	// Reported holds each report code sent to the goal agent and when.
	Reported map[string]time.Time `json:"reported,omitempty"`
	Outcome  string               `json:"outcome,omitempty"`
}

// Report codes the goal agent receives.
const (
	ReportTrustScreen    = "trust_screen"
	ReportBlocked        = "blocked"
	ReportDraft          = "draft_in_box"
	ReportBoxNotFound    = "box_not_found"
	ReportRecoveryFailed = "recovery_failed"
	ReportMovedByUser    = "moved_by_user"
)

// WatchdogReport is the mail a stuck peer's watchdog sends its goal agent.
func WatchdogReport(peer Peer, code, detail string, screen []string) (subject, body string) {
	subject = "watchdog: " + string(peer.Handle) + " " + code
	body = "The goal watchdog found " + peer.named() + " stuck: " + code + "."
	if detail != "" {
		body += " " + detail
	}
	body += " It pressed no key and moved nothing for this; the call is yours (the user still decides a dialog that asks for trust or a permission).\n\nLast lines on its screen:\n"
	for _, l := range screen {
		body += "  " + l + "\n"
	}
	return subject, body
}
