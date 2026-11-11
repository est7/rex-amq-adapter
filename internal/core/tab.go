package core

import (
	"regexp"
	"strings"
)

// WindowMode says who names a Rex tab (window), as the adapter's tab ledger
// records it. A tab the ledger does not know has the zero mode.
type WindowMode string

const (
	// WindowAuto is a follower tab: the adapter named it, and while it holds
	// one block it shows the pane label of the agent in that block.
	WindowAuto WindowMode = "auto"
	// WindowCrew is a tab claimed for a crew (join, tab): its label names
	// the crew, optionally "<crew> | <topic>".
	WindowCrew WindowMode = "crew"
	// WindowNarrator is reserved for a session's narrator tab.
	WindowNarrator WindowMode = "narrator"
	// WindowManual is a tab the user renamed: the adapter leaves it alone.
	WindowManual WindowMode = "manual"
)

// NarratorTab is the label, and crew-tab name, of a narrator tab.
const NarratorTab = "narrator"

// ClaimedTabMode is the mode of a tab claimed under name, a crew-tab name
// (crew.TabName): a narrator tab, else a crew's.
func ClaimedTabMode(name string) WindowMode {
	if name == NarratorTab {
		return WindowNarrator
	}
	return WindowCrew
}

var placeholderTab = regexp.MustCompile(`^Window [0-9a-f]{8}$`)

// PlaceholderTab reports whether label is the name Rex gives a window
// nobody named: "Window " and eight lowercase hex digits ("Window 4c6fe88d").
func PlaceholderTab(label string) bool { return placeholderTab.MatchString(label) }

// AdapterLabel reports whether label is a pane label the adapter writes for
// an agent ("<handle>" or "<role>(<handle>)", with or without " | <task>"),
// which names an agent, never a crew.
func AdapterLabel(label string) bool {
	head, _, _ := strings.Cut(label, "|")
	_, _, ok := adapterHead(head)
	return ok
}

// TabState is a tab's entry in the adapter's tab ledger: its mode, and the
// label the adapter last gave it. The zero TabState is a tab the ledger does
// not know.
type TabState struct {
	Mode WindowMode
	Last string
}

// FollowTab decides whether a tab shows want, the pane label of the agent
// in it. t is the tab's ledger entry, label what the tab shows now, and
// blocks how many blocks it holds. It returns the entry to record and
// whether to rename the tab to want.
//
// Only a follower tab follows: one the ledger records as auto, or one it
// does not know that still shows Rex's placeholder, which it then records
// as auto. A follower whose label is neither the placeholder, the label the
// adapter last gave it, nor want was renamed by someone else: it becomes
// manual and keeps its name. A crew, narrator, or manual tab, an unknown
// tab with a name, and a tab of several blocks (whose agents would contend
// for it) are never renamed.
func FollowTab(t TabState, label string, blocks int, want string) (TabState, bool) {
	switch t.Mode {
	case "":
		if !PlaceholderTab(label) {
			return t, false
		}
		t = TabState{Mode: WindowAuto, Last: label}
	case WindowAuto:
		if label != t.Last && label != want && !PlaceholderTab(label) {
			return TabState{Mode: WindowManual, Last: t.Last}, false
		}
	default:
		return t, false
	}
	if blocks != 1 || want == "" {
		return t, false
	}
	t.Last = want
	return t, label != want
}
