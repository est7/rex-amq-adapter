package screen

import (
	"regexp"
	"slices"
	"strings"
)

// countdown is the warning Claude Code draws on a permission dialog in an
// unattended session, followed by the dialog's question ("Do you want to
// proceed?", "Do you want to make this edit to <file>?"). When its clock runs
// out the dialog blocks the request itself and the agent goes on, so nobody
// needs to answer it. The whole warning, clock included, is matched: a
// fragment of it is prose an agent may write, quote, or run.
var countdown = regexp.MustCompile(`^⚠\x{FE0F}? ?Claude Code will automatically block this request in \d+:\d{2}, to avoid holding up an unattended session Do you want [^⚠│]*\?$`)

// option is one numbered choice of a dialog, the selected one marked, and
// firstOption the first of them.
var (
	option      = regexp.MustCompile(`^(❯ ?)?\d+\. `)
	firstOption = regexp.MustCompile(`^(❯ ?)?1\. `)
)

const (
	// dialogFooter closes Claude Code's permission dialog.
	dialogFooter = "Esc to cancel"
	// footerReach is how many plain lines below the footer may still be
	// the pane's own chrome: a rule, the status line, a task list.
	footerReach = 20
	// optionsReach is how many lines a narrow pane wraps the dialog's
	// numbered options into.
	optionsReach = 12
	// warningReach is how many lines a narrow pane wraps the warning and the
	// question into.
	warningReach = 10
)

// SelfDenyingCountdown reports whether screen's bottom dialog is Claude
// Code's permission dialog counting down to block the request itself: the
// last dialog footer near the bottom, with only the pane's chrome below it,
// its numbered options above it, and right above those the countdown warning,
// starting on a line of its own, and the dialog's question. A countdown quoted
// anywhere else (the transcript, an earlier dialog, the command or
// description the dialog asks about) does not count.
func SelfDenyingCountdown(screen string) bool {
	return selfDenyingCountdownFromPlain(PlainText(screen))
}

func selfDenyingCountdownFromPlain(plain string) bool {
	var lines []string
	for _, l := range strings.Split(plain, "\n") {
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
	if foot < 0 || slices.ContainsFunc(lines[foot+1:], interactive) {
		return false
	}
	first := -1
	for i := foot - 1; i >= 0 && i >= foot-optionsReach; i-- {
		if strings.Contains(lines[i], dialogFooter) {
			break // an earlier dialog's footer: the bottom one has no options
		}
		if firstOption.MatchString(lines[i]) {
			first = i
			break
		}
	}
	// The warning starts on a line of its own; the question follows it, and
	// either may wrap.
	for w := first - 1; w >= 0 && w >= first-warningReach; w-- {
		if strings.HasPrefix(lines[w], "⚠") {
			warning := strings.Join(strings.Fields(strings.Join(lines[w:first], " ")), " ")
			return countdown.MatchString(warning)
		}
	}
	return false
}

// interactive reports whether l belongs to something waiting for input (an
// input box, a dialog, a menu) rather than the pane's chrome. Below the
// footer it means the countdown dialog is not the one at the bottom.
func interactive(l string) bool {
	for _, p := range []string{"❯", "›", "│", "╭", "╰", "☐", "⚠", "Esc"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return option.MatchString(l) || strings.HasSuffix(l, "?")
}
