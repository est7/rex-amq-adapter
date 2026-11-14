package core

import (
	"regexp"
	"strings"
)

// WindowMode says who names a Rex tab (window), as the adapter's tab ledger
// records it. A tab the ledger does not know has the zero mode. A tab names
// no crew: a crew is its Rex session's.
type WindowMode string

const (
	// WindowAuto is a follower tab: the adapter named it, and while one
	// agent is in it the tab shows that agent's pane label.
	WindowAuto WindowMode = "auto"
	// WindowManual is a tab the user renamed: the adapter leaves it alone.
	WindowManual WindowMode = "manual"
)

var placeholderTab = regexp.MustCompile(`^Window [0-9a-f]{8}$`)

// PlaceholderTab reports whether label is the name Rex gives a window
// nobody named: "Window " and eight lowercase hex digits ("Window 4c6fe88d").
func PlaceholderTab(label string) bool { return placeholderTab.MatchString(label) }

// labelTask is the task of a pane label the adapter wrote for an agent
// ("<role>(<handle>) | <task>"); "" for any other label or one with no task.
func labelTask(label string) string {
	head, tail, ok := strings.Cut(label, "|")
	if _, _, adapter := adapterHead(head); !ok || !adapter {
		return ""
	}
	return Task(tail)
}

// TabState is a tab's entry in the adapter's tab ledger: its mode, and the
// label the adapter last gave it. The zero TabState is a tab the ledger does
// not know.
type TabState struct {
	Mode WindowMode
	Last string
}

// FollowTab decides what a tab shows: want, the pane label of the agent in
// it, while one agent is in it. t is the tab's ledger entry, label what the
// tab shows now, and agents how many of its blocks run a live agent; panes
// without one beside it (a preview, a shell) do not count. It returns the
// entry to record and the label to rename the tab to, "" for none.
//
// Only a follower tab follows: one the ledger records as auto, or one it
// does not know that still shows Rex's placeholder, which it then records
// as auto. A follower whose label is neither the placeholder, the label the
// adapter last gave it, nor want was renamed by someone else: it becomes
// manual and keeps its name. A follower of several agents, which would
// contend for it, drops the agent's label it shows to that label's task;
// one with no agent changes nothing. A manual tab and an unknown tab with a
// name are never renamed.
func FollowTab(t TabState, label string, agents int, want string) (TabState, string) {
	switch t.Mode {
	case "":
		if !PlaceholderTab(label) {
			return t, ""
		}
		t = TabState{Mode: WindowAuto, Last: label}
	case WindowAuto:
		if label != t.Last && label != want && !PlaceholderTab(label) {
			return TabState{Mode: WindowManual, Last: t.Last}, ""
		}
	default:
		return t, ""
	}
	if agents > 1 {
		// Several agents share the tab, so one agent's label would read as
		// everyone's (who's TAB column): keep only the task it names, the
		// team's work, and nothing when it names none.
		if task := labelTask(label); task != "" {
			t.Last = task
			return t, task
		}
		return t, ""
	}
	if agents != 1 || want == "" {
		return t, ""
	}
	t.Last = want
	if label == want {
		return t, ""
	}
	return t, want
}
