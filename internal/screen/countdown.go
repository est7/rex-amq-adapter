package screen

import (
	"regexp"
	"strings"
)

// countdown is the line Claude Code draws on a permission dialog in an
// unattended session. When its clock runs out the dialog blocks the request
// itself and the agent goes on, so nobody needs to answer it. The clock is
// part of the match: the bare phrase is prose an agent may write or quote.
var countdown = regexp.MustCompile(`Claude Code will automatically block this request in \d+:\d{2}`)

const (
	// dialogFooter closes Claude Code's permission dialog.
	dialogFooter = "Esc to cancel"
	// footerReach is how many plain lines below the footer may still be
	// the pane's own chrome: a rule, the status line, a task list.
	footerReach = 20
	// countdownReach is how many plain lines above the footer the dialog's
	// question, its options, and a wrapped countdown take up.
	countdownReach = 10
)

// SelfDenyingCountdown reports whether screen's bottom dialog is Claude
// Code's permission dialog counting down to block the request itself. The
// dialog stands in for the input box, and only the last dialog footer near
// the bottom counts, so a countdown quoted in the transcript above an empty
// input box or above another dialog does not.
func SelfDenyingCountdown(screen string) bool {
	if Check("claude", screen) == Empty {
		return false
	}
	var lines []string
	for _, l := range strings.Split(PlainText(screen), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	foot := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-footerReach; i-- {
		if strings.Contains(lines[i], dialogFooter) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return false
	}
	// The dialog starts after an earlier dialog's footer, if one is that close.
	from := max(0, foot-countdownReach)
	for i := foot - 1; i >= from; i-- {
		if strings.Contains(lines[i], dialogFooter) {
			from = i + 1
			break
		}
	}
	above := strings.Join(lines[from:foot], " ")
	return countdown.MatchString(strings.Join(strings.Fields(above), " "))
}
