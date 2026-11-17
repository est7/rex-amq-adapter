package screen

import (
	"regexp"
	"strings"
)

// countdown is the warning Claude Code draws on a permission dialog in an
// unattended session, right above the dialog's question. When its clock runs
// out the dialog blocks the request itself and the agent goes on, so nobody
// needs to answer it. The whole sentence, clock included, is matched: a
// fragment of it is prose an agent may write, quote, or run.
var countdown = regexp.MustCompile(`^⚠\x{FE0F}? ?Claude Code will automatically block this request in \d+:\d{2}, to avoid holding up an unattended session$`)

// option is one numbered choice of a dialog, the selected one marked.
var option = regexp.MustCompile(`^(❯ ?)?\d+\. `)

const (
	// dialogFooter closes Claude Code's permission dialog.
	dialogFooter = "Esc to cancel"
	// footerReach is how many plain lines below the footer may still be
	// the pane's own chrome: a rule, the status line, a task list.
	footerReach = 20
	// warningReach is how many lines a narrow pane wraps the warning into.
	warningReach = 6
)

// SelfDenyingCountdown reports whether screen's bottom dialog is Claude
// Code's permission dialog counting down to block the request itself: the
// last dialog footer near the bottom, its numbered options and question
// right above it, and right above those the countdown warning, starting on
// a line of its own. The dialog stands in for the input box. A countdown
// quoted anywhere else (the transcript, an earlier dialog, the command or
// description the dialog asks about) does not count.
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
	question := foot - 1
	for question >= 0 && option.MatchString(lines[question]) {
		question--
	}
	if foot < 0 || question < 0 || question == foot-1 || !strings.HasSuffix(lines[question], "?") {
		return false
	}
	for w := question - 1; w >= 0 && w >= question-warningReach; w-- {
		if strings.HasPrefix(lines[w], "⚠") {
			warning := strings.Join(strings.Fields(strings.Join(lines[w:question], " ")), " ")
			return countdown.MatchString(warning)
		}
	}
	return false
}
