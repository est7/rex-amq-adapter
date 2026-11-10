package core

import (
	"fmt"
	"strings"
)

// Summon on send: an agent addresses a peer by handle or role, and the
// adapter delivers to it, resumes it, or starts it, inside fixed guards.
// The guards are the boundary (est9: trust the main agent; cap the count,
// refuse cycles, limit depth, keep it under the grant).

const (
	// SummonCap is how many live agents one Rex session may hold before a
	// summon refuses to resume or start another: a session is one task, and
	// six panes is what one person can still follow.
	SummonCap = 6
	// MaxDepth is how many summons may lie between the user and an agent:
	// the user's agents are 0, what they summon 1, and 2 is the end.
	MaxDepth = 2
)

// RequestKinds are the amq message kinds that may summon. A reply or an FYI
// delivers only.
var RequestKinds = map[string]bool{"question": true, "review_request": true, "todo": true, "brainstorm": true}

// DefaultHarness is the harness a role starts in when the summoner names
// none: review roles on another family than the usual implementer.
func DefaultHarness(role string) Kind {
	switch role {
	case "reviewer", "editor", "qa", "explorer":
		return Codex
	}
	return Claude
}

// SummonCheck is what the guards decide for a resume or start.
type SummonCheck struct {
	Refused bool
	Reason  string
}

// GuardSummon applies the guards to a summoner at depth, with chain its
// handles from the user's agent down to itself (itself last), wanting
// target (a handle, or a role), with live agents in the session.
func GuardSummon(kind string, chain []Peer, depth int, target string, live int) SummonCheck {
	if !RequestKinds[kind] {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("a %s delivers but never summons: only a request (question, review_request, todo, brainstorm) starts or resumes an agent", kind)}
	}
	for _, p := range chain {
		if string(p.Handle) == target || p.Role != "" && p.Role == target {
			return SummonCheck{Refused: true, Reason: fmt.Sprintf("cycle: %s is already in the summon chain %s", target, chainText(chain))}
		}
	}
	if depth+1 > MaxDepth {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("too deep: you are at depth %d and the limit is %d (the user's agents are 0)", depth, MaxDepth)}
	}
	if live >= SummonCap {
		return SummonCheck{Refused: true, Reason: fmt.Sprintf("the session already holds %d live agents, the cap; deliver to one of them or wait for one to finish", live)}
	}
	return SummonCheck{}
}

func chainText(chain []Peer) string {
	parts := make([]string, len(chain))
	for i, p := range chain {
		parts[i] = p.named()
	}
	return strings.Join(parts, " -> ")
}

// LooksLikeHandle reports whether s has a handle's shape, <kind>-<NN>, as
// opposed to a role.
func LooksLikeHandle(s string) bool {
	i := strings.LastIndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	if _, err := ParseKind(s[:i]); err != nil {
		return false
	}
	return strings.Trim(s[i+1:], "0123456789") == ""
}

// SummonLine is what the summoner lists in its hand-back for each summon.
func SummonLine(action string, target string, handle Handle, parent Handle, depth int, why string) string {
	return fmt.Sprintf("summon: %s %s -> %s (parent %s, depth %d): %s", action, target, handle, parent, depth, why)
}
