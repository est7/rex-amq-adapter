// Package core holds the adapter's policy: which handle an agent gets, whether
// a doorbell may be typed, what to announce from a hook, and the text agents
// read. It takes observations and returns decisions; it performs no I/O.
package core

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind is an agent harness the adapter can adopt.
type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
	// Agy is the Antigravity CLI.
	Agy Kind = "agy"
	// Pi is the pi coding agent.
	Pi Kind = "pi"
)

// InProcess reports whether the agent receives mail inside its own process
// (Pi's extension calls pi.sendMessage). Such an agent has no waker: nothing
// is ever typed into its terminal.
func (k Kind) InProcess() bool { return k == Pi }

// ParseKind accepts the --agent value hooks are installed with.
func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case Claude, Codex, Agy, Pi:
		return k, nil
	}
	return "", fmt.Errorf("unsupported agent kind %q (want claude, codex, agy, or pi)", s)
}

// Handle is an AMQ agent handle. amq accepts [a-z0-9_-]+; the adapter also
// caps the length so a handle stays readable as a Rex block label.
type Handle string

var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func ParseHandle(s string) (Handle, error) {
	if !handleRe.MatchString(s) {
		return "", fmt.Errorf("invalid handle %q: want [a-z0-9][a-z0-9_-]{0,31}", s)
	}
	return Handle(s), nil
}

// DefaultBlockLabel is the label Rex gives every terminal block that was not
// labeled explicitly, including `rex run -- cmd` blocks.
const DefaultBlockLabel = "shell"

// HandleRequest is everything handle choice depends on.
type HandleRequest struct {
	Kind Kind
	// Label is the block's current label.
	Label string
	// Sticky is the handle this block's previous agent had, or "".
	Sticky Handle
	// Live holds handles bound to another live agent. They are never shared.
	Live map[Handle]bool
	// Encumbered holds handles an automatic name must avoid: reserved for a
	// departed agent, or owning a mailbox with undrained mail.
	Encumbered map[Handle]bool
}

// HandleChoice is the chosen handle and whether the block label should be
// set to it so the handle is visible in Rex.
type HandleChoice struct {
	Handle   Handle
	SetLabel bool
}

// ChooseHandle names an agent:
//
//  1. An explicit block label that is a valid handle (case-insensitive) is the
//     user's choice and is used as is, mail waiting or not. If another live
//     agent holds it, the first <label>-N that is neither live nor encumbered
//     is used and the label updated.
//  2. Otherwise the block's previous handle is kept unless another live agent
//     took it, so mail addressed to it still reaches this block.
//  3. Otherwise <kind> or <kind>-N, skipping live and encumbered handles, so a
//     new agent never inherits someone else's unread mail.
//
// The label is only written over Rex's default label, never over a custom
// label that is not a valid handle.
func ChooseHandle(r HandleRequest) HandleChoice {
	defaultLabel := r.Label == "" || r.Label == DefaultBlockLabel
	if !defaultLabel {
		if h, err := ParseHandle(strings.ToLower(r.Label)); err == nil {
			if !r.Live[h] {
				return HandleChoice{Handle: h}
			}
			return HandleChoice{Handle: firstFree(string(h), r.Live, r.Encumbered), SetLabel: true}
		}
	}
	if r.Sticky != "" && !r.Live[r.Sticky] {
		return HandleChoice{Handle: r.Sticky, SetLabel: defaultLabel}
	}
	return HandleChoice{Handle: firstFree(string(r.Kind), r.Live, r.Encumbered), SetLabel: defaultLabel}
}

// firstFree returns base, base-2, base-3 … whichever is first not in either set.
func firstFree(base string, a, b map[Handle]bool) Handle {
	free := func(h Handle) bool { return !a[h] && !b[h] }
	if h := Handle(base); free(h) {
		return h
	}
	// Keep base-N within the 32-character handle limit.
	if len(base) > 28 {
		base = strings.TrimRight(base[:28], "-_")
	}
	for i := 2; ; i++ {
		if h := Handle(fmt.Sprintf("%s-%d", base, i)); free(h) {
			return h
		}
	}
}
