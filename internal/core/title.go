package core

import (
	"regexp"
	"strings"
)

// A Rex session is one Epic of the human's: its label names the Epic
// ("auth-refresh") and belongs to the human. Agents never rename it for their
// own task; the task goes in their pane label (PaneLabel).

var placeholderSession = regexp.MustCompile(`^Session [0-9a-f]{8}$`)

// PlaceholderSession reports whether a Rex session label names no Epic: blank,
// or the name Rex gives a session nobody named ("Session 01a1cad4").
func PlaceholderSession(label string) bool {
	label = strings.TrimSpace(label)
	return label == "" || placeholderSession.MatchString(label)
}

// EpicLabel is the Epic a Rex session label names, made fit for a title: one
// line, single spaces, "|" written as "/" so the title keeps a single
// separator. "" when the label names no Epic.
func EpicLabel(label string) string {
	if PlaceholderSession(label) {
		return ""
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(label, "|", "/")), " ")
}

// ConversationTitle is the title an agent's harness shows for its
// conversation (the one /resume lists): its pane label, with the Epic its
// session is named for in brackets before the task, "<role>(<handle>) |
// [<epic>] <task>". Without a named Epic it is the pane label itself. The head
// stays before the separator, so TitleNamesHandle still finds the handle.
func ConversationTitle(h Handle, role, epic, task string) string {
	e := EpicLabel(epic)
	if e == "" {
		return PaneLabel(h, role, task)
	}
	title := PaneHead(h, role) + PaneSeparator + "[" + e + "]"
	if t := Task(task); t != "" {
		title += " " + t
	}
	return title
}

// TakesTitle reports whether a hook reply can name the harness's own
// conversation (Claude Code's sessionTitle, Pi's setSessionName). Codex takes
// none without its shared daemon, and Antigravity only asks for its terminal
// window title, which is not its conversation's name.
func (k Kind) TakesTitle() bool { return k == Claude || k == Pi }

// TitleStatus is how far an agent's conversation title got toward its
// harness.
type TitleStatus string

const (
	// TitleDesired: the title changed and has not been offered to the agent's
	// current conversation yet; the next hook that can carry it will.
	TitleDesired TitleStatus = "desired"
	// TitleOffered: a hook reply carried the title to the harness. Whether the
	// harness showed it is not known.
	TitleOffered TitleStatus = "offered"
	// TitleUnsupported: the harness takes no title from the adapter.
	TitleUnsupported TitleStatus = "unsupported"
)

// TitleState is an agent's conversation title as the adapter tracks it:
// Desired is what it should be, Offered what a hook last carried to the
// agent's current conversation ("" for none).
type TitleState struct {
	Status  TitleStatus `json:"status"`
	Desired string      `json:"desired"`
	Offered string      `json:"offered,omitempty"`
}

// TrackTitle is the state of a title desired for a harness of kind k, given
// what was last offered to its current conversation. A harness that takes no
// title is never counted as offered one, whatever its terminal shows.
func TrackTitle(k Kind, desired, offered string) TitleState {
	switch {
	case !k.TakesTitle():
		return TitleState{Status: TitleUnsupported, Desired: desired}
	case offered == desired:
		return TitleState{Status: TitleOffered, Desired: desired, Offered: offered}
	}
	return TitleState{Status: TitleDesired, Desired: desired, Offered: offered}
}
