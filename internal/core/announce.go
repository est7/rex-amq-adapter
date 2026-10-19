package core

import (
	"fmt"
	"sort"
)

// HookEvent is the agent hook an announcement is decided for.
type HookEvent int

const (
	// PromptSubmitted: UserPromptSubmit, the user starts a turn.
	PromptSubmitted HookEvent = iota
	// ToolFinished: PostToolUse, between tool calls inside a turn.
	ToolFinished
	// TurnEnding: Stop, the agent is about to end its turn.
	TurnEnding
)

// AnnounceInput is what an in-band announcement depends on.
type AnnounceInput struct {
	Event HookEvent
	// Unread: ids of undrained messages now.
	Unread []string
	// Announced: ids already announced to this agent.
	Announced map[string]bool
	// StopHookActive: the agent is already continuing because a Stop hook
	// blocked it (both harnesses report this on Stop).
	StopHookActive bool
}

// Announcement says what a hook emits. Speak: add the notice to the agent's
// context; for TurnEnding this means blocking the stop. Fresh counts ids not
// announced before.
type Announcement struct {
	Speak  bool
	Unread int
	Fresh  int
}

// DecideAnnounce tells an agent about mail it has not been told about. On
// TurnEnding it also blocks once when the agent would stop with any mail
// unread, even mail announced earlier in the turn; once that block is active
// it blocks again only for new mail, so a turn cannot loop on the same mail.
func DecideAnnounce(in AnnounceInput) Announcement {
	fresh := 0
	for _, id := range in.Unread {
		if !in.Announced[id] {
			fresh++
		}
	}
	a := Announcement{Unread: len(in.Unread), Fresh: fresh}
	switch {
	case len(in.Unread) == 0:
	case fresh > 0:
		a.Speak = true
	case in.Event == TurnEnding && !in.StopHookActive:
		a.Speak = true
	}
	return a
}

// MergeAnnounced is the next ledger: everything announced so far plus add,
// limited to ids still unread, so drained ids fall out.
func MergeAnnounced(prior map[string]bool, add, stillUnread []string) []string {
	addSet := map[string]bool{}
	for _, id := range add {
		addSet[id] = true
	}
	var out []string
	for _, id := range stillUnread {
		if prior[id] || addSet[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// HookNotice is the in-band text: counts and the drain command, never
// message content, which the agent reads through amq drain.
func HookNotice(a Announcement, h Handle, envCmd string, event HookEvent) string {
	lead := fmt.Sprintf("AMQ (skill: rex-amq-adapter): %d unread message(s) for %s", a.Unread, h)
	if a.Fresh > 0 && a.Fresh < a.Unread {
		lead += fmt.Sprintf(" (%d new)", a.Fresh)
	}
	act := "Run `eval \"$(" + envCmd + ")\" && amq drain --include-body`, act on each message, and reply where a reply is expected"
	switch event {
	case TurnEnding:
		return lead + " arrived before you finished. " + act + " before ending your turn."
	case ToolFinished:
		return lead + " arrived while you work. " + act + " when you reach a sensible point in your current task."
	}
	return lead + ". " + act + "."
}

// SessionNotice is the SessionStart context: who the agent is and how to use AMQ.
func SessionNotice(h Handle, envCmd string, unread int) string {
	s := fmt.Sprintf("AMQ: you are %s. Other agents in Rex reach you with `amq send --to %s`. "+
		"Before any amq command run `eval \"$(%s)\"` (each shell needs it). "+
		"Load the rex-amq-adapter skill before messaging other agents.", h, h, envCmd)
	if unread > 0 {
		s += fmt.Sprintf(" You have %d unread message(s): drain them with `amq drain --include-body` and act on them.", unread)
	}
	return s
}
