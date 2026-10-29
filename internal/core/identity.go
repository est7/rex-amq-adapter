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

// NameHandle names an agent that holds no handle yet:
// <kind>-<role>-<NN>, or <kind>-<NN> without a role, with the lowest NN
// (from 01) not in taken. The role is what the pane label says the agent is
// for (see Role). The kind is always the agent's own, so a handle never
// claims a harness it does not run on.
func NameHandle(kind Kind, label string, taken map[Handle]bool) Handle {
	role := Role(label)
	for n := 1; ; n++ {
		if h := composeHandle(kind, role, n); !taken[h] {
			return h
		}
	}
}

// Role reads an agent's role from its pane label: lowercased, runs of other
// characters folded to "-", a leading kind and a trailing number dropped. So
// "Reviewer", "reviewer-2", and a handle the adapter composed earlier
// ("codex-reviewer-01") all read as "reviewer", and Rex's default label, a
// bare kind, or "codex-10" read as no role.
func Role(label string) string {
	words := strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	if len(words) == 1 && words[0] == DefaultBlockLabel {
		return ""
	}
	if len(words) > 0 {
		if _, err := ParseKind(words[0]); err == nil {
			words = words[1:]
		}
	}
	if n := len(words); n > 0 && strings.Trim(words[n-1], "0123456789") == "" {
		words = words[:n-1]
	}
	return strings.Join(words, "-")
}

// composeHandle is <kind>[-<role>]-<NN>, the role cut to keep the handle
// within its 32 characters.
func composeHandle(kind Kind, role string, n int) Handle {
	num := fmt.Sprintf("%02d", n)
	base := string(kind)
	if room := 32 - len(base) - len("-") - len("-") - len(num); role != "" && room > 0 {
		if len(role) > room {
			role = strings.TrimRight(role[:room], "-")
		}
		base += "-" + role
	}
	return Handle(base + "-" + num)
}
