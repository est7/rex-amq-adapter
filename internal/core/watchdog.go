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

// Stuck reports whether mail is undelivered: it is unread and every doorbell
// for at least StuckFor deferred with the same code. code is that reason as
// the injector recorded it (box_not_found, draft_in_box, agent_blocked,
// trust_screen, …) and since when the run of outcomes began. Reports follow
// from any code; a move follows only from box_not_found on a tiny pane.
func Stuck(history []Attempt, unread int, now time.Time) (code string, since time.Time, ok bool) {
	if unread == 0 {
		return "", time.Time{}, false
	}
	last, since, ok := lastRun(history)
	if !ok || last.Progress != Deferred {
		return "", time.Time{}, false
	}
	return last.Code, since, now.Sub(since) >= StuckFor
}

// MayMove reports whether a stuck pane is the one shape the watchdog fixes
// itself: doorbells find no input box and the pane is too small to draw one.
func MayMove(code string, columns, rows int) bool {
	return code == "box_not_found" && TinyPane(columns, rows)
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
	// Restored: the app shows PrevActive again after rendering.
	Restored bool `json:"restored,omitempty"`
	Verified bool `json:"verified,omitempty"`
	// Attempts counts layout actions that failed; past MaxAttempts the
	// watchdog stops acting and reports.
	Attempts int `json:"attempts,omitempty"`
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
	ReportUndelivered    = "undelivered"
	ReportNoClient       = "no_app_client"
	ReportRestoreFailed  = "restore_failed"
)

// MaxAttempts bounds the layout actions one episode may retry.
const MaxAttempts = 2

// WatchdogReport is the mail a stuck peer's watchdog sends its goal agent.
// It says what the watchdog did to the pane so far, so a moved pane is not
// taken for an untouched one.
func WatchdogReport(peer Peer, ep Episode, code, detail string, screen []string) (subject, body string) {
	subject = "watchdog: " + string(peer.Handle) + " " + code
	body = "The goal watchdog found " + peer.named() + " stuck: " + code + "."
	if detail != "" {
		body += " " + detail
	}
	body += " It pressed no key."
	switch {
	case ep.Moved && ep.Rendered:
		body += " It had moved the pane to the " + OverflowTab + " tab and had the app render it"
		if ep.Restored {
			body += ", then put the app back on its previous window."
		} else {
			body += "; the app may still show that tab."
		}
	case ep.Moved:
		body += " It had moved the pane to the " + OverflowTab + " tab; the app has not rendered it."
	default:
		body += " It moved nothing."
	}
	body += " The call is yours (the user still decides a dialog that asks for trust or a permission).\n\nLast lines on its screen:\n"
	for _, l := range screen {
		body += "  " + l + "\n"
	}
	return subject, body
}
